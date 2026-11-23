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

type historyServer struct {
	crashServer
	resets   []string
	dropped  []string
	fail     error
	dropFail error
}

func (h *historyServer) ResetCloneHistory(_ context.Context, s domain.Server) error {
	h.j.add("reset-history %s", s.ID)
	h.resets = append(h.resets, s.ID)
	return h.fail
}

func (h *historyServer) DropUser(_ context.Context, s domain.Server, user string) error {
	h.j.add("drop-user %s %s", s.ID, user)
	h.dropped = append(h.dropped, s.ID+":"+user)
	return h.dropFail
}

func TestForkBranch_稼働中の親ブランチの現在状態から子ブランチを作成できる(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "parent-data")
	parent, _ := z.store.GetBranch(ctx, name("x"))

	var phases []string
	child, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{
		Source: name("x"), Name: name("x-try"), InitSQL: "DELETE FROM tmp;",
		Owner: "sso:alice", OwnerName: "alice@example.com", TTL: 24 * time.Hour,
	}, func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatalf("ForkBranch: %v", err)
	}
	if child.Status != domain.BranchReady {
		t.Fatalf("status = %s, want ready", child.Status)
	}
	if child.ParentBranch != name("x") || child.ParentSnapshot != "fork-x-try" {
		t.Errorf("parent = %q @ %q", child.ParentBranch, child.ParentSnapshot)
	}
	if child.Base != parent.Base || child.Snapshot != parent.Snapshot {
		t.Errorf("base = %s@%s, want %s@%s", child.Base, child.Snapshot, parent.Base, parent.Snapshot)
	}
	if child.Node != parent.Node || child.Port == parent.Port {
		t.Errorf("node=%s port=%d (parent node=%s port=%d)", child.Node, child.Port, parent.Node, parent.Port)
	}
	if child.Owner != "sso:alice" || child.OwnerName != "alice@example.com" || child.ExpiresAt == nil {
		t.Errorf("owner=%q owner_name=%q expires=%v", child.Owner, child.OwnerName, child.ExpiresAt)
	}
	if child.ForkRuntime != parent.Runtime {
		t.Errorf("ForkRuntime = %s, want %s", child.ForkRuntime, parent.Runtime)
	}
	if z.sim.ds["tank/db-branch/branches/x-try"].data != "parent-data" {
		t.Errorf("child data = %q, want parent-data", z.sim.ds["tank/db-branch/branches/x-try"].data)
	}
	// InitSQL must run via ExecAs as the child login, never as admin.
	if len(z.server.ranAs) != 1 || z.server.ranAs[0] != "x-try" {
		t.Errorf("ranAs = %v, want [x-try]", z.server.ranAs)
	}
	// Parent was not stopped and fork-* is hidden from parent checkpoints.
	if z.j.has("stop branch-x clean") {
		t.Errorf("live fork must not stop parent: %v", z.j.entries)
	}
	if cps, err := z.svc.ListCheckpoints(ctx, name("x")); err != nil || len(cps) != 0 {
		t.Errorf("parent checkpoints = %+v, err = %v (fork-* must not leak)", cps, err)
	}
	// Child's @reset snapshot carries PropRuntime.
	snaps, _ := z.sim.SnapshotInfos(ctx, "tank/db-branch/branches/x-try")
	if len(snaps) != 1 || snaps[0].Name != domain.ResetSnapshot || snaps[0].Props[domain.PropRuntime] != domain.RuntimeProp(mysql84) {
		t.Errorf("child snaps = %+v", snaps)
	}
	wantPhases := "snapshotting cloning starting init_sql stopping snapshotting starting"
	if got := strings.Join(phases, " "); got != wantPhases {
		t.Errorf("phases = %q, want %q", got, wantPhases)
	}
}

func TestForkBranch_エラーケースを適切に拒否する(t *testing.T) {
	t.Run("Lineageなし", func(t *testing.T) {
		e := newEnv(t)
		e.withBase(t)
		e.withBranch(t, "x")
		_, err := e.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil)
		if !errors.Is(err, domain.ErrCheckpointsUnavailable) {
			t.Fatalf("err = %v, want ErrCheckpointsUnavailable", err)
		}
	})
	t.Run("親なし", func(t *testing.T) {
		z := newZEnv(t)
		_, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("nope"), Name: name("c")}, nil)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("親がbusyまたはfailed", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		b, _ := z.store.GetBranch(ctx, name("x"))
		b.Status = domain.BranchResetting
		_ = z.store.PutBranch(ctx, b)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})
	t.Run("子名重複", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("x")}, nil); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("err = %v, want ErrAlreadyExists", err)
		}
	})
	t.Run("別ノード指定", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Node: "node2"}, nil); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})
	t.Run("他ノードの親", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		b, _ := z.store.GetBranch(ctx, name("x"))
		b.Node = "node2"
		_ = z.store.PutBranch(ctx, b)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, dbbranch.ErrOtherNode) {
			t.Fatalf("err = %v, want ErrOtherNode", err)
		}
	})
	t.Run("存在しないチェックポイントと不正名", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Checkpoint: "missing"}, nil); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing cp: err = %v", err)
		}
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Checkpoint: "-bad-"}, nil); !errors.Is(err, domain.ErrInvalidName) {
			t.Fatalf("bad cp: err = %v", err)
		}
	})
	t.Run("ランタイム未導入", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{}}
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, domain.ErrRuntimeNotInstalled) {
			t.Fatalf("err = %v, want ErrRuntimeNotInstalled", err)
		}
	})
	t.Run("ValidateForkは開始前に検証する", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if err := z.svc.ValidateFork(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}); err != nil {
			t.Fatalf("ValidateFork valid: %v", err)
		}
		if err := z.svc.ValidateFork(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Checkpoint: "nope"}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("ValidateFork missing cp: %v", err)
		}
	})
}

