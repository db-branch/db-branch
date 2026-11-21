package module_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/module"
)

var ctx = context.Background()

type fakeApt struct {
	installed  map[string]string
	candidates map[string]string
	ran        []string
	failOn     string
}

func (f *fakeApt) fail(op string) error {
	if f.failOn == op {
		return errors.New(op + " failed")
	}
	return nil
}

func (f *fakeApt) Installed(_ context.Context, prefix string) (map[string]string, error) {
	out := map[string]string{}
	for p, v := range f.installed {
		if strings.HasPrefix(p, prefix) {
			out[p] = v
		}
	}
	return out, f.fail("installed")
}
func (f *fakeApt) Candidate(_ context.Context, pkg string) (string, error) {
	return f.candidates[pkg], f.fail("candidate")
}
func (f *fakeApt) Search(_ context.Context, prefix string) (map[string]string, error) {
	return map[string]string{"db-branch-proxysql": "ProxySQL routing", "db-branch-Bad": "x", "db-branch-audit": "Audit trail"}, f.fail("search")
}
func (f *fakeApt) Install(_ context.Context, pkg, v string) error {
	f.ran = append(f.ran, "install "+pkg+"="+v)
	if err := f.fail("install"); err != nil {
		return err
	}
	f.installed[pkg] = v
	return nil
}
func (f *fakeApt) Remove(_ context.Context, pkg string) error {
	f.ran = append(f.ran, "remove "+pkg)
	delete(f.installed, pkg)
	return f.fail("remove")
}

func newService(t *testing.T) (*module.Service, *fakeApt) {
	apt := &fakeApt{installed: map[string]string{"db-branch": "1.0"}, candidates: map[string]string{"db-branch-proxysql": "1.0"}}
	st := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	return &module.Service{Store: st, Packages: apt}, apt
}

func TestInstall_いま入れられる版を記録し各ノードがその版に揃える(t *testing.T) {
	s, apt := newService(t)
	m, err := s.Install(ctx, "proxysql", "")
	if err != nil || m.Version != "1.0" || !m.Wanted {
		t.Fatalf("Install = %+v %v", m, err)
	}
	changed, err := s.Sync(ctx)
	if err != nil || !changed || strings.Join(apt.ran, ",") != "install db-branch-proxysql=1.0" {
		t.Errorf("sync: %v %v %v", changed, err, apt.ran)
	}
	if changed, _ := s.Sync(ctx); changed {
		t.Error("in line already: nothing to do")
	}
	// Another node's version differs: it is brought to the recorded one.
	apt.installed["db-branch-proxysql"] = "0.9"
	apt.ran = nil
	_, _ = s.Sync(ctx)
	if strings.Join(apt.ran, ",") != "install db-branch-proxysql=1.0" {
		t.Errorf("ran %v", apt.ran)
	}
}

func TestInstall_版を指定でき存在しない名前やパッケージは断る(t *testing.T) {
	s, _ := newService(t)
	if m, err := s.Install(ctx, "auth", "2.1"); err != nil || m.Version != "2.1" {
		t.Errorf("explicit version: %+v %v", m, err)
	}
	if _, err := s.Install(ctx, "Bad Name", ""); !errors.Is(err, domain.ErrInvalidModule) {
		t.Errorf("bad name: %v", err)
	}
	if _, err := s.Install(ctx, "nope", ""); !errors.Is(err, module.ErrUnavailable) {
		t.Errorf("no package: %v", err)
	}
	s2, apt := newService(t)
	apt.failOn = "candidate"
	if _, err := s2.Install(ctx, "proxysql", ""); err == nil {
		t.Error("apt failure: want error")
	}
}

func TestRemove_全ノードから消す(t *testing.T) {
	s, apt := newService(t)
	_, _ = s.Install(ctx, "proxysql", "")
	_, _ = s.Sync(ctx)
	if err := s.Remove(ctx, "proxysql"); err != nil {
		t.Fatal(err)
	}
	apt.ran = nil
	if changed, err := s.Sync(ctx); !changed || err != nil || strings.Join(apt.ran, ",") != "remove db-branch-proxysql" {
		t.Errorf("sync: %v %v %v", changed, err, apt.ran)
	}
	list, _ := s.List(ctx)
	if len(list) != 1 || list[0].Wanted {
		t.Errorf("list = %+v", list)
	}
	if err := s.Remove(ctx, "proxysql"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("removed twice: %v", err)
	}
}

