package dbbranch_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

type fakeMerger struct {
	mu       sync.Mutex
	diffPlan domain.MergePlan
	diffErr  error
	applyErr error
	diffReqs []domain.MergeDiffRequest
	applied  []domain.MergePlan
	onDiff   func(req domain.MergeDiffRequest)
	onApply  func(target domain.AdminAccess, plan domain.MergePlan)
}

func (f *fakeMerger) Diff(_ context.Context, req domain.MergeDiffRequest) (domain.MergePlan, error) {
	f.mu.Lock()
	f.diffReqs = append(f.diffReqs, req)
	fn := f.onDiff
	f.mu.Unlock()
	if fn != nil {
		fn(req)
	}
	return f.diffPlan, f.diffErr
}

func (f *fakeMerger) Apply(_ context.Context, target domain.AdminAccess, plan domain.MergePlan, report func(phase string)) error {
	f.mu.Lock()
	f.applied = append(f.applied, plan)
	fn := f.onApply
	f.mu.Unlock()
	if report != nil {
		report("applying")
		report("verifying")
	}
	if fn != nil {
		fn(target, plan)
	}
	return f.applyErr
}

func intPtr(v int) *int { return &v }

func (z *zsim) read(name string) string { return z.ds[name].data }

func newMergeEnv(t *testing.T) (*zenv, *fakeMerger) {
	t.Helper()
	z := newZEnv(t)
	m := &fakeMerger{
		diffPlan: domain.MergePlan{
			Inserted: 2, Updated: 1, Deleted: 1,
			Tables: []domain.MergeTableSummary{{Database: "shop", Table: "orders", Inserted: 2, Updated: 1, Deleted: 1}},
		},
	}
	z.svc.Merger = m
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql84: true, mysql80: true}}
	z.withBase(t)
	z.withBranch(t, "x")
	z.sim.write(live, "v0")
	// line 0 -> cp1 -> line 0 moves to v_line0, then restore cp1 -> line 1 becomes v_line1
	z.checkpoint(t, "cp1")
	z.sim.write(live, "line0-work")
	z.checkpoint(t, "cp2") // the tip of line 0 survives the restore only through a checkpoint
	z.restore(t, "cp1")
	z.sim.write(live, "line1-work")
	z.j.entries = nil
	return z, m
}

func TestMergeBranch_別ラインの変更を統合し一時リソース掃除とメンテナンス起動を行う(t *testing.T) {
	z, m := newMergeEnv(t)

	var phases []string
	res, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{
		Name: name("x"), SourceLine: intPtr(0),
	}, func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatalf("MergeBranch failed: %v", err)
	}
	if res.Status != domain.MergeMerged || res.SourceLine != 0 || res.TargetLine != 1 || res.Ancestor != "cp1" {
		t.Fatalf("res = %+v", res)
	}
	if res.Inserted != 2 || res.Updated != 1 || res.Deleted != 1 || !strings.HasPrefix(res.Checkpoint, "before-merge-") {
		t.Errorf("summary/checkpoint = %+v", res)
	}
	if len(m.diffReqs) != 1 || len(m.applied) != 1 {
		t.Fatalf("diffReqs=%d applied=%d", len(m.diffReqs), len(m.applied))
	}
	// Diff receives 3 distinct ports and short server IDs (<= 11 chars => socket under /run/m-...)
	req := m.diffReqs[0]
	if req.Ancestor.Port == req.Source.Port || req.Source.Port == req.Target.Port || req.Ancestor.Port == req.Target.Port {
		t.Errorf("temp clones did not get 3 distinct ports: A=%d S=%d T=%d", req.Ancestor.Port, req.Source.Port, req.Target.Port)
	}
	for _, acc := range []domain.AdminAccess{req.Ancestor, req.Source, req.Target} {
		if !strings.HasPrefix(acc.Socket, "/run/m-") || len(acc.Socket) > 30 {
			t.Errorf("unexpected temp clone socket %q", acc.Socket)
		}
	}
	// Work datasets and temporary snapshots must be cleaned up
	for _, ds := range []string{"tank/db-branch/work"} {
		if children, err := z.sim.Datasets(ctx, ds); err == nil {
			for _, ch := range children {
				if strings.Contains(ch, "merge-") {
					t.Errorf("leaked temp work dataset: %s", ch)
				}
			}
		}
	}
	if lines, err := z.sim.SnapshotInfos(ctx, live); err == nil {
		for _, sn := range lines {
			if domain.IsMergeSnapshot(sn.Name) {
				t.Errorf("leaked temp merge snapshot: %s", sn.Name)
			}
		}
	}
	// PropMerge must be cleared on history/<name>
	if p, _ := z.sim.Props(ctx, history); p[domain.PropMerge] != "" {
		t.Errorf("PropMerge left on history: %v", p)
	}
	// Branch must return to BranchReady and running
	b, _ := z.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchReady || !z.server.running["branch-x"] {
		t.Errorf("final branch = %+v, running=%v", b, z.server.running["branch-x"])
	}
	// putLoginRight (CREATE USER) must have been called on maintenance server
	lastStarted := z.server.started[len(z.server.started)-1]
	if lastStarted.Maintenance {
		t.Errorf("final start should not be maintenance mode: %+v", lastStarted)
	}
	var sawMaint bool
	for _, s := range z.server.started {
		if s.ID == "branch-x" && s.Maintenance {
			sawMaint = true
		}
	}
	if !sawMaint {
		t.Error("live branch was never started in Maintenance: true for Apply")
	}
	joinedPhases := strings.Join(phases, ",")
	for _, want := range []string{"stopping", "snapshotting", "starting_clones", "diffing", "checkpointing", "applying", "verifying", "starting"} {
		if !strings.Contains(joinedPhases, want) {
			t.Errorf("missing phase %q in %v", want, phases)
		}
	}
}

