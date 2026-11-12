package dbbranch_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

const replicaDS = "tank/db-branch/replicas/prod"

func list(n ...string) *[]string { return &n }

func TestReplicate_レプリカ先に含まれないノードは受け取らない(t *testing.T) {
	p := newPair(t)
	p.withBase(t)
	if _, err := p.origin.svc.SetReplicaNodes(ctx, name("prod"), list()); err != nil {
		t.Fatal(err)
	}
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.replica.j.entries) != 0 {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
	if _, err := p.origin.svc.SetReplicaNodes(ctx, name("prod"), list("node2")); err != nil {
		t.Fatal(err)
	}
	_ = p.replica.svc.Replicate(ctx)
	got, _ := p.origin.store.GetBase(ctx, name("prod"))
	if len(got.Replicas) == 0 {
		t.Errorf("a listed node should pull: %v", got.Replicas)
	}
}

func TestReplicate_外されたノードはコピーを消して記録も外す(t *testing.T) {
	p := newPair(t)
	base := p.withBase(t)
	_ = p.replica.svc.Replicate(ctx)
	snap := base.Snapshots[0].Name
	_, _ = p.origin.svc.SetReplicaNodes(ctx, name("prod"), list("node3"))
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.replica.j.has("zfs destroy " + replicaDS) {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
	got, _ := p.origin.store.GetBase(ctx, name("prod"))
	if got.HasReplica(snap, "node2") {
		t.Errorf("still recorded: %v", got.Replicas)
	}
}

func TestReplicate_外されたノードでもブランチが使っている間はコピーを残し配置先から外す(t *testing.T) {
	p := newPair(t)
	p.withBase(t)
	_ = p.replica.svc.Replicate(ctx)
	br, err := p.replica.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"}, nil)
	if err != nil || br.Node != "node2" {
		t.Fatalf("branch %+v %v", br, err)
	}
	_, _ = p.origin.svc.SetReplicaNodes(ctx, name("prod"), list())
	p.replica.j.entries = nil
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if p.replica.j.has("zfs destroy " + replicaDS) {
		t.Fatalf("destroyed under a live clone: %v", p.replica.j.entries)
	}
	got, _ := p.origin.store.GetBase(ctx, name("prod"))
	if !got.Draining("node2") {
		t.Errorf("node2 should be draining: %v", got.Replicas)
	}
	if _, err := p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("y"), Base: name("prod"), Node: "node2"}); err == nil {
		t.Error("a draining node must not be chosen")
	}
	if n, err := p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("y"), Base: name("prod")}); err != nil || n != "node1" {
		t.Errorf("PlaceBranch = %q %v", n, err)
	}
	if err := p.replica.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.replica.j.has("zfs destroy " + replicaDS) {
		t.Errorf("after the branch is gone: %v", p.replica.j.entries)
	}
}

func TestReplicate_コピーの整理の失敗は理由を返す(t *testing.T) {
	for _, failOn := range []string{"list-branches", "remove-replica"} {
		p := newPair(t)
		p.withBase(t)
		_ = p.replica.svc.Replicate(ctx)
		_, _ = p.origin.svc.SetReplicaNodes(ctx, name("prod"), list())
		p.replica.svc.Store = &failingStore{Store: p.origin.store, failOn: failOn}
		if err := p.replica.svc.Replicate(ctx); err == nil {
			t.Errorf("%s: want error", failOn)
		}
	}
}

func TestSetReplicaNodes_存在しないノードは断り起点と重複は畳んで並べる(t *testing.T) {
	e := newEnv(t)
	e.clustered()
	e.withBase(t)
	if _, err := e.svc.SetReplicaNodes(ctx, name("prod"), list("node9")); !errors.Is(err, domain.ErrInvalidReplicas) {
		t.Errorf("unknown node: %v", err)
	}
	b, err := e.svc.SetReplicaNodes(ctx, name("prod"), list("node3", "node1", " node2", "node3"))
	if err != nil || b.ReplicaNodes == nil || strings.Join(*b.ReplicaNodes, ",") != "node2,node3" {
		t.Fatalf("base %+v %v", b, err)
	}
	if b, _ = e.svc.SetReplicaNodes(ctx, name("prod"), nil); b.ReplicaNodes != nil {
		t.Errorf("nil should mean every node: %v", *b.ReplicaNodes)
	}
	if _, err := e.svc.SetReplicaNodes(ctx, name("nope"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing base: %v", err)
	}
	e.svc.Store = &failingStore{Store: e.store, failOn: "set-replicas"}
	if _, err := e.svc.SetReplicaNodes(ctx, name("prod"), nil); !errors.Is(err, errStore) {
		t.Errorf("store: %v", err)
	}
	e.svc.Store, e.svc.Nodes = e.store, nil
	if _, err := e.svc.SetReplicaNodes(ctx, name("prod"), list()); err != nil {
		t.Errorf("single node: %v", err)
	}
}

func TestCreateBase_レプリカ先を指定して作れる(t *testing.T) {
	e := newEnv(t)
	e.clustered()
	b, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84, ReplicaNodes: list("node2")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetBase(ctx, b.Name)
	if got.ReplicaNodes == nil || (*got.ReplicaNodes)[0] != "node2" {
		t.Errorf("stored = %+v", got)
	}
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("bad"), Runtime: mysql84, ReplicaNodes: list("nodeX")}, nil); !errors.Is(err, domain.ErrInvalidReplicas) {
		t.Errorf("unknown node: %v", err)
	}
	if _, err := e.store.GetBase(ctx, name("bad")); err == nil {
		t.Error("nothing should be reserved")
	}
	e.svc.Store = &failingStore{Store: e.store, failOn: "set-replicas"}
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("old"), Runtime: mysql84, ReplicaNodes: list()}, nil); !errors.Is(err, errStore) {
		t.Errorf("old cluster: %v", err)
	}
	if _, err := e.store.GetBase(ctx, name("old")); err == nil {
		t.Error("the reservation should be rolled back")
	}
}

func TestPlaceBranch_置ける先がなければ保持ノードと設定を伝える(t *testing.T) {
	e := newEnv(t)
	nodes := e.clustered()
	e.withBase(t)
	_, _ = e.svc.SetReplicaNodes(ctx, name("prod"), list("node2"))
	nodes.nodes[0].MemoryMB = 100
	nodes.nodes[1].MemoryMB = 100
	_, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")})
	if !errors.Is(err, domain.ErrNoCapacity) || !strings.Contains(err.Error(), "(node1)") || !strings.Contains(err.Error(), "kept only on") {
		t.Errorf("err = %v", err)
	}
	_, _ = e.svc.SetReplicaNodes(ctx, name("prod"), list())
	_, err = e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")})
	if err == nil || !strings.Contains(err.Error(), "no other node") {
		t.Errorf("err = %v", err)
	}
}