func TestForkBranch_チェックポイントとresetからフォークでき退避ラインも辿れる(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")
	z.checkpoint(t, "cp1")
	z.sim.write(live, "v2")
	z.checkpoint(t, "cp2")
	z.sim.write(live, "v3")
	// Restore cp1 so cp2 moves to hidden history line 0.
	z.tick()
	if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), "cp1", nil); err != nil {
		t.Fatal(err)
	}
	z.sim.write(live, "v1-mod")

	// Fork from cp2 on hidden line.
	c2, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("from-cp2"), Checkpoint: "cp2"}, nil)
	if err != nil {
		t.Fatalf("fork from cp2: %v", err)
	}
	if c2.ParentSnapshot != "ckpt-cp2" || z.sim.ds["tank/db-branch/branches/from-cp2"].data != "v2" {
		t.Errorf("from-cp2: snap=%q data=%q", c2.ParentSnapshot, z.sim.ds["tank/db-branch/branches/from-cp2"].data)
	}

	// Fork from @reset.
	cr, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("from-reset"), Checkpoint: domain.ResetPoint}, nil)
	if err != nil {
		t.Fatalf("fork from @reset: %v", err)
	}
	if cr.ParentSnapshot != domain.ResetSnapshot || z.sim.ds["tank/db-branch/branches/from-reset"].data != "" {
		t.Errorf("from-reset: snap=%q data=%q", cr.ParentSnapshot, z.sim.ds["tank/db-branch/branches/from-reset"].data)
	}
}

func TestForkBranch_一時停止中の親からフォークしても親はpausedのまま維持される(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "paused-data")
	if _, err := z.svc.PauseBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	z.j.entries = nil

	// Even with Clean: true, a paused parent stays stopped and is not restarted.
	c, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil)
	if err != nil {
		t.Fatalf("ForkBranch from paused: %v", err)
	}
	if c.Status != domain.BranchReady || z.sim.ds["tank/db-branch/branches/c"].data != "paused-data" {
		t.Fatalf("child status=%s data=%q", c.Status, z.sim.ds["tank/db-branch/branches/c"].data)
	}
	p, _ := z.store.GetBranch(ctx, name("x"))
	if p.Status != domain.BranchPaused || z.server.running["branch-x"] {
		t.Errorf("parent status=%s running=%v, want paused and stopped", p.Status, z.server.running["branch-x"])
	}
}

func TestForkBranch_clean指定時は親を停止してスナップショットを取りすぐ再開する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "clean-data")

	var sawForking bool
	z.server.onStop = func() {
		if b, err := z.store.GetBranch(ctx, name("x")); err == nil && b.Status == domain.BranchForking {
			sawForking = true
		}
	}
	c, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil)
	if err != nil {
		t.Fatalf("ForkBranch clean: %v", err)
	}
	if !sawForking {
		t.Error("parent was not in BranchForking during StopClean")
	}
	p, _ := z.store.GetBranch(ctx, name("x"))
	if p.Status != domain.BranchReady || !z.server.running["branch-x"] || c.Status != domain.BranchReady {
		t.Errorf("parent=%s running=%v child=%s", p.Status, z.server.running["branch-x"], c.Status)
	}
	if z.j.indexOf("stop branch-x clean") < 0 || z.j.indexOf("stop branch-x clean") > z.j.indexOf("zfs snapshot tank/db-branch/branches/x@fork-c") {
		t.Errorf("clean stop must precede snapshot: %v", z.j.entries)
	}
}

func TestForkBranch_clean指定時に正常停止に失敗したら予約を取り消し親を復帰する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.server.failOn = "stop-clean"

	_, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil)
	if !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Fatalf("err = %v, want ErrUncleanShutdown", err)
	}
	if _, err := z.store.GetBranch(ctx, name("c")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("failed clean fork left child record: %v", err)
	}
	p, _ := z.store.GetBranch(ctx, name("x"))
	if p.Status != domain.BranchReady || !z.server.running["branch-x"] {
		t.Errorf("parent not restored: status=%s running=%v", p.Status, z.server.running["branch-x"])
	}
}

func TestForkBranch_クローン失敗時はスナップショットと予約を取り消す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.failOn = "zfs clone"

	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err == nil {
		t.Fatal("want clone error")
	}
	if _, err := z.store.GetBranch(ctx, name("c")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("child reservation not cancelled: %v", err)
	}
	snaps, _ := z.sim.SnapshotInfos(ctx, live)
	for _, s := range snaps {
		if s.Name == "fork-c" {
			t.Errorf("fork-c snapshot leaked: %+v", snaps)
		}
	}
}