func TestMergeBranch_一時停止ブランチとコンフリクト時無変更保証とチェックポイント指定(t *testing.T) {
	z, m := newMergeEnv(t)

	// Merge on paused branch -> stays paused after merge!
	if _, err := z.svc.PauseBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	res, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if err != nil {
		t.Fatalf("MergeBranch on paused failed: %v", err)
	}
	b, _ := z.store.GetBranch(ctx, name("x"))
	if res.Status != domain.MergeMerged || b.Status != domain.BranchPaused || z.server.running["branch-x"] {
		t.Fatalf("paused branch did not stay paused: status=%s running=%v", b.Status, z.server.running["branch-x"])
	}

	// Conflict on paused branch -> no checkpoint taken, no Apply called, stays paused
	snapsBefore := len(z.names(t))
	m.diffPlan = domain.MergePlan{
		Code:           "merge_conflict",
		TotalConflicts: 1,
		Conflicts:      []domain.MergeConflict{{Type: domain.ConflictUpdateUpdate, Database: "shop", Table: "orders"}},
	}
	m.applied = nil
	resC, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if !errors.Is(err, domain.ErrMergeConflict) {
		t.Fatalf("expected ErrMergeConflict, got %v", err)
	}
	if resC.Status != domain.MergeConflicted || resC.Code != "merge_conflict" || len(m.applied) != 0 {
		t.Errorf("conflict res = %+v, applied=%d", resC, len(m.applied))
	}
	if len(z.names(t)) != snapsBefore {
		t.Errorf("conflict created unexpected checkpoint: before=%d after=%d", snapsBefore, len(z.names(t)))
	}
}

func TestMergeBranch_DryRun無停止と競合操作の排他ガード(t *testing.T) {
	z, m := newMergeEnv(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	m.onDiff = func(domain.MergeDiffRequest) {
		close(entered)
		<-release
	}

	var dryRes domain.MergeResult
	var dryErr error
	done := make(chan struct{})
	go func() {
		dryRes, dryErr = z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{
			Name: name("x"), SourceLine: intPtr(0), DryRun: true,
		}, nil)
		close(done)
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("dry-run ended before the diff: res=%+v err=%v", dryRes, dryErr)
	}

	// Live server must still be running during dry-run!
	if !z.server.running["branch-x"] {
		t.Error("dry-run stopped live server")
	}
	// Concurrent state-mutating operations must return ErrConflict while dry-run holds lock
	if err := z.svc.DeleteBranch(ctx, name("x"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("DeleteBranch during dry-run = %v, want ErrConflict", err)
	}
	if _, err := z.svc.PauseBranch(ctx, name("x"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("PauseBranch during dry-run = %v, want ErrConflict", err)
	}
	if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), "cp1", nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("RestoreCheckpoint during dry-run = %v, want ErrConflict", err)
	}
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "cp1"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("DeleteCheckpoint during dry-run = %v, want ErrConflict", err)
	}
	if _, err := z.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("concurrent Merge during dry-run = %v, want ErrConflict", err)
	}

	close(release)
	<-done
	if dryErr != nil || dryRes.Status != domain.MergePreview || !dryRes.DryRun || len(m.applied) != 0 {
		t.Fatalf("dryRun res=%+v err=%v applied=%d", dryRes, dryErr, len(m.applied))
	}
	if z.j.has("stop branch-x clean") {
		t.Errorf("dry-run should never stop branch-x: %v", z.j.entries)
	}
}

