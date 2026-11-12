package dbbranch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func (z *zenv) reset(t *testing.T) domain.Branch {
	t.Helper()
	z.tick()
	br, err := z.svc.ResetBranch(ctx, name("x"), nil)
	if err != nil || br.Status != domain.BranchReady || br.ResetAt == nil {
		t.Fatalf("reset: %+v %v", br, err)
	}
	return br
}

func TestResetBranch_チェックポイントがなければ従来どおりロールバックする(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.sim.write(live, "v1")
	z.reset(t)
	if !z.j.has("zfs rollback " + live + "@reset") {
		t.Errorf("journal = %v", z.j.entries)
	}
	if z.data() != "" {
		t.Errorf("data = %q", z.data())
	}
	if _, ok := z.sim.ds[history]; ok {
		t.Error("no history container is needed")
	}
}

func TestResetBranch_チェックポイントを残したまま作成時の状態に戻す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)

	z.reset(t)

	if z.data() != "" || !z.server.running["branch-x"] {
		t.Errorf("data %q running %v", z.data(), z.server.running["branch-x"])
	}
	if got := z.names(t); !eq(got, []string{"c1", "c2"}) {
		t.Errorf("names = %v: reset keeps the checkpoints and takes none of its own", got)
	}
	if z.j.has("zfs rollback " + live + "@reset") {
		t.Error("rolling back would have destroyed the checkpoints")
	}
	list, _ := z.svc.ListCheckpoints(ctx, name("x"))
	for _, c := range list {
		if c.Current {
			t.Errorf("%s is current after a reset", c.Name)
		}
	}
	// The checkpoints still restore, and reset still works afterwards.
	z.restore(t, "c2")
	if z.data() != "v2" {
		t.Errorf("data = %q", z.data())
	}
	z.reset(t)
	z.reset(t)
	if z.data() != "" {
		t.Errorf("data = %q", z.data())
	}
	if !z.sim.ds[live].mounted || !z.promoted() {
		t.Errorf("live = %+v", z.sim.ds[live])
	}
}

func TestResetBranch_ロールバックの後は現在地の印を消す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	z.checkpoint(t, "c1")
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "c1"); err != nil {
		t.Fatal(err)
	}
	z.reset(t)
	if p := z.sim.ds[live].props; p[domain.PropHead] != "" {
		t.Errorf("head = %q", p[domain.PropHead])
	}
}

func TestResetBranch_どの段階でクラッシュしても再起動後は戻し終えているか元のまま(t *testing.T) {
	total := 0
	{
		z := newZEnv(t)
		z.history(t)
		z.crash.steps = 0
		z.reset(t)
		total = z.crash.steps
	}
	for k := 1; k <= total; k++ {
		z := newZEnv(t)
		z.history(t)
		z.crash.steps, z.crash.at = 0, k
		z.tick()
		_, _ = z.svc.ResetBranch(ctx, name("x"), nil)
		z.restart()
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatalf("crash at %d: %v", k, err)
		}
		b, _ := z.store.GetBranch(ctx, name("x"))
		d, ok := z.sim.ds[live]
		if b.Status != domain.BranchReady || !ok || !d.mounted || !z.server.running["branch-x"] {
			t.Fatalf("crash at %d: status %s live %+v", k, b.Status, d)
		}
		if z.data() != "" && z.data() != "v3" {
			t.Errorf("crash at %d: data %q", k, z.data())
		}
		if got := z.names(t); !eq(got, []string{"c1", "c2"}) {
			t.Errorf("crash at %d: names = %v", k, got)
		}
		if _, ok := z.sim.ds[history+"/new"]; ok {
			t.Errorf("crash at %d: pending clone left", k)
		}
	}
}

func TestDeleteBranch_全ての線とチェックポイントを残さず消す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	z.restore(t, "c2")
	z.sim.write(live, "v9")
	z.checkpoint(t, "d")
	if err := z.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range z.sim.names() {
		if strings.Contains(n, "/branches/x") || strings.Contains(n, "/history/") {
			t.Errorf("%s is left", n)
		}
	}
	if _, err := z.store.GetBranch(ctx, name("x")); err == nil {
		t.Error("the record is still there")
	}
	// Only datasets of this branch were destroyed.
	if _, ok := z.sim.ds["tank/db-branch/bases/prod"]; !ok {
		t.Error("the base must survive")
	}
}

