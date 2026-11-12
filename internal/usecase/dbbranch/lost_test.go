package dbbranch_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// lostPair: node1 holds base prod and branch "x"; node2 has a copy of prod.
// Then node1 stops answering.
func lostPair(t *testing.T) *pair {
	t.Helper()
	p := newPair(t)
	p.withBase(t)
	if _, err := p.origin.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), InitSQL: "UPDATE users SET email = NULL"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	p.replica.svc.Nodes.(*fakeNodes).nodes[0].LastSeen = p.replica.now.Add(-time.Minute)
	p.origin.storage.datasets["tank/db-branch/branches"] = true
	p.replica.storage.datasets["tank/db-branch/branches"] = true
	p.origin.j.entries, p.replica.j.entries = nil, nil
	return p
}

func TestMarkLost_止まったノードのブランチを失われたものにする(t *testing.T) {
	p := lostPair(t)
	lost, err := p.replica.svc.MarkLost(ctx)
	if err != nil || len(lost) != 1 || lost[0].String() != "x" {
		t.Fatalf("MarkLost = %v %v", lost, err)
	}
	b, _ := p.replica.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchLost || !strings.Contains(b.Error, "node1") {
		t.Errorf("branch = %+v", b)
	}
	if again, _ := p.replica.svc.MarkLost(ctx); len(again) != 0 {
		t.Errorf("marked twice: %v", again)
	}
	p.replica.svc.Nodes = nil
	if got, err := p.replica.svc.MarkLost(ctx); err != nil || got != nil {
		t.Errorf("single node: %v %v", got, err)
	}
}

func TestMarkLost_ノード一覧が読めなければエラー(t *testing.T) {
	p := lostPair(t)
	p.replica.svc.Nodes.(*fakeNodes).err = errors.New("raft down")
	if _, err := p.replica.svc.MarkLost(ctx); err == nil {
		t.Error("want error")
	}
}

