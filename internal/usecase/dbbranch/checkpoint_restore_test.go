package dbbranch_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

// history builds: v1 -> c1, v2 -> c2, v3 (not checkpointed).
func (z *zenv) history(t *testing.T) {
	t.Helper()
	z.branch(t)
	z.sim.write(live, "v1")
	z.checkpoint(t, "c1")
	z.sim.write(live, "v2")
	z.checkpoint(t, "c2")
	z.sim.write(live, "v3")
	z.j.entries = nil
}

func (z *zenv) restore(t *testing.T, cp string) {
	if t != nil {
		t.Helper()
	}
	z.tick()
	br, err := z.svc.RestoreCheckpoint(ctx, name("x"), cp, nil)
	if (err != nil || br.Status != domain.BranchReady) && t != nil {
		t.Fatalf("restore %s: %+v %v", cp, br.Status, err)
	}
}

func (z *zenv) data() string { return z.sim.ds[live].data }

func (z *zenv) autoName(t *testing.T) string {
	t.Helper()
	for _, n := range z.names(t) {
		if strings.HasPrefix(n, "before-restore-") {
			return n
		}
	}
	t.Fatalf("no automatic checkpoint in %v", z.names(t))
	return ""
}

func TestRestoreCheckpoint_選んだ時点に戻し他のチェックポイントも残す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)

	z.restore(t, "c1")

	if z.data() != "v1" {
		t.Errorf("data = %q, want v1", z.data())
	}
	got := z.names(t)
	if len(got) != 3 || got[0] != "c1" || got[1] != "c2" || !strings.HasPrefix(got[2], "before-restore-") {
		t.Fatalf("names = %v", got)
	}
	list, _ := z.svc.ListCheckpoints(ctx, name("x"))
	if !list[0].Current || list[1].Current || list[2].Current || !list[2].Auto || list[0].Auto {
		t.Errorf("marks = %+v", list)
	}
	if list[0].Line != 0 || list[1].Line != 0 || list[2].Line != 0 {
		t.Errorf("all three were taken on line 0: %+v", list)
	}
	if !z.server.running["branch-x"] {
		t.Error("server not running")
	}
	d := z.sim.ds[live]
	if !d.mounted || d.props["canmount"] != "on" || d.props[domain.PropLine] != "1" || !z.promoted() {
		t.Errorf("live = %+v", d)
	}
	if _, ok := z.sim.ds[history+"/new"]; ok {
		t.Error("the pending clone is still there")
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropOp] != "" {
		t.Errorf("the intent is still recorded: %v", p)
	}
	// The data as it was before the restore is kept.
	auto := z.autoName(t)
	if h := z.sim.ds[history+"/0"]; h == nil || h.origin != live+"@ckpt-c1" {
		t.Errorf("the old line = %+v", h)
	} else {
		for _, s := range h.snaps {
			if s.name == "ckpt-"+auto && s.data != "v3" {
				t.Errorf("auto checkpoint holds %q, want v3", s.data)
			}
		}
	}
}

func TestRestoreCheckpoint_どれを何度戻しても全部戻せる(t *testing.T) {
	z := newZEnv(t)
	z.history(t)

	z.restore(t, "c1")
	z.restore(t, "c2")
	if z.data() != "v2" {
		t.Fatalf("data = %q, want v2", z.data())
	}
	z.restore(t, "c1")
	if z.data() != "v1" {
		t.Fatalf("data = %q, want v1", z.data())
	}
	// The state before the first restore, saved automatically.
	first := ""
	for _, n := range z.names(t) {
		if strings.HasPrefix(n, "before-restore-") && first == "" {
			first = n
		}
	}
	z.restore(t, first)
	if z.data() != "v3" {
		t.Fatalf("data = %q, want v3", z.data())
	}
	for _, want := range []string{"c1", "c2", first} {
		found := false
		for _, n := range z.names(t) {
			found = found || n == want
		}
		if !found {
			t.Errorf("%s was lost: %v", want, z.names(t))
		}
	}
	if d := z.sim.ds[live]; d.props[domain.PropLine] != "4" || !z.promoted() {
		t.Errorf("live = %+v", d)
	}
}

func TestRestoreCheckpoint_復元後に作ったチェックポイントが現在地になる(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	z.sim.write(live, "v1b")
	z.checkpoint(t, "d1")
	list, _ := z.svc.ListCheckpoints(ctx, name("x"))
	for _, c := range list {
		if c.Current != (c.Name == "d1") {
			t.Errorf("%s current = %v", c.Name, c.Current)
		}
		if c.Name == "d1" && c.Line != 1 {
			t.Errorf("d1 is on line %d, want 1", c.Line)
		}
	}
	z.restore(t, "c2")
	if z.data() != "v2" {
		t.Errorf("data = %q", z.data())
	}
}

