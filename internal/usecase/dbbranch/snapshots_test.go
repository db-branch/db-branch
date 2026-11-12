package dbbranch_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// snap takes another snapshot of prod one minute later.
func (e *env) snap(t *testing.T) string {
	t.Helper()
	e.now = e.now.Add(60e9)
	s, err := e.svc.SnapshotBase(ctx, name("prod"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Name
}

func snapNames(t *testing.T, e *env) []string {
	t.Helper()
	b, _ := e.store.GetBase(ctx, name("prod"))
	var out []string
	for _, s := range b.Snapshots {
		out = append(out, s.Name)
	}
	return out
}

func TestDeleteSnapshot_使われていない古いスナップショットを消す(t *testing.T) {
	e := newEnv(t)
	s1 := e.withBase(t).Snapshots[0].Name
	s2 := e.snap(t)
	if err := e.svc.DeleteSnapshot(ctx, name("prod"), s1); err != nil {
		t.Fatal(err)
	}
	if got := snapNames(t, e); len(got) != 1 || got[0] != s2 {
		t.Errorf("snapshots = %v", got)
	}
	if !e.j.has("zfs destroy tank/db-branch/bases/prod@" + s1) {
		t.Errorf("journal = %v", e.j.entries)
	}
}

func TestDeleteSnapshot_最新や使用中や存在しないものは消さない(t *testing.T) {
	e := newEnv(t)
	s1 := e.withBase(t).Snapshots[0].Name
	e.withBranch(t, "feature-x")
	s2 := e.snap(t)
	if err := e.svc.DeleteSnapshot(ctx, name("prod"), s2); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("latest: %v", err)
	}
	if err := e.svc.DeleteSnapshot(ctx, name("prod"), s1); !errors.Is(err, domain.ErrInUse) {
		t.Errorf("used: %v", err)
	}
	if err := e.svc.DeleteSnapshot(ctx, name("prod"), "s-none"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if err := e.svc.DeleteSnapshot(ctx, name("nope"), s1); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing base: %v", err)
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	b.Status = domain.BaseMaintenance
	_ = e.store.PutBase(ctx, b)
	if err := e.svc.DeleteSnapshot(ctx, name("prod"), s1); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("maintenance: %v", err)
	}
	if got := snapNames(t, e); len(got) != 2 {
		t.Errorf("snapshots = %v", got)
	}
}

func TestPruneSnapshots_新しいものがあり使われていない古いものを消す(t *testing.T) {
	e := newEnv(t)
	s1 := e.withBase(t).Snapshots[0].Name
	s2 := e.snap(t)
	e.withBranch(t, "on-s2")
	s3 := e.snap(t)
	b, _ := e.store.GetBase(ctx, name("prod"))
	b.SetPublic(s1, true)
	_ = e.store.PutBase(ctx, b)
	pruned, err := e.svc.PruneSnapshots(ctx)
	if err != nil || len(pruned) != 1 || pruned[0] != "prod@"+s1 {
		t.Fatalf("pruned %v %v", pruned, err)
	}
	b, _ = e.store.GetBase(ctx, name("prod"))
	if got := snapNames(t, e); len(got) != 2 || got[0] != s2 || got[1] != s3 || !b.Snapshots[1].Public {
		t.Errorf("snapshots = %+v", b.Snapshots)
	}
	if pruned, _ := e.svc.PruneSnapshots(ctx); len(pruned) != 0 {
		t.Errorf("again: %v", pruned)
	}
}

func TestPruneSnapshots_コピーが最新に追いつくまで待つ(t *testing.T) {
	p := newPair(t)
	s1 := p.withBase(t).Snapshots[0].Name
	_ = p.replica.svc.Replicate(ctx)
	s2 := p.snapshot(t)
	p.syncOriginSnaps(t)
	if pruned, _ := p.origin.svc.PruneSnapshots(ctx); len(pruned) != 0 {
		t.Errorf("node2 has only %s, pruned %v", s1, pruned)
	}
	_ = p.replica.svc.Replicate(ctx)
	if pruned, _ := p.origin.svc.PruneSnapshots(ctx); len(pruned) != 1 {
		t.Errorf("caught up, pruned %v", pruned)
	}
	// node2 drops its copy of s1 at the next replication; s2 stays.
	p.syncOriginSnaps(t)
	p.replica.j.entries = nil
	if err := p.replica.svc.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.replica.j.has(fmt.Sprintf("zfs destroy tank/db-branch/replicas/prod@%s", s1)) || p.replica.storage.snaps["tank/db-branch/replicas/prod@"+s2] == false {
		t.Errorf("journal = %v", p.replica.j.entries)
	}
	if pruned, _ := p.replica.svc.PruneSnapshots(ctx); len(pruned) != 0 {
		t.Errorf("not its base: %v", pruned)
	}
}

func TestPruneSnapshots_消せなければ残して続ける(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.snap(t)
	e.storage.failOn = "destroy-snapshot"
	if _, err := e.svc.PruneSnapshots(ctx); err == nil {
		t.Error("want error")
	}
}

func TestSetSnapshotPublic_誰でも作れるスナップショットを切り替える(t *testing.T) {
	e := newEnv(t)
	s1 := e.withBase(t).Snapshots[0].Name
	b, err := e.svc.SetSnapshotPublic(ctx, name("prod"), s1, true)
	if err != nil || !b.Snapshots[0].Public {
		t.Fatalf("%+v %v", b.Snapshots, err)
	}
	if got, _ := e.store.GetBase(ctx, name("prod")); !got.Snapshots[0].Public {
		t.Error("not stored")
	}
	if _, err := e.svc.SetSnapshotPublic(ctx, name("prod"), "s-none", true); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := e.svc.SetSnapshotPublic(ctx, name("nope"), s1, true); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing base: %v", err)
	}
}