func TestRecreateBranch_コピーを持つ別のノードで同じ名前と資格情報で作り直す(t *testing.T) {
	p := lostPair(t)
	_, _ = p.replica.svc.MarkLost(ctx)
	old, _ := p.replica.store.GetBranch(ctx, name("x"))

	node, err := p.replica.svc.PlaceRecreate(ctx, name("x"))
	if err != nil || node != "node2" {
		t.Fatalf("PlaceRecreate = %q %v", node, err)
	}
	b, err := p.replica.svc.RecreateBranch(ctx, name("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Node != "node2" || b.Status != domain.BranchReady || b.Password != old.Password || b.User != old.User || b.Snapshot != old.Snapshot || b.Error != "" {
		t.Errorf("recreated = %+v (old %+v)", b, old)
	}
	if !p.replica.j.has("exec branch-x") || !p.replica.j.has("zfs clone tank/db-branch/replicas/prod@"+old.Snapshot+" tank/db-branch/branches/x") {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
	if sql := p.replica.server.sql["branch-x"]; len(sql) < 2 || sql[1] != "UPDATE users SET email = NULL" {
		t.Errorf("init sql = %v", sql)
	}
}

func TestRecreateBranch_失われていないものや置き場のないものは断る(t *testing.T) {
	p := lostPair(t)
	if _, err := p.replica.svc.RecreateBranch(ctx, name("x"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("not lost: %v", err)
	}
	if _, err := p.replica.svc.PlaceRecreate(ctx, name("nope")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	_, _ = p.replica.svc.MarkLost(ctx)
	p.replica.svc.Nodes.(*fakeNodes).nodes[1].LastSeen = p.replica.now.Add(-time.Minute)
	if _, err := p.replica.svc.PlaceRecreate(ctx, name("x")); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("no other holder up: %v", err)
	}
	if _, err := p.replica.svc.RecreateBranch(ctx, name("x"), nil); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("recreate without a place: %v", err)
	}
}

func TestDeleteBranch_失われたブランチはどこからでも消せて戻ったノードが片付ける(t *testing.T) {
	p := lostPair(t)
	_, _ = p.replica.svc.MarkLost(ctx)
	if err := p.replica.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	b, _ := p.replica.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchDeleting || len(p.replica.j.entries) != 0 {
		t.Errorf("branch %+v journal %v", b, p.replica.j.entries)
	}
	// node1 comes back and finishes the delete.
	if err := p.origin.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.origin.store.GetBranch(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record left: %v", err)
	}
}

func TestReconcile_戻ったノードは自分の失われたブランチを再開し他へ移ったものを片付ける(t *testing.T) {
	p := lostPair(t)
	if _, err := p.origin.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("y"), Base: name("prod"), Node: "node1"}, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = p.replica.svc.MarkLost(ctx)
	if _, err := p.replica.svc.RecreateBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	p.origin.j.entries = nil
	p.origin.svc.Streams.(*fakeStreams).snaps = map[string][]string{}
	if err := p.origin.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if y, _ := p.origin.store.GetBranch(ctx, name("y")); y.Status != domain.BranchReady || y.Error != "" {
		t.Errorf("y = %+v", y)
	}
	if !p.origin.j.has("remove branch-x") || !p.origin.j.has("zfs destroy tank/db-branch/branches/x") {
		t.Errorf("journal = %v", p.origin.j.entries)
	}
	if p.origin.j.has("zfs destroy tank/db-branch/branches/y") {
		t.Error("own branch destroyed")
	}
	if x, _ := p.origin.store.GetBranch(ctx, name("x")); x.Node != "node2" {
		t.Errorf("record of the moved branch changed: %+v", x)
	}
}

func TestReconcile_記録のないデータセットは消さない(t *testing.T) {
	p := lostPair(t)
	p.origin.storage.datasets["tank/db-branch/branches/unknown"] = true
	if err := p.origin.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if p.origin.j.has("zfs destroy tank/db-branch/branches/unknown") {
		t.Error("a dataset without a record must be left alone")
	}
	p.origin.svc.Streams = nil
	if err := p.origin.svc.Reconcile(ctx); err != nil {
		t.Errorf("without streams: %v", err)
	}
}

func Test失われたブランチまわりのストアや下回りの失敗はそのまま返す(t *testing.T) {
	cases := map[string]func(p *pair) error{
		"mark lost: list branches": func(p *pair) error {
			p.replica.svc.Store = &failingStore{Store: p.replica.store, failOn: "list-branches"}
			_, err := p.replica.svc.MarkLost(ctx)
			return err
		},
		"reconcile: list branches": func(p *pair) error {
			p.origin.svc.Store = &failingStore{Store: p.origin.store, failOn: "list-branches"}
			return p.origin.svc.Reconcile(ctx)
		},
		"reconcile: children": func(p *pair) error {
			p.origin.svc.Streams.(*fakeStreams).failOn = "children"
			return p.origin.svc.Reconcile(ctx)
		},
		"reconcile: exists": func(p *pair) error {
			p.origin.storage.failOn = "exists"
			return p.origin.svc.Reconcile(ctx)
		},
		"reconcile: start": func(p *pair) error {
			_, _ = p.replica.svc.MarkLost(ctx)
			p.origin.server.running["branch-x"] = false
			p.origin.server.failOn = "start"
			return p.origin.svc.Reconcile(ctx)
		},
		"recreate: list branches": func(p *pair) error {
			_, _ = p.replica.svc.MarkLost(ctx)
			p.replica.svc.Store = &failingStore{Store: p.replica.store, failOn: "list-branches"}
			_, err := p.replica.svc.RecreateBranch(ctx, name("x"), nil)
			return err
		},
		"recreate: nodes": func(p *pair) error {
			_, _ = p.replica.svc.MarkLost(ctx)
			p.replica.svc.Nodes.(*fakeNodes).err = errors.New("raft down")
			_, err := p.replica.svc.PlaceRecreate(ctx, name("x"))
			return err
		},
		"recreate: missing base": func(p *pair) error {
			_, _ = p.replica.svc.MarkLost(ctx)
			b, _ := p.replica.store.GetBranch(ctx, name("x"))
			b.Base = name("gone")
			_ = p.replica.store.PutBranch(ctx, b)
			_, err := p.replica.svc.PlaceRecreate(ctx, name("x"))
			return err
		},
		"recreate: build": func(p *pair) error {
			_, _ = p.replica.svc.MarkLost(ctx)
			p.replica.storage.failOn = "clone"
			_, err := p.replica.svc.RecreateBranch(ctx, name("x"), nil)
			return err
		},
		"recreate: missing": func(p *pair) error {
			_, err := p.replica.svc.RecreateBranch(ctx, name("nope"), nil)
			return err
		},
		"runtime in use: list bases": func(p *pair) error {
			p.origin.svc.Store = &failingStore{Store: p.origin.store, failOn: "list-bases"}
			_, err := p.origin.svc.RuntimeInUse(ctx, mysql84)
			return err
		},
		"runtime in use: list branches": func(p *pair) error {
			p.origin.svc.Store = &failingStore{Store: p.origin.store, failOn: "list-branches"}
			_, err := p.origin.svc.RuntimeInUse(ctx, domain.RuntimeRef{Engine: domain.MySQL, Version: "1"})
			return err
		},
		"replicate: list bases": func(p *pair) error {
			p.replica.svc.Store = &failingStore{Store: p.replica.store, failOn: "list-bases"}
			return p.replica.svc.Replicate(ctx)
		},
	}
	for what, run := range cases {
		if err := run(lostPair(t)); err == nil {
			t.Errorf("%s: want error", what)
		}
	}
}

func TestRecreateBranch_単一ノードでは作り直す先がない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	b, _ := e.store.GetBranch(ctx, name("x"))
	b.Status = domain.BranchLost
	_ = e.store.PutBranch(ctx, b)
	if _, err := e.svc.PlaceRecreate(ctx, name("x")); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("err = %v", err)
	}
}

