package dbbranch_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func createReq(n string, record bool) dbbranch.CreateBranchRequest {
	return dbbranch.CreateBranchRequest{Name: name(n), Base: name("prod"), Record: record}
}

// optioner is a module's ServerOptions: it adds lines for recorded branches.
func (z *zenv) options() {
	z.svc.ServerOptions = func(_ context.Context, br domain.Branch, maintenance bool) ([]string, error) {
		if !br.Record {
			return nil, nil
		}
		return []string{"log_bin=binlog"}, nil
	}
}

func lastStart(z *zenv) domain.Server { return z.server.started[len(z.server.started)-1] }

func TestCreateBranch_記録するブランチにはモジュールの追加設定でサーバーを起動する(t *testing.T) {
	z := newZEnv(t)
	z.options()
	z.withBase(t)

	if _, err := z.svc.CreateBranch(ctx, createReq("x", true), nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range z.server.started {
		if s.ID == "branch-x" && (len(s.Extra) != 1 || s.Extra[0] != "log_bin=binlog") {
			t.Errorf("start of %s without the extras: %+v", s.ID, s)
		}
	}
	if b, _ := z.store.GetBranch(ctx, name("x")); !b.Record {
		t.Error("the branch should remember that it records")
	}
	if _, err := z.svc.CreateBranch(ctx, createReq("y", false), nil); err != nil {
		t.Fatal(err)
	}
	if s := lastStart(z); s.ID != "branch-y" || len(s.Extra) != 0 {
		t.Errorf("a branch that does not record = %+v", s)
	}
}

func TestCreateBranch_追加設定を得られなければ作成を失敗させる(t *testing.T) {
	z := newZEnv(t)
	z.svc.ServerOptions = func(_ context.Context, _ domain.Branch, _ bool) ([]string, error) {
		return nil, errors.New("no licence")
	}
	z.withBase(t)

	_, err := z.svc.CreateBranch(ctx, createReq("x", true), nil)
	if err == nil || !strings.Contains(err.Error(), "no licence") {
		t.Fatalf("err = %v", err)
	}
}

func TestRecover_追加設定を得られないとき記録を頼んだブランチは起動せずそれ以外は起動する(t *testing.T) {
	z := newZEnv(t)
	z.options()
	z.withBase(t)
	for _, b := range []struct {
		n      string
		record bool
	}{{"rec", true}, {"plain", false}} {
		if _, err := z.svc.CreateBranch(ctx, createReq(b.n, b.record), nil); err != nil {
			t.Fatal(err)
		}
	}
	var logged []string
	z.svc.Logf = func(f string, a ...any) { logged = append(logged, f) }
	z.svc.ServerOptions = func(_ context.Context, _ domain.Branch, _ bool) ([]string, error) {
		return nil, errors.New("licence lapsed")
	}
	z.server.running = map[string]bool{}

	err := z.svc.Recover(ctx)
	if err == nil || !strings.Contains(err.Error(), "licence lapsed") {
		t.Errorf("a branch that records must not start without the module: %v", err)
	}
	if z.server.running["branch-rec"] || !z.server.running["branch-plain"] || len(logged) != 1 {
		t.Errorf("running=%v logged=%v", z.server.running, logged)
	}
}

func TestRestoreCheckpointWith_データを戻した直後に保守モードでモジュールの処理を走らせる(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	var calls []string
	z.svc.AfterRestore = func(_ context.Context, br domain.Branch, note string) error {
		calls = append(calls, note)
		s := lastStart(z)
		if !s.Maintenance || !z.server.running["branch-x"] || z.data() != "v1" {
			t.Errorf("hook saw maintenance=%v running=%v data=%q", s.Maintenance, z.server.running["branch-x"], z.data())
		}
		return nil
	}
	z.tick()
	var phases []string
	if _, err := z.svc.RestoreCheckpointWith(ctx, name("x"), "c1", `{"v":1,"line":0,"upto":"u:3"}`, func(p string) { phases = append(phases, p) }); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != `{"v":1,"line":0,"upto":"u:3"}` {
		t.Fatalf("calls = %v", calls)
	}
	if s := lastStart(z); s.Maintenance || !z.server.running["branch-x"] {
		t.Errorf("the branch should end up in normal mode: %+v", s)
	}
	if got := strings.Join(phases, " "); got != "cloning stopping checkpointing swapping promoting starting replaying" {
		t.Errorf("phases = %q", got)
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropNote] != "" {
		t.Errorf("the note is still recorded: %v", p)
	}
}

func TestRestoreCheckpoint_メモが無ければモジュールの処理は呼ばない(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.svc.AfterRestore = func(_ context.Context, _ domain.Branch, _ string) error { t.Error("called without a note"); return nil }
	z.restore(t, "c1")
}

func TestRestoreCheckpointWith_モジュールの処理が失敗しても復元は完了し記録に残す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	var logged []string
	z.svc.Logf = func(f string, a ...any) { logged = append(logged, f) }
	z.svc.AfterRestore = func(_ context.Context, _ domain.Branch, _ string) error { return errors.New("binlog is gone") }
	z.tick()
	br, err := z.svc.RestoreCheckpointWith(ctx, name("x"), "c1", "n", nil)
	if err != nil || br.Status != domain.BranchReady || z.data() != "v1" || len(logged) != 1 {
		t.Fatalf("br=%v err=%v data=%q logged=%v", br.Status, err, z.data(), logged)
	}
}