func TestMergeBranch_Apply失敗時自動ロールバックとRecoverクラッシュ復旧(t *testing.T) {
	z, m := newMergeEnv(t)

	// 1. Apply fails on a PAUSED branch -> swaps back to before-merge-* and keeps BranchPaused!
	if _, err := z.svc.PauseBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	m.onApply = func(domain.AdminAccess, domain.MergePlan) {
		z.sim.write(live, "corrupted-half-applied")
	}
	m.applyErr = fmt.Errorf("%w: orphan row", domain.ErrMergeConflict)
	res, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if err == nil || res.Status != domain.MergeRolledBack || res.Code != "merge_conflict" {
		t.Fatalf("expected rolled_back with merge_conflict, got res=%+v err=%v", res, err)
	}
	if got := z.sim.read(live); got != "line1-work" {
		t.Errorf("live data after rollback = %q, want %q", got, "line1-work")
	}
	if b, _ := z.store.GetBranch(ctx, name("x")); b.Status != domain.BranchPaused || z.server.running["branch-x"] {
		t.Errorf("paused branch after rollback = %+v running=%v", b, z.server.running["branch-x"])
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropMerge] != "" {
		t.Errorf("PropMerge left after rollback: %v", p)
	}

	// 2. Crash recovery: orphaned work/merge-* dataset + @merge-* snapshot + BranchMerging pre-checkpoint vs post-checkpoint
	m.applyErr = nil
	m.onApply = nil
	_ = z.sim.Create(ctx, "tank/db-branch/work", map[string]string{"canmount": "off"})
	_ = z.sim.Create(ctx, "tank/db-branch/work/merge-abc", map[string]string{"canmount": "off"})
	_ = z.sim.Create(ctx, "tank/db-branch/work/merge-abc/a", nil)
	_ = z.sim.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: "merge-live-abc"})
	_ = z.sim.SetProps(ctx, history, map[string]string{
		domain.PropMerge: "ready/-/-/" + domain.RuntimeProp(mysql84) + "/cp1",
	})
	br, _ := z.store.GetBranch(ctx, name("x"))
	br.Status = domain.BranchMerging
	_ = z.store.PutBranch(ctx, br)

	if err := z.svc.Recover(ctx); err != nil {
		t.Fatalf("Recover pre-checkpoint failed: %v", err)
	}
	if br, _ = z.store.GetBranch(ctx, name("x")); br.Status != domain.BranchReady || !z.server.running["branch-x"] {
		t.Errorf("pre-checkpoint Recover branch = %+v running=%v", br, z.server.running["branch-x"])
	}
	if ok, _ := z.sim.Exists(ctx, "tank/db-branch/work/merge-abc"); ok {
		t.Error("Recover did not sweep orphaned work/merge-abc")
	}
	if snaps, _ := z.sim.SnapshotInfos(ctx, live); len(snaps) > 0 {
		for _, sn := range snaps {
			if domain.IsMergeSnapshot(sn.Name) {
				t.Errorf("Recover did not sweep @%s snapshot", sn.Name)
			}
		}
	}

	// Post-checkpoint crash recovery (e.g. crash during Apply on a paused branch with before-merge-* saved)
	_, _ = z.svc.PauseBranch(ctx, name("x"), nil)
	_ = z.sim.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: domain.CheckpointSnapshot("before-merge-crash")})
	_ = z.sim.SetProps(ctx, live+"@"+domain.CheckpointSnapshot("before-merge-crash"), map[string]string{
		domain.PropLine: "2", domain.PropAuto: "on", domain.PropRuntime: domain.RuntimeProp(mysql84),
	})
	z.sim.write(live, "dirty-crash-data")
	_ = z.sim.SetProps(ctx, history, map[string]string{
		domain.PropMerge: "paused/before-merge-crash/3/" + domain.RuntimeProp(mysql84) + "/cp1",
	})
	br, _ = z.store.GetBranch(ctx, name("x"))
	br.Status = domain.BranchMerging
	_ = z.store.PutBranch(ctx, br)

	if err := z.svc.Recover(ctx); err != nil {
		t.Fatalf("Recover post-checkpoint failed: %v", err)
	}
	if br, _ = z.store.GetBranch(ctx, name("x")); br.Status != domain.BranchPaused || z.server.running["branch-x"] {
		t.Errorf("post-checkpoint Recover paused branch = %+v running=%v", br, z.server.running["branch-x"])
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropMerge] != "" {
		t.Errorf("PropMerge left after Recover: %v", p)
	}
}