func TestForkBranch_アップグレード済み親からフォークした子はreset後もランタイムとLegacyAuthを維持する(t *testing.T) {
	z := newZEnv(t)
	mysql80 := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql80: true, mysql84: true}}
	if _, err := z.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql80, Dump: "/d.sql"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil); err != nil {
		t.Fatal(err)
	}
	z.server.plan = domain.UpgradePlan{LegacyAuth: true}
	if _, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: mysql84}, nil); err != nil {
		t.Fatal(err)
	}
	z.sim.write(live, "upgraded-data")

	c, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil)
	if err != nil {
		t.Fatalf("ForkBranch: %v", err)
	}
	if c.Runtime != mysql84 || !c.LegacyAuth || c.ForkRuntime != mysql84 || !c.ForkLegacyAuth {
		t.Fatalf("child = %+v", c)
	}
	// Rollback reset path: child keeps mysql84 and LegacyAuth: true.
	z.sim.write("tank/db-branch/branches/c", "dirty")
	r1, err := z.svc.ResetBranch(ctx, name("c"), nil)
	if err != nil {
		t.Fatalf("ResetBranch (rollback): %v", err)
	}
	if r1.Runtime != mysql84 || !r1.LegacyAuth || z.sim.ds["tank/db-branch/branches/c"].data != "upgraded-data" {
		t.Fatalf("after rollback reset: rt=%s legacy=%v data=%q", r1.Runtime, r1.LegacyAuth, z.sim.ds["tank/db-branch/branches/c"].data)
	}
	// Swap reset path (when child has a checkpoint): also keeps mysql84 and LegacyAuth: true.
	z.tick()
	if _, err := z.svc.CreateCheckpoint(ctx, name("c"), "cp1"); err != nil {
		t.Fatal(err)
	}
	z.sim.write("tank/db-branch/branches/c", "dirty2")
	z.tick()
	r2, err := z.svc.ResetBranch(ctx, name("c"), nil)
	if err != nil {
		t.Fatalf("ResetBranch (swap): %v", err)
	}
	if r2.Runtime != mysql84 || !r2.LegacyAuth || z.sim.ds["tank/db-branch/branches/c"].data != "upgraded-data" {
		t.Fatalf("after swap reset: rt=%s legacy=%v data=%q", r2.Runtime, r2.LegacyAuth, z.sim.ds["tank/db-branch/branches/c"].data)
	}
	// Forking from parent's before-upgrade checkpoint gets mysql80 and LegacyAuth: false.
	cps, _ := z.svc.ListCheckpoints(ctx, name("x"))
	if len(cps) == 0 {
		t.Fatal("expected before-upgrade checkpoint on parent")
	}
	oldChild, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("old"), Checkpoint: cps[0].Name}, nil)
	if err != nil {
		t.Fatalf("fork from before-upgrade: %v", err)
	}
	if oldChild.Runtime != mysql80 || oldChild.LegacyAuth || oldChild.ForkRuntime != mysql80 || oldChild.ForkLegacyAuth {
		t.Fatalf("oldChild = %+v, want mysql80 without LegacyAuth", oldChild)
	}
}

func TestDeleteBranch_子ブランチがある間は親を削除できず子削除後にスナップショット回収して親を消せる(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")
	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c1")}, nil); err != nil {
		t.Fatal(err)
	}
	// Parent delete is refused with ErrInUse and touches nothing.
	z.j.entries = nil
	err := z.svc.DeleteBranch(ctx, name("x"), nil)
	if !errors.Is(err, domain.ErrInUse) || !strings.Contains(err.Error(), "c1") {
		t.Fatalf("DeleteBranch(parent) = %v, want ErrInUse naming c1", err)
	}
	if len(z.j.entries) != 0 {
		t.Errorf("refused parent delete touched system: %v", z.j.entries)
	}
	// Parent reset uses swap while child lives, moving fork-c1 to history/x/0.
	z.sim.write(live, "v2")
	z.tick()
	if _, err := z.svc.ResetBranch(ctx, name("x"), nil); err != nil {
		t.Fatalf("ResetBranch(parent) with child: %v", err)
	}
	if z.sim.ds["tank/db-branch/branches/c1"].data != "v1" {
		t.Errorf("child data damaged by parent reset: %q", z.sim.ds["tank/db-branch/branches/c1"].data)
	}
	// Delete child: destroys child dataset, finds & destroys fork-c1 on parent's hidden line, and collects history/x.
	if err := z.svc.DeleteBranch(ctx, name("c1"), nil); err != nil {
		t.Fatalf("DeleteBranch(c1): %v", err)
	}
	if _, ok := z.sim.ds[history+"/0"]; ok {
		t.Errorf("parent hidden line was not collected after child deletion: %v", z.sim.names())
	}
	// Now parent can be deleted cleanly.
	if err := z.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatalf("DeleteBranch(parent) after child gone: %v", err)
	}
}

func TestDeleteBranch_lostな親に子がいればabandonLostも拒否する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c1")}, nil); err != nil {
		t.Fatal(err)
	}
	p, _ := z.store.GetBranch(ctx, name("x"))
	p.Node = "other-node"
	p.Status = domain.BranchLost
	_ = z.store.PutBranch(ctx, p)

	err := z.svc.DeleteBranch(ctx, name("x"), nil)
	if !errors.Is(err, domain.ErrInUse) {
		t.Fatalf("abandonLost with child: err = %v, want ErrInUse", err)
	}
	p2, _ := z.store.GetBranch(ctx, name("x"))
	if p2.Status != domain.BranchLost {
		t.Errorf("parent status changed to %s", p2.Status)
	}
}

func TestDeleteBranch_チェックポイント起点の子を消しても親のチェックポイントは消さずgoneだけ回収する(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")
	z.checkpoint(t, "cp1")
	z.checkpoint(t, "cp2")
	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c1"), Checkpoint: "cp1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c2"), Checkpoint: "cp2"}, nil); err != nil {
		t.Fatal(err)
	}
	// Parent deletes cp2 while c2 lives -> renamed to gone-*.
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "cp2"); err != nil {
		t.Fatal(err)
	}
	// Delete c1: cp1 must remain on parent!
	if err := z.svc.DeleteBranch(ctx, name("c1"), nil); err != nil {
		t.Fatal(err)
	}
	if cps, _ := z.svc.ListCheckpoints(ctx, name("x")); len(cps) != 1 || cps[0].Name != "cp1" {
		t.Fatalf("parent checkpoints after c1 delete = %+v, want [cp1]", cps)
	}
	// Delete c2: gone-* snapshot on parent is collected!
	if err := z.svc.DeleteBranch(ctx, name("c2"), nil); err != nil {
		t.Fatal(err)
	}
	snaps, _ := z.sim.SnapshotInfos(ctx, live)
	for _, s := range snaps {
		if strings.HasPrefix(s.Name, domain.GonePrefix) {
			t.Errorf("gone snapshot not collected after c2 delete: %+v", snaps)
		}
	}
}