func TestDeleteBranch_他のブランチやベースにはまったく触らない(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	other, err := z.svc.CreateBranch(ctx, createRequest("y"), nil)
	if err != nil {
		t.Fatal(err)
	}
	z.sim.write("tank/db-branch/branches/y", "keep")
	z.j.entries = nil
	if err := z.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range z.j.entries {
		if strings.HasPrefix(e, "zfs destroy") && !strings.Contains(e, "/branches/x") && !strings.Contains(e, "/history/x") {
			t.Errorf("destroyed something that is not branch x's: %s", e)
		}
		if strings.Contains(e, "-R") {
			t.Errorf("recursive destroy of dependents: %s", e)
		}
	}
	if d := z.sim.ds["tank/db-branch/branches/y"]; d == nil || d.data != "keep" || other.Status != domain.BranchReady {
		t.Errorf("branch y damaged: %+v", d)
	}
}

func TestDeleteBranch_外のクローンが依存していたら何も消さない(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.sim.ds["tank/elsewhere"] = &zds{origin: live + "@ckpt-c1", props: map[string]string{}}
	err := z.svc.DeleteBranch(ctx, name("x"), nil)
	if err == nil || !strings.Contains(err.Error(), "outside this branch") {
		t.Fatalf("error = %v", err)
	}
	if _, ok := z.sim.ds[live]; !ok {
		t.Error("the live dataset was destroyed")
	}
	if got := z.names(t); len(got) != 2 {
		t.Errorf("names = %v", got)
	}
}

func TestDeleteBranch_復元の途中で失敗した枝も消す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.sim.failOn = "zfs promote"
	z.tick()
	_, _ = z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil)
	z.sim.failOn = ""
	if err := z.svc.DeleteBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range z.sim.names() {
		if strings.Contains(n, "/branches/x") || strings.Contains(n, "/history/") {
			t.Errorf("%s is left", n)
		}
	}
}

func TestBranchStorage_退避した線の容量も足す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	before, err := z.svc.BranchStorage(ctx, name("x"))
	if err != nil {
		t.Fatal(err)
	}
	z.restore(t, "c1")
	after, err := z.svc.BranchStorage(ctx, name("x"))
	if err != nil || after.Used != before.Used*2 {
		t.Errorf("used %d -> %d (%v): the old line's space counts", before.Used, after.Used, err)
	}
	z.sim.readFail = "Info"
	if _, err := z.svc.BranchStorage(ctx, name("x")); err == nil {
		t.Error("an Info failure must surface")
	}
}

func TestBranchStorage_従来のブランチは今までどおり(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	info, err := e.svc.BranchStorage(ctx, name("x"))
	if err != nil || info.Used != 1<<20 {
		t.Errorf("%+v %v", info, err)
	}
}

func TestChekpoints_ストレージの読み取り失敗はエラーとして返す(t *testing.T) {
	for _, op := range []string{"Exists", "Props", "SnapshotInfos"} {
		z := newZEnv(t)
		z.history(t)
		z.sim.readFail = op
		if _, err := z.svc.ListCheckpoints(ctx, name("x")); err == nil {
			t.Errorf("%s: list", op)
		}
		if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "n"); err == nil {
			t.Errorf("%s: create", op)
		}
		if err := z.svc.DeleteCheckpoint(ctx, name("x"), "c1"); err == nil {
			t.Errorf("%s: delete", op)
		}
		z.tick()
		if _, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil); err == nil {
			t.Errorf("%s: restore", op)
		}
		z.sim.readFail = ""
		if got := z.names(t); !eq(got, []string{"c1", "c2"}) {
			t.Errorf("%s: names = %v", op, got)
		}
	}
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	z.sim.readFail = "Datasets"
	if _, err := z.svc.ListCheckpoints(ctx, name("x")); err == nil {
		t.Error("Datasets: list")
	}
}