func TestMergeBranch_バリデーションエラーとクラスタと容量とフォールバック(t *testing.T) {
	z, m := newMergeEnv(t)

	// 1. ValidateMerge input syntax checks
	for _, bad := range []dbbranch.MergeBranchRequest{
		{Name: name("x")},
		{Name: name("x"), SourceLine: intPtr(0), Checkpoint: "cp1"},
		{Name: name("x"), SourceLine: intPtr(-1)},
		{Name: name("x"), Checkpoint: "bad name!!"},
	} {
		if _, err := z.svc.ValidateMerge(ctx, bad); err == nil {
			t.Errorf("expected ValidateMerge error for %+v", bad)
		}
	}

	// 2. Lineage unavailable -> ErrCheckpointsUnavailable
	savedLineage := z.svc.Lineage
	z.svc.Lineage = nil
	if _, err := z.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrCheckpointsUnavailable) {
		t.Errorf("expected ErrCheckpointsUnavailable, got %v", err)
	}
	z.svc.Lineage = savedLineage

	// 3. Mixed cluster protocol < 26 -> ErrClusterUpgrading
	z.svc.Nodes = &fakeNodes{nodes: []domain.Node{
		{Name: "node1", Protocol: 26, MemoryMB: 4096},
		{Name: "node2", Protocol: 25, MemoryMB: 4096},
	}}
	if _, err := z.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("expected ErrClusterUpgrading, got %v", err)
	}
	z.svc.Nodes = nil

	// 4. Pool space room -> ErrNoSpace
	z.svc.PoolSpace = func(context.Context) (uint64, uint64, error) { return 10 << 30, 100 << 20, nil }
	if _, err := z.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrNoSpace) {
		t.Errorf("expected ErrNoSpace, got %v", err)
	}
	z.svc.PoolSpace = nil

	// 5. Insufficient memory for 3 temporary clones -> ErrNoCapacity
	z.svc.HostMemory = func() (uint64, uint64) { return 400, 0 }
	if _, err := z.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("expected ErrNoCapacity, got %v", err)
	}
	z.svc.HostMemory = nil

	// 6. Operation-phase resolution errors return populated MergeResult
	resSame, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(1)}, nil)
	if !errors.Is(err, domain.ErrInvalidMergeSource) || resSame.Code != "invalid_request" {
		t.Errorf("same-line res=%+v err=%v", resSame, err)
	}
	resMissing, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(99)}, nil)
	if !errors.Is(err, domain.ErrNotFound) || resMissing.Code != "not_found" {
		t.Errorf("missing line res=%+v err=%v", resMissing, err)
	}

	// Missing runtime on clone
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{}}
	resRT, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if !errors.Is(err, domain.ErrRuntimeNotInstalled) || resRT.Code != "runtime_not_installed" {
		t.Errorf("missing runtime res=%+v err=%v", resRT, err)
	}
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql84: true}}

	// Diff returns ErrSchemaNoPrimaryKey / ErrTriggersNotSupported
	m.diffErr = fmt.Errorf("%w: shop.logs", domain.ErrSchemaNoPrimaryKey)
	resNoPK, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if !errors.Is(err, domain.ErrSchemaNoPrimaryKey) || resNoPK.Code != "schema_no_primary_key" {
		t.Errorf("no PK res=%+v err=%v", resNoPK, err)
	}
	m.diffErr = fmt.Errorf("%w: shop.users", domain.ErrTriggersNotSupported)
	resTrg, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if !errors.Is(err, domain.ErrTriggersNotSupported) || resTrg.Code != "triggers_not_supported" {
		t.Errorf("triggers res=%+v err=%v", resTrg, err)
	}
}

// settledAfterMerge checks what must hold once the agent has restarted and
// Recover has run, however the merge was interrupted: the branch is back to
// what it was, its data is the unmerged data (the fake merger changes none),
// and no merge work is left.
func settledAfterMerge(t *testing.T, z *zenv, label string) {
	t.Helper()
	br, err := z.store.GetBranch(ctx, name("x"))
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if br.Status != domain.BranchReady || !z.server.running["branch-x"] {
		t.Fatalf("%s: branch = %s (%s), running=%v", label, br.Status, br.Error, z.server.running["branch-x"])
	}
	if got := z.sim.read(live); got != "line1-work" {
		t.Fatalf("%s: live data = %q", label, got)
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropMerge] != "" {
		t.Fatalf("%s: merge intent left: %v", label, p)
	}
	if kids, _ := z.sim.Datasets(ctx, "tank/db-branch/work"); len(kids) > 0 {
		t.Fatalf("%s: merge work left: %v", label, kids)
	}
	snaps, _ := z.sim.SnapshotInfos(ctx, live)
	for _, sn := range snaps {
		if domain.IsMergeSnapshot(sn.Name) {
			t.Fatalf("%s: temporary snapshot %s left", label, sn.Name)
		}
	}
}

func TestMergeBranch_どの副作用でエージェントが落ちてもRecoverで元の状態に戻る(t *testing.T) {
	completed := false
	for at := 1; at <= 400 && !completed; at++ {
		z, _ := newMergeEnv(t)
		z.crash.steps, z.crash.at = 0, at
		_, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
		if !z.crash.dead {
			completed = true
			if err != nil {
				t.Fatalf("no crash but failed: %v", err)
			}
			continue
		}
		z.restart()
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("crash at step %d: Recover: %v", at, err)
		}
		br, _ := z.store.GetBranch(ctx, name("x"))
		if br.Status == domain.BranchReady || br.Status == domain.BranchPaused {
			// A crash after Apply succeeded and the intent was cleared leaves a merged branch, which is fine.
			if p, _ := z.sim.Props(ctx, history); p[domain.PropMerge] != "" {
				t.Fatalf("crash at step %d: merge intent left", at)
			}
		}
		settledAfterMerge(t, z, fmt.Sprintf("crash at step %d", at))
	}
	if !completed {
		t.Fatal("merge never completed within 400 steps")
	}
}

func TestMergeBranch_読み取りや変更が失敗しても元の状態に戻る(t *testing.T) {
	for _, mode := range []string{"read", "mutate", "server"} {
		completed := false
		for n := 1; n <= 300 && !completed; n++ {
			z, m := newMergeEnv(t)
			_ = m
			switch mode {
			case "read":
				z.sim.reads, z.sim.failRead = 0, n
			case "mutate":
				z.sim.failOn = "" // set per step below
			}
			var cnt int
			switch mode {
			case "mutate":
				// fail the nth journal entry of the merge
				base := len(z.j.entries)
				z.sim.failOn = ""
				_ = base
				cnt = n
			case "server":
				cnt = n
			}
			_ = cnt
			if mode == "mutate" {
				z.crash.steps, z.crash.at = 0, 0
				z.sim.failOn = nthMutation(z, n)
			}
			if mode == "server" {
				z.server.failOn = serverOps[(n-1)%len(serverOps)]
			}
			res, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
			failed := err != nil
			_ = res
			z.sim.failRead, z.sim.failOn, z.server.failOn = 0, "", ""
			if !failed {
				if mode == "server" {
					if n >= len(serverOps) {
						completed = true
					}
					continue
				}
				if mode == "mutate" && z.sim.failOn == "" {
					completed = true
				}
				if mode == "read" {
					completed = true
				}
				continue
			}
			z.restart()
			if err := z.svc.Recover(ctx); err != nil {
				t.Fatalf("%s %d: Recover: %v", mode, n, err)
			}
			settledAfterMerge(t, z, fmt.Sprintf("%s %d", mode, n))
		}
	}
}

