package update_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/update"
)

var ctx = context.Background()

type fakePkgs struct {
	installed map[string]string
	versions  map[string][]domain.PackageVersion
	upgraded  []map[string]string
	failOn    string
	onUpgrade func()
}

func (f *fakePkgs) fail(op string) error {
	if f.failOn == op {
		return errors.New(op + " failed")
	}
	return nil
}
func (f *fakePkgs) Refresh(context.Context) error { return f.fail("refresh") }
func (f *fakePkgs) Installed(context.Context, string) (map[string]string, error) {
	return f.installed, f.fail("installed")
}
func (f *fakePkgs) Versions(_ context.Context, pkg string) ([]domain.PackageVersion, error) {
	return f.versions[pkg], f.fail("versions")
}
func (f *fakePkgs) Upgrade(_ context.Context, v map[string]string) error {
	f.upgraded = append(f.upgraded, v)
	if f.onUpgrade != nil {
		f.onUpgrade()
	}
	return f.fail("upgrade")
}

type fakeMembers struct {
	leader       string
	voters       int
	transferred  int
	failTransfer bool
}

func (m *fakeMembers) Leader() string { return m.leader }
func (m *fakeMembers) Voters() int    { return m.voters }
func (m *fakeMembers) Transfer() error {
	m.transferred++
	if m.failTransfer {
		return errors.New("no")
	}
	m.leader = "other"
	return nil
}

type env struct {
	*update.Service
	store   *filestore.Store
	pkgs    *fakePkgs
	members *fakeMembers
	nodes   []domain.Node
	now     time.Time
	changed int
}

func newEnv(t *testing.T) *env {
	e := &env{store: &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}, now: time.Unix(10000, 0).UTC()}
	e.pkgs = &fakePkgs{
		installed: map[string]string{"db-branch": "1.0.0", "db-branch-auth": "1.0.0", "db-branchx": "9"},
		versions: map[string][]domain.PackageVersion{
			"db-branch":      {{Version: "2.0.0"}, {Version: "1.2.0"}, {Version: "1.1.0"}, {Version: "1.0.0"}, {Version: "0.9.0"}},
			"db-branch-auth": {{Version: "1.3.0", Requires: "1.3.0"}, {Version: "1.2.0", Requires: "1.2.0"}, {Version: "1.1.0", Requires: "1.1.0"}, {Version: "1.0.0"}},
		},
	}
	e.members = &fakeMembers{leader: "n3", voters: 3}
	e.nodes = []domain.Node{
		{Name: "n1", Version: "1.0.0", LastSeen: e.now, Modules: []string{"auth 1.0.0"}},
		{Name: "n2", Version: "1.0.0", LastSeen: e.now, Modules: []string{"auth 1.0.0"}},
		{Name: "n3", Version: "1.0.0", LastSeen: e.now, Modules: []string{"auth 1.0.0"}},
	}
	_ = e.store.PutModule(ctx, domain.ModuleSpec{Name: "auth", Version: "1.0.0", Wanted: true})
	_ = e.store.PutModule(ctx, domain.ModuleSpec{Name: "gone", Version: "1.0.0"})
	n := 0
	e.Service = &update.Service{
		Store: e.store, Modules: e.store, Packages: e.pkgs, Cluster: e.members, Self: "n1",
		Nodes:     func(context.Context) ([]domain.Node, error) { return e.nodes, nil },
		Clock:     func() time.Time { return e.now },
		DownAfter: 30 * time.Second,
		NewID:     func() string { n++; return "r" + string(rune('0'+n)) },
		Changed:   func() { e.changed++ },
	}
	return e
}

// as makes the service act as node name (the executor of that node).
func (e *env) as(name string) { e.Self = name }

func (e *env) setVersion(node, v string) {
	for i := range e.nodes {
		if e.nodes[i].Name == node {
			e.nodes[i].Version = v
		}
	}
}