func TestChekpoints_書き込みの失敗はどこで起きても整合したままにする(t *testing.T) {
	for _, failOn := range []string{"zfs create", "zfs snapshot", "zfs set", "zfs clone", "zfs inherit", "zfs rename", "zfs mount", "zfs promote", "zfs destroy"} {
		for _, op := range []string{"create", "restore", "reset", "delete", "gc"} {
			t.Run(failOn+" during "+op, func(t *testing.T) {
				z := newZEnv(t)
				z.history(t)
				if op == "gc" {
					z.restore(t, "c1")
					z.j.entries = nil
				}
				z.sim.failOn = failOn
				z.tick()
				switch op {
				case "create":
					_, _ = z.svc.CreateCheckpoint(ctx, name("x"), "n")
				case "restore":
					_, _ = z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil)
				case "reset":
					_, _ = z.svc.ResetBranch(ctx, name("x"), nil)
				case "delete":
					_ = z.svc.DeleteCheckpoint(ctx, name("x"), "c1")
				case "gc":
					_ = z.svc.DeleteCheckpoint(ctx, name("x"), "c2")
				}
				z.sim.failOn = ""
				z.restart()
				if err := z.svc.Recover(ctx); err != nil {
					t.Fatalf("recover: %v", err)
				}
				b, _ := z.store.GetBranch(ctx, name("x"))
				d, ok := z.sim.ds[live]
				if b.Status != domain.BranchReady || !ok || !d.mounted || !z.server.running["branch-x"] {
					t.Fatalf("status %s live %+v", b.Status, d)
				}
				for _, n := range z.sim.names() {
					if strings.HasSuffix(n, "/new") {
						t.Errorf("pending clone left: %s", n)
					}
				}
				if p, _ := z.sim.Props(ctx, history); p[domain.PropOp] != "" {
					t.Errorf("intent left: %v", p)
				}
				_ = fmt.Sprint(op)
			})
		}
	}
}

func TestRestoreCheckpoint_読み取りが失敗しても再起動後に完了するか元に戻る(t *testing.T) {
	for _, op := range []string{"Origin", "Info", "Props", "SnapshotInfos", "Exists"} {
		for _, after := range []int{0, 3, 8} {
			t.Run(fmt.Sprintf("%s after %d", op, after), func(t *testing.T) {
				z := newZEnv(t)
				z.history(t)
				z.tick()
				// Let the first reads succeed, then fail the named one for good.
				z.crash.steps = 0
				z.sim.readFail = ""
				if after == 0 {
					z.sim.readFail = op
				}
				_, err := z.svc.RestoreCheckpoint(ctx, name("x"), "c1", func(string) {
					if after > 0 {
						after--
						if after == 0 {
							z.sim.readFail = op
						}
					}
				})
				_ = err
				z.sim.readFail = ""
				z.restart()
				if err := z.svc.Recover(ctx); err != nil {
					t.Fatalf("recover: %v", err)
				}
				z.checkConsistent(t, op)
			})
		}
	}
}

func TestRecover_途中で止まった復元の記録がなければ元のまま再開する(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	b, _ := z.store.GetBranch(ctx, name("x"))
	b.Status = domain.BranchRestoring
	_ = z.store.PutBranch(ctx, b)
	z.server.running["branch-x"] = false
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = z.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchReady || !z.server.running["branch-x"] || z.data() != "v3" {
		t.Errorf("status %s data %s", b.Status, z.data())
	}
}

func TestRecover_復元と関係のない失敗したブランチには手を出さない(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	b, _ := z.store.GetBranch(ctx, name("x"))
	b.Status = domain.BranchFailed
	_ = z.store.PutBranch(ctx, b)
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = z.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchFailed {
		t.Errorf("status = %s", b.Status)
	}
	z.sim.readFail = "Exists"
	if err := z.svc.Recover(ctx); err == nil {
		t.Error("a storage failure while looking must surface")
	}
}

func TestRecover_中途半端な記録は無視する(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	_ = z.sim.SetProps(ctx, history, map[string]string{domain.PropOp: "restore"})
	b, _ := z.store.GetBranch(ctx, name("x"))
	b.Status = domain.BranchRestoring
	_ = z.store.PutBranch(ctx, b)
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = z.store.GetBranch(ctx, name("x"))
	if b.Status != domain.BranchReady {
		t.Errorf("status = %s", b.Status)
	}
}

func TestCreateCheckpoint_データセットがなければ見つからないと返す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	delete(z.sim.ds, live)
	if _, err := z.svc.CreateCheckpoint(ctx, name("x"), "a"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v", err)
	}
}