func TestRestoreCheckpointWith_クラッシュしても再起動後に保守モードで処理をやり直す(t *testing.T) {
	// Crash before every side effect in turn: the hook must run to the end
	// at least once and the branch must end up normal and ready.
	for at := 1; at < 200; at++ {
		z := newZEnv(t)
		z.history(t)
		calls := 0
		z.svc.AfterRestore = func(_ context.Context, _ domain.Branch, note string) error {
			calls++
			if note != "n" {
				t.Errorf("note = %q", note)
			}
			return nil
		}
		z.crash.steps, z.crash.at = 0, at
		z.tick()
		_, err := z.svc.RestoreCheckpointWith(ctx, name("x"), "c1", "n", nil)
		if err == nil {
			if at < 20 {
				t.Fatalf("at=%d: no crash happened", at)
			}
			if z.data() != "v1" || calls != 1 {
				t.Fatalf("an undisturbed restore: data=%q calls=%d", z.data(), calls)
			}
			return
		}
		z.restart()
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("at=%d: recover: %v", at, err)
		}
		b, _ := z.store.GetBranch(ctx, name("x"))
		switch {
		case z.data() == "v3":
			// Crashed before the swap: nothing changed, hook never needed.
		case z.data() == "v1" && calls >= 1 && b.Status == domain.BranchReady && !lastStart(z).Maintenance && z.server.running["branch-x"]:
		default:
			t.Fatalf("at=%d: data=%q calls=%d status=%s last=%+v", at, z.data(), calls, b.Status, lastStart(z))
		}
	}
}

func TestRestoreCheckpointWith_作成時の状態を復元しても今の状態を残しモジュールの処理を呼ぶ(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")
	var calls []string
	z.svc.AfterRestore = func(_ context.Context, _ domain.Branch, note string) error { calls = append(calls, note); return nil }
	z.tick()

	br, err := z.svc.RestoreCheckpointWith(ctx, name("x"), domain.ResetPoint, "n", nil)
	if err != nil || br.Status != domain.BranchReady {
		t.Fatalf("br=%v err=%v", br.Status, err)
	}
	if len(calls) != 1 || z.j.has("zfs rollback tank/db-branch/branches/x@reset") || z.data() != "" {
		t.Errorf("calls=%v data=%q journal=%v", calls, z.data(), z.j.entries)
	}
	auto := z.autoName(t)
	if h := z.sim.ds[history+"/0"]; h == nil {
		t.Fatal("the old line must be kept")
	}
	if g, _ := z.svc.CheckpointGraph(ctx, name("x")); g.Head != "" || g.Lines[len(g.Lines)-1].From != "reset" {
		t.Errorf("graph = %+v", g)
	}
	z.tick()
	if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), auto, nil); err != nil {
		t.Fatal(err)
	}
	if z.data() != "v1" {
		t.Errorf("data = %q, want the kept v1", z.data())
	}
}