func TestCheck_リポジトリの版を調べて記録する(t *testing.T) {
	e := newEnv(t)
	v, err := e.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.CheckedBy != "n1" || !v.CheckedAt.Equal(e.now) || e.changed != 1 {
		t.Errorf("view = %+v changed=%d", v, e.changed)
	}
	if v.LatestMinor != "1.2.0" {
		t.Errorf("latest minor = %q", v.LatestMinor)
	}
	core := v.Packages[0]
	if core.Name != "db-branch" || core.Installed != "1.0.0" || len(core.Available) != 3 || core.Available[0] != (update.Candidate{Version: "2.0.0", Kind: domain.UpdateMajor}) || core.Available[1].Version != "1.2.0" || core.Available[1].Kind != domain.UpdateMinor {
		t.Errorf("core = %+v", core)
	}
	if len(v.Packages) != 2 || v.Packages[1].Name != "db-branch-auth" || len(v.Packages[1].Available) != 3 || v.Packages[1].Available[0].Requires != "1.3.0" {
		t.Errorf("packages = %+v", v.Packages)
	}
	if len(v.Nodes) != 3 || !v.Nodes[2].Leader || v.Nodes[0].Modules["auth"] != "1.0.0" {
		t.Errorf("nodes = %+v", v.Nodes)
	}
}

func TestCheck_何も調べていなければ候補は空_失敗はそのまま返す(t *testing.T) {
	e := newEnv(t)
	v, err := e.View(ctx)
	if err != nil || v.LatestMinor != "" || len(v.Packages) != 2 || len(v.Packages[0].Available) != 0 || v.Rollout != nil {
		t.Errorf("view = %+v %v", v, err)
	}
	for _, op := range []string{"refresh", "installed", "versions"} {
		e := newEnv(t)
		e.pkgs.failOn = op
		if _, err := e.Check(ctx); err == nil {
			t.Errorf("%s failing: want error", op)
		}
	}
}

func TestView_最も古いノードを基準にし_ダウングレードは出さない(t *testing.T) {
	e := newEnv(t)
	e.setVersion("n1", "1.1.0")
	_, _ = e.Check(ctx)
	v, _ := e.View(ctx)
	if v.Packages[0].Installed != "1.0.0" || v.Packages[0].Available[1].Version != "1.2.0" {
		t.Errorf("core = %+v", v.Packages[0])
	}
}

func apply(t *testing.T, e *env, target string) domain.Rollout {
	t.Helper()
	r, err := e.Apply(ctx, target, "admin")
	if err != nil {
		t.Fatalf("Apply(%s): %v", target, err)
	}
	return r
}

func TestApply_フォロワーを名前順に_リーダーを最後に並べモジュールを選ぶ(t *testing.T) {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	r := apply(t, e, "1.2.0")
	if strings.Join(r.Order, ",") != "n1,n2,n3" || r.Status != domain.RolloutRunning || r.By != "admin" || r.Target != "1.2.0" {
		t.Errorf("rollout = %+v", r)
	}
	// 1.3.0 of the module needs db-branch 1.3.0, so 1.2.0 is chosen.
	if r.Modules["db-branch-auth"] != "1.2.0" || len(r.Modules) != 1 {
		t.Errorf("modules = %v", r.Modules)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "db-branch-auth") || !strings.Contains(r.Notes[0], "1.3.0") {
		t.Errorf("notes = %v", r.Notes)
	}

	e = newEnv(t)
	e.members.leader = "n1"
	_, _ = e.Check(ctx)
	if r := apply(t, e, "1.1.0"); strings.Join(r.Order, ",") != "n2,n3,n1" || r.Modules["db-branch-auth"] != "1.1.0" || len(r.Notes) != 1 {
		t.Errorf("leader n1: %+v", r)
	}
}