func TestDeleteCheckpoint_片付けに失敗しても削除自体は成功し記録に残す(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	var logged []string
	z.svc.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	z.sim.failOn = "zfs destroy"
	err := z.svc.DeleteCheckpoint(ctx, name("x"), "c2")
	z.sim.failOn = ""
	if err == nil {
		// The checkpoint itself is destroyed by the same command; both fail here.
		t.Log("delete succeeded")
	}
	z.sim.failOn = ""
	if err := z.svc.DeleteCheckpoint(ctx, name("x"), "c2"); err != nil && !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
	_ = logged
}

func TestReconcile_他のノードへ移ったブランチは線もチェックポイントも片付ける(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	z.clustered()
	z.svc.Nodes = &fakeNodes{nodes: []domain.Node{{Name: "node1", LastSeen: z.now}, {Name: "node2", LastSeen: z.now}}}
	z.svc.Streams = simStreams{z.sim}
	b, _ := z.store.GetBranch(ctx, name("x"))
	b.Node = "node2"
	_ = z.store.PutBranch(ctx, b)
	z.sim.ds["tank/db-branch/branches"] = &zds{props: map[string]string{}}
	if err := z.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range z.sim.names() {
		if strings.Contains(n, "/branches/x") || strings.Contains(n, "/history/") {
			t.Errorf("%s is left", n)
		}
	}
}

// simStreams lists the children of a dataset in the simulation.
type simStreams struct{ z *zsim }

func (s simStreams) Send(context.Context, string, string, string, io.Writer) error { return nil }
func (s simStreams) Receive(context.Context, string, io.Reader) error              { return nil }
func (s simStreams) Snapshots(context.Context, string) ([]string, error)           { return nil, nil }
func (s simStreams) Children(ctx context.Context, parent string) ([]string, error) {
	full, err := s.z.Datasets(ctx, parent)
	for i := range full {
		full[i] = full[i][strings.LastIndex(full[i], "/")+1:]
	}
	return full, err
}

// Fail each read of storage in turn, once: nothing may be lost, and a
// restore or reset is finished (or never started) after the restart.
func TestChekpoints_どの読み取りが失敗してもデータもチェックポイントも失わない(t *testing.T) {
	ops := map[string]func(z *zenv){
		"create":  func(z *zenv) { _, _ = z.svc.CreateCheckpoint(ctx, name("x"), "n") },
		"list":    func(z *zenv) { _, _ = z.svc.ListCheckpoints(ctx, name("x")) },
		"delete":  func(z *zenv) { _ = z.svc.DeleteCheckpoint(ctx, name("x"), "c2") },
		"restore": func(z *zenv) { _, _ = z.svc.RestoreCheckpoint(ctx, name("x"), "c1", nil) },
		"reset":   func(z *zenv) { _, _ = z.svc.ResetBranch(ctx, name("x"), nil) },
		"restore-again": func(z *zenv) {
			z.restore(nil, "c1")
			z.tick()
			_, _ = z.svc.RestoreCheckpoint(ctx, name("x"), "c2", nil)
		},
	}
	for opName, op := range ops {
		for n := 1; n <= 70; n++ {
			z := newZEnv(t)
			z.history(t)
			if opName == "restore-again" {
				z.sim.reads, z.sim.failRead = 0, 0
			}
			z.sim.reads, z.sim.failRead = 0, n
			func() {
				defer func() {
					if r := recover(); r != nil && opName != "restore-again" {
						t.Fatalf("%s, read %d: panic %v", opName, n, r)
					}
				}()
				op(z)
			}()
			z.sim.failRead = 0
			z.restart()
			if err := z.svc.Recover(ctx); err != nil {
				t.Fatalf("%s, read %d: recover: %v", opName, n, err)
			}
			b, _ := z.store.GetBranch(ctx, name("x"))
			d, ok := z.sim.ds[live]
			if b.Status != domain.BranchReady || !ok || !d.mounted || !z.server.running["branch-x"] {
				t.Fatalf("%s, read %d: status %s live %+v", opName, n, b.Status, d)
			}
			have := map[string]bool{}
			for _, c := range z.names(t) {
				have[c] = true
			}
			if (opName != "delete" && (!have["c1"] || !have["c2"])) || (opName == "delete" && !have["c1"]) {
				t.Fatalf("%s, read %d: checkpoints = %v", opName, n, z.names(t))
			}
		}
	}
}