func TestRestoreCheckpointWith_リセット点が無ければ断る(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.sim.ds[live].snaps = z.sim.ds[live].snaps[1:]
	if _, err := z.svc.RestoreCheckpointWith(ctx, name("x"), domain.ResetPoint, "n", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestRestoreCheckpointWith_作成時の状態への復元はクラッシュしても失われない(t *testing.T) {
	for at := 1; at < 200; at++ {
		z := newZEnv(t)
		z.history(t)
		calls := 0
		z.svc.AfterRestore = func(_ context.Context, _ domain.Branch, note string) error { calls++; return nil }
		z.crash.steps, z.crash.at = 0, at
		z.tick()
		if _, err := z.svc.RestoreCheckpointWith(ctx, name("x"), domain.ResetPoint, "n", nil); err == nil {
			if calls != 1 || z.data() != "" {
				t.Fatalf("an undisturbed restore: calls=%d data=%q", calls, z.data())
			}
			return
		}
		z.restart()
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("at=%d: recover: %v", at, err)
		}
		b, _ := z.store.GetBranch(ctx, name("x"))
		if b.Status != domain.BranchReady || !z.server.running["branch-x"] || lastStart(z).Maintenance {
			t.Fatalf("at=%d: status=%s running=%v", at, b.Status, z.server.running)
		}
		switch z.data() {
		case "v3": // crashed before the swap: nothing changed
		case "":
			if calls < 1 || len(z.names(t)) != 3 {
				t.Fatalf("at=%d: calls=%d checkpoints=%v", at, calls, z.names(t))
			}
		default:
			t.Fatalf("at=%d: data = %q", at, z.data())
		}
	}
}

func TestCheckpointGraph_線の親と元のチェックポイントを返す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	z.sim.write(live, "w")
	z.checkpoint(t, "d1")
	z.restore(t, "c2") // from line 0 again, while line 1 is set aside

	g, err := z.svc.CheckpointGraph(ctx, name("x"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Live != 2 || len(g.Lines) != 3 {
		t.Fatalf("graph = %+v", g)
	}
	l0, l1, l2 := g.Lines[0], g.Lines[1], g.Lines[2]
	if l0.Parent != nil || l0.From != "" || l0.Live {
		t.Errorf("line 0 = %+v", l0)
	}
	if l1.Parent == nil || *l1.Parent != 0 || l1.From != "c1" || l1.Live {
		t.Errorf("line 1 = %+v", l1)
	}
	if l2.Parent == nil || *l2.Parent != 0 || l2.From != "c2" || !l2.Live {
		t.Errorf("line 2 = %+v", l2)
	}
	if g.Head != "c2" {
		t.Errorf("head = %q", g.Head)
	}
}

func TestCheckpointGraph_リセットから作った線はリセットが元になる(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.tick()
	if _, err := z.svc.ResetBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	g, err := z.svc.CheckpointGraph(ctx, name("x"))
	if err != nil {
		t.Fatal(err)
	}
	if last := g.Lines[len(g.Lines)-1]; last.From != "reset" || last.Parent == nil || *last.Parent != 0 || !last.Live {
		t.Errorf("lines = %+v", g.Lines)
	}
}

func TestCheckpointGraph_保存先が使えない場合と他ノードのブランチは断る(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.CheckpointGraph(ctx, name("x")); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("no lineage: %v", err)
	}
	z := newZEnv(t)
	if _, err := z.svc.CheckpointGraph(ctx, name("nope")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

func TestRestoreCheckpointWith_メモは何でも書けるが大きすぎるものは断る(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.svc.AfterRestore = func(_ context.Context, _ domain.Branch, note string) error { got = note; return nil }
	z.tick()
	odd := "{\"x\": \"a b/c=d'e\\n\u3042\"}"
	if _, err := z.svc.RestoreCheckpointWith(ctx, name("x"), "c1", odd, nil); err != nil || got != odd {
		t.Fatalf("err=%v note=%q", err, got)
	}
	huge := strings.Repeat("x", 4097)
	if _, err := z.svc.RestoreCheckpointWith(ctx, name("x"), "c1", huge, nil); !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("restore: %v", err)
	}
}

var got string