func TestReconcile_離れている間に消されたブランチを戻ったノードが片付ける(t *testing.T) {
	p := lostPair(t)
	_, _ = p.replica.svc.MarkLost(ctx)
	_ = p.replica.svc.DeleteBranch(ctx, name("x"), nil)
	// node1 was only cut off: its agent kept running, so no Recover.
	if err := p.origin.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.origin.store.GetBranch(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record left: %v", err)
	}
	if !p.origin.j.has("zfs destroy tank/db-branch/branches/x") {
		t.Errorf("journal = %v", p.origin.j.entries)
	}
}

func TestRecover_共通ユーザーの古いブランチに自分の名前のユーザーを足す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "old")
	b, _ := e.store.GetBranch(ctx, name("old"))
	b.User = dbbranch.BranchUser
	_ = e.store.PutBranch(ctx, b)
	e.server.sql = map[string][]string{}
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetBranch(ctx, name("old"))
	if got.User != "old" || len(e.server.sql["branch-old"]) != 1 || !strings.Contains(e.server.sql["branch-old"][0], "'old'@'%' IDENTIFIED BY '"+b.Password+"'") {
		t.Errorf("branch %+v sql %v", got, e.server.sql)
	}
	e.server.sql = map[string][]string{}
	_ = e.svc.Recover(ctx)
	if len(e.server.sql["branch-old"]) != 0 {
		t.Errorf("done once only: %v", e.server.sql)
	}
	b.User = dbbranch.BranchUser
	_ = e.store.PutBranch(ctx, b)
	e.server.failOn = "exec"
	if err := e.svc.Recover(ctx); err == nil {
		t.Error("exec failure: want error")
	}
}
