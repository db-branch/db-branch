package dbbranch_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

var pg17 = domain.RuntimeRef{Engine: domain.Postgres, Version: "17.2"}

const (
	u24 = "ubuntu-24.04"
	u26 = "ubuntu-26.04"
)

// mixedPair is a pair of nodes where node1 and node3 run Ubuntu 24.04 and
// node2 runs 26.04; both services run PostgreSQL and MySQL.
func mixedPair(t *testing.T) *pair {
	t.Helper()
	p := newPair(t)
	for _, e := range []*env{p.origin, p.replica} {
		e.svc.Servers[domain.Postgres] = e.server
		e.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql84: true, pg17: true}}
		for i, d := range []string{u24, u26, u24} {
			e.svc.Nodes.(*fakeNodes).nodes[i].System.Distro = d
		}
	}
	p.origin.svc.Distro, p.replica.svc.Distro = u24, u26
	return p
}

func (p *pair) pgBase(t *testing.T, rt domain.RuntimeRef) domain.Base {
	t.Helper()
	b, err := p.origin.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: rt, Dump: "/dumps/prod.sql.gz"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.origin.svc.Streams.(*fakeStreams).snaps["tank/db-branch/bases/prod"] = []string{b.Snapshots[0].Name}
	return b
}

func wantCollation(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "collation") {
		t.Errorf("err = %v, want a conflict explaining the collation risk", err)
	}
}

func TestCreateBase_スナップショットに作成元のディストリビューションを記録する(t *testing.T) {
	p := mixedPair(t)
	b := p.pgBase(t, pg17)
	if got, _ := p.origin.store.GetBase(ctx, b.Name); got.Snapshots[0].Distro != u24 {
		t.Errorf("distro = %q", got.Snapshots[0].Distro)
	}
	p.origin.svc.Distro = u26
	p.origin.now = p.origin.now.Add(time.Minute)
	snap, err := p.origin.svc.SnapshotBase(ctx, name("prod"), nil)
	if err != nil || snap.Distro != u26 {
		t.Errorf("refreshed snapshot = %+v %v", snap, err)
	}
}

func TestPlaceBranch_PostgreSQLは作成元と同じディストリビューションのノードにだけ置く(t *testing.T) {
	p := mixedPair(t)
	b := p.pgBase(t, pg17)
	snap := b.Snapshots[0].Name
	_ = p.origin.store.AddReplica(ctx, name("prod"), snap, "node2")
	_ = p.origin.store.AddReplica(ctx, name("prod"), snap, "node3")
	nodes := p.origin.svc.Nodes.(*fakeNodes)
	nodes.nodes[0].LastSeen = p.origin.now.Add(-time.Minute) // node1 is down
	n, err := p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")})
	if err != nil || n != "node3" {
		t.Errorf("PlaceBranch = %q %v, want node3", n, err)
	}
	nodes.nodes[2].LastSeen = p.origin.now.Add(-time.Minute) // node3 too
	_, err = p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")})
	wantCollation(t, err) // only node2, of another distro, is left up
}

func TestPlaceBranch_別ディストリビューションのノードしか残らなければ衝突として拒む(t *testing.T) {
	p := mixedPair(t)
	b := p.pgBase(t, pg17)
	_ = p.origin.store.AddReplica(ctx, name("prod"), b.Snapshots[0].Name, "node2")
	p.origin.svc.Nodes.(*fakeNodes).nodes[0].LastSeen = p.origin.now.Add(-time.Minute)
	_, err := p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")})
	wantCollation(t, err)
}

func TestPlaceBranch_明示したノードが別ディストリビューションなら拒む(t *testing.T) {
	p := mixedPair(t)
	b := p.pgBase(t, pg17)
	_ = p.origin.store.AddReplica(ctx, name("prod"), b.Snapshots[0].Name, "node2")
	_, err := p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"})
	wantCollation(t, err)
	if _, err := p.origin.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"}, nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("CreateBranch err = %v", err)
	}
}

func TestPlaceBranch_記録の無い古いスナップショットは担当ノードの今の配布版を元とみなす(t *testing.T) {
	p := mixedPair(t)
	b := p.pgBase(t, pg17)
	b.Snapshots[0].Distro = ""
	if err := p.origin.store.PutBase(ctx, b); err != nil {
		t.Fatal(err)
	}
	_ = p.origin.store.AddReplica(ctx, name("prod"), b.Snapshots[0].Name, "node2")
	_, err := p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"})
	wantCollation(t, err)
	// a node on another service view: origin known only through the node list
	p.replica.svc.Distro = ""
	_, err = p.replica.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"})
	wantCollation(t, err)
}