func TestAdopt_手で入れたモジュールをクラスタのものにする(t *testing.T) {
	s, apt := newService(t)
	apt.installed["db-branch-proxysql"] = "0.9"
	apt.installed["db-branch-Weird"] = "1"
	if err := s.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx)
	if len(list) != 1 || list[0].Name != "proxysql" || list[0].Version != "0.9" || !list[0].Wanted {
		t.Errorf("list = %+v", list)
	}
	// A decision already recorded (here: removed) wins over what is installed.
	_ = s.Remove(ctx, "proxysql")
	_ = s.Adopt(ctx)
	if list, _ := s.List(ctx); list[0].Wanted {
		t.Error("adopt must not undo a removal")
	}
}

func Test失敗はそのまま返す(t *testing.T) {
	for _, op := range []string{"installed", "install", "remove"} {
		s, apt := newService(t)
		_, _ = s.Install(ctx, "proxysql", "")
		if op == "remove" {
			apt.installed["db-branch-proxysql"] = "1.0"
			_ = s.Remove(ctx, "proxysql")
		}
		apt.failOn = op
		if _, err := s.Sync(ctx); err == nil {
			t.Errorf("sync with %s failing: want error", op)
		}
	}
	s, apt := newService(t)
	apt.failOn = "installed"
	if err := s.Adopt(ctx); err == nil {
		t.Error("adopt: want error")
	}
}

type brokenStore struct{ domain.ModuleStore }

func (brokenStore) ListModules(context.Context) ([]domain.ModuleSpec, error) {
	return nil, errors.New("raft down")
}

func Test記録を読めなければエラー(t *testing.T) {
	s := &module.Service{Store: brokenStore{}, Packages: &fakeApt{installed: map[string]string{}}}
	if _, err := s.Sync(ctx); err == nil {
		t.Error("sync")
	}
	if err := s.Remove(ctx, "x"); err == nil {
		t.Error("remove")
	}
	if err := s.Adopt(ctx); err == nil {
		t.Error("adopt")
	}
}

func TestAvailable_リポジトリにあるモジュールを説明つきで返す(t *testing.T) {
	s, apt := newService(t)
	got, err := s.Available(ctx)
	if err != nil || len(got) != 2 || got["audit"] != "Audit trail" {
		t.Errorf("Available = %v %v", got, err)
	}
	apt.failOn = "search"
	if _, err := s.Available(ctx); err == nil {
		t.Error("want error")
	}
}

func TestSync_ロールアウトで入れた版は記録が古くても戻さない(t *testing.T) {
	s, apt := newService(t)
	_, _ = s.Install(ctx, "proxysql", "1.0")
	_, _ = s.Sync(ctx)
	// A rollout moved this node to 1.1; the record catches up when it ends.
	apt.installed["db-branch-proxysql"] = "1.1"
	s.Rolled = func(context.Context) map[string]string { return map[string]string{"db-branch-proxysql": "1.1"} }
	apt.ran = nil
	if changed, err := s.Sync(ctx); changed || err != nil || len(apt.ran) != 0 {
		t.Errorf("sync: %v %v %v", changed, err, apt.ran)
	}
	// Another version still converges to the record.
	apt.installed["db-branch-proxysql"] = "0.9"
	_, _ = s.Sync(ctx)
	if strings.Join(apt.ran, ",") != "install db-branch-proxysql=1.0" {
		t.Errorf("ran %v", apt.ran)
	}
}

func TestInstallRuntime_エンジン用のランタイムモジュールを記録する(t *testing.T) {
	s, apt := newService(t)
	apt.candidates["db-branch-rt-clickhouse"] = "0.3.0"

	m, err := s.InstallRuntime(ctx, "clickhouse", "", "")
	want := domain.ModuleSpec{Name: "rt-clickhouse", Version: "0.3.0", Wanted: true, Kind: domain.ModuleKindRuntime, Source: domain.SourceAPT}
	if err != nil || m != want {
		t.Fatalf("InstallRuntime = %+v, %v; want %+v", m, err, want)
	}
	changed, err := s.Sync(ctx)
	if err != nil || !changed || strings.Join(apt.ran, ",") != "install db-branch-rt-clickhouse=0.3.0" {
		t.Errorf("sync: %v %v %v", changed, err, apt.ran)
	}
}