func TestApply_断る場合(t *testing.T) {
	e := newEnv(t)
	if _, err := e.Apply(ctx, "1.2.0", "a"); !errors.Is(err, domain.ErrInvalidUpdate) {
		t.Errorf("without a check: %v", err)
	}
	_, _ = e.Check(ctx)
	if _, err := e.Apply(ctx, "9.9.9", "a"); !errors.Is(err, domain.ErrInvalidUpdate) {
		t.Errorf("unknown version: %v", err)
	}
	if _, err := e.Apply(ctx, "2.0.0", "a"); !errors.Is(err, domain.ErrInvalidUpdate) || !strings.Contains(err.Error(), "major") {
		t.Errorf("major update: %v", err)
	}
	if _, err := e.Apply(ctx, "0.9.0", "a"); !errors.Is(err, domain.ErrInvalidUpdate) {
		t.Errorf("downgrade: %v", err)
	}
	if _, err := e.Apply(ctx, "1.0.0", "a"); !errors.Is(err, domain.ErrInvalidUpdate) {
		t.Errorf("nothing to do: %v", err)
	}
	e.nodes[1].LastSeen = e.now.Add(-time.Minute)
	if _, err := e.Apply(ctx, "1.2.0", "a"); !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "n2") {
		t.Errorf("a node is down: %v", err)
	}
	e.nodes[1].LastSeen = e.now
	e.members.leader = ""
	if _, err := e.Apply(ctx, "1.2.0", "a"); !errors.Is(err, domain.ErrNoLeader) {
		t.Errorf("no leader: %v", err)
	}
	e.members.leader = "n3"
	e.setVersion("n2", "2.0.0")
	if _, err := e.Apply(ctx, "1.2.0", "a"); !errors.Is(err, domain.ErrInvalidUpdate) {
		t.Errorf("one node is on another major: %v", err)
	}
	e.setVersion("n2", "1.0.0")
	apply(t, e, "1.2.0")
	if _, err := e.Apply(ctx, "1.1.0", "a"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("already running: %v", err)
	}
	e = newEnv(t)
	_, _ = e.Check(ctx)
	e.Nodes = func(context.Context) ([]domain.Node, error) { return nil, errors.New("down") }
	if _, err := e.Apply(ctx, "1.2.0", "a"); err == nil {
		t.Error("nodes unreadable: want error")
	}
}

func TestApply_目標の版のノードは飛ばして再開できる(t *testing.T) {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	e.setVersion("n1", "1.2.0")
	r := apply(t, e, "1.2.0")
	if r.Nodes["n1"].State != domain.NodeDone || r.Nodes["n2"].State != domain.NodePending {
		t.Errorf("rollout = %+v", r)
	}
}

func TestApply_単一ノードは自分を更新する(t *testing.T) {
	e := newEnv(t)
	e.Cluster = nil
	e.nodes = e.nodes[:1]
	_, _ = e.Check(ctx)
	if r := apply(t, e, "1.2.0"); strings.Join(r.Order, ",") != "n1" {
		t.Errorf("order = %v", r.Order)
	}
}

func TestCancel_走っているロールアウトだけを止める(t *testing.T) {
	e := newEnv(t)
	if err := e.Cancel(ctx); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("nothing to cancel: %v", err)
	}
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	if err := e.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutCanceled {
		t.Errorf("rollout = %+v", r)
	}
	if err := e.Cancel(ctx); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("cancel twice: %v", err)
	}
	if v, _ := e.View(ctx); v.Rollout == nil || v.Rollout.Status != domain.RolloutCanceled {
		t.Errorf("view = %+v", v.Rollout)
	}
}

// settle runs the executor of node name once and, if the node's version
// changed (apt restarted it), reports the new version in its heartbeat.
func (e *env) step(t *testing.T, name string) {
	t.Helper()
	e.as(name)
	if err := e.Tick(ctx); err != nil {
		t.Fatalf("Tick as %s: %v", name, err)
	}
}

