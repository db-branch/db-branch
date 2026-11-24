package raftstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

var ctx = context.Background()

// cluster is n in-memory Raft nodes; followers forward writes straight to the leader's store.
type cluster struct {
	stores []*raftstore.Store
	trans  []*raft.InmemTransport
}

type directForwarder struct{ c *cluster }

func (d directForwarder) Forward(ctx context.Context, apiAddr string, cmd raftstore.Command) (uint64, error) {
	for _, s := range d.c.stores {
		if s.APIAddress() == apiAddr {
			return s.ApplyAt(ctx, cmd)
		}
	}
	return 0, fmt.Errorf("no node at %s", apiAddr)
}

func newCluster(t *testing.T, n int) *cluster { return newClusterWith(t, n, true) }

func newClusterWith(t *testing.T, n int, forward bool) *cluster {
	t.Helper()
	c := &cluster{}
	var servers []raft.Server
	for i := 0; i < n; i++ {
		addr, tr := raft.NewInmemTransport(raft.ServerAddress(fmt.Sprintf("node%d", i)))
		c.trans = append(c.trans, tr)
		servers = append(servers, raft.Server{ID: raft.ServerID(fmt.Sprintf("node%d", i)), Address: addr})
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
			NodeID:    fmt.Sprintf("node%d", i),
			APIAddr:   fmt.Sprintf("api%d:7070", i),
			Transport: c.trans[i],
			InMemory:  true,
			Forwarder: forwarderIf(forward, c),
			Fast:      true,
		})
		if err != nil {
			t.Fatal(err)
		}
		c.stores = append(c.stores, s)
		t.Cleanup(func() { _ = s.Close() })
	}
	if err := c.stores[0].Bootstrap(servers); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *cluster) leader(t *testing.T) *raftstore.Store {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range c.stores {
			if s.IsLeader() {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader elected")
	return nil
}

// registerNodes writes each store's node record so followers can find the leader's API.
func (c *cluster) registerNodes(t *testing.T) {
	t.Helper()
	l := c.leader(t)
	for i, s := range c.stores {
		if err := l.PutNode(ctx, domain.Node{Name: fmt.Sprintf("node%d", i), Address: s.APIAddress(), RaftAddress: fmt.Sprintf("node%d", i), Protocol: raftstore.Protocol}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range c.stores {
		eventually(t, func() bool { return len(s.State().Nodes) == len(c.stores) })
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStore_リーダーが自動で選ばれ書き込みが全ノードに複製される(t *testing.T) {
	c := newCluster(t, 3)
	l := c.leader(t)

	if err := l.PutBase(ctx, domain.Base{Name: name("prod"), Status: domain.BaseReady}); err != nil {
		t.Fatal(err)
	}
	for _, s := range c.stores {
		eventually(t, func() bool { _, err := s.GetBase(ctx, name("prod")); return err == nil })
	}
}

func TestStore_フォロワーへの書き込みはリーダーに転送される(t *testing.T) {
	c := newCluster(t, 3)
	c.registerNodes(t)
	var follower *raftstore.Store
	for _, s := range c.stores {
		if !s.IsLeader() {
			follower = s
			break
		}
	}
	eventually(t, func() bool { nodes, _ := follower.ListNodes(ctx); return len(nodes) == 3 })

	if err := follower.PutBranch(ctx, domain.Branch{Name: name("x"), Status: domain.BranchReady}); err != nil {
		t.Fatal(err)
	}
	for _, s := range c.stores {
		eventually(t, func() bool { _, err := s.GetBranch(ctx, name("x")); return err == nil })
	}
	if err := follower.DeleteBranch(ctx, name("x")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, err := follower.GetBranch(ctx, name("x")); return errors.Is(err, domain.ErrNotFound) })
}

func TestStore_リーダーが落ちたら残りから新しいリーダーが選ばれる(t *testing.T) {
	c := newCluster(t, 3)
	old := c.leader(t)
	_ = old.PutBase(ctx, domain.Base{Name: name("prod")})
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	var next *raftstore.Store
	eventually(t, func() bool {
		for _, s := range c.stores {
			if s != old && s.IsLeader() {
				next = s
				return true
			}
		}
		return false
	})
	eventually(t, func() bool {
		_, err := next.GetBase(ctx, name("prod"))
		return err == nil
	})
	if err := next.PutBase(ctx, domain.Base{Name: name("after")}); err != nil {
		t.Errorf("new leader cannot write: %v", err)
	}
}

func TestStore_リーダーがいなければErrNoLeader(t *testing.T) {
	addr, tr := raft.NewInmemTransport("lonely")
	s, err := raftstore.Open(raftstore.Options{NodeID: "lonely", Transport: tr, InMemory: true, Fast: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = addr
	if err := s.PutBase(ctx, domain.Base{Name: name("x")}); !errors.Is(err, domain.ErrNoLeader) {
		t.Errorf("error = %v, want ErrNoLeader", err)
	}
}

func TestStore_一覧と削除と参加用の秘密値(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	for _, n := range []string{"b", "a"} {
		_ = l.PutBase(ctx, domain.Base{Name: name(n)})
		_ = l.PutBranch(ctx, domain.Branch{Name: name(n)})
		_ = l.PutNode(ctx, domain.Node{Name: n})
	}
	bases, _ := l.ListBases(ctx)
	branches, _ := l.ListBranches(ctx)
	nodes, _ := l.ListNodes(ctx)
	if len(bases) != 2 || bases[0].Name.String() != "a" || len(branches) != 2 || branches[0].Name.String() != "a" || len(nodes) != 2 || nodes[0].Name != "a" {
		t.Errorf("lists: %v %v %v", bases, branches, nodes)
	}
	if err := l.DeleteBase(ctx, name("a")); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteNode(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{l.DeleteBase(ctx, name("zz")), l.DeleteBranch(ctx, name("zz"))} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing delete: %v", err)
		}
	}
	if _, err := l.GetBranch(ctx, name("zz")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing get: %v", err)
	}
	if err := l.SetJoinSecretHash(ctx, "h"); err != nil || l.JoinSecretHash() != "h" {
		t.Errorf("join secret: %v %q", err, l.JoinSecretHash())
	}
	if id, addr := l.Leader(); id != "node0" || addr == "" {
		t.Errorf("Leader = %q %q", id, addr)
	}
}

func TestStore_ディスクに保存し再起動後も状態が残る(t *testing.T) {
	dir := t.TempDir()
	if raftstore.HasState(dir) {
		t.Fatal("empty dir has no state")
	}
	open := func() *raftstore.Store {
		_, tr := raft.NewInmemTransport("disk0")
		s, err := raftstore.Open(raftstore.Options{NodeID: "disk0", Dir: dir, Transport: tr, Fast: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	if err := s.Bootstrap([]raft.Server{{ID: "disk0", Address: "disk0"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, s.IsLeader)
	if err := s.PutBase(ctx, domain.Base{Name: name("prod")}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if !raftstore.HasState(dir) {
		t.Fatal("state should exist on disk")
	}
	r := open()
	defer r.Close()
	eventually(t, func() bool { _, err := r.GetBase(ctx, name("prod")); return err == nil })
	if len(r.State().Bases) != 1 {
		t.Errorf("state = %+v", r.State())
	}
}

func TestStore_置き場を作れなければ開けない(t *testing.T) {
	file := t.TempDir() + "/file"
	_ = os.WriteFile(file, nil, 0o600)
	_, tr := raft.NewInmemTransport("x")
	if _, err := raftstore.Open(raftstore.Options{NodeID: "x", Dir: file + "/raft", Transport: tr}); err == nil {
		t.Error("want error")
	}
}

func TestStore_後から投票者を追加できる(t *testing.T) {
	_, tr0 := raft.NewInmemTransport("n0")
	_, tr1 := raft.NewInmemTransport("n1")
	tr0.Connect("n1", tr1)
	tr1.Connect("n0", tr0)
	s0, _ := raftstore.Open(raftstore.Options{NodeID: "n0", Transport: tr0, InMemory: true, Fast: true})
	s1, _ := raftstore.Open(raftstore.Options{NodeID: "n1", Transport: tr1, InMemory: true, Fast: true})
	defer s0.Close()
	defer s1.Close()
	_ = s0.Bootstrap([]raft.Server{{ID: "n0", Address: "n0"}})
	eventually(t, s0.IsLeader)
	_ = s0.PutBase(ctx, domain.Base{Name: name("prod")})

	if err := s0.AddVoter("n1", "n1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, err := s1.GetBase(ctx, name("prod")); return err == nil })
}

func TestStore_リーダーのAPIが分からなければ転送できない(t *testing.T) {
	c := newCluster(t, 3)
	c.leader(t)
	for _, s := range c.stores {
		if !s.IsLeader() {
			if err := s.PutBase(ctx, domain.Base{Name: name("x")}); !errors.Is(err, domain.ErrNoLeader) {
				t.Errorf("error = %v", err)
			}
			return
		}
	}
}

func TestStore_適用に失敗したコマンドはエラーを返す(t *testing.T) {
	c := newCluster(t, 1)
	if err := c.leader(t).Apply(ctx, raftstore.Command{Op: "nope"}); err == nil {
		t.Error("want error")
	}
}

func forwarderIf(on bool, c *cluster) raftstore.Forwarder {
	if on {
		return directForwarder{c}
	}
	return nil
}

func TestStore_転送役がないフォロワーは書き込めない(t *testing.T) {
	c := newClusterWith(t, 3, false)
	c.leader(t)
	for _, s := range c.stores {
		if !s.IsLeader() {
			if err := s.PutBase(ctx, domain.Base{Name: name("x")}); !errors.Is(err, domain.ErrNoLeader) {
				t.Errorf("error = %v", err)
			}
			return
		}
	}
}

func TestOpen_設定が不正なら開いたファイルを閉じて失敗する(t *testing.T) {
	dir := t.TempDir()
	_, tr := raft.NewInmemTransport("x")
	if _, err := raftstore.Open(raftstore.Options{NodeID: "", Dir: dir, Transport: tr}); err == nil {
		t.Fatal("empty node ID: want error")
	}
	// The bolt file lock must be released: opening again must not hang.
	s, err := raftstore.Open(raftstore.Options{NodeID: "x", Dir: dir, Transport: tr, Fast: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}

func TestStore_挿入は全ノードで一意になりフォロワーからでもAlreadyExistsが返る(t *testing.T) {
	c := newCluster(t, 3)
	l := c.leader(t)
	c.registerNodes(t)
	var follower *raftstore.Store
	for _, s := range c.stores {
		if s != l {
			follower = s
		}
	}
	if err := follower.InsertBranch(ctx, domain.Branch{Name: name("x"), Node: "node1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.InsertBranch(ctx, domain.Branch{Name: name("x"), Node: "node2"}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("leader insert: %v", err)
	}
	if err := follower.InsertBase(ctx, domain.Base{Name: name("p")}); err != nil {
		t.Fatal(err)
	}
	if err := follower.InsertBase(ctx, domain.Base{Name: name("p")}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("follower base insert: %v", err)
	}
}

func TestStore_オペレーションをどのノードからも読める(t *testing.T) {
	c := newCluster(t, 3)
	l := c.leader(t)
	c.registerNodes(t)
	_ = l.PutOperation(ctx, operation.Operation{ID: "op_a", State: operation.Running, Seq: 1})
	_ = l.PutOperation(ctx, operation.Operation{ID: "op_b", State: operation.Running, Seq: 2})
	for _, s := range c.stores {
		eventually(t, func() bool { _, err := s.GetOperation(ctx, "op_b"); return err == nil })
		list, _ := s.ListOperations(ctx)
		if len(list) != 2 || list[0].ID != "op_b" {
			t.Errorf("list = %+v", list)
		}
	}
	if _, err := l.GetOperation(ctx, "op_zz"); !errors.Is(err, operation.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestStore_適用したコマンドを各ノードで通知する(t *testing.T) {
	addr, tr := raft.NewInmemTransport("solo")
	seen := make(chan string, 10)
	s, err := raftstore.Open(raftstore.Options{NodeID: "solo", Transport: tr, InMemory: true, Fast: true,
		OnApply: func(c raftstore.Command) { seen <- c.Op }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_ = s.Bootstrap([]raft.Server{{ID: "solo", Address: addr}})
	eventually(t, s.IsLeader)
	_ = s.PutBranch(ctx, domain.Branch{Name: name("x")})
	_ = s.InsertBranch(ctx, domain.Branch{Name: name("x")}) // rejected: no notification
	_ = s.DeleteBranch(ctx, name("x"))
	for _, want := range []string{"put_branch", "delete_branch"} {
		select {
		case got := <-seen:
			if got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no notification for %s", want)
		}
	}
}

func TestStore_フォロワーは書いた直後に自分で読める(t *testing.T) {
	c := newCluster(t, 3)
	l := c.leader(t)
	c.registerNodes(t)
	for _, f := range c.stores {
		if f == l {
			continue
		}
		for i := 0; i < 20; i++ {
			n := name(fmt.Sprintf("b%d", i))
			if err := f.PutBranch(ctx, domain.Branch{Name: n}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.GetBranch(ctx, n); err != nil {
				t.Fatalf("read after write on a follower: %v", err)
			}
		}
		return
	}
}

func TestAdminStore_パスワードはクラスタで一つでどのノードからも同じものを読む(t *testing.T) {
	c := newCluster(t, 3)
	l := c.leader(t)
	c.registerNodes(t)
	if _, err := (raftstore.AdminStore{Store: l}).Load(); !errors.Is(err, auth.ErrNoCredentials) {
		t.Errorf("empty: %v", err)
	}
	var follower *raftstore.Store
	for _, s := range c.stores {
		if s != l {
			follower = s
		}
	}
	want := auth.Credentials{Salt: []byte("s"), Hash: []byte("h"), Generation: 3}
	if err := (raftstore.AdminStore{Store: follower}).Save(want); err != nil {
		t.Fatal(err)
	}
	for _, s := range c.stores {
		eventually(t, func() bool {
			got, err := (raftstore.AdminStore{Store: s}).Load()
			return err == nil && got.Generation == 3 && string(got.Hash) == "h"
		})
	}
}

func TestAdminStore_初期化は未設定のときだけ効く(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	a := raftstore.AdminStore{Store: l}
	got, err := a.Init(auth.Credentials{Hash: []byte("first")})
	if err != nil || string(got.Hash) != "first" {
		t.Fatalf("first init: %+v %v", got, err)
	}
	got, err = a.Init(auth.Credentials{Hash: []byte("late")})
	if err != nil || string(got.Hash) != "first" {
		t.Errorf("a late init must not overwrite: %+v %v", got, err)
	}
}

func TestStore_古い版のノードがいる間は新しいコマンドを書かない(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	if l.ClusterProtocol() != raftstore.Protocol {
		t.Errorf("no members yet: %d", l.ClusterProtocol())
	}
	_ = l.PutNode(ctx, domain.Node{Name: "new", Protocol: raftstore.Protocol})
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: 3})
	_ = l.PutBase(ctx, domain.Base{Name: name("p"), Snapshots: []domain.BaseSnapshot{{Name: "s1"}}})
	if err := l.AddReplica(ctx, name("p"), "s1", "new"); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("add_replica with a protocol-3 node: %v", err)
	}
	if err := l.SetJoinSecretHash(ctx, "h"); err != nil {
		t.Errorf("an old command must still work: %v", err)
	}
	if err := l.SetBaseReplicaNodes(ctx, name("p"), &[]string{"new"}); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("set_base_replica_nodes with a protocol-3 node: %v", err)
	}
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: raftstore.Protocol})
	if err := l.AddReplica(ctx, name("p"), "s1", "new"); err != nil {
		t.Errorf("after the upgrade: %v", err)
	}
	if err := l.SetBaseReplicaNodes(ctx, name("p"), &[]string{"new"}); err != nil {
		t.Errorf("setting after the upgrade: %v", err)
	}
	if err := l.RemoveReplica(ctx, name("p"), "new"); err != nil {
		t.Errorf("remove_replica after the upgrade: %v", err)
	}
	_ = l.PutNode(ctx, domain.Node{Name: "future", Protocol: raftstore.Protocol + 5})
	if l.ClusterProtocol() != raftstore.Protocol {
		t.Errorf("capped at our own: %d", l.ClusterProtocol())
	}
}

func TestStore_全ノードに入れるランタイムを記録する(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	m80 := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
	m84 := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}
	for _, r := range []domain.RuntimeRef{m84, m80, m84} {
		if err := l.AddRuntime(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.Runtimes(); len(got) != 2 || got[0] != m80 {
		t.Errorf("Runtimes = %v", got)
	}
	_ = l.RemoveRuntime(ctx, m80)
	if got := l.Runtimes(); len(got) != 1 || got[0] != m84 {
		t.Errorf("after remove = %v", got)
	}
}

func TestStore_ルーティング用ユーザーを記録する(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	_ = l.PutRouteUser(ctx, domain.RouteUser{Name: "b", Password: "p"})
	_ = l.PutRouteUser(ctx, domain.RouteUser{Name: "a", Password: "p"})
	if list, _ := l.ListRouteUsers(ctx); len(list) != 2 || list[0].Name != "a" {
		t.Errorf("list = %+v", list)
	}
	if err := l.DeleteRouteUser(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteRouteUser(ctx, "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
}

func TestStore_クラスタのモジュールを記録する(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	_ = l.PutModule(ctx, domain.ModuleSpec{Name: "proxysql", Version: "1.0", Wanted: true})
	_ = l.PutModule(ctx, domain.ModuleSpec{Name: "audit", Version: "2.0", Wanted: true})
	_ = l.PutModule(ctx, domain.ModuleSpec{Name: "audit", Version: "2.0", Wanted: false})
	list, _ := l.ListModules(ctx)
	if len(list) != 2 || list[0].Name != "audit" || list[0].Wanted || !list[1].Wanted {
		t.Errorf("list = %+v", list)
	}
}

func TestStore_設定を範囲ごとに記録する(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	if v, _ := l.GetSettings(ctx, "core"); len(v) != 0 {
		t.Errorf("empty = %v", v)
	}
	_ = l.PutSettings(ctx, "core", map[string]string{"branch.memory_mb": "256"})
	_ = l.PutSettings(ctx, "proxysql", map[string]string{"comment_routing": "false"})
	v, _ := l.GetSettings(ctx, "core")
	v["branch.memory_mb"] = "mutated"
	if got, _ := l.GetSettings(ctx, "core"); got["branch.memory_mb"] != "256" {
		t.Errorf("core = %v", got)
	}
	if got, _ := l.GetSettings(ctx, "proxysql"); got["comment_routing"] != "false" {
		t.Errorf("proxysql = %v", got)
	}
}

func TestStore_試用の開始は最初の記録が残る(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	if at, err := l.TrialStarted(ctx); err != nil || !at.IsZero() {
		t.Errorf("not begun: %s %v", at, err)
	}
	first := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if at, err := l.StartTrial(ctx, first); err != nil || !at.Equal(first) {
		t.Fatalf("begun: %s %v", at, err)
	}
	// A node that has not caught up asks again: the first start stays.
	if at, err := l.StartTrial(ctx, first.AddDate(0, 0, 20)); err != nil || !at.Equal(first) {
		t.Errorf("a later start: %s %v", at, err)
	}
	if at, _ := l.TrialStarted(ctx); !at.Equal(first) {
		t.Errorf("recorded: %s", at)
	}
	// Until every member knows the command, no trial begins.
	c2 := newCluster(t, 1)
	l2 := c2.leader(t)
	_ = l2.PutNode(ctx, domain.Node{Name: "old", Protocol: 9})
	if _, err := l2.StartTrial(ctx, first); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("an older member: %v", err)
	}
}

func TestStore_APIトークンを記録し消せる(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	t0 := time.Unix(100, 0)
	_ = l.PutToken(ctx, domain.APIToken{ID: "b", CreatedAt: t0.Add(time.Second)})
	_ = l.PutToken(ctx, domain.APIToken{ID: "a", CreatedAt: t0})
	if list, _ := l.ListTokens(ctx); len(list) != 2 || list[0].ID != "a" {
		t.Errorf("list = %+v", list)
	}
	if err := l.DeleteToken(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteToken(ctx, "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
}

func TestStore_モジュールのデータを記録し消せる(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	if err := l.PutModuleData(ctx, "auth", "user/alice", []byte(`{"role":"viewer"}`)); err != nil {
		t.Fatal(err)
	}
	_ = l.PutModuleData(ctx, "auth", "user/bob", []byte("b"))
	_ = l.PutModuleData(ctx, "audit", "x", []byte("y"))
	got, _ := l.ListModuleData(ctx, "auth")
	if len(got) != 2 || string(got["user/alice"]) != `{"role":"viewer"}` {
		t.Errorf("auth = %v", got)
	}
	got["user/bob"][0] = 'z'
	if again, _ := l.ListModuleData(ctx, "auth"); string(again["user/bob"]) != "b" {
		t.Error("the list must be a copy")
	}
	_ = l.DeleteModuleData(ctx, "auth", "user/alice")
	_ = l.DeleteModuleData(ctx, "auth", "user/bob")
	_ = l.DeleteModuleData(ctx, "auth", "user/bob")
	if got, _ := l.ListModuleData(ctx, "auth"); len(got) != 0 {
		t.Errorf("after delete = %v", got)
	}
	if got, _ := l.ListModuleData(ctx, "audit"); string(got["x"]) != "y" {
		t.Errorf("audit = %v", got)
	}
}

func TestStore_更新スケジュールの保存と実行結果と削除(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	if _, err := l.GetBaseRefresh(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	for _, n := range []string{"b", "a"} {
		if err := l.SetBaseRefresh(ctx, domain.BaseRefresh{Base: name(n), Every: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RecordBaseRefreshRun(ctx, name("a"), domain.RefreshRun{OK: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.GetBaseRefresh(ctx, name("a")); got.LastRun == nil || !got.LastRun.OK {
		t.Errorf("get = %+v", got)
	}
	if list, _ := l.ListBaseRefreshes(ctx); len(list) != 2 || list[0].Base.String() != "a" {
		t.Errorf("list = %v", list)
	}
	if err := l.ClearBaseRefresh(ctx, name("a")); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{l.ClearBaseRefresh(ctx, name("a")), l.RecordBaseRefreshRun(ctx, name("a"), domain.RefreshRun{})} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
	}
}

func TestStore_ロールアウトを記録し_リーダーを他のノードへ譲れる(t *testing.T) {
	c := newCluster(t, 3)
	l := c.leader(t)
	if got, _ := l.GetUpdateCheck(ctx); !got.At.IsZero() {
		t.Errorf("check before any: %+v", got)
	}
	if _, err := l.GetRollout(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no rollout yet: %v", err)
	}
	if err := l.PutUpdateCheck(ctx, domain.UpdateCheck{By: "node0"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.GetUpdateCheck(ctx); got.By != "node0" {
		t.Errorf("check = %+v", got)
	}
	at := time.Unix(1, 0).UTC()
	r := domain.NewRollout("r1", "1.1.0", []string{"node1"}, map[string]string{"node1": "1.0.0"}, at)
	if err := l.StartRollout(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := l.StartRollout(ctx, r); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second start: %v", err)
	}
	if err := l.RecordRollout(ctx, domain.RolloutEvent{Rollout: "r1", Node: "node1", State: domain.NodeUpdating, At: at}); err != nil {
		t.Fatal(err)
	}
	if got, err := l.GetRollout(ctx); err != nil || got.Nodes["node1"].State != domain.NodeUpdating {
		t.Errorf("rollout = %+v %v", got, err)
	}
	if n := l.Voters(); n != 3 {
		t.Errorf("voters = %d", n)
	}
	if err := l.TransferLeadership(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return !l.IsLeader() })
}

func TestStore_匿名の上限は全ノードが新しい版になるまで書かれない(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: 13})
	_ = l.PutBase(ctx, domain.Base{Name: name("p")})
	two := 2
	if err := l.SetBaseAnonymous(ctx, name("p"), domain.AnonymousLimits{MaxBranches: &two}); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("set_base_anonymous with a protocol-13 node: %v", err)
	}
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: raftstore.Protocol})
	if err := l.SetBaseAnonymous(ctx, name("p"), domain.AnonymousLimits{MaxBranches: &two}); err != nil {
		t.Fatalf("after the upgrade: %v", err)
	}
	if b, _ := l.GetBase(ctx, name("p")); b.Anonymous.MaxBranches == nil || *b.Anonymous.MaxBranches != 2 {
		t.Errorf("not stored: %+v", b.Anonymous)
	}
}

func TestStore_クラスタのユーザーidは最初の記録が残り全メンバーが新版になるまで書かれない(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	if ids := l.ServiceIDs(); ids.Valid() {
		t.Fatalf("before: %v", ids)
	}
	first := domain.ServiceIDs{UID: 109, GID: 112}
	if err := l.InitServiceIDs(ctx, first); err != nil {
		t.Fatal(err)
	}
	_ = l.InitServiceIDs(ctx, domain.ServiceIDs{UID: 103, GID: 109})
	if ids := l.ServiceIDs(); ids != first {
		t.Errorf("recorded %v", ids)
	}
	c2 := newCluster(t, 1)
	l2 := c2.leader(t)
	_ = l2.PutNode(ctx, domain.Node{Name: "old", Protocol: 14})
	if err := l2.InitServiceIDs(ctx, first); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("an older member: %v", err)
	}
}

func TestStore_クラスタidは最初の記録が残り全メンバーが新版になるまで書かれない(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	if id := l.ClusterID(ctx); id != "" {
		t.Fatalf("before: %q", id)
	}
	first, second := strings.Repeat("0a", 16), strings.Repeat("1b", 16)
	if got, err := l.InitClusterID(ctx, first); err != nil || got != first {
		t.Fatalf("first: %q %v", got, err)
	}
	if got, err := l.InitClusterID(ctx, second); err != nil || got != first {
		t.Errorf("second must return the recorded id: %q %v", got, err)
	}
	if id := l.ClusterID(ctx); id != first {
		t.Errorf("recorded %q", id)
	}
	var _ domain.ClusterIDStore = l
	c2 := newCluster(t, 1)
	l2 := c2.leader(t)
	_ = l2.PutNode(ctx, domain.Node{Name: "old", Protocol: 16})
	if _, err := l2.InitClusterID(ctx, first); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("an older member: %v", err)
	}
	if id := l2.ClusterID(ctx); id != "" {
		t.Errorf("nothing written: %q", id)
	}
}

func TestStore_ブランチの記録の有無は作成時に決まり後から書き換えても変わらない(t *testing.T) {
	c := newCluster(t, 1)
	l := c.leader(t)
	for _, tc := range []struct {
		name         string
		first, later bool
	}{{"rec", true, false}, {"plain", false, true}} {
		if err := l.InsertBranch(ctx, domain.Branch{Name: name(tc.name), Record: tc.first}); err != nil {
			t.Fatal(err)
		}
		if err := l.PutBranch(ctx, domain.Branch{Name: name(tc.name), Status: domain.BranchReady, Record: tc.later}); err != nil {
			t.Fatal(err)
		}
		got, _ := l.GetBranch(ctx, name(tc.name))
		if got.Record != tc.first || got.Status != domain.BranchReady {
			t.Errorf("%s: %+v, want record=%v and the other fields replaced", tc.name, got, tc.first)
		}
	}
}

func TestProtocol_ランタイムモジュールのために予約した番号に達している(t *testing.T) {
	if raftstore.Protocol < domain.ProtocolRuntimeModules {
		t.Errorf("Protocol = %d, below domain.ProtocolRuntimeModules (%d)", raftstore.Protocol, domain.ProtocolRuntimeModules)
	}
}

func TestPutModule_ランタイムモジュールの種類と配布元を保存し_従来のモジュールは変わらない(t *testing.T) {
	l := newCluster(t, 1).leader(t)
	want := []domain.ModuleSpec{
		{Name: "auth", Version: "1.0", Wanted: true},
		{Name: "rt-clickhouse", Version: "0.3.0", Wanted: true, Kind: domain.ModuleKindRuntime, Source: domain.SourceLocal},
	}
	for _, m := range want {
		if err := l.PutModule(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.ListModules(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("modules = %+v, %v", got, err)
	}
	for _, m := range got {
		if m.Name == "rt-clickhouse" && (m.Kind != domain.ModuleKindRuntime || m.Source != domain.SourceLocal) {
			t.Errorf("runtime module = %+v", m)
		}
		if m.Name == "auth" && (m.Kind != "" || m.Source != "") {
			t.Errorf("plain module = %+v", m)
		}
	}
}

func TestStore_古い版のノードがいる間はランタイムモジュールを記録しない(t *testing.T) {
	l := newCluster(t, 1).leader(t)
	_ = l.PutNode(ctx, domain.Node{Name: "new", Protocol: raftstore.Protocol})
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: 18})
	rt := domain.ModuleSpec{Name: "rt-clickhouse", Version: "0.3.0", Wanted: true, Kind: domain.ModuleKindRuntime}

	if err := l.PutModule(ctx, rt); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("a runtime module with a node at protocol 18: %v, want ErrClusterUpgrading", err)
	}
	if err := l.PutModule(ctx, domain.ModuleSpec{Name: "auth", Version: "1.0", Wanted: true}); err != nil {
		t.Errorf("a plain module must still be recorded: %v", err)
	}
	if got, _ := l.ListModules(ctx); len(got) != 1 || got[0].Name != "auth" {
		t.Errorf("modules = %+v", got)
	}
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolRuntimeModules})
	if err := l.PutModule(ctx, rt); err != nil {
		t.Errorf("after the upgrade: %v", err)
	}
	// removing it (Wanted false) is a runtime module still: no node at 18 can be left to misread it
	_ = l.PutNode(ctx, domain.Node{Name: "older", Protocol: 18})
	rt.Wanted = false
	if err := l.PutModule(ctx, rt); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("changing a runtime module with a node at 18: %v", err)
	}
}

func TestStore_delete_moduleは全ノードがプロトコル22になるまで書かれない(t *testing.T) {
	if raftstore.Protocol < domain.ProtocolModuleForget {
		t.Fatalf("Protocol = %d, below domain.ProtocolModuleForget (%d)", raftstore.Protocol, domain.ProtocolModuleForget)
	}
	l := newCluster(t, 1).leader(t)
	_ = l.PutModule(ctx, domain.ModuleSpec{Name: "audit", Version: "1.0", Wanted: false})
	_ = l.PutModule(ctx, domain.ModuleSpec{Name: "proxysql", Version: "1.0", Wanted: true})
	_ = l.PutNode(ctx, domain.Node{Name: "new", Protocol: raftstore.Protocol})
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: 21})

	if err := l.DeleteModule(ctx, "audit"); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("with a node at protocol 21: %v, want ErrClusterUpgrading", err)
	}
	if got, _ := l.ListModules(ctx); len(got) != 2 {
		t.Fatalf("deleted while upgrading: %+v", got)
	}

	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolModuleForget})
	if err := l.DeleteModule(ctx, "audit"); err != nil {
		t.Fatalf("after upgrade to 22: %v", err)
	}
	got, err := l.ListModules(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "proxysql" {
		t.Errorf("modules after delete = %+v, %v", got, err)
	}
}

func TestStore_migrate_branch_ownerは全ノードがプロトコル23になるまで書かれない(t *testing.T) {
	if raftstore.Protocol < domain.ProtocolBranchOwnerID {
		t.Fatalf("Protocol = %d, below domain.ProtocolBranchOwnerID (%d)", raftstore.Protocol, domain.ProtocolBranchOwnerID)
	}
	l := newCluster(t, 1).leader(t)
	_ = l.PutBranch(ctx, domain.Branch{Name: name("feat"), Base: name("prod"), Status: domain.BranchReady, Owner: "ci"})
	_ = l.PutNode(ctx, domain.Node{Name: "new", Protocol: raftstore.Protocol})
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: 22})

	if err := l.MigrateBranchOwner(ctx, name("feat"), "token:abc", "ci"); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("with a node at protocol 22: %v, want ErrClusterUpgrading", err)
	}
	if b, _ := l.GetBranch(ctx, name("feat")); b.Owner != "ci" || b.OwnerName != "" {
		t.Fatalf("migrated while upgrading: %+v", b)
	}

	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolBranchOwnerID})
	if err := l.MigrateBranchOwner(ctx, name("feat"), "token:abc", "ci"); err != nil {
		t.Fatalf("after upgrade to 23: %v", err)
	}
	if b, _ := l.GetBranch(ctx, name("feat")); b.Owner != "token:abc" || b.OwnerName != "ci" {
		t.Errorf("branch after migrate = %+v", b)
	}
}

func TestSessionRevocations_ログアウトは全ノードがプロトコル24になるまで書かれず_なれば全ノードに届く(t *testing.T) {
	if raftstore.Protocol < domain.ProtocolSessionRevocation {
		t.Fatalf("Protocol = %d, below domain.ProtocolSessionRevocation (%d)", raftstore.Protocol, domain.ProtocolSessionRevocation)
	}
	cl := newCluster(t, 1)
	l := cl.leader(t)
	rev := raftstore.SessionRevocations{Store: l}
	now := time.Now()
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolSessionRevocation - 1})
	if err := rev.Revoke("s1", now.Add(time.Hour), now); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("with an old node: %v, want ErrClusterUpgrading", err)
	}
	if rev.Revoked("s1") {
		t.Fatal("revoked while upgrading")
	}
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolSessionRevocation})
	if err := rev.Revoke("s1", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if !rev.Revoked("s1") || rev.Revoked("s2") {
		t.Error("s1 must be revoked and only s1")
	}
}

func TestBranchFork_フォークブランチは全ノードがプロトコル25になるまで書かれない(t *testing.T) {
	if raftstore.Protocol < domain.ProtocolBranchFork {
		t.Fatalf("Protocol = %d, below domain.ProtocolBranchFork (%d)", raftstore.Protocol, domain.ProtocolBranchFork)
	}
	cl := newCluster(t, 1)
	l := cl.leader(t)
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolBranchFork - 1})
	forked := domain.Branch{
		Name: name("child"), Base: name("prod"), ParentBranch: name("parent"),
		ParentSnapshot: "fork-child", Status: domain.BranchProvisioning,
	}
	if err := l.InsertBranch(ctx, forked); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("with old node: %v, want ErrClusterUpgrading", err)
	}
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolBranchFork})
	if err := l.InsertBranch(ctx, forked); err != nil {
		t.Fatalf("after upgrade to 25: %v", err)
	}
	got, err := l.GetBranch(ctx, name("child"))
	if err != nil || got.ParentBranch != name("parent") || got.ParentSnapshot != "fork-child" {
		t.Fatalf("got = %+v, err = %v", got, err)
	}
}