func TestPlaceBranch_MySQLはディストリビューションが違っても置ける(t *testing.T) {
	p := mixedPair(t)
	b := p.pgBase(t, mysql84)
	if b.Snapshots[0].Distro != u24 {
		t.Fatalf("recorded anyway: %+v", b.Snapshots)
	}
	_ = p.origin.store.AddReplica(ctx, name("prod"), b.Snapshots[0].Name, "node2")
	n, err := p.origin.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Node: "node2"})
	if err != nil || n != "node2" {
		t.Errorf("PlaceBranch = %q %v", n, err)
	}
	if _, err := p.origin.svc.SetReplicaNodes(ctx, name("prod"), list("node2")); err != nil {
		t.Errorf("mysql replicas on another distro: %v", err)
	}
}

func TestSetReplicaNodes_PostgreSQLのベースに別ディストリビューションのノードは加えられない(t *testing.T) {
	p := mixedPair(t)
	p.pgBase(t, pg17)
	_, err := p.origin.svc.SetReplicaNodes(ctx, name("prod"), list("node3", "node2"))
	wantCollation(t, err)
	if got, err := p.origin.svc.SetReplicaNodes(ctx, name("prod"), list("node3")); err != nil || got.ReplicaNodes == nil {
		t.Errorf("same distro: %+v %v", got, err)
	}
	p.origin.svc.Nodes.(*fakeNodes).err = errors.New("raft down")
	if _, err := p.origin.svc.SetReplicaNodes(ctx, name("prod"), list("node3")); err == nil {
		t.Error("node list failure: want error")
	}
}

func TestCreateBase_PostgreSQLのレプリカ先に別ディストリビューションのノードは指定できない(t *testing.T) {
	p := mixedPair(t)
	_, err := p.origin.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: pg17, ReplicaNodes: list("node2")}, nil)
	wantCollation(t, err)
	if _, err := p.origin.store.GetBase(ctx, name("prod")); err == nil {
		t.Error("base must not be created")
	}
}

func TestReplicate_PostgreSQLのコピーは別ディストリビューションのノードへ送らない(t *testing.T) {
	p := mixedPair(t)
	p.pgBase(t, pg17)
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.replica.j.entries) != 0 {
		t.Errorf("node2 (other distro) received: %v", p.replica.j.entries)
	}
}

func TestReplicate_MySQLのコピーは別ディストリビューションのノードにも送る(t *testing.T) {
	p := mixedPair(t)
	p.pgBase(t, mysql84)
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.replica.j.entries) == 0 {
		t.Error("mysql copy should be received")
	}
}

func TestPlaceRecreate_ノード喪失後の作り直しも同じディストリビューションに限る(t *testing.T) {
	p := mixedPair(t)
	b := p.pgBase(t, pg17)
	if _, err := p.origin.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil); err != nil {
		t.Fatal(err)
	}
	_ = p.origin.store.AddReplica(ctx, name("prod"), b.Snapshots[0].Name, "node2")
	p.replica.svc.Nodes.(*fakeNodes).nodes[0].LastSeen = p.replica.now.Add(-time.Minute)
	_, err := p.replica.svc.PlaceRecreate(ctx, name("x"))
	wantCollation(t, err)
	_, err = p.replica.svc.RecreateBranch(ctx, name("x"), nil)
	if err == nil {
		t.Error("recreate on another distro must be refused")
	}
	_ = p.origin.store.AddReplica(ctx, name("prod"), b.Snapshots[0].Name, "node3")
	if n, err := p.replica.svc.PlaceRecreate(ctx, name("x")); err != nil || n != "node3" {
		t.Errorf("PlaceRecreate = %q %v, want node3", n, err)
	}
}

func TestPlaceBranch_単独ノードでも自分のディストリビューションで判定する(t *testing.T) {
	e := newEnv(t)
	e.svc.Servers[domain.Postgres] = e.server
	e.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{pg17: true}}
	e.svc.Distro = u26
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: pg17}, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.PlaceBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}); err != nil || n != "node1" {
		t.Errorf("PlaceBranch = %q %v", n, err)
	}
}