func TestTick_ノードが順番に更新され_最後にリーダーが譲ってから更新する(t *testing.T) {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	r := apply(t, e, "1.2.0")
	e.pkgs.onUpgrade = nil
	// Not n2's turn yet.
	e.step(t, "n2")
	if len(e.pkgs.upgraded) != 0 {
		t.Fatal("n2 must wait for n1")
	}
	// n1: records "updating" before running apt, with the core and the module.
	var during domain.NodeUpdateState
	e.pkgs.onUpgrade = func() { cur, _ := e.store.GetRollout(ctx); during = cur.Nodes["n1"].State }
	e.step(t, "n1")
	if during != domain.NodeUpdating {
		t.Errorf("state while apt runs = %q", during)
	}
	if len(e.pkgs.upgraded) != 1 || e.pkgs.upgraded[0]["db-branch"] != "1.2.0" || e.pkgs.upgraded[0]["db-branch-auth"] != "1.2.0" {
		t.Fatalf("upgraded = %v", e.pkgs.upgraded)
	}
	e.pkgs.onUpgrade = nil
	// The apt run returned but n1 still reports the old version: nothing more happens.
	e.step(t, "n2")
	e.step(t, "n3")
	if len(e.pkgs.upgraded) != 1 {
		t.Fatalf("others started too early: %v", e.pkgs.upgraded)
	}
	// n1 restarts into the new version; the leader (n3) sees it and marks it done.
	e.setVersion("n1", "1.2.0")
	e.step(t, "n3")
	cur, _ := e.store.GetRollout(ctx)
	if cur.Nodes["n1"].State != domain.NodeDone {
		t.Fatalf("n1 = %+v", cur.Nodes["n1"])
	}
	e.step(t, "n2")
	if len(e.pkgs.upgraded) != 2 {
		t.Fatalf("n2 did not start: %v", e.pkgs.upgraded)
	}
	e.setVersion("n2", "1.2.0")
	e.step(t, "n3")
	// Now the leader n3 is up: it first hands leadership over, then updates.
	if e.members.transferred != 1 || len(e.pkgs.upgraded) != 2 {
		t.Fatalf("transferred=%d upgraded=%d", e.members.transferred, len(e.pkgs.upgraded))
	}
	e.step(t, "n3")
	if len(e.pkgs.upgraded) != 3 {
		t.Fatalf("n3 did not start after the transfer: %v", e.pkgs.upgraded)
	}
	// The new leader sees n3 on the target and completes the rollout, recording the module.
	e.setVersion("n3", "1.2.0")
	e.members.leader = "n2"
	e.step(t, "n2")
	cur, _ = e.store.GetRollout(ctx)
	if cur.ID != r.ID || cur.Status != domain.RolloutDone || !cur.FinishedAt.Equal(e.now) {
		t.Fatalf("rollout = %+v", cur)
	}
	mods, _ := e.store.ListModules(ctx)
	if mods[0].Name != "auth" || mods[0].Version != "1.2.0" || mods[1].Version != "1.0.0" {
		t.Errorf("modules = %+v", mods)
	}
	if got := e.Rolled(ctx); got["db-branch-auth"] != "1.2.0" {
		t.Errorf("rolled = %v", got)
	}
	e.step(t, "n2") // nothing left to do
}

func TestTick_リーダーが交代できなければ更新を始めない(t *testing.T) {
	e := newEnv(t)
	e.members.failTransfer = true
	e.nodes = e.nodes[2:] // only the leader is left to update
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.as("n3")
	if err := e.Tick(ctx); err == nil {
		t.Error("transfer failure: want error")
	}
	if len(e.pkgs.upgraded) != 0 {
		t.Error("must not update while leading")
	}
	// With nobody else to lead, the node updates itself.
	e.members.voters = 1
	e.step(t, "n3")
	if len(e.pkgs.upgraded) != 1 {
		t.Errorf("single voter: %v", e.pkgs.upgraded)
	}
}

func TestTick_aptが失敗したらロールアウトを止めて理由を残す(t *testing.T) {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.pkgs.failOn = "upgrade"
	e.step(t, "n1")
	r, _ := e.store.GetRollout(ctx)
	if r.Status != domain.RolloutFailed || !strings.Contains(r.Nodes["n1"].Error, "upgrade failed") || !strings.Contains(r.Error, "n1") {
		t.Errorf("rollout = %+v", r)
	}
	e.pkgs.failOn = ""
	e.step(t, "n2")
	if len(e.pkgs.upgraded) != 1 {
		t.Error("nothing continues after a failure")
	}
}