func TestJoinToken_プロトコル27で期限つき1回限り消費が動き混在クラスタではErrClusterUpgrading(t *testing.T) {
	if raftstore.Protocol < domain.ProtocolJoinToken {
		t.Fatalf("Protocol = %d, below domain.ProtocolJoinToken (%d)", raftstore.Protocol, domain.ProtocolJoinToken)
	}
	cl := newCluster(t, 2)
	l := cl.leader(t)
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolJoinToken - 1})
	now := time.Unix(1000, 0)
	if err := l.SetJoinToken(ctx, "h1", now.Add(time.Hour)); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("SetJoinToken with old node: %v, want ErrClusterUpgrading", err)
	}
	if err := l.ConsumeJoinSecret(ctx, "h1", now); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("ConsumeJoinSecret with old node: %v, want ErrClusterUpgrading", err)
	}

	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolJoinToken})
	if err := l.SetJoinToken(ctx, "h1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.ConsumeJoinSecret(ctx, "h1", now); err != nil {
		t.Fatalf("first ConsumeJoinSecret: %v", err)
	}
	if err := l.ConsumeJoinSecret(ctx, "h1", now); !errors.Is(err, raftstore.ErrInvalidJoinSecret) {
		t.Fatalf("second ConsumeJoinSecret: %v, want ErrInvalidJoinSecret", err)
	}
}

