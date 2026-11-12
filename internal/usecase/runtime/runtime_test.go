package runtime_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/runtime"
)

var (
	ctx     = context.Background()
	mysql84 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}
	art     = domain.Artifact{URL: "https://example.test/mysql.tar.xz", SHA256: "abc", Size: 10}
)

type fakeCatalog struct{ arts map[string]domain.Artifact }

func (c fakeCatalog) Lookup(ref domain.RuntimeRef, arch string) ([]domain.Artifact, error) {
	a, ok := c.arts[ref.String()+"/"+arch]
	if !ok {
		return nil, domain.ErrRuntimeNotFound
	}
	if extra, ok := c.arts[ref.String()+"/"+arch+"/1"]; ok {
		return []domain.Artifact{a, extra}, nil
	}
	return []domain.Artifact{a}, nil
}

func (c fakeCatalog) Available(engine domain.Engine, arch string) []domain.RuntimeRef {
	return []domain.RuntimeRef{{Engine: engine, Version: arch}}
}

type fakeCache struct {
	got []domain.Artifact
	err error
}

func (c *fakeCache) Get(_ context.Context, a domain.Artifact) (string, error) {
	c.got = append(c.got, a)
	if c.err != nil {
		return "", c.err
	}
	return "/cache/" + a.SHA256, nil
}

type fakeStore struct {
	installed map[domain.RuntimeRef]string
	removed   []domain.RuntimeRef
	err       error
}

func (s *fakeStore) List(context.Context) ([]domain.RuntimeRef, error) {
	refs := make([]domain.RuntimeRef, 0, len(s.installed))
	for r := range s.installed {
		refs = append(refs, r)
	}
	return refs, s.err
}

func (s *fakeStore) Has(_ context.Context, ref domain.RuntimeRef) (bool, error) {
	_, ok := s.installed[ref]
	return ok, s.err
}

func (s *fakeStore) Install(_ context.Context, ref domain.RuntimeRef, archives []string) error {
	s.installed[ref] = strings.Join(archives, ",")
	return nil
}

func (s *fakeStore) Remove(_ context.Context, ref domain.RuntimeRef) error {
	s.removed = append(s.removed, ref)
	delete(s.installed, ref)
	return nil
}

func newService() (runtime.Service, *fakeCache, *fakeStore) {
	cache := &fakeCache{}
	store := &fakeStore{installed: map[domain.RuntimeRef]string{}}
	svc := runtime.Service{
		Catalog: fakeCatalog{arts: map[string]domain.Artifact{"mysql@8.4.13/amd64": art}},
		Cache:   cache,
		Store:   store,
		Arch:    "amd64",
	}
	return svc, cache, store
}

func TestInstall_カタログのアーカイブを取得して展開する(t *testing.T) {
	svc, cache, store := newService()

	if err := svc.Install(ctx, mysql84); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cache.got, []domain.Artifact{art}) {
		t.Errorf("downloaded %+v", cache.got)
	}
	if store.installed[mysql84] != "/cache/abc" {
		t.Errorf("installed from %q", store.installed[mysql84])
	}
}

func TestInstall_インストール済みならダウンロードしない(t *testing.T) {
	svc, cache, store := newService()
	store.installed[mysql84] = "/old"

	if err := svc.Install(ctx, mysql84); !errors.Is(err, domain.ErrRuntimeInstalled) {
		t.Fatalf("error = %v, want ErrRuntimeInstalled", err)
	}
	if len(cache.got) != 0 {
		t.Error("should not download")
	}
}

func TestInstall_カタログにないバージョンは失敗する(t *testing.T) {
	svc, cache, _ := newService()

	err := svc.Install(ctx, domain.RuntimeRef{Engine: domain.MySQL, Version: "1.0.0"})
	if !errors.Is(err, domain.ErrRuntimeNotFound) {
		t.Fatalf("error = %v, want ErrRuntimeNotFound", err)
	}
	if len(cache.got) != 0 {
		t.Error("should not download")
	}
}

func TestInstall_ダウンロードに失敗したら展開しない(t *testing.T) {
	svc, cache, store := newService()
	boom := errors.New("boom")
	cache.err = boom

	if err := svc.Install(ctx, mysql84); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}
	if len(store.installed) != 0 {
		t.Error("should not install")
	}
}

func Testストアの失敗はそのまま返す(t *testing.T) {
	boom := errors.New("boom")
	svc, _, store := newService()
	store.err = boom

	if err := svc.Install(ctx, mysql84); !errors.Is(err, boom) {
		t.Errorf("Install error = %v", err)
	}
	if err := svc.Remove(ctx, mysql84); !errors.Is(err, boom) {
		t.Errorf("Remove error = %v", err)
	}
	if _, err := svc.List(ctx); !errors.Is(err, boom) {
		t.Errorf("List error = %v", err)
	}
}