func TestExpire_子がある期限切れ親はスキップし子が消えた後に削除する(t *testing.T) {
	z := newZEnv(t)
	z.withBase(t)
	if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("a-parent"), Base: name("prod"), TTL: time.Hour}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("a-parent"), Name: name("z-child"), TTL: 2 * time.Hour}, nil); err != nil {
		t.Fatal(err)
	}
	// After 90m: parent expired, child still alive -> parent skipped with no error.
	z.now = z.now.Add(90 * time.Minute)
	deleted, err := z.svc.Expire(ctx)
	if err != nil || len(deleted) != 0 {
		t.Fatalf("at 90m: deleted=%v err=%v", deleted, err)
	}
	// After 3h: both expired -> child deleted first, then parent deleted too.
	z.now = z.now.Add(2 * time.Hour)
	deleted, err = z.svc.Expire(ctx)
	if err != nil || len(deleted) != 2 {
		t.Fatalf("at 3h: deleted=%v err=%v", deleted, err)
	}
}

func TestForkBranch_リソース制限と空き容量ガードとメモリ予算とプロトコル25を守る(t *testing.T) {
	t.Run("MaxTTL超過と既定適用", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.svc.ResourceLimits = func() dbbranch.Limits { return dbbranch.Limits{MaxTTL: 2 * time.Hour, RefQuotaBytes: 4 << 30} }
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), TTL: 3 * time.Hour}, nil); !errors.Is(err, domain.ErrInvalidTTL) {
			t.Fatalf("over MaxTTL: %v", err)
		}
		c, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil)
		if err != nil || c.ExpiresAt == nil || c.ExpiresAt.Sub(z.now) != 2*time.Hour {
			t.Fatalf("default MaxTTL: c=%+v err=%v", c, err)
		}
		if p, _ := z.sim.Props(ctx, "tank/db-branch/branches/c"); p["refquota"] != "4294967296" {
			t.Errorf("refquota = %q, want 4294967296", p["refquota"])
		}
	})
	t.Run("MaxBranchesPerUser", func(t *testing.T) {
		z := newZEnv(t)
		z.withBase(t)
		if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Owner: "u1"}, nil); err != nil {
			t.Fatal(err)
		}
		z.svc.ResourceLimits = func() dbbranch.Limits { return dbbranch.Limits{MaxBranchesPerUser: 1} }
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Owner: "u1"}, nil); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatalf("over quota: %v", err)
		}
	})
	t.Run("PoolSpace不足", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.svc.PoolSpace = func(context.Context) (uint64, uint64, error) { return 20 << 30, 512 << 20, nil }
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, domain.ErrNoSpace) {
			t.Fatalf("low space: %v", err)
		}
	})
	t.Run("メモリ予算不足", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		// BranchMB = 128 + 256 = 384; 1 branch running, budget 500MB cannot fit 2 branches (768MB).
		z.svc.HostMemory = func() (uint64, uint64) { return 666, 0 }
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, domain.ErrNoCapacity) {
			t.Fatalf("no memory: %v", err)
		}
	})
	t.Run("プロトコル25未満のノードがある間はErrClusterUpgrading", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		nodes := &fakeNodes{nodes: []domain.Node{
			{Name: "node1", LastSeen: z.now, Protocol: domain.ProtocolBranchFork},
			{Name: "node2", LastSeen: z.now, Protocol: domain.ProtocolBranchFork - 1},
		}}
		z.svc.Nodes = nodes
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, domain.ErrClusterUpgrading) {
			t.Fatalf("upgrading: %v", err)
		}
		nodes.nodes[1].Protocol = domain.ProtocolBranchFork
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err != nil {
			t.Fatalf("after upgrade: %v", err)
		}
	})
}

func TestForkBranch_Record有効時はCloneHistoryResetterで履歴を初期化する(t *testing.T) {
	z := newZEnv(t)
	hs := &historyServer{crashServer: crashServer{z.server, z.crash}}
	z.svc.Servers[domain.MySQL] = hs
	z.options()
	z.svc.OpenPassword = func() string { return "shared-pw" }
	z.withBase(t)
	if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Record: true}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Record: true, Access: domain.AccessOpen}, nil)
	if err != nil {
		t.Fatalf("ForkBranch record: %v", err)
	}
	if !c.Record || c.Access != domain.AccessOpen || c.Password != "shared-pw" {
		t.Errorf("child = %+v", c)
	}
	if len(hs.resets) != 1 || hs.resets[0] != "branch-c" {
		t.Errorf("ResetCloneHistory calls = %v, want [branch-c]", hs.resets)
	}
	// Non-recorded child does not call ResetCloneHistory (unless RecordNew returns true).
	hs.resets = nil
	z.svc.RecordNew = func(context.Context, domain.Branch) bool { return true }
	c2, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c2")}, nil)
	if err != nil || !c2.Record || len(hs.resets) != 1 {
		t.Errorf("RecordNew child: rec=%v resets=%v err=%v", c2.Record, hs.resets, err)
	}
}

