package dbbranch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

type fakeNodes struct {
	nodes []domain.Node
	err   error
}

func (f *fakeNodes) PutNode(context.Context, domain.Node) error { return nil }
func (f *fakeNodes) DeleteNode(context.Context, string) error   { return nil }
func (f *fakeNodes) ListNodes(context.Context) ([]domain.Node, error) {
	return f.nodes, f.err
}

// clustered makes the env one node ("node1") of a cluster with node2 and node3.
func (e *env) clustered() *fakeNodes {
	n := &fakeNodes{}
	for _, name := range []string{"node1", "node2", "node3"} {
		n.nodes = append(n.nodes, domain.Node{Name: name, LastSeen: e.now, MemoryMB: 8192})
	}
	e.svc.Nodes = n
	e.svc.DownAfter = 30 * time.Second
	return n
}

func TestCreateBase_作ったノードを担当として記録する(t *testing.T) {
	e := newEnv(t)
	b := e.withBase(t)
	if b.Node != "node1" {
		t.Errorf("node = %q", b.Node)
	}
	if got, _ := e.store.GetBase(ctx, b.Name); got.Node != "node1" {
		t.Errorf("stored node = %q", got.Node)
	}
}

func TestCreateBranch_ベースを持つノードに置き担当として記録する(t *testing.T) {
	e := newEnv(t)
	e.clustered()
	e.withBase(t)
	node, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")})
	if err != nil || node != "node1" {
		t.Fatalf("PlaceBranch = %q, %v", node, err)
	}
	b := e.withBranch(t, "x")
	if b.Node != "node1" {
		t.Errorf("node = %q", b.Node)
	}
}

func TestPlaceBranch_ベースのノードが止まっていれば置けない(t *testing.T) {
	e := newEnv(t)
	nodes := e.clustered()
	e.withBase(t)
	nodes.nodes[0].LastSeen = e.now.Add(-time.Minute)
	if _, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("err = %v", err)
	}
	nodes.err = errors.New("raft down")
	if _, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}); err == nil {
		t.Error("node list failure: want error")
	}
}

func TestPlaceBranch_メモリ予算を超えるなら置けない(t *testing.T) {
	e := newEnv(t)
	nodes := e.clustered()
	e.withBase(t)
	// (128MB + overhead) per branch; a 400MB node fits none.
	nodes.nodes[0].MemoryMB = 400
	if _, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("err = %v", err)
	}
}

func TestPlaceBranch_ノードが報告したARC上限をメモリ予算から引く(t *testing.T) {
	e := newEnv(t)
	nodes := e.clustered()
	e.withBase(t)
	req := dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}
	// A 1000MB node has 750MB for branches by MemoryShare: one of (128MB + overhead) fits.
	nodes.nodes[0].MemoryMB = 1000
	if _, err := e.svc.PlaceBranch(ctx, req); err != nil {
		t.Fatalf("without a reported cap: %v", err)
	}
	nodes.nodes[0].ArcMaxMB = 900 // raised ARC: 100MB left, not enough for one
	if _, err := e.svc.PlaceBranch(ctx, req); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("raised ARC cap: err = %v", err)
	}
}

func TestPlaceBranch_存在しないベースやスナップショットは先に断る(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("nope")}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing base: %v", err)
	}
	e.withBase(t)
	if _, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Snapshot: "s-nope"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing snapshot: %v", err)
	}
}

func TestCreateBranch_他のノードを指定されたら自分では作らない(t *testing.T) {
	e := newEnv(t)
	e.clustered()
	e.withBase(t)
	_, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"}, nil)
	if !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("err = %v (node2 lacks the snapshot)", err)
	}
	if _, err := e.store.GetBranch(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Error("nothing should be reserved")
	}
}

func TestCreateBranch_ポートはノードごとに割り当てる(t *testing.T) {
	e := newEnv(t)
	e.withBase(t) // port 40000 on node1
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("remote"), Node: "node2", Port: 40001})
	b := e.withBranch(t, "x")
	if b.Port != 40001 {
		t.Errorf("port = %d, want 40001 (40001 on node2 does not collide)", b.Port)
	}
}