func TestTick_アップグレード中に自分が止められたのは失敗ではない(t *testing.T) {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	cctx, cancel := context.WithCancel(ctx)
	e.pkgs.failOn = "upgrade"
	e.pkgs.onUpgrade = cancel // the restart cancels the agent's context
	e.as("n1")
	if err := e.Tick(cctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutRunning || r.Nodes["n1"].State != domain.NodeUpdating {
		t.Errorf("rollout = %+v", r)
	}
}

func TestTick_時間内に新しい版を報告しなければ失敗(t *testing.T) {
	e := newEnv(t)
	e.Timeout = 15 * time.Minute
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.step(t, "n1")
	e.now = e.now.Add(14 * time.Minute)
	for i := range e.nodes {
		e.nodes[i].LastSeen = e.now
	}
	e.step(t, "n3")
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutRunning {
		t.Fatalf("too early: %+v", r)
	}
	e.now = e.now.Add(2 * time.Minute)
	for i := range e.nodes {
		e.nodes[i].LastSeen = e.now
	}
	e.step(t, "n3")
	r, _ := e.store.GetRollout(ctx)
	if r.Status != domain.RolloutFailed || !strings.Contains(r.Error, "n1") || !strings.Contains(r.Nodes["n1"].Error, "15m") {
		t.Errorf("rollout = %+v", r)
	}
}

func TestTick_メンバーから消えたノードがあれば失敗(t *testing.T) {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.nodes = e.nodes[:2]
	e.step(t, "n3")
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutFailed || !strings.Contains(r.Error, "n3") {
		t.Errorf("rollout = %+v", r)
	}
}

func TestTick_単一ノードは自分で更新し_再起動後に完了する(t *testing.T) {
	e := newEnv(t)
	e.Cluster = nil
	e.nodes = e.nodes[:1]
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.step(t, "n1")
	if len(e.pkgs.upgraded) != 1 {
		t.Fatalf("upgraded = %v", e.pkgs.upgraded)
	}
	e.setVersion("n1", "1.2.0") // the agent restarted
	e.step(t, "n1")
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutDone {
		t.Errorf("rollout = %+v", r)
	}
}

func TestTick_読めなければ何もしない(t *testing.T) {
	e := newEnv(t)
	if err := e.Tick(ctx); err != nil {
		t.Errorf("no rollout: %v", err)
	}
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.Nodes = func(context.Context) ([]domain.Node, error) { return nil, errors.New("down") }
	if err := e.Tick(ctx); err == nil {
		t.Error("want error")
	}
}

func TestCheckDue_有効で一日たっていれば確認する(t *testing.T) {
	e := newEnv(t)
	e.members.leader = "n1"
	if err := e.CheckDue(ctx, false); err != nil || e.pkgs.failOn != "" {
		t.Fatal(err)
	}
	if c, _ := e.store.GetUpdateCheck(ctx); !c.At.IsZero() {
		t.Error("disabled: must not check")
	}
	if err := e.CheckDue(ctx, true); err != nil {
		t.Fatal(err)
	}
	first, _ := e.store.GetUpdateCheck(ctx)
	e.now = e.now.Add(23 * time.Hour)
	e.members.leader = "n2" // a follower never checks on its own
	e.now = e.now.Add(48 * time.Hour)
	_ = e.CheckDue(ctx, true)
	if c, _ := e.store.GetUpdateCheck(ctx); !c.At.Equal(first.At) {
		t.Error("a follower checked")
	}
	e.members.leader = "n1"
	e.now = e.now.Add(-48 * time.Hour)
	_ = e.CheckDue(ctx, true)
	if c, _ := e.store.GetUpdateCheck(ctx); !c.At.Equal(first.At) {
		t.Error("checked again within a day")
	}
	e.now = e.now.Add(2 * time.Hour)
	_ = e.CheckDue(ctx, true)
	if c, _ := e.store.GetUpdateCheck(ctx); !c.At.After(first.At) {
		t.Error("not checked after a day")
	}
}

func TestRolled_ロールアウトが無ければ空(t *testing.T) {
	e := newEnv(t)
	if got := e.Rolled(ctx); len(got) != 0 {
		t.Errorf("rolled = %v", got)
	}
}

// flaky fails the store call named in fail.
type flaky struct {
	domain.UpdateStore
	domain.ModuleStore
	fail map[string]bool
}

var errBroken = errors.New("broken")

func (f *flaky) GetUpdateCheck(c context.Context) (domain.UpdateCheck, error) {
	if f.fail["getcheck"] {
		return domain.UpdateCheck{}, errBroken
	}
	return f.UpdateStore.GetUpdateCheck(c)
}
func (f *flaky) PutUpdateCheck(c context.Context, u domain.UpdateCheck) error {
	if f.fail["putcheck"] {
		return errBroken
	}
	return f.UpdateStore.PutUpdateCheck(c, u)
}
func (f *flaky) GetRollout(c context.Context) (domain.Rollout, error) {
	if f.fail["getrollout"] {
		return domain.Rollout{}, errBroken
	}
	return f.UpdateStore.GetRollout(c)
}
func (f *flaky) StartRollout(c context.Context, r domain.Rollout) error {
	if f.fail["start"] {
		return errBroken
	}
	return f.UpdateStore.StartRollout(c, r)
}
func (f *flaky) RecordRollout(c context.Context, ev domain.RolloutEvent) error {
	if f.fail["record"] || (f.fail["recordnode"] && ev.Node != "") {
		return errBroken
	}
	return f.UpdateStore.RecordRollout(c, ev)
}
func (f *flaky) ListModules(c context.Context) ([]domain.ModuleSpec, error) {
	if f.fail["list"] {
		return nil, errBroken
	}
	return f.ModuleStore.ListModules(c)
}
func (f *flaky) PutModule(c context.Context, m domain.ModuleSpec) error {
	if f.fail["putmodule"] {
		return errBroken
	}
	return f.ModuleStore.PutModule(c, m)
}

func (e *env) breakIt(what string) { e.useFlaky(map[string]bool{what: true}) }

func (e *env) useFlaky(fail map[string]bool) *flaky {
	f := &flaky{UpdateStore: e.store, ModuleStore: e.store, fail: fail}
	e.Store, e.Modules = f, f
	return f
}

func TestエラーはそのままAPIへ返す(t *testing.T) {
	prepare := func(t *testing.T) *env {
		e := newEnv(t)
		_, _ = e.Check(ctx)
		return e
	}
	for _, what := range []string{"getcheck", "getrollout", "list"} {
		e := prepare(t)
		e.breakIt(what)
		if _, err := e.View(ctx); err == nil {
			t.Errorf("View with %s broken: want error", what)
		}
	}
	e := prepare(t)
	e.Nodes = func(context.Context) ([]domain.Node, error) { return nil, errBroken }
	if _, err := e.View(ctx); err == nil {
		t.Error("View with nodes broken: want error")
	}
	for _, what := range []string{"putcheck", "list", "getcheck"} {
		e := prepare(t)
		e.breakIt(what)
		if _, err := e.Check(ctx); err == nil {
			t.Errorf("Check with %s broken: want error", what)
		}
	}
	e = prepare(t)
	e.members.leader = "n1"
	e.breakIt("getcheck")
	if err := e.CheckDue(ctx, true); err == nil {
		t.Error("CheckDue: want error")
	}
	for _, what := range []string{"getcheck", "getrollout", "list", "start"} {
		e := prepare(t)
		e.breakIt(what)
		if _, err := e.Apply(ctx, "1.2.0", "a"); err == nil {
			t.Errorf("Apply with %s broken: want error", what)
		}
	}
	for _, what := range []string{"getrollout", "record"} {
		e := prepare(t)
		apply(t, e, "1.2.0")
		e.breakIt(what)
		if err := e.Cancel(ctx); err == nil {
			t.Errorf("Cancel with %s broken: want error", what)
		}
	}
	e = prepare(t)
	e.breakIt("getrollout")
	if got := e.Rolled(ctx); len(got) != 0 {
		t.Errorf("rolled = %v", got)
	}
}

func TestTick_記録に失敗したら次の周期にやり直す(t *testing.T) {
	// The executor cannot record that it started: apt must not run.
	e := newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.breakIt("recordnode")
	e.as("n1")
	if err := e.Tick(ctx); err == nil || len(e.pkgs.upgraded) != 0 {
		t.Errorf("record failure: %v upgraded=%v", err, e.pkgs.upgraded)
	}
	// apt failed and the failure cannot be recorded either.
	e.useFlaky(map[string]bool{})
	e.pkgs.failOn = "upgrade"
	e.Store.(*flaky).fail["recordnode"] = false
	e.pkgs.onUpgrade = func() { e.Store.(*flaky).fail["recordnode"] = true }
	if err := e.Tick(ctx); err == nil {
		t.Error("recording the apt failure failed: want error")
	}
	// The leader's supervising fails to read.
	e = newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.as("n3")
	e.breakIt("getrollout")
	if err := e.Tick(ctx); err == nil {
		t.Error("getrollout broken: want error")
	}
}

func TestTick_リーダーの監督が失敗しても止まらず返す(t *testing.T) {
	// done cannot be recorded
	e := newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.step(t, "n1")
	e.setVersion("n1", "1.2.0")
	e.useFlaky(map[string]bool{"record": true})
	e.as("n3")
	if err := e.Tick(ctx); err == nil {
		t.Error("record broken: want error")
	}
	// the module record cannot be written when the rollout completes
	e = newEnv(t)
	e.nodes = e.nodes[:1]
	e.Cluster = nil
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.step(t, "n1")
	e.setVersion("n1", "1.2.0")
	e.breakIt("putmodule")
	if err := e.Tick(ctx); err == nil {
		t.Error("putmodule broken: want error")
	}
	e.useFlaky(map[string]bool{"list": true})
	if err := e.Tick(ctx); err == nil {
		t.Error("list broken: want error")
	}
	// failing to timestamp a timeout is reported too
	e = newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.step(t, "n1")
	e.now = e.now.Add(time.Hour)
	for i := range e.nodes {
		e.nodes[i].LastSeen = e.now
	}
	e.useFlaky(map[string]bool{"record": true})
	e.as("n3")
	if err := e.Tick(ctx); err == nil {
		t.Error("timeout record broken: want error")
	}
}

func TestApply_新しい版が要る新しいモジュールや大きな版は選ばない(t *testing.T) {
	e := newEnv(t)
	e.pkgs.versions["db-branch-auth"] = append([]domain.PackageVersion{{Version: "2.0.0"}}, e.pkgs.versions["db-branch-auth"]...)
	_, _ = e.Check(ctx)
	r := apply(t, e, "1.2.0")
	if r.Modules["db-branch-auth"] != "1.2.0" {
		t.Errorf("modules = %v", r.Modules)
	}
	// With no compatible newer version the module is left alone.
	e = newEnv(t)
	e.pkgs.versions["db-branch-auth"] = []domain.PackageVersion{{Version: "1.1.0", Requires: "1.9.0"}}
	_, _ = e.Check(ctx)
	if r := apply(t, e, "1.2.0"); len(r.Modules) != 0 || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "stays at 1.0.0") {
		t.Errorf("rollout = %+v", r)
	}
}