func TestRecover_フォーク途中で再起動した親をreadyに戻し子を削除で掃除できる(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	// Simulate crash while parent was forking and child was provisioning with fork-c already taken.
	if err := z.sim.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: "fork-c"}); err != nil {
		t.Fatal(err)
	}
	if err := z.sim.Clone(ctx, domain.Snapshot{Dataset: live, Name: "fork-c"}, "tank/db-branch/branches/c"); err != nil {
		t.Fatal(err)
	}
	p, _ := z.store.GetBranch(ctx, name("x"))
	p.Status = domain.BranchForking
	_ = z.store.PutBranch(ctx, p)
	_ = z.store.InsertBranch(ctx, domain.Branch{
		Name: name("c"), Base: p.Base, Snapshot: p.Snapshot, Runtime: p.Runtime,
		Status: domain.BranchProvisioning, Port: 40005, Node: "node1", User: "c", Password: "pw",
		ParentBranch: name("x"), ParentSnapshot: "fork-c",
	})
	z.restart()

	if err := z.svc.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	p, _ = z.store.GetBranch(ctx, name("x"))
	c, _ := z.store.GetBranch(ctx, name("c"))
	if p.Status != domain.BranchReady || !z.server.running["branch-x"] {
		t.Errorf("parent after recover: status=%s running=%v", p.Status, z.server.running["branch-x"])
	}
	if c.Status != domain.BranchFailed {
		t.Errorf("child after recover: status=%s, want failed", c.Status)
	}
	// Deleting failed child cleans up fork-c on parent.
	if err := z.svc.DeleteBranch(ctx, name("c"), nil); err != nil {
		t.Fatalf("DeleteBranch(c): %v", err)
	}
	snaps, _ := z.sim.SnapshotInfos(ctx, live)
	for _, s := range snaps {
		if s.Name == "fork-c" {
			t.Errorf("fork-c still present after deleting failed child")
		}
	}
}

func TestForkBranch_多段フォークで孫ブランチを作成してリーフ順に削除できる(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")
	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c1")}, nil); err != nil {
		t.Fatal(err)
	}
	z.sim.write("tank/db-branch/branches/c1", "v2")
	gc, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("c1"), Name: name("c2")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gc.ParentBranch != name("c1") || z.sim.ds["tank/db-branch/branches/c2"].data != "v2" {
		t.Fatalf("grandchild = %+v data=%q", gc, z.sim.ds["tank/db-branch/branches/c2"].data)
	}
	if err := z.svc.DeleteBranch(ctx, name("c1"), nil); !errors.Is(err, domain.ErrInUse) {
		t.Fatalf("delete c1 with grandchild: %v", err)
	}
	if err := z.svc.DeleteBranch(ctx, name("c2"), nil); err != nil {
		t.Fatal(err)
	}
	if err := z.svc.DeleteBranch(ctx, name("c1"), nil); err != nil {
		t.Fatal(err)
	}
	if err := z.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestForkBranch_スナップショット失敗やInitSQL失敗や履歴リセット失敗を安全に処理する(t *testing.T) {
	t.Run("ライブスナップショット失敗で予約取り消し", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.sim.failOn = "zfs snapshot"
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err == nil {
			t.Fatal("want snapshot error")
		}
		if _, err := z.store.GetBranch(ctx, name("c")); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("child reservation not deleted: %v", err)
		}
	})
	t.Run("clean時のスナップショットとクローン失敗で親を復帰", func(t *testing.T) {
		for _, failOp := range []string{"zfs snapshot", "zfs clone"} {
			z := newZEnv(t)
			z.branch(t)
			z.sim.failOn = failOp
			if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil); err == nil {
				t.Fatalf("%s: want error", failOp)
			}
			p, _ := z.store.GetBranch(ctx, name("x"))
			if p.Status != domain.BranchReady || !z.server.running["branch-x"] {
				t.Errorf("%s: parent not restored: status=%s running=%v", failOp, p.Status, z.server.running["branch-x"])
			}
			if _, err := z.store.GetBranch(ctx, name("c")); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("%s: child left behind: %v", failOp, err)
			}
		}
	})
	t.Run("InitSQL失敗やResetCloneHistory失敗は子をfailedにする", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.server.failOn = "exec-as"
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c1"), InitSQL: "BAD"}, nil); err == nil {
			t.Fatal("want init sql error")
		}
		if b, _ := z.store.GetBranch(ctx, name("c1")); b.Status != domain.BranchFailed {
			t.Errorf("c1 status = %s, want failed", b.Status)
		}

		z2 := newZEnv(t)
		hs := &historyServer{crashServer: crashServer{z2.server, z2.crash}, fail: errors.New("reset binlog failed")}
		z2.svc.Servers[domain.MySQL] = hs
		z2.options()
		z2.branch(t)
		if _, err := z2.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c2"), Record: true}, nil); err == nil {
			t.Fatal("want reset clone history error")
		}
		if b, _ := z2.store.GetBranch(ctx, name("c2")); b.Status != domain.BranchFailed {
			t.Errorf("c2 status = %s, want failed", b.Status)
		}
	})
	t.Run("recoverForkingのRecord必須エラーは親をfailedにする", func(t *testing.T) {
		z := newZEnv(t)
		z.options()
		z.withBase(t)
		if _, err := z.svc.CreateBranch(ctx, createReq("x", true), nil); err != nil {
			t.Fatal(err)
		}
		p, _ := z.store.GetBranch(ctx, name("x"))
		p.Status = domain.BranchForking
		_ = z.store.PutBranch(ctx, p)
		z.svc.ServerOptions = nil
		z.restart()
		if err := z.svc.Recover(ctx); !errors.Is(err, dbbranch.ErrRecordRequired) {
			t.Fatalf("Recover = %v, want ErrRecordRequired", err)
		}
		if b, _ := z.store.GetBranch(ctx, name("x")); b.Status != domain.BranchFailed {
			t.Errorf("status = %s, want failed", b.Status)
		}
	})
	t.Run("子削除時の親スナップショット破棄失敗はログに記録し削除は進める", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err != nil {
			t.Fatal(err)
		}
		var logged []string
		z.svc.Logf = func(f string, a ...any) { logged = append(logged, f) }
		z.sim.failOn = "zfs destroy tank/db-branch/branches/x@fork-c"
		if err := z.svc.DeleteBranch(ctx, name("c"), nil); err != nil {
			t.Fatalf("DeleteBranch(c): %v", err)
		}
		if len(logged) == 0 {
			t.Error("expected warning log when fork snapshot destroy fails")
		}
	})
	t.Run("ポート枯渇やリセットスナップショット欠損を拒否する", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		// Exhaust ports (40000 base, 40001 branch x -> limit range to 40000..40001).
		z.svc.SetLimits(128, dbbranch.PortRange{Min: 40000, Max: 40001})
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, dbbranch.ErrNoFreePort) {
			t.Fatalf("no free port: %v", err)
		}
		z.svc.SetLimits(128, dbbranch.PortRange{Min: 40000, Max: 40010})
		// Remove @reset from parent live dataset and try @reset fork.
		z.sim.ds[live].snaps = nil
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Checkpoint: domain.ResetPoint}, nil); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing @reset: %v", err)
		}
		// Remove live dataset entirely and try live fork.
		delete(z.sim.ds, live)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing live dataset: %v", err)
		}
	})
	t.Run("クリーンフォークで親再起動に失敗すると親も子もfailedになる", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.server.failOn = "start"
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil); err == nil {
			t.Fatal("want start error")
		}
		p, _ := z.store.GetBranch(ctx, name("x"))
		c, _ := z.store.GetBranch(ctx, name("c"))
		if p.Status != domain.BranchFailed || c.Status != domain.BranchFailed {
			t.Errorf("parent=%s child=%s, want both failed", p.Status, c.Status)
		}
	})
	t.Run("フォーク済みの親から@resetフォークすると親の@resetのPropRuntimeを引き継ぐ", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c1")}, nil); err != nil {
			t.Fatal(err)
		}
		// Add a checkpoint and restore so @reset moves to a non-live line, exercising snapshotProp loop.
		z.tick()
		if _, err := z.svc.CreateCheckpoint(ctx, name("c1"), "cp1"); err != nil {
			t.Fatal(err)
		}
		z.tick()
		if _, err := z.svc.RestoreCheckpoint(ctx, name("c1"), "cp1", nil); err != nil {
			t.Fatal(err)
		}
		c2, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("c1"), Name: name("c2"), Checkpoint: domain.ResetPoint}, nil)
		if err != nil || c2.Runtime != mysql84 {
			t.Fatalf("c2 = %+v, err = %v", c2, err)
		}
	})
	t.Run("予約取り消しのDeleteBranch失敗時は子をfailedに残す", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.svc.Store = failDeleteStore{Store: z.store, err: errors.New("delete store failed")}
		z.sim.failOn = "zfs clone"
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err == nil {
			t.Fatal("want error")
		}
		if b, _ := z.store.GetBranch(ctx, name("c")); b.Status != domain.BranchFailed {
			t.Errorf("status = %s, want failed", b.Status)
		}
	})
	t.Run("完了時のPutBranch失敗は子サーバーを即停止する", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.svc.Store = failPutOnReadyStore{Store: z.store, err: errors.New("raft lost")}
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err == nil {
			t.Fatal("want put error")
		}
		if z.server.running["branch-c"] {
			t.Error("child server must be stopped when final saveBranch fails")
		}
		// recoverForking with saveBranch failure also stops the server immediately.
		p, _ := z.store.GetBranch(ctx, name("x"))
		p.Status = domain.BranchForking
		_ = z.store.PutBranch(ctx, p)
		z.restart()
		if err := z.svc.Recover(ctx); err == nil {
			t.Fatal("want recover saveBranch error")
		}
		if z.server.running["branch-x"] {
			t.Error("parent server must be stopped when recoverForking saveBranch fails")
		}
	})
	t.Run("親のfamily読取失敗時もログして子削除を完了する", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err != nil {
			t.Fatal(err)
		}
		var logged []string
		z.svc.Logf = func(f string, a ...any) { logged = append(logged, f) }
		// Fail Props only when reading parent's live dataset (after child's dataset is destroyed).
		z.sim.readFail = "Props"
		// Make child live dataset already gone so destroyBranchData won't call Props on child,
		// and cleanupParentFork hits Props error on parent.
		delete(z.sim.ds, "tank/db-branch/branches/c")
		if err := z.svc.DeleteBranch(ctx, name("c"), nil); err != nil {
			t.Fatalf("DeleteBranch: %v", err)
		}
		if len(logged) == 0 {
			t.Error("expected log on parent family read error")
		}
	})
}

