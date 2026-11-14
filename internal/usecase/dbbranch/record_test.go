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
	if got := strings.Join(phases, " "); got != "cloning stopping swapping promoting starting replaying" {
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
			if at < 10 {
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
	if h := z.sim.ds[history+"/0"]; h != nil {
		t.Error("the old line held no checkpoint, so nothing keeps it: the restore saves nothing by itself")
	}
	if g, _ := z.svc.CheckpointGraph(ctx, name("x")); g.Head != "" || g.Lines[len(g.Lines)-1].From != "reset" {
		t.Errorf("graph = %+v", g)
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
			if calls < 1 || len(z.names(t)) != 2 {
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

func extras(z *zenv, id string) []string {
	var out []string
	for _, s := range z.server.started {
		if s.ID == id {
			out = s.Extra
		}
	}
	return out
}

func TestCreateBranch_全ブランチ記録の設定は作成の瞬間に一度だけ効き後から変えても既存のブランチは変わらない(t *testing.T) {
	z := newZEnv(t)
	z.options()
	z.withBase(t)
	asked := 0
	all := true
	z.svc.RecordNew = func(_ context.Context, br domain.Branch) bool { asked++; return all }

	if _, err := z.svc.CreateBranch(ctx, createReq("a", false), nil); err != nil {
		t.Fatal(err)
	}
	all = false
	if _, err := z.svc.CreateBranch(ctx, createReq("b", false), nil); err != nil {
		t.Fatal(err)
	}
	all = true
	if _, err := z.svc.CreateBranch(ctx, createReq("c", true), nil); err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Errorf("asked %d times: a branch that asked to record needs no answer", asked)
	}
	rec := func(n string) bool { b, _ := z.store.GetBranch(ctx, name(n)); return b.Record }
	if !rec("a") || rec("b") || !rec("c") {
		t.Fatalf("a=%v b=%v c=%v", rec("a"), rec("b"), rec("c"))
	}

	// The setting is now the opposite of what each branch got; a restart
	// starts every branch the way it was created.
	all = false
	z.restart()
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(extras(z, "branch-a")) == 0 || len(extras(z, "branch-b")) != 0 {
		t.Errorf("a=%v b=%v", extras(z, "branch-a"), extras(z, "branch-b"))
	}
	all = true
	z.restart()
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(extras(z, "branch-b")) != 0 || rec("b") {
		t.Errorf("b became recorded: %v", extras(z, "branch-b"))
	}
	if asked != 2 {
		t.Errorf("the setting was asked again after creation: %d", asked)
	}
}

func TestRecover_記録するブランチはモジュールが無ければ失敗として理由を示しモジュールが戻れば再開する(t *testing.T) {
	z := newZEnv(t)
	z.options()
	z.withBase(t)
	for _, n := range []string{"rec", "plain"} {
		if _, err := z.svc.CreateBranch(ctx, createReq(n, n == "rec"), nil); err != nil {
			t.Fatal(err)
		}
	}
	z.svc.ServerOptions = nil // the module is not installed or not loaded
	z.restart()
	err := z.svc.Recover(ctx)
	if !errors.Is(err, dbbranch.ErrRecordRequired) {
		t.Fatalf("err = %v", err)
	}
	b, _ := z.store.GetBranch(ctx, name("rec"))
	if b.Status != domain.BranchFailed || !strings.Contains(b.Error, "history module") || z.server.running["branch-rec"] || !z.server.running["branch-plain"] {
		t.Errorf("status=%s reason=%q running=%v", b.Status, b.Error, z.server.running)
	}

	// Still missing at the next start: stays failed, with the reason.
	z.restart()
	if err := z.svc.Recover(ctx); !errors.Is(err, dbbranch.ErrRecordRequired) {
		t.Fatalf("second start: %v", err)
	}
	if b, _ := z.store.GetBranch(ctx, name("rec")); b.Status != domain.BranchFailed {
		t.Errorf("status = %s", b.Status)
	}

	// The module is back: the branch starts, recorded, and is ready again.
	z.options()
	z.restart()
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = z.store.GetBranch(ctx, name("rec"))
	if b.Status != domain.BranchReady || b.Error != "" || !z.server.running["branch-rec"] || len(extras(z, "branch-rec")) == 0 {
		t.Errorf("status=%s reason=%q running=%v", b.Status, b.Error, z.server.running)
	}
}

func TestRecover_記録するブランチはライセンスが無ければ起動せずそれ以外の失敗は再試行しない(t *testing.T) {
	z := newZEnv(t)
	z.options()
	z.withBase(t)
	if _, err := z.svc.CreateBranch(ctx, createReq("rec", true), nil); err != nil {
		t.Fatal(err)
	}
	b, _ := z.store.GetBranch(ctx, name("rec"))
	b.Status, b.Error = domain.BranchFailed, "disk error"
	_ = z.store.PutBranch(ctx, b)
	z.restart()
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := z.store.GetBranch(ctx, name("rec")); b.Status != domain.BranchFailed || z.server.running["branch-rec"] {
		t.Errorf("an unrelated failure was retried: %s running=%v", b.Status, z.server.running)
	}
}

func TestResetBranch_記録するブランチはモジュールが拒むとリセットも起動しない(t *testing.T) {
	z := newZEnv(t)
	z.options()
	z.withBase(t)
	if _, err := z.svc.CreateBranch(ctx, createReq("rec", true), nil); err != nil {
		t.Fatal(err)
	}
	z.svc.ServerOptions = func(context.Context, domain.Branch, bool) ([]string, error) {
		return nil, errors.New("licence lapsed")
	}
	_, err := z.svc.ResetBranch(ctx, name("rec"), nil)
	if !errors.Is(err, dbbranch.ErrRecordRequired) || !strings.Contains(err.Error(), "licence lapsed") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := z.store.GetBranch(ctx, name("rec")); b.Status != domain.BranchFailed || !b.Record {
		t.Errorf("status=%s record=%v", b.Status, b.Record)
	}
}

func TestRestoreCheckpoint_記録するブランチはモジュールが拒むと復元後も起動しない(t *testing.T) {
	z := newZEnv(t)
	z.options()
	z.withBase(t)
	if _, err := z.svc.CreateBranch(ctx, createReq("x", true), nil); err != nil {
		t.Fatal(err)
	}
	z.sim.write(live, "v0")
	z.tick()
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "c1"); err != nil {
		t.Fatal(err)
	}
	z.svc.ServerOptions = func(context.Context, domain.Branch, bool) ([]string, error) {
		return nil, errors.New("licence lapsed")
	}
	z.tick()
	_, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil)
	if !errors.Is(err, dbbranch.ErrRecordRequired) {
		t.Fatalf("err = %v", err)
	}
	if z.server.running["branch-x"] {
		t.Error("the server must not run unrecorded")
	}
}