func TestInstallRuntime_APT以外の配布は版が必須でAPTの同期の対象にしない(t *testing.T) {
	s, apt := newService(t)
	if _, err := s.InstallRuntime(ctx, "duckdb", "", domain.SourceLocal); err == nil {
		t.Error("a local runtime module without a version was accepted")
	}
	m, err := s.InstallRuntime(ctx, "duckdb", "0.1.0", domain.SourceLocal)
	if err != nil || m.Source != domain.SourceLocal || m.Version != "0.1.0" {
		t.Fatalf("InstallRuntime = %+v, %v", m, err)
	}
	if changed, err := s.Sync(ctx); err != nil || changed || len(apt.ran) != 0 {
		t.Errorf("APT was used for a local module: %v %v %v", changed, err, apt.ran)
	}
	if _, err := s.InstallRuntime(ctx, "duckdb", "0.1.0", "carrier-pigeon"); err == nil {
		t.Error("an unknown source was accepted")
	}
}

func TestInstallRuntime_不正なエンジン名やパッケージなしは断る(t *testing.T) {
	s, _ := newService(t)
	for _, e := range []string{"mysql", "postgres", "Bad Name", ""} {
		if _, err := s.InstallRuntime(ctx, e, "1.0", ""); !errors.Is(err, domain.ErrInvalidModule) {
			t.Errorf("InstallRuntime(%q) = %v, want ErrInvalidModule", e, err)
		}
	}
	if _, err := s.InstallRuntime(ctx, "nopackage", "", ""); !errors.Is(err, module.ErrUnavailable) {
		t.Errorf("no package: %v", err)
	}
}

func TestInstallRuntime_全ノードがプロトコル21になるまで記録しない(t *testing.T) {
	s, apt := newService(t)
	apt.candidates["db-branch-rt-clickhouse"] = "0.3.0"
	nodes := []domain.Node{{Name: "a", Protocol: 21}, {Name: "b", Protocol: 18}}
	s.Nodes = func(context.Context) ([]domain.Node, error) { return nodes, nil }

	if _, err := s.InstallRuntime(ctx, "clickhouse", "", ""); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("with a node at protocol 18: %v, want ErrClusterUpgrading", err)
	}
	if list, _ := s.List(ctx); len(list) != 0 {
		t.Errorf("recorded anyway: %+v", list)
	}
	nodes[1].Protocol = 21
	if _, err := s.InstallRuntime(ctx, "clickhouse", "", ""); err != nil {
		t.Errorf("every node at 21: %v", err)
	}
	s.Nodes = func(context.Context) ([]domain.Node, error) { return nil, errors.New("down") }
	if _, err := s.InstallRuntime(ctx, "clickhouse", "", ""); err == nil {
		t.Error("an unreadable node list was taken as permission")
	}
	// plain modules never wait for it
	s.Nodes = func(context.Context) ([]domain.Node, error) { return []domain.Node{{Name: "old", Protocol: 18}}, nil }
	if _, err := s.Install(ctx, "proxysql", ""); err != nil {
		t.Errorf("Install of a plain module = %v", err)
	}
}

func TestランタイムモジュールのパッケージはAdoptされずAvailableにも出ない(t *testing.T) {
	s, apt := newService(t)
	apt.installed["db-branch-rt-clickhouse"] = "0.3.0"
	apt.installed["db-branch-auth"] = "1.0"
	if err := s.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx)
	if len(list) != 1 || list[0].Name != "auth" {
		t.Errorf("adopted = %+v, want only the plain module", list)
	}
}

func TestRemove_ランタイムモジュールも名前で消せる(t *testing.T) {
	s, apt := newService(t)
	apt.candidates["db-branch-rt-clickhouse"] = "0.3.0"
	_, _ = s.InstallRuntime(ctx, "clickhouse", "", "")
	_, _ = s.Sync(ctx)
	apt.ran = nil
	if err := s.Remove(ctx, "rt-clickhouse"); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.Sync(ctx); err != nil || !changed || strings.Join(apt.ran, ",") != "remove db-branch-rt-clickhouse" {
		t.Errorf("sync after remove: %v %v %v", changed, err, apt.ran)
	}
}
