package licensing_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
)

type fakeModules struct {
	available  map[string]string
	specs      []domain.ModuleSpec
	installed  []string
	fail       error
	listFail   error
	installErr error
}

func (f *fakeModules) Available(context.Context) (map[string]string, error) {
	return f.available, f.fail
}
func (f *fakeModules) List(context.Context) ([]domain.ModuleSpec, error) { return f.specs, f.listFail }
func (f *fakeModules) Install(_ context.Context, name, _ string) (domain.ModuleSpec, error) {
	if f.installErr != nil {
		return domain.ModuleSpec{}, f.installErr
	}
	f.installed = append(f.installed, name)
	return domain.ModuleSpec{Name: name, Wanted: true}, nil
}

type fakeSettings struct {
	put  map[string]string
	fail error
}

func (f *fakeSettings) Put(_ context.Context, scope string, v map[string]string) error {
	if scope != "core" || f.fail != nil {
		return errors.Join(errors.New("cannot save"), f.fail)
	}
	f.put = v
	return nil
}

func (f *fakeSettings) Values(_ context.Context, scope string) (map[string]string, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.put, nil
}

func keyFor(t *testing.T, root license.LocalSigner, l license.License) string {
	t.Helper()
	issuing := license.NewLocalSigner()
	cert, _ := license.Certify(ctx, root, issuing.Public(), "t", t0.Add(-time.Hour), t0.Add(time.Hour))
	key, err := license.Issue(ctx, issuing, cert, l)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func setup(t *testing.T) (*licensing.Service, license.LocalSigner, *fakeModules, *fakeSettings) {
	root := license.NewLocalSigner()
	mods := &fakeModules{
		available: map[string]string{"auth": "users", "audit": "audit log", "proxysql": "routing"},
		specs:     []domain.ModuleSpec{{Name: "audit", Wanted: true}},
	}
	set := &fakeSettings{}
	s := &licensing.Service{Root: root.Public(), Clock: func() time.Time { return t0 }, Nodes: func() int { return 3 }, Modules: mods, Settings: set}
	return s, root, mods, set
}

func TestVerify_署名と状態と対象モジュールを返す(t *testing.T) {
	s, root, _, set := setup(t)
	key := keyFor(t, root, license.License{ID: "L1", Customer: "Acme", Modules: []string{"auth", "audit", "sso"}, MaxNodes: 5, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	r, err := s.Verify(ctx, key)
	if err != nil || r.State != license.Valid || r.License.Customer != "Acme" {
		t.Fatalf("%+v %v", r, err)
	}
	if !slices.Equal(r.Modules, []string{"audit", "auth"}) || !slices.Equal(r.Unavailable, []string{"sso"}) {
		t.Errorf("modules %v unavailable %v", r.Modules, r.Unavailable)
	}
	if set.put != nil {
		t.Error("verifying saves nothing")
	}
	over := keyFor(t, root, license.License{ID: "L2", Modules: []string{"auth"}, MaxNodes: 2, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	if r, _ := s.Verify(ctx, over); r.State != license.OverNodes {
		t.Errorf("over nodes: %s", r.State)
	}
	if _, err := s.Verify(ctx, "dbbl1.bad"); !errors.Is(err, license.ErrInvalid) {
		t.Errorf("garbage: %v", err)
	}
	other := license.NewLocalSigner()
	if _, err := s.Verify(ctx, keyFor(t, other, license.License{ID: "L3", Modules: []string{"auth"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})); !errors.Is(err, license.ErrInvalid) {
		t.Errorf("another root: %v", err)
	}
}

func TestApply_正しいキーを保存し対象のモジュールを有効にする(t *testing.T) {
	s, root, mods, set := setup(t)
	key := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth", "audit"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	r, err := s.Apply(ctx, " "+key+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if set.put["license.key"] != key {
		t.Errorf("saved %v", set.put)
	}
	// audit is already enabled; proxysql is not in the licence.
	if !slices.Equal(mods.installed, []string{"auth"}) || !slices.Equal(r.Enabled, []string{"auth"}) {
		t.Errorf("installed %v, report %v", mods.installed, r.Enabled)
	}
}

func TestApply_使えないキーは保存しない(t *testing.T) {
	s, root, mods, set := setup(t)
	expired := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, IssuedAt: t0.Add(-30 * time.Minute), Expires: t0.Add(-time.Minute)})
	if _, err := s.Apply(ctx, expired); !errors.Is(err, licensing.ErrUnusable) {
		t.Errorf("expired: %v", err)
	}
	if _, err := s.Apply(ctx, "nonsense"); err == nil {
		t.Error("invalid key applied")
	}
	if set.put != nil || len(mods.installed) != 0 {
		t.Errorf("saved %v installed %v", set.put, mods.installed)
	}
}

func TestApply_全モジュールのライセンスは勝手に入れない(t *testing.T) {
	s, root, mods, set := setup(t)
	key := keyFor(t, root, license.License{ID: "L1", Modules: []string{"*"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	r, err := s.Apply(ctx, key)
	if err != nil || set.put == nil || len(mods.installed) != 0 || !r.All {
		t.Errorf("%+v %v installed %v", r, err, mods.installed)
	}
}

func TestLicensing_オフラインAPTエラー時でもVerifyとApplyが成功する(t *testing.T) {
	s, root, mods, set := setup(t)
	mods.fail = errors.New("apt: cannot reach the repository")
	key := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth", "*"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	r, err := s.Verify(ctx, key)
	if err != nil || r.State != license.Valid || !r.All || !r.Offline || !slices.Equal(r.Unavailable, []string{"auth"}) {
		t.Fatalf("verify: %+v %v", r, err)
	}
	if _, err := s.Apply(ctx, key); err != nil || set.put["license.key"] != key || len(mods.installed) != 0 {
		t.Errorf("apply: %v saved %v installed %v", err, set.put, mods.installed)
	}
}

func TestVerify_キャンセルされたときはオフライン扱いにせずエラーを返す(t *testing.T) {
	s, root, mods, _ := setup(t)
	mods.fail = context.Canceled
	key := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Verify(c, key); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
}

func TestVerify_ルート鍵がなければ検証できない(t *testing.T) {
	s, _, _, _ := setup(t)
	s.Root = nil
	if _, err := s.Verify(ctx, "x"); !errors.Is(err, licensing.ErrNoRoot) {
		t.Errorf("%v", err)
	}
}

func TestApply_保存や有効化に失敗したら伝える(t *testing.T) {
	s, root, mods, set := setup(t)
	key := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	set.fail = errors.New("no leader")
	if _, err := s.Apply(ctx, key); err == nil {
		t.Error("save failure")
	}
	set.fail = nil
	mods.listFail = errors.New("store down")
	if _, err := s.Apply(ctx, key); err == nil {
		t.Error("list failure")
	}
	mods.listFail = nil
	mods.installErr = errors.New("no package")
	r, err := s.Apply(ctx, key)
	if err == nil || len(r.Enabled) != 0 {
		t.Errorf("install failure: %+v %v", r, err)
	}
}

func TestCurrent_保存したキーを確かめる(t *testing.T) {
	s, root, _, set := setup(t)
	if _, ok, err := s.Current(ctx); ok || err != nil {
		t.Errorf("no key: %v %v", ok, err)
	}
	key := keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})
	set.put = map[string]string{"license.key": key}
	r, ok, err := s.Current(ctx)
	if !ok || err != nil || r.License.ID != "L1" {
		t.Errorf("%+v %v %v", r, ok, err)
	}
	set.fail = errors.New("no leader")
	if _, _, err := s.Current(ctx); err == nil {
		t.Error("settings unreadable")
	}
}

type fakeTrial struct {
	at               time.Time
	failGet, failPut error
	starts           int
}

func (f *fakeTrial) TrialStarted(context.Context) (time.Time, error) { return f.at, f.failGet }

func (f *fakeTrial) StartTrial(_ context.Context, at time.Time) (time.Time, error) {
	if f.failPut != nil {
		return time.Time{}, f.failPut
	}
	f.starts++
	if f.at.IsZero() {
		f.at = at
	}
	return f.at, nil
}

func TestStartTrial_試用はクラスターで一度だけ始まる(t *testing.T) {
	s, _, _, _ := setup(t)
	if at, err := s.StartTrial(ctx); err != nil || !at.IsZero() {
		t.Errorf("a store without a trial: %s %v", at, err)
	}
	tr := &fakeTrial{}
	s.Trial = tr
	at, err := s.StartTrial(ctx)
	if err != nil || !at.Equal(t0) || !tr.at.Equal(t0) {
		t.Fatalf("started %s %v, stored %s", at, err, tr.at)
	}
	s.Clock = func() time.Time { return t0.AddDate(0, 0, 40) }
	if again, err := s.StartTrial(ctx); err != nil || !again.Equal(t0) || tr.starts != 1 {
		t.Errorf("a second start: %s %v (%d writes)", again, err, tr.starts)
	}
	// A start that cannot be read is not a reason to begin again.
	tr.failGet = errors.New("no leader")
	if _, err := s.StartTrial(ctx); err == nil || tr.starts != 1 {
		t.Errorf("unreadable: %v (%d writes)", err, tr.starts)
	}
	s.Trial = &fakeTrial{failPut: errors.New("cluster upgrading")}
	if at, err := s.StartTrial(ctx); err == nil || !at.IsZero() {
		t.Errorf("unsaved: %s %v", at, err)
	}
}

func TestCurrent_キーがなければ試用の状態を返す(t *testing.T) {
	s, root, _, set := setup(t)
	if r, ok, err := s.Current(ctx); ok || err != nil || r.Trial {
		t.Errorf("a store without a trial: %+v %v %v", r, ok, err)
	}
	tr := &fakeTrial{}
	s.Trial = tr
	if r, ok, err := s.Current(ctx); ok || err != nil || r.Trial {
		t.Errorf("before a paid module was loaded: %+v %v %v", r, ok, err)
	}
	_, _ = s.StartTrial(ctx)
	r, ok, err := s.Current(ctx)
	if ok || err != nil || !r.Trial || !r.All || r.State != license.Valid || !r.License.Expires.Equal(t0.AddDate(0, 0, 30)) {
		t.Fatalf("on trial: %+v %v %v", r, ok, err)
	}
	s.Clock = func() time.Time { return t0.AddDate(0, 0, 31) }
	if r, _, _ := s.Current(ctx); !r.Trial || r.State != license.Expired {
		t.Errorf("after the trial: %+v", r)
	}
	// A saved key replaces the trial, even after it ended.
	set.put = map[string]string{"license.key": keyFor(t, root, license.License{ID: "L1", Modules: []string{"auth"}, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0)})}
	if r, ok, err := s.Current(ctx); !ok || err != nil || r.Trial || r.License.ID != "L1" {
		t.Errorf("with a key: %+v %v %v", r, ok, err)
	}
	set.put = nil
	tr.failGet = errors.New("no leader")
	if _, ok, err := s.Current(ctx); ok || err == nil {
		t.Errorf("trial unreadable: %v %v", ok, err)
	}
}