func Test他のノードが担当するものは操作しない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("remote"), Base: name("prod"), Node: "node2", Status: domain.BranchReady, Runtime: mysql84})
	_ = e.store.PutBase(ctx, domain.Base{Name: name("far"), Node: "node2", Status: domain.BaseReady, Runtime: mysql84})
	checks := map[string]error{
		"reset":         func() error { _, err := e.svc.ResetBranch(ctx, name("remote"), nil); return err }(),
		"delete":        e.svc.DeleteBranch(ctx, name("remote"), nil),
		"snapshot base": func() error { _, err := e.svc.SnapshotBase(ctx, name("far"), nil); return err }(),
		"start base":    e.svc.StartBase(ctx, name("far")),
		"stop base":     e.svc.StopBase(ctx, name("far")),
		"delete base":   e.svc.DeleteBase(ctx, name("far")),
	}
	for what, err := range checks {
		if !errors.Is(err, dbbranch.ErrOtherNode) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if b, _ := e.store.GetBranch(ctx, name("remote")); b.Status != domain.BranchReady {
		t.Errorf("remote branch touched: %+v", b)
	}
	if len(e.j.entries) != 0 {
		t.Errorf("did work: %v", e.j.entries)
	}
}

func TestExpireとRecover_自分が担当するブランチだけを扱う(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	past := e.now.Add(-time.Hour)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("remote"), Base: name("prod"), Node: "node2", Status: domain.BranchReady, Runtime: mysql84, ExpiresAt: &past})
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("remote-new"), Base: name("prod"), Node: "node2", Status: domain.BranchProvisioning, Runtime: mysql84})
	if deleted, err := e.svc.Expire(ctx); err != nil || len(deleted) != 0 {
		t.Errorf("Expire = %v, %v", deleted, err)
	}
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := e.store.GetBranch(ctx, name("remote-new")); b.Status != domain.BranchProvisioning {
		t.Errorf("another node's create was marked: %+v", b)
	}
	if len(e.j.entries) != 0 {
		t.Errorf("did work: %v", e.j.entries)
	}
}

func TestAdopt_担当のない記録はデータセットを持つノードが引き取る(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "here")
	// Records written before nodes were tracked.
	for _, n := range []string{"here"} {
		b, _ := e.store.GetBranch(ctx, name(n))
		b.Node = ""
		_ = e.store.PutBranch(ctx, b)
	}
	base, _ := e.store.GetBase(ctx, name("prod"))
	base.Node = ""
	_ = e.store.PutBase(ctx, base)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("elsewhere"), Base: name("prod")})
	_ = e.store.PutBase(ctx, domain.Base{Name: name("gone")})

	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{"here": "node1", "elsewhere": ""} {
		if b, _ := e.store.GetBranch(ctx, name(n)); b.Node != want {
			t.Errorf("branch %s node = %q, want %q", n, b.Node, want)
		}
	}
	for n, want := range map[string]string{"prod": "node1", "gone": ""} {
		if b, _ := e.store.GetBase(ctx, name(n)); b.Node != want {
			t.Errorf("base %s node = %q, want %q", n, b.Node, want)
		}
	}
}

func TestRuntimeInUse_自分のベースかブランチが使っていれば使用中(t *testing.T) {
	e := newEnv(t)
	other := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
	if used, err := e.svc.RuntimeInUse(ctx, mysql84); err != nil || used {
		t.Errorf("empty: %v %v", used, err)
	}
	_ = e.store.PutBase(ctx, domain.Base{Name: name("far"), Node: "node2", Runtime: other})
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("far-b"), Node: "node2", Runtime: other})
	if used, _ := e.svc.RuntimeInUse(ctx, other); used {
		t.Error("another node's base and branch do not count")
	}
	e.withBase(t)
	if used, _ := e.svc.RuntimeInUse(ctx, mysql84); !used {
		t.Error("own base")
	}
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("mine"), Node: "node1", Runtime: other})
	if used, _ := e.svc.RuntimeInUse(ctx, other); !used {
		t.Error("own branch")
	}
}

func TestPlaceBranch_dbbranchのidがクラスタと違うノードには置かず理由を409で返す(t *testing.T) {
	e := newEnv(t)
	nodes := e.clustered()
	e.withBase(t)
	nodes.nodes[0].IDProblem = "uid 103 here, uid 109 in the cluster"
	_, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node1"})
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "uid 103") {
		t.Errorf("err = %v", err)
	}
}