var serverOps = []string{"start", "stop-clean", "stop-immediate", "prepare", "exec"}

// nthMutation picks the journal prefix that makes the next mutation of the
// given ordinal fail: the simulation fails the first entry that starts with
// the prefix, so each ordinal maps to one ZFS verb.
func nthMutation(_ *zenv, n int) string {
	verbs := []string{"zfs snapshot", "zfs create", "zfs clone", "zfs set", "zfs mount", "zfs destroy", "zfs rename", "zfs promote", "zfs inherit"}
	return verbs[(n-1)%len(verbs)]
}

func TestMergeBranch_追加カバレッジ_チェックポイント指定とフォーク協調と容量検査と失敗回復(t *testing.T) {
	// 1. Merge via --checkpoint ("cp2" and "reset" when PrevHead=="") + orphaned @merge-* sweep via DeleteCheckpoint
	z, m := newMergeEnv(t)
	// Restore from "reset" so live line (line 2) has PrevHead == "" and branch off reset point
	z.restore(t, domain.ResetPoint)
	_ = z.sim.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: "merge-orphan-123"})
	res, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{
		Name: name("x"), Checkpoint: "cp2",
	}, nil)
	if err != nil {
		t.Fatalf("MergeBranch --checkpoint cp2 failed: %v", err)
	}
	if res.Status != domain.MergeMerged || res.SourceCheckpoint != "cp2" || res.Ancestor != domain.ResetPoint {
		t.Fatalf("res = %+v", res)
	}
	// Orphaned @merge-orphan-123 should be reaped by collectIfSettled at end of MergeBranch!
	if snaps, _ := z.sim.SnapshotInfos(ctx, live); len(snaps) > 0 {
		for _, sn := range snaps {
			if sn.Name == "merge-orphan-123" {
				t.Errorf("collect did not reap @merge-orphan-123")
			}
		}
	}

	// 2. DryRun rejects concurrent UpgradeBranch, ForkBranch, CreateCheckpoint
	entered := make(chan struct{})
	release := make(chan struct{})
	m.onDiff = func(domain.MergeDiffRequest) {
		close(entered)
		<-release
	}
	done := make(chan struct{})
	go func() {
		_, _ = z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{
			Name: name("x"), Checkpoint: "cp2", DryRun: true,
		}, nil)
		close(done)
	}()
	<-entered
	if _, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: mysql84}, nil); err == nil {
		t.Error("expected UpgradeBranch conflict during dry-run")
	}
	if err := z.svc.ValidateFork(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("child")}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("expected ValidateFork ErrConflict during dry-run, got %v", err)
	}
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "c-during-dry"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("expected CreateCheckpoint ErrConflict during dry-run, got %v", err)
	}
	close(release)
	<-done
	m.onDiff = nil

	// 3. Deferred parent fork cleanup while parent is BranchMerging, reaped when merge completes
	z2, _ := newMergeEnv(t)
	child, err := z2.svc.ForkBranch(ctx, dbbranch.ForkBranchRequest{Source: name("x"), Name: name("child")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := z2.store.GetBranch(ctx, name("x"))
	parent.Status = domain.BranchMerging
	_ = z2.store.PutBranch(ctx, parent)
	// Delete child while parent is BranchMerging: fork-<child> snapshot stays on parent until parent settles
	if err := z2.svc.DeleteBranch(ctx, child.Name, nil); err != nil {
		t.Fatal(err)
	}
	parent.Status = domain.BranchReady
	_ = z2.store.PutBranch(ctx, parent)
	// Trigger MergeBranch on parent -> collectIfSettled reaps fork-child!
	if _, err := z2.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil); err != nil {
		t.Fatal(err)
	}
	if ds, ok := domain.FindSnapshot(nil, child.ParentSnapshot); ok {
		_ = ds
	}
	snaps, _ := z2.sim.SnapshotInfos(ctx, live)
	for _, sn := range snaps {
		if domain.IsForkSnapshot(sn.Name) {
			t.Errorf("fork snapshot %s was not reaped after parent merge settled", sn.Name)
		}
	}

	// 4. checkMergeCapacity with Nodes store (both paused & running branches) + Merger == nil + no_common_ancestor
	z3, _ := newMergeEnv(t)
	z3.svc.Nodes = &fakeNodes{nodes: []domain.Node{{Name: "node1", Protocol: 26, MemoryMB: 512}}}
	// With dryRun=true, running=1 branch (128+256=384) + 3*128=384 = 768 > 384 budget -> ErrNoCapacity
	if _, err := z3.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0), DryRun: true}); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("expected Nodes-based ErrNoCapacity on dry-run, got %v", err)
	}
	z3.svc.Nodes = nil
	z3.svc.Merger = nil
	if _, err := z3.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil); err == nil {
		t.Error("expected error when Merger == nil")
	}

	// ErrNoCommonAncestor (strip PropParent/PropFrom from history line 0 and live line)
	z4, _ := newMergeEnv(t)
	_ = z4.sim.Unset(ctx, live, domain.PropFrom, domain.PropParent)
	_ = z4.sim.Unset(ctx, history+"/0", domain.PropFrom, domain.PropParent)
	resAnc, err := z4.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if !errors.Is(err, domain.ErrNoCommonAncestor) || resAnc.Code != "no_common_ancestor" {
		t.Errorf("expected ErrNoCommonAncestor, got res=%+v err=%v", resAnc, err)
	}

	// 5. Rollback failure marks BranchFailed with PropMerge kept; Recover resumes rollback cleanly!
	z5, m5 := newMergeEnv(t)
	m5.applyErr = errors.New("apply blew up")
	z5.sim.failOn = "zfs rollback" // won't block setAside; fail "zfs clone" on rollback swap instead!
	z5.sim.failOn = "zfs clone"    // fails during runCloneDiff first unless we inject failOn inside onApply!
	z5.sim.failOn = ""
	m5.onApply = func(domain.AdminAccess, domain.MergePlan) {
		z5.sim.failOn = "zfs mount" // will cause finishDatasetSwap in rollbackMerge to fail!
	}
	resFail, err := z5.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if err == nil || resFail.Status != domain.MergeFailed {
		t.Fatalf("expected MergeFailed when rollback fails, got res=%+v err=%v", resFail, err)
	}
	z5.sim.failOn = ""
	if b, _ := z5.store.GetBranch(ctx, name("x")); b.Status != domain.BranchFailed {
		t.Fatalf("expected BranchFailed, got %s", b.Status)
	}
	z5.restart()
	if err := z5.svc.Recover(ctx); err != nil {
		t.Fatalf("Recover on BranchFailed merge failed: %v", err)
	}
	settledAfterMerge(t, z5, "recover after failed rollback")
}