func TestPersonalTokenRoles_プロトコル28未満の混在クラスタではErrClusterUpgrading(t *testing.T) {
	if raftstore.Protocol < domain.ProtocolPersonalTokenRoles {
		t.Fatalf("Protocol = %d, below domain.ProtocolPersonalTokenRoles (%d)", raftstore.Protocol, domain.ProtocolPersonalTokenRoles)
	}
	cl := newCluster(t, 1)
	l := cl.leader(t)
	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolPersonalTokenRoles - 1})

	// Service tokens (no OwnerRoles) still work during rolling upgrade.
	if err := l.PutToken(ctx, domain.APIToken{ID: "svc1", Name: "ci", Scope: domain.ScopeBranches}); err != nil {
		t.Fatalf("service token while upgrading: %v", err)
	}
	// Personal tokens carrying OwnerRoles require Protocol 28 so old nodes cannot drop roles.
	tok := domain.APIToken{ID: "per1", Name: "mcp", Scope: domain.ScopeBranches, Owner: "sso:alice", OwnerRoles: []string{"viewer"}}
	if err := l.PutToken(ctx, tok); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("personal token with OwnerRoles while upgrading: %v, want ErrClusterUpgrading", err)
	}

	_ = l.PutNode(ctx, domain.Node{Name: "old", Protocol: domain.ProtocolPersonalTokenRoles})
	if err := l.PutToken(ctx, tok); err != nil {
		t.Fatalf("personal token after upgrade to 28: %v", err)
	}
	list, _ := l.ListTokens(ctx)
	var found domain.APIToken
	for _, x := range list {
		if x.ID == "per1" {
			found = x
		}
	}
	if len(found.OwnerRoles) != 1 || found.OwnerRoles[0] != "viewer" {
		t.Fatalf("stored OwnerRoles = %v, want [viewer]", found.OwnerRoles)
	}
}
