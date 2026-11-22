package raftstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/membership"
)

// growingCluster starts n connected stores; only node0 is bootstrapped, so the
// others can be joined.
func growingCluster(t *testing.T, n int) *cluster {
	t.Helper()
	c := &cluster{}
	for i := 0; i < n; i++ {
		_, tr := raft.NewInmemTransport(raft.ServerAddress(fmt.Sprintf("node%d", i)))
		c.trans = append(c.trans, tr)
	}
	for i := range c.trans {
		for j := range c.trans {
			if i != j {
				c.trans[i].Connect(c.trans[j].LocalAddr(), c.trans[j])
			}
		}
	}
	for i := 0; i < n; i++ {
		s, err := raftstore.Open(raftstore.Options{
			NodeID: fmt.Sprintf("node%d", i), APIAddr: fmt.Sprintf("api%d:7070", i),
			Transport: c.trans[i], InMemory: true, Fast: true, Forwarder: directForwarder{c},
		})
		if err != nil {
			t.Fatal(err)
		}
		c.stores = append(c.stores, s)
		t.Cleanup(func() { _ = s.Close() })
	}
	if err := c.stores[0].Bootstrap([]raft.Server{{ID: "node0", Address: "node0"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, c.stores[0].IsLeader)
	return c
}

func (c *cluster) joinAll(t *testing.T) {
	t.Helper()
	for i := 1; i < len(c.stores); i++ {
		if err := c.stores[0].AddNonvoter(fmt.Sprintf("node%d", i), fmt.Sprintf("node%d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func voterIDs(s *raftstore.Store) (voters, non int) {
	servers, err := s.Servers(context.Background())
	if err != nil {
		return -1, -1
	}
	for _, sv := range servers {
		if sv.Voter {
			voters++
		} else {
			non++
		}
	}
	return
}

func TestStore_非投票者として参加すると複製を受けるが投票者には数えない(t *testing.T) {
	c := growingCluster(t, 2)
	_ = c.stores[0].PutBase(ctx, domain.Base{Name: name("prod")})
	if err := c.stores[0].AddNonvoter("node1", "node1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, err := c.stores[1].GetBase(ctx, name("prod")); return err == nil })
	if v, n := voterIDs(c.stores[0]); v != 1 || n != 1 {
		t.Errorf("voters, non-voters = %d, %d", v, n)
	}
	if c.stores[0].Voters() != 1 {
		t.Errorf("Voters() = %d", c.stores[0].Voters())
	}
}

func TestStore_非投票者を昇格し降格できる(t *testing.T) {
	c := growingCluster(t, 2)
	c.joinAll(t)
	if err := c.stores[0].Promote(ctx, "node1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := voterIDs(c.stores[0]); v != 2 {
		t.Fatalf("voters after promote = %d", v)
	}
	if err := c.stores[0].Demote(ctx, "node1"); err != nil {
		t.Fatal(err)
	}
	if v, n := voterIDs(c.stores[0]); v != 1 || n != 1 {
		t.Errorf("after demote: %d, %d", v, n)
	}
}

func TestStore_未知のノードは昇格できない(t *testing.T) {
	c := growingCluster(t, 1)
	if err := c.stores[0].Promote(ctx, "ghost"); err == nil {
		t.Error("promote of an unknown server: want error")
	}
}

func TestStore_フォロワーはサーバー一覧を返せるがリーダーでなければ昇格できない(t *testing.T) {
	c := growingCluster(t, 2)
	c.joinAll(t)
	eventually(t, func() bool { v, n := voterIDs(c.stores[1]); return v == 1 && n == 1 })
	if err := c.stores[1].Promote(ctx, "node1"); err == nil {
		t.Error("promote on a follower: want error")
	}
}

func TestStore_6ノードは自動運転で5投票者に収束する(t *testing.T) {
	c := growingCluster(t, 6)
	c.joinAll(t)
	// every node heartbeats as healthy
	now := time.Now()
	for i := range c.stores {
		if err := c.stores[0].PutNode(ctx, domain.Node{Name: fmt.Sprintf("node%d", i), Address: c.stores[i].APIAddress(), LastSeen: now, Protocol: raftstore.Protocol}); err != nil {
			t.Fatal(err)
		}
	}
	clock := now
	svc := &membership.Service{
		Members: c.stores[0], Self: "node0", DownAfter: 30 * time.Second,
		Clock: func() time.Time { return clock },
		Nodes: c.stores[0].ListNodes,
	}
	for i := 0; i < 20; i++ {
		clock = clock.Add(10 * time.Second)
		if _, err := svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		// keep heartbeats fresh
		for j := range c.stores {
			_ = c.stores[0].PutNode(ctx, domain.Node{Name: fmt.Sprintf("node%d", j), Address: c.stores[j].APIAddress(), LastSeen: clock, Protocol: raftstore.Protocol})
		}
	}
	if v, n := voterIDs(c.stores[0]); v != 5 || n != 1 {
		t.Fatalf("voters, non-voters = %d, %d", v, n)
	}
	// writes still commit with the voters only
	if err := c.stores[0].PutBase(ctx, domain.Base{Name: name("after")}); err != nil {
		t.Fatal(err)
	}
}

func TestStore_AddNonvoter_同名別アドレスは拒否し同アドレスは冪等で降格しない(t *testing.T) {
	c := growingCluster(t, 2)
	if err := c.stores[0].AddNonvoter("", "node1"); err == nil {
		t.Error("empty id: want error")
	}
	if err := c.stores[0].AddNonvoter("node1", ""); err == nil {
		t.Error("empty addr: want error")
	}
	if err := c.stores[1].AddNonvoter("node1", "node1"); !errors.Is(err, domain.ErrNoLeader) {
		t.Errorf("follower AddNonvoter: err = %v, want ErrNoLeader", err)
	}
	if err := c.stores[0].AddNonvoter("node1", "node1"); err != nil {
		t.Fatal(err)
	}
	// Duplicate ID with a different address must be rejected.
	if err := c.stores[0].AddNonvoter("node1", "other-host:7071"); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate ID different addr: err = %v, want ErrAlreadyExists", err)
	}
	// Same address with a different ID must also be rejected.
	if err := c.stores[0].AddNonvoter("node2", "node1"); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("same addr different ID: err = %v, want ErrAlreadyExists", err)
	}
	// Promote node1 to voter, then retry AddNonvoter with the SAME address: must be idempotent and keep Voter suffrage.
	if err := c.stores[0].Promote(ctx, "node1"); err != nil {
		t.Fatal(err)
	}
	if err := c.stores[0].AddNonvoter("node1", "node1"); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if v, n := voterIDs(c.stores[0]); v != 2 || n != 0 {
		t.Errorf("after idempotent retry: voters=%d non=%d, want 2, 0", v, n)
	}
}

func TestStore_RemoveServerで構成からノードを外せる(t *testing.T) {
	c := growingCluster(t, 3)
	c.joinAll(t)
	if err := c.stores[0].RemoveServer(context.Background(), "node2"); err != nil {
		t.Fatal(err)
	}
	servers, err := c.stores[0].Servers(context.Background())
	if err != nil || len(servers) != 2 {
		t.Fatalf("servers = %v, %v", servers, err)
	}
	for _, s := range servers {
		if s.ID == "node2" {
			t.Fatal("node2 is still a member")
		}
	}
	// A second removal of the same node changes nothing and does not fail.
	if err := c.stores[0].RemoveServer(context.Background(), "node2"); err != nil {
		t.Fatal(err)
	}
}

func TestStore_RemoveServerはリーダー以外ではErrNoLeader(t *testing.T) {
	c := growingCluster(t, 2)
	c.joinAll(t)
	eventually(t, func() bool { return len(serversOf(c.stores[1])) == 2 })
	err := c.stores[1].RemoveServer(context.Background(), "node0")
	if !errors.Is(err, domain.ErrNoLeader) {
		t.Fatalf("err = %v, want ErrNoLeader", err)
	}
}

func serversOf(s *raftstore.Store) []membership.Server {
	out, _ := s.Servers(context.Background())
	return out
}