func TestRestoreCheckpoint_進捗を報告する(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	var phases []string
	z.tick()
	if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", func(p string) { phases = append(phases, p) }); err != nil {
		t.Fatal(err)
	}
	want := "cloning stopping checkpointing swapping promoting starting"
	if got := strings.Join(phases, " "); got != want {
		t.Errorf("phases = %q, want %q", got, want)
	}
}

func TestRestoreCheckpoint_受け付けない場合を断る(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), "nope", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown checkpoint: %v", err)
	}
	if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), "Bad Name", nil); !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("invalid name: %v", err)
	}
	if _, err := z.svc.RestoreCheckpoint(ctx, name("nope"), "c1", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown branch: %v", err)
	}
	b, _ := z.store.GetBranch(ctx, name("x"))
	b.Status = domain.BranchResetting
	_ = z.store.PutBranch(ctx, b)
	if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("busy branch: %v", err)
	}
}

func TestRestoreCheckpoint_自動の一件だけ上限を超えられる(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.max = 2
	z.restore(t, "c1") // 2 checkpoints + the automatic one
	if got := len(z.names(t)); got != 3 {
		t.Fatalf("names = %v", z.names(t))
	}
	z.tick()
	_, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c2", nil)
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "limit 2") {
		t.Fatalf("error = %v, want a conflict naming the limit", err)
	}
	b, _ := z.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchReady {
		t.Errorf("status = %s", b.Status)
	}
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), z.autoName(t)); err != nil {
		t.Fatal(err)
	}
	z.restore(t, "c2")
}

func TestDeleteCheckpoint_復元元の線が依存するものは見えなくして線ごと片付ける(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	auto := z.autoName(t)

	// c1 is what the old line was cloned from: it cannot be destroyed yet.
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "c1"); err != nil {
		t.Fatal(err)
	}
	if got := z.names(t); len(got) != 2 {
		t.Fatalf("names = %v", got)
	}
	gone := false
	for _, s := range z.sim.ds[live].snaps {
		gone = gone || strings.HasPrefix(s.name, domain.GonePrefix)
	}
	if !gone {
		t.Error("the deleted checkpoint should wait under another name")
	}
	if z.sim.ds[history+"/0"] == nil {
		t.Fatal("the old line is still needed for c2 and the automatic checkpoint")
	}

	for _, n := range []string{"c2", auto} {
		if err := z.svc.DeleteCheckpoint(ctx, name("x"), n); err != nil {
			t.Fatal(err)
		}
	}
	// No checkpoint left on the old line: it and the waiting c1 are destroyed.
	if _, ok := z.sim.ds[history+"/0"]; ok {
		t.Error("the old line should be gone")
	}
	if _, ok := z.sim.ds[history]; ok {
		t.Error("the empty history container should be gone")
	}
	for _, s := range z.sim.ds[live].snaps {
		if strings.HasPrefix(s.name, domain.GonePrefix) {
			t.Errorf("%s was not destroyed", s.name)
		}
	}
	if z.data() != "v1" {
		t.Errorf("data = %q", z.data())
	}
}

func TestRestoreCheckpoint_空になった旧線は復元の最後に片付く(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")
	z.checkpoint(t, "c1")
	z.restore(t, "c1")
	// The automatic checkpoint is on the old line, so the line stays; delete it
	// and the line is garbage.
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), z.autoName(t)); err != nil {
		t.Fatal(err)
	}
	for _, n := range z.sim.names() {
		if strings.HasPrefix(n, history) {
			t.Errorf("%s is left", n)
		}
	}
}

func TestRestoreCheckpoint_スワップ前の失敗は何も変えずに戻す(t *testing.T) {
	for _, failOn := range []string{"zfs clone", "zfs snapshot", "zfs set canmount=off"} {
		t.Run(failOn, func(t *testing.T) {
			z := newZEnv(t)
			z.history(t)
			z.sim.failOn = failOn
			z.tick()
			_, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil)
			if err == nil {
				t.Fatal("want an error")
			}
			z.sim.failOn = ""
			b, _ := z.store.GetBranch(ctx, name("x"))
			if b.Status != domain.BranchReady || z.data() != "v3" || !z.server.running["branch-x"] {
				t.Errorf("status %s data %s running %v: %v", b.Status, z.data(), z.server.running["branch-x"], err)
			}
			if d := z.sim.ds[live]; !d.mounted || d.props["canmount"] == "off" {
				t.Errorf("live dataset left unmounted: %+v", d)
			}
			if got := z.names(t); !eq(got, []string{"c1", "c2"}) {
				t.Errorf("names = %v", got)
			}
			if _, ok := z.sim.ds[history+"/new"]; ok {
				t.Error("the pending clone is still there")
			}
			// And it works afterwards.
			z.restore(t, "c1")
			if z.data() != "v1" {
				t.Errorf("data = %q", z.data())
			}
		})
	}
}

