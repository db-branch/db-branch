package membership_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/membership"
)

type fakeRoster struct {
	notLeader bool
	servers   []membership.Server
	removed   []string
	err       error
	listErr   error
}

func (f *fakeRoster) IsLeader() bool { return !f.notLeader }
func (f *fakeRoster) Servers(context.Context) ([]membership.Server, error) {
	return f.servers, f.listErr
}
func (f *fakeRoster) RemoveServer(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return f.err
}

type fakeCatalog struct {
	bases    []domain.Base
	branches []domain.Branch
	nodes    map[string]time.Time
	calls    []string
	err      error
	// failOp limits err to one operation ("" fails them all, incl. listings).
	failOp string
}

func (f *fakeCatalog) fail(op string) error {
	if f.err != nil && (f.failOp == "" || f.failOp == op) {
		return f.err
	}
	return nil
}

func (f *fakeCatalog) ListBases(context.Context) ([]domain.Base, error) {
	return f.bases, f.fail("bases")
}
func (f *fakeCatalog) ListBranches(context.Context) ([]domain.Branch, error) {
	return f.branches, f.fail("branches")
}
func (f *fakeCatalog) ListNodes(context.Context) ([]domain.Node, error) {
	var out []domain.Node
	for n, seen := range f.nodes {
		out = append(out, domain.Node{Name: n, LastSeen: seen})
	}
	return out, f.fail("nodes")
}
func (f *fakeCatalog) DeleteBranch(_ context.Context, n domain.Name) error {
	f.calls = append(f.calls, "delete branch "+n.String())
	return f.fail("delete branch")
}
func (f *fakeCatalog) DeleteBase(_ context.Context, n domain.Name) error {
	f.calls = append(f.calls, "delete base "+n.String())
	return f.fail("delete base")
}
func (f *fakeCatalog) RemoveReplica(_ context.Context, base domain.Name, node string) error {
	f.calls = append(f.calls, "forget replica "+base.String()+" on "+node)
	return f.fail("forget replica")
}
func (f *fakeCatalog) DeleteNode(_ context.Context, node string) error {
	f.calls = append(f.calls, "delete node "+node)
	return f.fail("delete node")
}

type removeEnv struct {
	r   *fakeRoster
	c   *fakeCatalog
	svc *membership.Remover
}

// newRemoveEnv has n00 (self, leader), n01, n02 voting and n03 as a non-voter,
// all healthy.
func newRemoveEnv() *removeEnv {
	e := &removeEnv{
		r: &fakeRoster{servers: []membership.Server{{ID: "n00", Voter: true}, {ID: "n01", Voter: true}, {ID: "n02", Voter: true}, {ID: "n03"}}},
		c: &fakeCatalog{nodes: map[string]time.Time{"n00": t0, "n01": t0, "n02": t0, "n03": t0}},
	}
	e.svc = &membership.Remover{Roster: e.r, Catalog: e.c, Self: "n00", Clock: func() time.Time { return t0 }, DownAfter: 30 * time.Second}
	return e
}

func (e *removeEnv) down(id string) { e.c.nodes[id] = t0.Add(-time.Hour) }

func TestRemover_故障ノードをRaftの構成とノード記録から外す(t *testing.T) {
	e := newRemoveEnv()
	e.down("n02")
	if _, err := e.svc.Remove(context.Background(), "n02", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.r.removed, []string{"n02"}) || !slices.Equal(e.c.calls, []string{"delete node n02"}) {
		t.Fatalf("removed=%v calls=%v", e.r.removed, e.c.calls)
	}
}

func TestRemover_クォーラムを壊す削除は拒否する(t *testing.T) {
	e := newRemoveEnv()
	e.down("n01")
	_, err := e.svc.Remove(context.Background(), "n02", false)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if len(e.r.removed) != 0 || len(e.c.calls) != 0 {
		t.Fatalf("nothing may change: %v %v", e.r.removed, e.c.calls)
	}
}

func TestRemover_投票しないノードはクォーラムに関係なく外せる(t *testing.T) {
	e := newRemoveEnv()
	e.down("n01")
	if _, err := e.svc.Remove(context.Background(), "n03", false); err != nil {
		t.Fatal(err)
	}
}

func TestRemover_リーダー自身と未知のノードと非リーダーは拒否する(t *testing.T) {
	e := newRemoveEnv()
	if _, err := e.svc.Remove(context.Background(), "n00", false); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("self: %v", err)
	}
	if _, err := e.svc.Remove(context.Background(), "zz", false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	e.r.notLeader = true
	if _, err := e.svc.Remove(context.Background(), "n03", false); !errors.Is(err, domain.ErrNoLeader) {
		t.Fatalf("follower: %v", err)
	}
}