type failDeleteStore struct {
	domain.Store
	err error
}

func (s failDeleteStore) DeleteBranch(context.Context, domain.Name) error { return s.err }

type failPutOnReadyStore struct {
	domain.Store
	err error
}

func (s failPutOnReadyStore) PutBranch(ctx context.Context, b domain.Branch) error {
	if b.Status == domain.BranchReady {
		return s.err
	}
	return s.Store.PutBranch(ctx, b)
}

func TestForkBranch_open親からprivateフォーク時に親とレガシーのDBユーザーを除去する(t *testing.T) {
	z := newZEnv(t)
	hs := &historyServer{crashServer: crashServer{z.server, z.crash}}
	z.svc.Servers[domain.MySQL] = hs
	z.svc.OpenPassword = func() string { return "open-secret" }
	z.withBase(t)
	if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Access: domain.AccessOpen}, nil); err != nil {
		t.Fatal(err)
	}

	c, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Access: domain.AccessPrivate}, nil)
	if err != nil {
		t.Fatalf("ForkBranch: %v", err)
	}
	if c.Access != domain.AccessPrivate || c.Password == "open-secret" {
		t.Fatalf("child access=%s pw=%q", c.Access, c.Password)
	}
	want := "branch-c:x branch-c:branch"
	if got := strings.Join(hs.dropped, " "); got != want {
		t.Errorf("dropped = %q, want %q", got, want)
	}
	// DropUser must happen after CreateUser and before taking @reset.
	if z.j.indexOf("drop-user branch-c x") < 0 || z.j.indexOf("drop-user branch-c x") > z.j.indexOf("zfs snapshot tank/db-branch/branches/c@reset") {
		t.Errorf("drop-user order wrong: %v", z.j.entries)
	}

	// Failure in DropUser fails the fork.
	hs.dropFail = errors.New("cannot drop parent role")
	if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c2")}, nil); err == nil {
		t.Fatal("want DropUser error")
	}
	if b, _ := z.store.GetBranch(ctx, name("c2")); b.Status != domain.BranchFailed {
		t.Errorf("c2 status = %s, want failed", b.Status)
	}
}