func TestRestoreCheckpoint_スワップ後の失敗は再起動で完了する(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.sim.failOn = "zfs promote"
	z.tick()
	_, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	b, _ := z.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchFailed || !strings.Contains(b.Error, "restart") {
		t.Errorf("status %s: %q", b.Status, b.Error)
	}
	z.sim.failOn = ""
	z.restart()
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = z.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchReady || z.data() != "v1" || !z.promoted() {
		t.Errorf("status %s data %s origin %q", b.Status, z.data(), z.sim.ds[live].origin)
	}
}

// checkRestored asserts the branch is either exactly as before the restore
// (v3, line 0) or fully restored (v1, line 1), with every checkpoint still
// there and nothing half-done left behind.
func (z *zenv) checkConsistent(t *testing.T, label string) (restored bool) {
	t.Helper()
	b, err := z.store.GetBranch(ctx, name("x"))
	if err != nil || b.Status != domain.BranchReady {
		t.Fatalf("%s: status %v %v", label, b.Status, err)
	}
	d, ok := z.sim.ds[live]
	if !ok {
		t.Fatalf("%s: the data directory is missing: %v", label, z.sim.names())
	}
	if !d.mounted || d.props["canmount"] == "off" {
		t.Errorf("%s: live dataset not mounted: %+v", label, d)
	}
	if !z.server.running["branch-x"] {
		t.Errorf("%s: server not running", label)
	}
	switch {
	case z.data() == "v1" && d.props[domain.PropLine] == "1":
		restored = true
	case z.data() == "v3" && d.props[domain.PropLine] == "":
	default:
		t.Errorf("%s: data %q on line %q is neither the old nor the restored state", label, z.data(), d.props[domain.PropLine])
	}
	if _, ok := z.sim.ds[history+"/new"]; ok {
		t.Errorf("%s: pending clone left", label)
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropOp] != "" {
		t.Errorf("%s: intent left: %v", label, p)
	}
	have := map[string]bool{}
	for _, n := range z.names(t) {
		have[n] = true
	}
	if !have["c1"] || !have["c2"] {
		t.Errorf("%s: checkpoints = %v", label, z.names(t))
	}
	if restored && !z.promoted() {
		t.Errorf("%s: not promoted: %s", label, d.origin)
	}
	return restored
}

// Kill the agent before each side effect of a restore in turn; after the
// restart the branch is old or restored, never in between.
func TestRestoreCheckpoint_どの段階でクラッシュしても再起動後は旧状態か復元済み(t *testing.T) {
	total := 0
	{
		z := newZEnv(t)
		z.history(t)
		z.crash.steps = 0
		z.restore(t, "c1")
		total = z.crash.steps
	}
	if total < 10 {
		t.Fatalf("a restore makes only %d side effects?", total)
	}
	sawOld, sawRestored := false, false
	for k := 1; k <= total; k++ {
		z := newZEnv(t)
		z.history(t)
		z.crash.steps, z.crash.at = 0, k
		z.tick()
		_, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil)
		if err == nil {
			t.Fatalf("crash at %d: no error", k)
		}
		z.restart()
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("crash at %d: recover: %v", k, err)
		}
		if z.checkConsistent(t, fmt.Sprintf("crash at step %d", k)) {
			sawRestored = true
		} else {
			sawOld = true
		}
		// A second recovery changes nothing.
		z.restart()
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("crash at %d: second recover: %v", k, err)
		}
		z.checkConsistent(t, fmt.Sprintf("crash at step %d, recovered twice", k))
	}
	if !sawOld || !sawRestored {
		t.Errorf("expected crashes on both sides of the intent: old=%v restored=%v", sawOld, sawRestored)
	}
}

// A second crash while resuming is resumed as well.
func TestRestoreCheckpoint_復旧中に再びクラッシュしても次の再起動で完了する(t *testing.T) {
	for k := 3; k <= 30; k += 3 {
		for j := 1; j <= 12; j++ {
			z := newZEnv(t)
			z.history(t)
			z.crash.steps, z.crash.at = 0, k
			z.tick()
			_, _ = z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil)
			z.restart()
			z.crash.at = j
			_ = z.svc.Recover(ctx)
			z.restart()
			if err := z.svc.Recover(ctx); err != nil {
				t.Fatalf("crashes at %d and %d: %v", k, j, err)
			}
			z.checkConsistent(t, fmt.Sprintf("crashes at %d and %d", k, j))
		}
	}
}

// promoted: the live dataset owns the history, so it descends from the base only.
func (z *zenv) promoted() bool {
	o := z.sim.ds[live].origin
	return !strings.HasPrefix(o, history+"/") && strings.HasPrefix(o, "tank/db-branch/bases/")
}