func TestExecBase_止まっていれば起動してSQLを流す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	if err := e.svc.ExecBase(ctx, name("prod"), "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if e.j.entries[0] != "start base-prod" || e.server.sql["base-prod"][len(e.server.sql["base-prod"])-1] != "INSERT INTO t VALUES (1)" {
		t.Errorf("journal %v", e.j.entries)
	}
	e.j.entries = nil
	_ = e.svc.ExecBase(ctx, name("prod"), "UPDATE t SET a = 2")
	if e.j.entries[0] != "exec base-prod" {
		t.Errorf("already running: %v", e.j.entries)
	}
	if err := e.svc.ExecBase(ctx, name("nope"), "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	e.server.failOn = "start"
	e.server.running["base-prod"] = false
	if err := e.svc.ExecBase(ctx, name("prod"), "x"); err == nil {
		t.Error("start failure")
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	b.Status = domain.BaseMaintenance
	_ = e.store.PutBase(ctx, b)
	if err := e.svc.ExecBase(ctx, name("prod"), "x"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("maintenance: %v", err)
	}
}

func TestCreateBranch_所有者と接続の公開範囲を記録する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.svc.OpenPassword = func() string { return "shared1" }
	b, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("open-one"), Base: name("prod"), Owner: "alice", Access: domain.AccessOpen}, nil)
	if err != nil || b.Owner != "alice" || !b.Open() || b.Password != "shared1" {
		t.Fatalf("%+v %v", b, err)
	}
	p, _ := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("mine"), Base: name("prod"), Owner: "bob"}, nil)
	if p.Access != domain.AccessPrivate || p.Password == "shared1" {
		t.Errorf("private: %+v", p)
	}
}