func TestService_既定値_時計と番号と制限時間(t *testing.T) {
	e := newEnv(t)
	e.Clock, e.NewID, e.Timeout = nil, nil, 0
	_, _ = e.Check(ctx)
	// Real clock: the heartbeats of the fixture are long gone.
	for i := range e.nodes {
		e.nodes[i].LastSeen = time.Now()
	}
	r := apply(t, e, "1.2.0")
	if len(r.ID) != 12 || time.Since(r.StartedAt) > time.Minute {
		t.Errorf("rollout = %+v", r)
	}
	e.step(t, "n1")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	e.Changed = nil
	if err := e.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
}

// beat refreshes every heartbeat except those of the nodes named in silent.
func (e *env) beat(silent ...string) {
	for i := range e.nodes {
		if !slices.Contains(silent, e.nodes[i].Name) {
			e.nodes[i].LastSeen = e.now
		}
	}
}

// firstDone runs the rollout until n1 is done, with the leader n3 supervising.
func firstDone(t *testing.T) *env {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.step(t, "n1")
	e.setVersion("n1", "1.2.0")
	e.step(t, "n3")
	if r, _ := e.store.GetRollout(ctx); r.Nodes["n1"].State != domain.NodeDone {
		t.Fatalf("n1 not done: %+v", r)
	}
	return e
}