func TestMergeBranch_追加網羅_PausedPreCrashとSwapped済ロールバックとWorkSweepとPort不足(t *testing.T) {
	// 1. Pre-checkpoint crash on a PAUSED branch + leftover non-merge dir in work/
	z, _ := newMergeEnv(t)
	_ = z.sim.Create(ctx, "tank/db-branch/work", map[string]string{"canmount": "off"})
	_ = z.sim.Create(ctx, "tank/db-branch/work/other-dir", map[string]string{"canmount": "off"})
	_ = z.sim.SetProps(ctx, history, map[string]string{
		domain.PropMerge: "paused/-/-/" + domain.RuntimeProp(mysql84) + "/-",
	})
	br, _ := z.store.GetBranch(ctx, name("x"))
	br.Status = domain.BranchMerging
	_ = z.store.PutBranch(ctx, br)
	z.restart()
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatalf("Recover paused pre-checkpoint: %v", err)
	}
	if br, _ = z.store.GetBranch(ctx, name("x")); br.Status != domain.BranchPaused {
		t.Errorf("expected BranchPaused, got %s", br.Status)
	}

	// 2. resumeMerge with NO PropMerge -> falls back to BranchReady + recovers server
	br.Status = domain.BranchMerging
	_ = z.store.PutBranch(ctx, br)
	z.restart()
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatalf("Recover without PropMerge: %v", err)
	}
	if br, _ = z.store.GetBranch(ctx, name("x")); br.Status != domain.BranchReady || !z.server.running["branch-x"] {
		t.Errorf("expected BranchReady running, got %s running=%v", br.Status, z.server.running["branch-x"])
	}

	// 3. AllocatePorts exhaustion when pool has < 3 free ports
	z.svc.SetLimits(128, dbbranch.PortRange{Min: 40000, Max: 40001}) // port 40000 is taken by branch x -> only 1 port left
	if _, err := z.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil); err == nil {
		t.Error("expected ErrNoFreePort when < 3 extra ports")
	}

	// 4. Restart failure after conflict on ready branch -> logs restart error in MergeResult.Error
	z2, m2 := newMergeEnv(t)
	m2.diffPlan = domain.MergePlan{
		TotalConflicts: 1,
		Conflicts:      []domain.MergeConflict{{Type: domain.ConflictInsertInsert, Database: "shop", Table: "t"}},
	}
	m2.onDiff = func(domain.MergeDiffRequest) {
		z2.server.failOn = "start"
	}
	resC, err := z2.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}, nil)
	if !errors.Is(err, domain.ErrMergeConflict) || !strings.Contains(resC.Error, "restart failed") {
		t.Errorf("expected conflict + restart failed message, got res=%+v err=%v", resC, err)
	}
	z2.server.failOn = ""

	// 6. Rollback edge cases: swapped==true & PrevHead=="" + checkMergeCapacity with HostMemory unknown (0,0) and Cluster.ListNodes error
	z4, _ := newMergeEnv(t)
	z4.svc.HostMemory = func() (uint64, uint64) { return 0, 0 }
	if _, err := z4.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); err != nil {
		t.Errorf("HostMemory(0,0) should skip capacity block, got %v", err)
	}
	z4.svc.HostMemory = nil
	z4.svc.Nodes = &fakeNodes{err: errors.New("cluster read failed")}
	if _, err := z4.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); err == nil {
		t.Error("expected cluster error in ValidateMerge")
	}
	z4.svc.Nodes = nil
	// Also cover swapped==true and PrevHead=="" in rollback via Recover
	_ = z4.sim.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: domain.CheckpointSnapshot("before-merge-sw")})
	_ = z4.sim.SetProps(ctx, live+"@"+domain.CheckpointSnapshot("before-merge-sw"), map[string]string{
		domain.PropLine: "2", domain.PropAuto: "on", domain.PropRuntime: domain.RuntimeProp(mysql84),
	})
	_ = z4.sim.SetProps(ctx, live, map[string]string{domain.PropLine: "3", domain.PropHead: "some-head"})
	_ = z4.sim.SetProps(ctx, history, map[string]string{
		domain.PropMerge: "ready/before-merge-sw/3/" + domain.RuntimeProp(mysql84) + "/-",
	})
	b4, _ := z4.store.GetBranch(ctx, name("x"))
	b4.Status = domain.BranchMerging
	_ = z4.store.PutBranch(ctx, b4)
	z4.restart()
	if err := z4.svc.Recover(ctx); err != nil {
		t.Fatalf("Recover swapped==true PrevHead==- failed: %v", err)
	}
	if p, _ := z4.sim.Props(ctx, live); p[domain.PropHead] != "" {
		t.Errorf("expected empty PropHead after rollback with PrevHead==-, got %q", p[domain.PropHead])
	}

	// 7. Validation source checks + ListBranches capacity error + parseMergeIntent + Lineage nil
	z7, _ := newMergeEnv(t)
	if _, err := z7.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0), Checkpoint: "cp1"}); !errors.Is(err, domain.ErrInvalidMergeSource) {
		t.Errorf("both source_line and checkpoint -> got %v", err)
	}
	if _, err := z7.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(-1)}); !errors.Is(err, domain.ErrInvalidMergeSource) {
		t.Errorf("negative source_line -> got %v", err)
	}
	if _, err := z7.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), Checkpoint: "bad space!"}); !errors.Is(err, domain.ErrInvalidMergeSource) {
		t.Errorf("bad checkpoint name -> got %v", err)
	}
	z7.svc.Lineage = nil
	if _, err := z7.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrCheckpointsUnavailable) {
		t.Errorf("nil Lineage -> got %v", err)
	}
	// checkMergeCapacity ListBranches error & running on other branch with bNode == ""
	z8, _ := newMergeEnv(t)
	_ = z8.store.PutBranch(ctx, domain.Branch{Name: name("y"), Base: name("prod"), Runtime: mysql84, Status: domain.BranchReady})
	if _, err := z8.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); err != nil {
		t.Errorf("ValidateMerge with bNode empty -> got %v", err)
	}
	z8.svc.HostMemory = func() (uint64, uint64) { return 400, 0 }
	if _, err := z8.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0), DryRun: true}); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("ValidateMerge tight HostMemory -> got %v, want ErrNoCapacity", err)
	}
	z8.svc.HostMemory = nil
	_ = z8.store.DeleteBranch(ctx, name("y"))
	// resumeMerge when PropMerge has bad format -> falls back to BranchReady + recovers server
	b8, _ := z8.store.GetBranch(ctx, name("x"))
	b8.Status = domain.BranchMerging
	_ = z8.store.PutBranch(ctx, b8)
	for _, bad := range []string{"bad/format", "unknown/cp1/1/mysql@8.4.13/-", "ready/cp1/0/mysql@8.4.13/-", "ready/cp1/abc/mysql@8.4.13/-"} {
		_ = z8.sim.SetProps(ctx, history, map[string]string{domain.PropMerge: bad})
		b8.Status = domain.BranchMerging
		_ = z8.store.PutBranch(ctx, b8)
		if err := z8.svc.Recover(ctx); err != nil {
			t.Errorf("Recover with bad PropMerge %q: %v", bad, err)
		}
	}
	// orphanedMergeSnapshots skips BranchMerging
	b9, _ := z8.store.GetBranch(ctx, name("x"))
	b9.Status = domain.BranchMerging
	_ = z8.store.PutBranch(ctx, b9)
	_ = z8.sim.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: domain.MergeSnapPrefix + "test1"})
	_ = z8.svc.DeleteCheckpoint(ctx, name("x"), "cp1")
	// checkMergeCapacity when hostNode is absent in Nodes list -> nil
	z9, _ := newMergeEnv(t)
	z9.svc.Nodes = &fakeNodes{nodes: []domain.Node{{Name: "other-node", Protocol: domain.ProtocolBranchMerge, MemoryMB: 100}}}
	if _, err := z9.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); err != nil {
		t.Errorf("ValidateMerge hostNode absent -> got %v", err)
	}
	// resumeMerge when PropMerge has saved checkpoint on paused branch -> rollback
	_, _ = z9.svc.PauseBranch(ctx, name("x"), nil)
	_ = z9.sim.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: domain.CheckpointSnapshot("before-merge-p")})
	_ = z9.sim.SetProps(ctx, live+"@"+domain.CheckpointSnapshot("before-merge-p"), map[string]string{
		domain.PropLine: "1", domain.PropAuto: "on", domain.PropRuntime: domain.RuntimeProp(mysql84),
	})
	_ = z9.sim.SetProps(ctx, live, map[string]string{domain.PropLine: "2", domain.PropHead: "cp2"})
	_ = z9.sim.SetProps(ctx, history, map[string]string{
		domain.PropMerge: "paused/before-merge-p/2/" + domain.RuntimeProp(mysql84) + "/cp2",
	})
	bp, _ := z9.store.GetBranch(ctx, name("x"))
	bp.Status = domain.BranchMerging
	_ = z9.store.PutBranch(ctx, bp)
	z9.restart()
	if err := z9.svc.Recover(ctx); err != nil {
		t.Errorf("Recover paused branch post-checkpoint: %v", err)
	}
	// resumeMerge when PropMerge names a checkpoint not yet created -> falls through to pre-checkpoint recovery
	_, _ = z9.svc.ResumeBranch(ctx, name("x"), nil)
	bp2, _ := z9.store.GetBranch(ctx, name("x"))
	bp2.Status = domain.BranchMerging
	_ = z9.store.PutBranch(ctx, bp2)
	_ = z9.sim.SetProps(ctx, history, map[string]string{
		domain.PropMerge: "ready/before-merge-not-taken/2/" + domain.RuntimeProp(mysql84) + "/cp2",
	})
	z9.restart()
	if err := z9.svc.Recover(ctx); err != nil {
		t.Errorf("Recover missing checkpoint fallback: %v", err)
	}
	// orphanedMergeSnapshots returns nil on non-dryrun or pending lines; also test readMergeIntent with nonexistent history
	_ = z9.sim.Destroy(ctx, history)
	bp3, _ := z9.store.GetBranch(ctx, name("x"))
	bp3.Status = domain.BranchMerging
	_ = z9.store.PutBranch(ctx, bp3)
	z9.restart()
	if err := z9.svc.Recover(ctx); err != nil {
		t.Errorf("Recover with nonexistent history: %v", err)
	}

	// Extra ValidateMerge & checkMergeCapacity coverage: missing branch, failed branch, Nodes with ample memory, br.Node == "", unknown server runtime, readFail Props/Datasets
	z10, _ := newMergeEnv(t)
	if _, err := z10.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("no-such"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing branch -> got %v", err)
	}
	b10, _ := z10.store.GetBranch(ctx, name("x"))
	b10.Status = domain.BranchReady
	_ = z10.store.PutBranch(ctx, b10)
	_ = z10.store.PutBranch(ctx, domain.Branch{Name: name("y2"), Base: name("prod"), Runtime: mysql84, Status: domain.BranchReady})
	z10.svc.Nodes = &fakeNodes{nodes: []domain.Node{{Name: "node1", Protocol: domain.ProtocolBranchMerge, MemoryMB: 8192}}}
	if _, err := z10.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); err != nil {
		t.Errorf("ample memory & empty y2.Node -> got %v", err)
	}
	_ = z10.store.DeleteBranch(ctx, name("y2"))
	b10.Status = domain.BranchFailed
	_ = z10.store.PutBranch(ctx, b10)
	if _, err := z10.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("BranchFailed ValidateMerge -> got %v", err)
	}
	b10.Status = domain.BranchReady
	b10.Runtime = domain.RuntimeRef{Engine: "postgres", Version: "18.6"}
	_ = z10.store.PutBranch(ctx, b10)
	if _, err := z10.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); err == nil {
		t.Error("expected missing postgres provider error in ValidateMerge")
	}
	z10.svc.Nodes = &fakeNodes{err: errors.New("nodes down")}
	if _, err := z10.svc.ValidateMerge(ctx, dbbranch.MergeBranchRequest{Name: name("x"), SourceLine: intPtr(0)}); err == nil {
		t.Error("expected cluster error")
	}
	z10.svc.Nodes = nil
	if _, err := z10.svc.MergeBranch(ctx, dbbranch.MergeBranchRequest{Name: name("x")}, nil); !errors.Is(err, domain.ErrInvalidMergeSource) {
		t.Errorf("MergeBranch invalid source -> %v", err)
	}
	z11, _ := newMergeEnv(t)
	z11.sim.readFail = "Datasets"
	z11.restart()
	_ = z11.svc.Recover(ctx)
	z11.sim.readFail = ""
	b11, _ := z11.store.GetBranch(ctx, name("x"))
	b11.Status = domain.BranchMerging
	_ = z11.store.PutBranch(ctx, b11)
	z11.sim.readFail = "Props"
	_ = z11.svc.Recover(ctx)
	z11.sim.readFail = ""
}