func TestForkBranch_LiveFreeze対応エンジンの正常系とPostFreeze失敗時のスナップショット回収(t *testing.T) {
	t.Run("正常系はPreFreezeとPostFreezeでスナップショットを囲む", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.freezing(&freezeProvider{liveFrz: true})
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err != nil {
			t.Fatalf("ForkBranch: %v", err)
		}
		order := []int{z.at("prefreeze"), z.at("zfs snapshot " + live + "@fork-c"), z.at("postfreeze tok")}
		for i, pos := range order {
			if pos < 0 || (i > 0 && pos < order[i-1]) {
				t.Fatalf("order = %v in %q", order, z.j.entries)
			}
		}
	})
	t.Run("PostFreeze失敗時は作成済みforkスナップショットも破棄してリークさせない", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		z.freezing(&freezeProvider{liveFrz: true, postErr: errors.New("unfreeze failed")})
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err == nil {
			t.Fatal("want PostFreeze error")
		}
		if _, err := z.store.GetBranch(ctx, name("c")); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("child record leaked: %v", err)
		}
		snaps, _ := z.sim.SnapshotInfos(ctx, live)
		for _, s := range snaps {
			if s.Name == "fork-c" {
				t.Errorf("fork-c snapshot leaked after PostFreeze failure: %+v", snaps)
			}
		}
	})
}

func TestDeleteBranch_親のアップグレード中やリストア中に子を消してもPropUpgradeを壊さず完了後に回収される(t *testing.T) {
	t.Run("親のUpgradeBranch中に子削除してもPropUpgradeとhistoryRootを破壊しない", func(t *testing.T) {
		z := newZEnv(t)
		mysql80 := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
		z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql80: true, mysql84: true}}
		if _, err := z.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql80, Dump: "/d.sql"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("a-child")}, nil); err != nil {
			t.Fatal(err)
		}
		// Simulate parent mid-upgrade: status=upgrading and history/x holds PropUpgrade.
		if err := z.sim.Create(ctx, history, map[string]string{"canmount": "off"}); err != nil {
			t.Fatal(err)
		}
		if err := z.sim.SetProps(ctx, history, map[string]string{domain.PropUpgrade: domain.RuntimeProp(mysql80) + "/" + domain.RuntimeProp(mysql84) + "/before-upgrade-1"}); err != nil {
			t.Fatal(err)
		}
		p, _ := z.store.GetBranch(ctx, name("x"))
		p.Status = domain.BranchUpgrading
		_ = z.store.PutBranch(ctx, p)

		// Delete child while parent is upgrading: must NOT touch history/x or PropUpgrade.
		if err := z.svc.DeleteBranch(ctx, name("a-child"), nil); err != nil {
			t.Fatalf("DeleteBranch(a-child): %v", err)
		}
		if ok, _ := z.sim.Exists(ctx, history); !ok {
			t.Fatal("history/x was destroyed while PropUpgrade was active!")
		}
		props, _ := z.sim.Props(ctx, history)
		if props[domain.PropUpgrade] == "" {
			t.Fatalf("PropUpgrade was wiped: %v", props)
		}
		// Recover finishes parent's upgrade and collects orphaned fork-a-child and empty history/x.
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		snaps, _ := z.sim.SnapshotInfos(ctx, live)
		for _, s := range snaps {
			if s.Name == "fork-a-child" {
				t.Errorf("orphaned fork-a-child not collected after parent upgrade settled: %+v", snaps)
			}
		}
		if ok, _ := z.sim.Exists(ctx, history); ok {
			t.Errorf("empty history/x should be dropped once PropUpgrade is cleared")
		}
	})

	t.Run("Recoverで子BranchDeletingが親BranchUpgradingより辞書順で先でも親を先に復旧する", func(t *testing.T) {
		z := newZEnv(t)
		mysql80 := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
		z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql80: true, mysql84: true}}
		if _, err := z.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql80, Dump: "/d.sql"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("parent"), Base: name("prod")}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("parent"), Name: name("child")}, nil); err != nil {
			t.Fatal(err)
		}
		parentLive := "tank/db-branch/branches/parent"
		parentHist := "tank/db-branch/history/parent"
		// Simulate parent mid-upgrade (after before-upgrade checkpoint saved, runtime switched to 8.4).
		if err := z.sim.Create(ctx, parentHist, map[string]string{"canmount": "off"}); err != nil {
			t.Fatal(err)
		}
		if err := z.sim.Snapshot(ctx, domain.Snapshot{Dataset: parentLive, Name: domain.CheckpointSnapshot("before-upgrade-1")}); err != nil {
			t.Fatal(err)
		}
		_ = z.sim.SetProps(ctx, parentLive+"@"+domain.CheckpointSnapshot("before-upgrade-1"), map[string]string{
			domain.PropLine: "0", domain.PropAuto: "on", domain.PropRuntime: domain.RuntimeProp(mysql80),
		})
		_ = z.sim.SetProps(ctx, parentHist, map[string]string{domain.PropUpgrade: domain.RuntimeProp(mysql80) + "/" + domain.RuntimeProp(mysql84) + "/before-upgrade-1"})
		p, _ := z.store.GetBranch(ctx, name("parent"))
		p.Status = domain.BranchUpgrading
		p.Runtime = mysql84
		_ = z.store.PutBranch(ctx, p)
		c, _ := z.store.GetBranch(ctx, name("child"))
		c.Status = domain.BranchDeleting
		_ = z.store.PutBranch(ctx, c)
		z.restart()

		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		// Parent rolled back to mysql80 via PropUpgrade; child was deleted and fork-child cleaned up.
		pAfter, _ := z.store.GetBranch(ctx, name("parent"))
		if pAfter.Status != domain.BranchReady || pAfter.Runtime != mysql80 {
			t.Errorf("parent after recover = %s (%s), want ready (%s)", pAfter.Status, pAfter.Runtime, mysql80)
		}
		if _, err := z.store.GetBranch(ctx, name("child")); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("child still exists after recover: %v", err)
		}
	})

	t.Run("cleanupParentForkで親のGetBranchが一時エラーならフェイルクローズして親リネージを触らない", func(t *testing.T) {
		z := newZEnv(t)
		z.branch(t)
		if _, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err != nil {
			t.Fatal(err)
		}
		var logged []string
		z.svc.Logf = func(f string, a ...any) { logged = append(logged, f) }
		z.svc.Store = failGetParentStore{Store: z.store, parent: name("x"), err: errors.New("raft unavailable")}
		if err := z.svc.DeleteBranch(ctx, name("c"), nil); err != nil {
			t.Fatalf("DeleteBranch(c): %v", err)
		}
		if len(logged) == 0 {
			t.Error("expected warning log on parent GetBranch error")
		}
		// fork-c was left untouched while GetBranch failed, then reaped by normal collect once store recovers.
		snaps, _ := z.sim.SnapshotInfos(ctx, live)
		found := false
		for _, s := range snaps {
			if s.Name == "fork-c" {
				found = true
			}
		}
		if !found {
			t.Error("cleanupParentFork should fail closed and not touch parent snapshots when GetBranch fails")
		}
	})
}