func TestTick_前のノードが落ちて次が始められないまま15分たてば失敗(t *testing.T) {
	e := firstDone(t)
	e.now = e.now.Add(14 * time.Minute)
	e.beat("n1") // n1 went down after it was done
	e.step(t, "n3")
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutRunning {
		t.Fatalf("too early: %+v", r)
	}
	e.now = e.now.Add(2 * time.Minute)
	e.beat("n1")
	e.step(t, "n3")
	r, _ := e.store.GetRollout(ctx)
	if r.Status != domain.RolloutFailed || !strings.Contains(r.Error, "n2") || !strings.Contains(r.Error, "n1") || !strings.Contains(r.Error, "15m") {
		t.Errorf("rollout = %+v", r)
	}
	if len(e.pkgs.upgraded) != 1 {
		t.Errorf("n2 must not have started: %v", e.pkgs.upgraded)
	}
}

func TestTick_次のノード自身が落ちていて始められなければ失敗(t *testing.T) {
	e := firstDone(t)
	e.now = e.now.Add(16 * time.Minute)
	e.beat("n2")
	e.step(t, "n3")
	r, _ := e.store.GetRollout(ctx)
	if r.Status != domain.RolloutFailed || !strings.Contains(r.Error, "n2 is down") {
		t.Errorf("rollout = %+v", r)
	}
}