func TestRemove_インストール済みのランタイムを削除する(t *testing.T) {
	svc, _, store := newService()
	store.installed[mysql84] = "/x"

	if err := svc.Remove(ctx, mysql84); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.removed, []domain.RuntimeRef{mysql84}) {
		t.Errorf("removed %+v", store.removed)
	}
}

func TestRemove_インストールされていなければ失敗する(t *testing.T) {
	svc, _, store := newService()

	if err := svc.Remove(ctx, mysql84); !errors.Is(err, domain.ErrRuntimeNotInstalled) {
		t.Fatalf("error = %v, want ErrRuntimeNotInstalled", err)
	}
	if len(store.removed) != 0 {
		t.Error("should not remove")
	}
}

func TestList_インストール済みのランタイムを返す(t *testing.T) {
	svc, _, store := newService()
	store.installed[mysql84] = "/x"

	got, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []domain.RuntimeRef{mysql84}) {
		t.Errorf("List = %+v", got)
	}
}

func TestAvailable_このノードのアーキテクチャで取得できる版を返す(t *testing.T) {
	svc, _, _ := newService()

	got := svc.Available(domain.MySQL)
	if !reflect.DeepEqual(got, []domain.RuntimeRef{{Engine: domain.MySQL, Version: "amd64"}}) {
		t.Errorf("Available = %+v", got)
	}
}

func TestRemove_使われているランタイムは消さない(t *testing.T) {
	svc, _, store := newService()
	store.installed[mysql84] = "/x"
	svc.InUse = func(context.Context, domain.RuntimeRef) (bool, error) { return true, nil }
	if err := svc.Remove(ctx, mysql84); !errors.Is(err, domain.ErrInUse) {
		t.Errorf("in use: %v", err)
	}
	svc.InUse = func(context.Context, domain.RuntimeRef) (bool, error) { return false, errors.New("store down") }
	if err := svc.Remove(ctx, mysql84); err == nil || errors.Is(err, domain.ErrInUse) {
		t.Errorf("check failure: %v", err)
	}
	svc.InUse = func(context.Context, domain.RuntimeRef) (bool, error) { return false, nil }
	if err := svc.Remove(ctx, mysql84); err != nil {
		t.Errorf("unused: %v", err)
	}
}

func TestMissing_入っていないものだけ返す(t *testing.T) {
	svc, _, store := newService()
	store.installed[mysql84] = "/x"
	other := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
	got, err := svc.Missing(ctx, []domain.RuntimeRef{mysql84, other})
	if err != nil || len(got) != 1 || got[0] != other {
		t.Errorf("Missing = %v %v", got, err)
	}
	store.err = errors.New("disk")
	if _, err := svc.Missing(ctx, []domain.RuntimeRef{other}); err == nil {
		t.Error("want error")
	}
}

func TestArtifactPath_指定のアーキテクチャの検証済みのコピーを返す(t *testing.T) {
	svc, cache, _ := newService()
	path, err := svc.ArtifactPath(ctx, mysql84, "amd64", 0)
	if err != nil || path != "/cache/abc" || len(cache.got) != 1 {
		t.Errorf("path %q err %v got %v", path, err, cache.got)
	}
	if _, err := svc.ArtifactPath(ctx, mysql84, "riscv64", 0); !errors.Is(err, domain.ErrRuntimeNotFound) {
		t.Errorf("unknown arch: %v", err)
	}
	if _, err := svc.ArtifactPath(ctx, mysql84, "amd64", 1); !errors.Is(err, domain.ErrRuntimeNotFound) {
		t.Errorf("unknown arch: %v", err)
	}
}

func TestInstall_複数のアーカイブを順に取得してまとめて展開する(t *testing.T) {
	svc, cache, store := newService()
	pg := domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}
	svc.Catalog = fakeCatalog{arts: map[string]domain.Artifact{
		"postgres@18.6/amd64":   {URL: "https://x/server.deb", SHA256: "s1"},
		"postgres@18.6/amd64/1": {URL: "https://x/libpq.deb", SHA256: "s2", Part: 1},
	}}
	if err := svc.Install(ctx, pg); err != nil {
		t.Fatal(err)
	}
	if store.installed[pg] != "/cache/s1,/cache/s2" || len(cache.got) != 2 {
		t.Errorf("installed %q got %v", store.installed[pg], cache.got)
	}
}

func TestInstall_OSに合うプラットフォームでカタログを引く(t *testing.T) {
	svc, cache, store := newService()
	svc.Distro = "ubuntu-26.04"
	svc.Catalog = fakeCatalog{arts: map[string]domain.Artifact{"mysql@8.4.13/amd64/ubuntu-26.04": art}}
	if err := svc.Install(ctx, mysql84); err != nil || len(cache.got) != 1 || len(store.installed) != 1 {
		t.Fatalf("install: %v", err)
	}
	if got := svc.Available(domain.MySQL); len(got) != 1 || got[0].Version != "amd64/ubuntu-26.04" {
		t.Errorf("Available asked for %v", got)
	}
}