func holdings(e *removeEnv) {
	e.c.bases = []domain.Base{
		{Name: nm("origin"), Node: "n03"},
		{Name: nm("copied"), Node: "n01", Replicas: map[string][]string{"s1": {"n03"}}},
		{Name: nm("other"), Node: "n01"},
	}
	e.c.branches = []domain.Branch{{Name: nm("b-on-dead"), Base: nm("other"), Node: "n03"}, {Name: nm("b-elsewhere"), Base: nm("other"), Node: "n01"}}
}

func TestRemover_ベースやブランチが残るノードはforceなしで拒否し一覧を示す(t *testing.T) {
	e := newRemoveEnv()
	holdings(e)
	_, err := e.svc.Remove(context.Background(), "n03", false)
	if !errors.Is(err, domain.ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
	for _, want := range []string{"origin", "copied", "b-on-dead"} {
		if !contains(err.Error(), want) {
			t.Errorf("%q is not listed: %v", want, err)
		}
	}
	if contains(err.Error(), "b-elsewhere") || len(e.r.removed) != 0 || len(e.c.calls) != 0 {
		t.Fatalf("nothing may change: %v %v %v", err, e.r.removed, e.c.calls)
	}
}

func TestRemover_forceならレプリカ参照とブランチとベースを掃除して外す(t *testing.T) {
	e := newRemoveEnv()
	holdings(e)
	rep, err := e.svc.Remove(context.Background(), "n03", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"delete branch b-on-dead", "delete base origin", "forget replica copied on n03", "delete node n03"}
	if !slices.Equal(e.c.calls, want) || !slices.Equal(e.r.removed, []string{"n03"}) {
		t.Fatalf("calls=%v removed=%v", e.c.calls, e.r.removed)
	}
	if !slices.Equal(rep.Branches, []string{"b-on-dead"}) || !slices.Equal(rep.Bases, []string{"origin"}) || !slices.Equal(rep.Replicas, []string{"copied"}) {
		t.Fatalf("report = %+v", rep)
	}
}

func TestRemover_forceでも他ノードのブランチが元にしているベースは消さない(t *testing.T) {
	e := newRemoveEnv()
	e.c.bases = []domain.Base{{Name: nm("origin"), Node: "n03"}}
	e.c.branches = []domain.Branch{{Name: nm("b-elsewhere"), Base: nm("origin"), Node: "n01"}}
	_, err := e.svc.Remove(context.Background(), "n03", true)
	if !errors.Is(err, domain.ErrInUse) || !contains(err.Error(), "b-elsewhere") {
		t.Fatalf("err = %v", err)
	}
	if len(e.r.removed) != 0 || len(e.c.calls) != 0 {
		t.Fatalf("nothing may change: %v %v", e.r.removed, e.c.calls)
	}
}

func TestRemover_途中の失敗はそのまま返し再実行できる(t *testing.T) {
	e := newRemoveEnv()
	holdings(e)
	e.c.err = errors.New("store down")
	if _, err := e.svc.Remove(context.Background(), "n03", true); err == nil {
		t.Fatal("want error")
	}
	if len(e.r.removed) != 0 {
		t.Fatal("the node must stay in Raft while cleanup failed")
	}
	e = newRemoveEnv()
	e.r.err = errors.New("raft down")
	if _, err := e.svc.Remove(context.Background(), "n03", false); err == nil || len(e.c.calls) != 0 {
		t.Fatalf("err=%v calls=%v: the node record stays while RemoveServer fails", err, e.c.calls)
	}
}

func TestRemover_すでにRaftから外れたノードの記録だけを消せる(t *testing.T) {
	e := newRemoveEnv()
	e.r.servers = e.r.servers[:3]
	if _, err := e.svc.Remove(context.Background(), "n03", false); err != nil {
		t.Fatal(err)
	}
	if len(e.r.removed) != 0 || !slices.Equal(e.c.calls, []string{"delete node n03"}) {
		t.Fatalf("removed=%v calls=%v", e.r.removed, e.c.calls)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func nm(s string) domain.Name {
	n, err := domain.ParseName(s)
	if err != nil {
		panic(err)
	}
	return n
}

func TestRemover_どの段階の失敗もRaftから外す前なら構成を変えない(t *testing.T) {
	for _, op := range []string{"nodes", "bases", "branches", "delete branch", "delete base", "forget replica", "delete node"} {
		e := newRemoveEnv()
		holdings(e)
		e.c.err, e.c.failOp = errors.New("store down"), op
		if _, err := e.svc.Remove(context.Background(), "n03", true); err == nil {
			t.Errorf("%s: want error", op)
		}
		if op != "delete node" && len(e.r.removed) != 0 {
			t.Errorf("%s: the node left Raft: %v", op, e.r.removed)
		}
	}
}

func TestRemover_構成を読めなければ何も変えない(t *testing.T) {
	e := newRemoveEnv()
	e.r.listErr = errors.New("raft down")
	if _, err := e.svc.Remove(context.Background(), "n03", true); err == nil || len(e.c.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, e.c.calls)
	}
}