func TestTick_順番が来ても動かないノードで止まったままにならない(t *testing.T) {
	e := firstDone(t)
	e.now = e.now.Add(16 * time.Minute)
	e.beat() // n2 is healthy but never starts
	e.step(t, "n3")
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutFailed || !strings.Contains(r.Error, "n2 did not start") {
		t.Errorf("rollout = %+v", r)
	}
}

func TestTick_待ち時間は前のノードが終わった時から数える(t *testing.T) {
	e := newEnv(t)
	_, _ = e.Check(ctx)
	apply(t, e, "1.2.0")
	e.now = e.now.Add(10 * time.Minute)
	e.beat()
	e.step(t, "n1") // n1 starts late but within its own limit
	e.setVersion("n1", "1.2.0")
	e.now = e.now.Add(10 * time.Minute) // 20 min after the start, 10 after n1 started
	e.beat()
	e.step(t, "n3")
	if r, _ := e.store.GetRollout(ctx); r.Status != domain.RolloutRunning || r.Nodes["n1"].State != domain.NodeDone {
		t.Errorf("rollout = %+v", r)
	}
}

func TestView_モジュールの版は記録ではなくノードが動かしている版(t *testing.T) {
	e := newEnv(t)
	// The recorded spec is stale (1.0.0); the nodes run 1.1.0, n2 runs 1.2.0.
	e.nodes[0].Modules = []string{"auth 1.1.0"}
	e.nodes[1].Modules = []string{"auth 1.2.0"}
	e.nodes[2].Modules = []string{"auth 1.1.0"}
	_, _ = e.Check(ctx)
	v, _ := e.View(ctx)
	auth := v.Packages[1]
	if auth.Installed != "1.1.0" || !auth.Mixed || len(auth.Available) != 2 || auth.Available[1].Version != "1.2.0" {
		t.Errorf("auth = %+v", auth)
	}
	if core := v.Packages[0]; core.Mixed || core.Installed != "1.0.0" {
		t.Errorf("core = %+v", core)
	}
	e.setVersion("n2", "1.1.0")
	if v, _ := e.View(ctx); !v.Packages[0].Mixed || v.Packages[0].Installed != "1.0.0" {
		t.Errorf("core with differing nodes = %+v", v.Packages[0])
	}
	// A module no node reports yet falls back to the record.
	for i := range e.nodes {
		e.nodes[i].Modules = nil
	}
	if v, _ := e.View(ctx); v.Packages[1].Installed != "1.0.0" || v.Packages[1].Mixed {
		t.Errorf("auth = %+v", v.Packages[1])
	}
}

func TestApply_モジュールの更新は実際に入っている版から決める(t *testing.T) {
	e := newEnv(t)
	for i := range e.nodes {
		e.nodes[i].Modules = []string{"auth 1.2.0"} // newer than the stale record
	}
	_, _ = e.Check(ctx)
	// 1.2.0 is already there, so a 1.1.0 target must not "update" the module.
	if r := apply(t, e, "1.1.0"); len(r.Modules) != 0 {
		t.Errorf("modules = %v", r.Modules)
	}
}
