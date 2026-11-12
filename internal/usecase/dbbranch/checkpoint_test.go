package dbbranch_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

const (
	live    = "tank/db-branch/branches/x"
	history = "tank/db-branch/history/x"
)

// crashServer wraps the fake server so the simulated crash can strike it too.
type crashServer struct {
	*fakeServer
	c *crasher
}

func (s crashServer) PrepareClone(ctx context.Context, sp domain.Server) error {
	if err := s.c.step(); err != nil {
		return err
	}
	return s.fakeServer.PrepareClone(ctx, sp)
}

func (s crashServer) Start(ctx context.Context, sp domain.Server) error {
	if err := s.c.step(); err != nil {
		return err
	}
	return s.fakeServer.Start(ctx, sp)
}

func (s crashServer) Stop(ctx context.Context, sp domain.Server, m domain.StopMode) error {
	if err := s.c.step(); err != nil {
		return err
	}
	return s.fakeServer.Stop(ctx, sp, m)
}

// crashStore lets the crash strike between state changes.
type crashStore struct {
	domain.Store
	c *crasher
}

func (s crashStore) PutBranch(ctx context.Context, b domain.Branch) error {
	if err := s.c.step(); err != nil {
		return err
	}
	return s.Store.PutBranch(ctx, b)
}

// zenv is an environment on the ZFS simulation with checkpoint support.
type zenv struct {
	*env
	sim   *zsim
	crash *crasher
	max   int
}

func newZEnv(t *testing.T) *zenv {
	t.Helper()
	e := newEnv(t)
	z := &zenv{env: e, crash: &crasher{}, max: 20}
	z.sim = newZsim(e.j, t.TempDir(), func() time.Time { return e.now }, z.crash)
	z.wire(e.svc)
	e.svc.Servers = map[domain.Engine]domain.DBServer{domain.MySQL: crashServer{e.server, z.crash}}
	e.svc.Store = crashStore{e.store, z.crash}
	return z
}

func (z *zenv) wire(svc *dbbranch.Service) {
	svc.Storage, svc.Lineage = z.sim, z.sim
	svc.MaxCheckpoints = func() int { return z.max }
}

// restart is the agent starting again on the same disk and state.
func (z *zenv) restart() {
	z.crash.revive()
	z.server.running = map[string]bool{}
}

// branch creates branch x from a base and gives it some data.
func (z *zenv) branch(t *testing.T) {
	t.Helper()
	z.withBase(t)
	z.withBranch(t, "x")
	z.sim.write(live, "v0")
	z.j.entries = nil
}

func (z *zenv) tick() { z.now = z.now.Add(time.Minute) }

func (z *zenv) checkpoint(t *testing.T, name string) domain.Checkpoint {
	t.Helper()
	z.tick()
	c, err := z.svc.CreateCheckpoint(ctx, name_("x"), name)
	if err != nil {
		t.Fatalf("checkpoint %q: %v", name, err)
	}
	return c
}

func name_(s string) domain.Name { return name(s) }

func (z *zenv) names(t *testing.T) []string {
	t.Helper()
	list, err := z.svc.ListCheckpoints(ctx, name("x"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCreateCheckpoint_サーバーを止めずに稼働中のデータセットのスナップショットを取る(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")

	c := z.checkpoint(t, "before-migration")

	if c.Name != "before-migration" || c.Line != 0 || c.Auto || !c.Current || !c.CreatedAt.Equal(z.now) {
		t.Errorf("checkpoint = %+v", c)
	}
	if !z.j.has("zfs snapshot " + live + "@ckpt-before-migration") {
		t.Errorf("journal = %v", z.j.entries)
	}
	for _, e := range z.j.entries {
		if e == "stop branch-x clean" || e == "stop branch-x immediate" {
			t.Errorf("the server must keep running: %v", z.j.entries)
		}
	}
	if !z.server.running["branch-x"] {
		t.Error("server stopped")
	}
}

func TestCreateCheckpoint_名前を省くと時刻から付ける(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.tick()
	c, err := z.svc.CreateCheckpoint(ctx, name("x"), "")
	if err != nil || c.Name != "20261109-120100" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestCreateCheckpoint_名前の重複と不正な名前を断る(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.checkpoint(t, "a")
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "a"); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "Bad Name"); !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("invalid: %v", err)
	}
}

func TestCreateCheckpoint_上限を超えると409相当で断る(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.max = 2
	z.checkpoint(t, "a")
	z.checkpoint(t, "b")
	_, err := z.svc.CreateCheckpoint(ctx, name("x"), "c")
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if got := z.names(t); !eq(got, []string{"a", "b"}) {
		t.Errorf("names = %v", got)
	}
	z.svc.MaxCheckpoints = nil // the default
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "c"); err != nil {
		t.Errorf("default limit: %v", err)
	}
}

func TestCreateCheckpoint_受け付けない状態や場所を断る(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	if _, err := z.svc.CreateCheckpoint(ctx, name("nope"), "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown branch: %v", err)
	}
	b, _ := z.store.GetBranch(ctx, name("x"))
	b.Status = domain.BranchFailed
	_ = z.store.PutBranch(ctx, b)
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "a"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("failed branch: %v", err)
	}
	b.Status, b.Node = domain.BranchReady, "elsewhere"
	_ = z.store.PutBranch(ctx, b)
	z.svc.Nodes = &fakeNodes{}
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "a"); !errors.Is(err, dbbranch.ErrOtherNode) {
		t.Errorf("other node: %v", err)
	}
	if _, err := z.svc.ListCheckpoints(ctx, name("x")); !errors.Is(err, dbbranch.ErrOtherNode) {
		t.Errorf("list on another node: %v", err)
	}
}

func TestCheckpoints_ZFS以外のストレージでは使えないと伝える(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	if _, err := e.svc.CreateCheckpoint(ctx, name("x"), "a"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("create: %v", err)
	}
	if _, err := e.svc.ListCheckpoints(ctx, name("x")); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("list: %v", err)
	}
	if err := e.svc.DeleteCheckpoint(ctx, name("x"), "a"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("delete: %v", err)
	}
	if _, err := e.svc.RestoreCheckpoint(ctx, name("x"), "a", nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("restore: %v", err)
	}
}

func TestDeleteCheckpoint_スナップショットを消す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.checkpoint(t, "a")
	z.checkpoint(t, "b")
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "a"); err != nil {
		t.Fatal(err)
	}
	if got := z.names(t); !eq(got, []string{"b"}) {
		t.Errorf("names = %v", got)
	}
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleted twice: %v", err)
	}
	if err := z.svc.DeleteCheckpoint(ctx, name("nope"), "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown branch: %v", err)
	}
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "Bad Name"); !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("invalid name: %v", err)
	}
}

func createRequest(n string) dbbranch.CreateBranchRequest {
	return dbbranch.CreateBranchRequest{Name: name(n), Base: name("prod")}
}