func TestLoadBase_ダンプを流しその間はメンテナンスにする(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	var phases []string
	if err := e.svc.LoadBase(ctx, name("prod"), "/uploads/u1.sql.gz", func(p string) { phases = append(phases, p) }); err != nil {
		t.Fatal(err)
	}
	if e.j.indexOf("start base-prod") < 0 || e.j.indexOf("load base-prod /uploads/u1.sql.gz") < e.j.indexOf("start base-prod") {
		t.Errorf("journal %v", e.j.entries)
	}
	if len(phases) == 0 || phases[len(phases)-1] != "loading" {
		t.Errorf("phases %v", phases)
	}
	if b, _ := e.store.GetBase(ctx, name("prod")); b.Status != domain.BaseReady {
		t.Errorf("status after: %s", b.Status)
	}
	// A failed load leaves the base ready (its snapshots are untouched).
	e.server.failOn = "load"
	if err := e.svc.LoadBase(ctx, name("prod"), "/x.sql", nil); err == nil {
		t.Error("load failure")
	}
	if b, _ := e.store.GetBase(ctx, name("prod")); b.Status != domain.BaseReady {
		t.Errorf("status after failure: %s", b.Status)
	}
	e.server.failOn = "start"
	e.server.running["base-prod"] = false
	if err := e.svc.LoadBase(ctx, name("prod"), "/x.sql", nil); err == nil {
		t.Error("start failure")
	}
	if err := e.svc.LoadBase(ctx, name("nope"), "/x.sql", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if err := e.svc.LoadBase(ctx, name("prod"), "", nil); err == nil {
		t.Error("an empty dump path should fail")
	}
}

func TestDescribeBase_説明を付けて変えられる(t *testing.T) {
	e := newEnv(t)
	b, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84, Description: "masked production, refreshed weekly"}, nil)
	if err != nil || b.Description != "masked production, refreshed weekly" {
		t.Fatalf("%+v %v", b, err)
	}
	b, err = e.svc.DescribeBase(ctx, name("prod"), "  for demos  ")
	if err != nil || b.Description != "for demos" {
		t.Errorf("%+v %v", b, err)
	}
	if got, _ := e.store.GetBase(ctx, name("prod")); got.Description != "for demos" {
		t.Errorf("stored %q", got.Description)
	}
	if _, err := e.svc.DescribeBase(ctx, name("prod"), strings.Repeat("x", 1001)); err == nil {
		t.Error("too long")
	}
	if _, err := e.svc.DescribeBase(ctx, name("nope"), "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("long"), Runtime: mysql84, Description: strings.Repeat("x", 1001)}, nil); err == nil {
		t.Error("too long at creation")
	}
}

func TestSetAnonymous_匿名の上限をベースに保存し継承に戻せる(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	three := 3
	b, err := e.svc.SetAnonymous(ctx, name("prod"), domain.AnonymousLimits{MaxBranches: &three, MaxTTL: 4 * time.Hour})
	if err != nil || b.Anonymous.MaxBranches == nil || *b.Anonymous.MaxBranches != 3 || b.Anonymous.MaxTTL != 4*time.Hour {
		t.Fatalf("%+v %v", b.Anonymous, err)
	}
	if got, _ := e.store.GetBase(ctx, name("prod")); got.Anonymous.MaxTTL != 4*time.Hour {
		t.Errorf("stored %+v", got.Anonymous)
	}
	if b, err = e.svc.SetAnonymous(ctx, name("prod"), domain.AnonymousLimits{}); err != nil || !b.Anonymous.Inherits() {
		t.Errorf("inherit: %+v %v", b.Anonymous, err)
	}
}

func TestSetAnonymous_不正な値と存在しないベースと保存の失敗はエラーにする(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	neg := -1
	if _, err := e.svc.SetAnonymous(ctx, name("prod"), domain.AnonymousLimits{MaxBranches: &neg}); !errors.Is(err, domain.ErrInvalidAnonymous) {
		t.Errorf("negative: %v", err)
	}
	if _, err := e.svc.SetAnonymous(ctx, name("nope"), domain.AnonymousLimits{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	e.svc.Store = &failingStore{Store: e.store, failOn: "set-anonymous"}
	if _, err := e.svc.SetAnonymous(ctx, name("prod"), domain.AnonymousLimits{}); !errors.Is(err, errStore) {
		t.Errorf("store: %v", err)
	}
}