type failGetParentStore struct {
	domain.Store
	parent domain.Name
	err    error
}

func (s failGetParentStore) GetBranch(ctx context.Context, n domain.Name) (domain.Branch, error) {
	if n == s.parent {
		return domain.Branch{}, s.err
	}
	return s.Store.GetBranch(ctx, n)
}

func TestForkBranch_clean失敗時のrestoreParentでensureRunning失敗なら親をfailedにする(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.failOn = "zfs snapshot"
	z.server.failOn = "start"

	child, err := z.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if child.Name != (domain.Name{}) {
		t.Errorf("child should be empty when cancelled: %+v", child)
	}
	p, _ := z.store.GetBranch(ctx, name("x"))
	if p.Status != domain.BranchFailed {
		t.Errorf("parent status = %s, want failed when restoreParent ensureRunning fails", p.Status)
	}

	// Also verify cloneCleanFork returning child when parent restart fails after clone.
	z2 := newZEnv(t)
	z2.branch(t)
	z2.server.failOn = "start"
	child2, err := z2.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil)
	if err == nil {
		t.Fatal("want parent restart error")
	}
	if child2.Name != name("c") {
		t.Errorf("child2.Name = %q, want c (kept failed in store)", child2.Name)
	}

	// saveBranch failure in cloneCleanFork (both restoreParent and post-clone) stops parent immediately.
	z3 := newZEnv(t)
	z3.branch(t)
	z3.svc.Store = failPutOnReadyStore{Store: z3.store, err: errors.New("raft lost")}
	z3.sim.failOn = "zfs snapshot"
	if _, err := z3.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil); err == nil {
		t.Fatal("want restoreParent saveBranch error")
	}
	if z3.server.running["branch-x"] {
		t.Error("parent must be stopped when restoreParent saveBranch fails")
	}

	z4 := newZEnv(t)
	z4.branch(t)
	z4.svc.Store = failPutOnReadyStore{Store: z4.store, err: errors.New("raft lost")}
	child4, err := z4.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil)
	if err == nil || child4.Name != name("c") {
		t.Fatalf("post-clone saveBranch failure: child=%+v err=%v", child4, err)
	}
	if z4.server.running["branch-x"] {
		t.Error("parent must be stopped when post-clone saveBranch fails")
	}

	// InsertBranch failure during cleanStop reverts parent back to BranchReady.
	z5 := newZEnv(t)
	z5.branch(t)
	z5.svc.Store = failInsertStore{Store: z5.store, err: errors.New("insert failed")}
	if _, err := z5.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c"), Clean: true}, nil); err == nil {
		t.Fatal("want InsertBranch error")
	}
	if p5, _ := z5.store.GetBranch(ctx, name("x")); p5.Status != domain.BranchReady {
		t.Errorf("parent status after clean InsertBranch failure = %s, want ready", p5.Status)
	}
	if _, err := z5.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err == nil {
		t.Fatal("want non-clean InsertBranch error")
	}

	// doDelete Store.DeleteBranch failure returns error so Recover can retry.
	z6 := newZEnv(t)
	z6.branch(t)
	if _, err := z6.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("c")}, nil); err != nil {
		t.Fatal(err)
	}
	z6.svc.Store = failDeleteStore{Store: z6.store, err: errors.New("delete failed")}
	if err := z6.svc.DeleteBranch(ctx, name("c"), nil); err == nil {
		t.Fatal("want Store.DeleteBranch error")
	}
}

type failInsertStore struct {
	domain.Store
	err error
}

func (s failInsertStore) InsertBranch(context.Context, domain.Branch) error { return s.err }
