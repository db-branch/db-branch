package dbbranch_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

var mysql80 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}

// Upgrader behaviour of the fake server.
func (f *fakeServer) CheckUpgrade(_ context.Context, s domain.Server, to domain.RuntimeRef, own domain.Login) (domain.UpgradePlan, error) {
	f.j.add("check-upgrade %s %s %s", s.ID, s.Runtime, to)
	f.checkedFor = own
	return f.plan, f.checkErr
}

func (f *fakeServer) StartUpgraded(_ context.Context, s domain.Server, own domain.Login) (domain.UpgradeLog, error) {
	f.j.add("start-upgraded %s %s", s.ID, s.Runtime)
	f.upgraded = append(f.upgraded, s)
	f.started = append(f.started, s)
	f.checkedFor = own
	if f.upFail != nil {
		f.running[s.ID] = false
		return domain.UpgradeLog{Findings: f.upFindings, Excerpt: f.upExcerpt}, f.upFail
	}
	f.running[s.ID] = true
	return domain.UpgradeLog{Findings: f.upFindings, Excerpt: f.upExcerpt, NeedsStats: f.upNeedsStats}, nil
}

func (f *fakeServer) StartMajorUpgraded(ctx context.Context, s domain.Server, own domain.Login, up domain.UpgradeContext) (domain.UpgradeLog, error) {
	f.majorUpgrades = append(f.majorUpgrades, up)
	_, hasDeadline := ctx.Deadline()
	f.upDeadlines = append(f.upDeadlines, hasDeadline)
	return f.StartUpgraded(ctx, s, own)
}

func (f *fakeServer) UpdateMissingStats(ctx context.Context, s domain.Server) error {
	f.j.add("update-missing-stats %s %s", s.ID, s.Runtime)
	f.statsCalls = append(f.statsCalls, s)
	if f.statsHook != nil {
		f.statsHook(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.statsErr
}

func (s crashServer) StartUpgraded(ctx context.Context, sp domain.Server, own domain.Login) (domain.UpgradeLog, error) {
	if err := s.c.step(); err != nil {
		return domain.UpgradeLog{}, err
	}
	return s.fakeServer.StartUpgraded(ctx, sp, own)
}

func (s crashServer) StartMajorUpgraded(ctx context.Context, sp domain.Server, own domain.Login, up domain.UpgradeContext) (domain.UpgradeLog, error) {
	if err := s.c.step(); err != nil {
		return domain.UpgradeLog{}, err
	}
	return s.fakeServer.StartMajorUpgraded(ctx, sp, own, up)
}

// uenv: a base "prod" on MySQL 8.0 with branch "x", both runtimes installed.
func newUEnv(t *testing.T) *zenv {
	t.Helper()
	z := newZEnv(t)
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql80: true, mysql84: true}}
	if _, err := z.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql80, Dump: "/dumps/prod.sql.gz"}, nil); err != nil {
		t.Fatal(err)
	}
	z.withBranch(t, "x")
	z.sim.write(live, "v0")
	z.j.entries = nil
	return z
}

func (z *zenv) upgrade(t *testing.T, req dbbranch.UpgradeRequest) (domain.UpgradeResult, error) {
	t.Helper()
	z.tick()
	if req.Name == (domain.Name{}) {
		req.Name = name("x")
	}
	if req.To == (domain.RuntimeRef{}) {
		req.To = mysql84
	}
	return z.svc.UpgradeBranch(ctx, req, nil)
}

func (z *zenv) branchNow(t *testing.T) domain.Branch {
	t.Helper()
	b, err := z.store.GetBranch(ctx, name("x"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (z *zenv) autoNamed(t *testing.T, prefix string) string {
	t.Helper()
	for _, n := range z.names(t) {
		if strings.HasPrefix(n, prefix) {
			return n
		}
	}
	t.Fatalf("no checkpoint %s* in %v", prefix, z.names(t))
	return ""
}

func TestUpgradeBranch_停止してチェックポイントを残し新しいランタイムで起動する(t *testing.T) {
	z := newUEnv(t)
	z.server.upFindings = []domain.Finding{{Level: domain.LevelWarning, Code: domain.CodeLogWarning, Message: "MY-1: careful"}}
	z.server.plan = domain.UpgradePlan{LegacyAuth: true, Findings: []domain.Finding{{Level: domain.LevelWarning, Code: domain.CodeNativePassword, Message: "native"}}}

	res, err := z.upgrade(t, dbbranch.UpgradeRequest{PoolFree: 100 << 30})
	if err != nil {
		t.Fatal(err)
	}

	b := z.branchNow(t)
	if res.Status != domain.UpgradeUpgraded || res.From != mysql80 || res.To != mysql84 || b.Runtime != mysql84 || b.Status != domain.BranchReady || !b.LegacyAuth {
		t.Fatalf("result = %+v, branch = %+v", res, b)
	}
	if !strings.HasPrefix(res.Checkpoint, "before-upgrade-") || len(res.Log) != 1 || res.LogExcerpt != "" {
		t.Errorf("result = %+v", res)
	}
	var codes []string
	for _, f := range res.PreCheck {
		codes = append(codes, f.Code)
	}
	if strings.Join(codes, ",") != "upgrade_path,disk_space,native_password_users" {
		t.Errorf("pre-check = %v", codes)
	}
	stop, snap, start := z.j.indexOf("stop branch-x clean"), z.j.indexOf("zfs snapshot "+live+"@ckpt-"+res.Checkpoint), z.j.indexOf("start-upgraded branch-x mysql@8.4.13")
	if stop < 0 || snap < stop || start < snap {
		t.Errorf("order: stop %d, checkpoint %d, start %d: %v", stop, snap, start, z.j.entries)
	}
	if len(z.server.upgraded) != 1 || z.server.upgraded[0].Runtime != mysql84 || !z.server.upgraded[0].LegacyAuth || z.server.checkedFor.User != "x" {
		t.Errorf("started with %+v", z.server.upgraded)
	}
	list, _ := z.svc.ListCheckpoints(ctx, name("x"))
	last := list[len(list)-1]
	if last.Name != res.Checkpoint || !last.Auto || last.Runtime != "mysql@8.0.46" {
		t.Errorf("checkpoints = %+v", list)
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropUpgrade] != "" {
		t.Errorf("upgrade intent left: %v", p)
	}
}

func TestUpgradeBranch_自動チェックポイントは上限に数えず他と合わせて5つまで残す(t *testing.T) {
	z := newUEnv(t)
	z.max = 1
	z.checkpoint(t, "mine")
	for i := 0; i < 7; i++ {
		if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err != nil {
			t.Fatal(err)
		}
		z.restore(t, "mine") // back to 8.0, where the next upgrade starts again
	}
	autos := 0
	for _, n := range z.names(t) {
		if strings.HasPrefix(n, "before-") {
			autos++
		}
	}
	if autos != domain.MaxAutoCheckpoints {
		t.Errorf("%d automatic checkpoints in %v", autos, z.names(t))
	}
}

func TestUpgradeBranch_確認だけなら何も変えない(t *testing.T) {
	z := newUEnv(t)
	z.server.plan = domain.UpgradePlan{Findings: []domain.Finding{{Level: domain.LevelError, Code: domain.CodeConfigInvalid, Message: "bad"}}}

	res, err := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true})

	if err != nil || res.Status != domain.UpgradeChecked || !res.CheckOnly || !domain.HasErrors(res.PreCheck) {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	for _, e := range z.j.entries {
		if strings.HasPrefix(e, "stop ") || strings.HasPrefix(e, "zfs ") {
			t.Errorf("a check changed something: %v", z.j.entries)
		}
	}
	if b := z.branchNow(t); b.Runtime != mysql80 || b.Status != domain.BranchReady {
		t.Errorf("branch = %+v", b)
	}
}

func TestUpgradeBranch_確認だけならチェックポイントのない保管先でも使える(t *testing.T) {
	z := newUEnv(t)
	z.svc.Lineage = nil
	res, err := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true})
	if err != nil || res.Status != domain.UpgradeChecked {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); !errors.Is(err, domain.ErrCheckpointsUnavailable) {
		t.Errorf("upgrade without lineage: %v", err)
	}
}

func TestUpgradeBranch_事前確認のエラーは止め強制すれば進む(t *testing.T) {
	z := newUEnv(t)
	z.server.plan = domain.UpgradePlan{Findings: []domain.Finding{{Level: domain.LevelError, Code: domain.CodeNativePasswordGone, Message: "gone"}}}

	res, err := z.upgrade(t, dbbranch.UpgradeRequest{})
	if !errors.Is(err, domain.ErrUpgradeRefused) || res.Status != domain.UpgradeRefused || len(res.PreCheck) < 2 {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if b := z.branchNow(t); b.Runtime != mysql80 || b.Status != domain.BranchReady {
		t.Errorf("branch = %+v", b)
	}
	if z.j.has("stop branch-x clean") {
		t.Error("the server was stopped")
	}

	res, err = z.upgrade(t, dbbranch.UpgradeRequest{Force: true})
	if err != nil || res.Status != domain.UpgradeUpgraded || z.branchNow(t).Runtime != mysql84 {
		t.Errorf("forced: %+v %v", res, err)
	}
}

func TestUpgradeBranch_事前確認そのものが失敗したらブランチをそのままにする(t *testing.T) {
	z := newUEnv(t)
	z.server.checkErr = errors.New("cannot read accounts")
	_, err := z.upgrade(t, dbbranch.UpgradeRequest{})
	if err == nil || !strings.Contains(err.Error(), "cannot read accounts") {
		t.Fatalf("err = %v", err)
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady || b.Runtime != mysql80 {
		t.Errorf("branch = %+v", b)
	}
}

func TestUpgradeBranch_起動に失敗したら自動で戻りログの抜粋を残す(t *testing.T) {
	z := newUEnv(t)
	z.server.upFail = errors.New("server stopped while starting")
	z.server.upExcerpt = "[ERROR] [MY-013170] Upgrade failed"
	z.server.upFindings = []domain.Finding{{Level: domain.LevelError, Code: domain.CodeLogError, Message: "MY-013170"}}

	res, err := z.upgrade(t, dbbranch.UpgradeRequest{})

	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	b := z.branchNow(t)
	if res.Status != domain.UpgradeRolledBack || !strings.Contains(res.LogExcerpt, "Upgrade failed") || len(res.Log) != 1 || res.Error == "" {
		t.Errorf("result = %+v", res)
	}
	if b.Runtime != mysql80 || b.Status != domain.BranchReady || b.LegacyAuth || b.Error != "" {
		t.Errorf("branch = %+v", b)
	}
	last := z.server.started[len(z.server.started)-1]
	if last.Runtime != mysql80 || !z.server.running["branch-x"] {
		t.Errorf("last start = %+v", last)
	}
	if z.data() != "v0" || !z.promoted() {
		t.Errorf("data = %q", z.data())
	}
	_ = z.autoNamed(t, "before-upgrade-")
	_ = z.autoNamed(t, "before-restore-")
}

func TestUpgradeBranch_ロールバックしない指定なら失敗のまま残しチェックポイントから戻せる(t *testing.T) {
	z := newUEnv(t)
	z.server.upFail = errors.New("boom")

	res, err := z.upgrade(t, dbbranch.UpgradeRequest{NoRollback: true})

	b := z.branchNow(t)
	if err == nil || res.Status != domain.UpgradeFailed || b.Status != domain.BranchFailed || b.Runtime != mysql84 ||
		!strings.HasPrefix(b.Error, domain.UpgradeFailedPrefix) || !strings.Contains(b.Error, res.Checkpoint) {
		t.Fatalf("result = %+v, branch = %+v, err = %v", res, b, err)
	}
	if err := z.svc.Recover(ctx); err != nil || z.branchNow(t).Status != domain.BranchFailed {
		t.Errorf("a restart must keep it: %v %s", err, z.branchNow(t).Status)
	}
	z.server.upFail = nil
	z.tick()
	got, err := z.svc.RestoreCheckpoint(ctx, name("x"), res.Checkpoint, nil)
	if err != nil || got.Status != domain.BranchReady || got.Runtime != mysql80 || !z.server.running["branch-x"] {
		t.Errorf("restore: %+v %v", got, err)
	}
}

func TestUpgradeBranch_ロールバックも失敗したら手で戻せる失敗状態にする(t *testing.T) {
	z := newUEnv(t)
	z.server.upFail = errors.New("boom")
	z.sim.failOn = "zfs clone"

	res, err := z.upgrade(t, dbbranch.UpgradeRequest{})

	b := z.branchNow(t)
	if err == nil || res.Status != domain.UpgradeFailed || b.Status != domain.BranchFailed || !strings.HasPrefix(b.Error, domain.UpgradeFailedPrefix) || !strings.Contains(b.Error, "rollback") {
		t.Fatalf("result = %+v, branch = %+v, err = %v", res, b, err)
	}
	if b.CanRestore() != nil {
		t.Error("a failed rollback must leave the branch restorable")
	}
}

func TestUpgradeBranch_旧サーバーを綺麗に止められなければ何も変えずに戻す(t *testing.T) {
	z := newUEnv(t)
	z.server.failOn = "stop-clean"

	res, err := z.upgrade(t, dbbranch.UpgradeRequest{})

	if !errors.Is(err, domain.ErrUncleanShutdown) || res.Status != domain.UpgradeAborted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	z.server.failOn = ""
	if b := z.branchNow(t); b.Status != domain.BranchReady || b.Runtime != mysql80 || !z.server.running["branch-x"] {
		t.Errorf("branch = %+v running = %v", b, z.server.running)
	}
	if names := z.names(t); len(names) != 0 {
		t.Errorf("checkpoints = %v", names)
	}
}

func TestUpgradeBranch_チェックポイントを取れなければ何も変えずに戻す(t *testing.T) {
	z := newUEnv(t)
	z.sim.failOn = "zfs snapshot"
	res, err := z.upgrade(t, dbbranch.UpgradeRequest{})
	if err == nil || res.Status != domain.UpgradeAborted {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	z.sim.failOn = ""
	if b := z.branchNow(t); b.Status != domain.BranchReady || b.Runtime != mysql80 || !z.server.running["branch-x"] {
		t.Errorf("branch = %+v", b)
	}
}

func TestUpgradeBranch_受け付けない依頼は理由を返す(t *testing.T) {
	z := newUEnv(t)
	pg := domain.RuntimeRef{Engine: domain.Postgres, Version: "17.5"}
	cases := []struct {
		label string
		req   dbbranch.UpgradeRequest
		want  error
		text  string
	}{
		{"downgrade", dbbranch.UpgradeRequest{To: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.30"}}, domain.ErrUpgradeRefused, "downgrade"},
		{"other engine", dbbranch.UpgradeRequest{To: pg}, domain.ErrUpgradeRefused, "one engine"},
		{"lts skip", dbbranch.UpgradeRequest{To: domain.RuntimeRef{Engine: domain.MySQL, Version: "9.1.0"}}, domain.ErrUpgradeRefused, "8.4"},
		{"not installed", dbbranch.UpgradeRequest{To: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.99"}}, domain.ErrRuntimeNotInstalled, "db-branch runtime install mysql@8.4.99"},
		{"unknown branch", dbbranch.UpgradeRequest{Name: name("nope")}, domain.ErrNotFound, ""},
	}
	for _, c := range cases {
		_, err := z.upgrade(t, c.req)
		if !errors.Is(err, c.want) || !strings.Contains(fmt.Sprint(err), c.text) {
			t.Errorf("%s: err = %v", c.label, err)
		}
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady {
		t.Errorf("status = %s", b.Status)
	}
}

func TestUpgradeBranch_ブランチが準備完了でなければ受け付けない(t *testing.T) {
	z := newUEnv(t)
	b := z.branchNow(t)
	b.Status = domain.BranchResetting
	_ = z.store.PutBranch(ctx, b)
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("err = %v", err)
	}
}

func TestUpgradeBranch_アップグレード手段のないエンジンは断る(t *testing.T) {
	z := newUEnv(t)
	z.svc.Servers = map[domain.Engine]domain.DBServer{domain.MySQL: plainServer{z.server}}
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); !errors.Is(err, domain.ErrUnsupportedEngine) {
		t.Errorf("err = %v", err)
	}
}

// plainServer hides the fake's upgrade methods.
type plainServer struct{ domain.DBServer }

func TestUpgradeBranch_古いノードが混じるクラスタでは断る(t *testing.T) {
	z := newUEnv(t)
	n := z.clustered()
	n.nodes = []domain.Node{{Name: "node1", Protocol: 15}}
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("err = %v", err)
	}
	if res, err := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true}); err != nil || res.Status != domain.UpgradeChecked {
		t.Errorf("a check needs no new protocol: %+v %v", res, err)
	}
	n.nodes = []domain.Node{{Name: "node1", Protocol: domain.ProtocolBranchRuntime}}
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err != nil {
		t.Errorf("all nodes new: %v", err)
	}
	n.err = errors.New("raft down")
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{To: domain.RuntimeRef{Engine: domain.MySQL, Version: "9.1.0"}}); err == nil {
		t.Error("node list error ignored")
	}
}

func TestUpgradeBranch_空き容量の見積もりを事前確認に含める(t *testing.T) {
	cases := []struct {
		free  uint64
		level domain.FindingLevel
		text  string
	}{
		{0, domain.LevelInfo, "unknown"},
		{math.MaxUint64, domain.LevelInfo, "unknown"},
		{1 << 20, domain.LevelError, "has 1.0 MiB"},
		{600 << 20, domain.LevelWarning, "only 600.0 MiB"},
		{50 << 30, domain.LevelInfo, "50.0 GiB"},
	}
	for _, c := range cases {
		z := newUEnv(t)
		res, err := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true, PoolFree: c.free})
		if err != nil {
			t.Fatal(err)
		}
		f := res.PreCheck[1]
		if f.Code != domain.CodeDiskSpace || f.Level != c.level || !strings.Contains(f.Message, c.text) {
			t.Errorf("free %d: %+v", c.free, f)
		}
	}
	z := newUEnv(t)
	z.sim.readFail = "Info"
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true}); err == nil {
		t.Error("Info error ignored")
	}
}

func TestRestoreCheckpoint_チェックポイントのランタイムへ切り替える(t *testing.T) {
	z := newUEnv(t)
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err != nil {
		t.Fatal(err)
	}
	z.checkpoint(t, "on-84")
	before := z.autoNamed(t, "before-upgrade-")

	z.restore(t, before)
	if b := z.branchNow(t); b.Runtime != mysql80 {
		t.Fatalf("after restoring %s: %+v", before, b)
	}
	if last := z.server.started[len(z.server.started)-1]; last.Runtime != mysql80 {
		t.Errorf("started on %s", last.Runtime)
	}
	z.restore(t, "on-84")
	if b := z.branchNow(t); b.Runtime != mysql84 {
		t.Fatalf("after restoring on-84: %+v", b)
	}
	// What the branch left each time is recorded with the runtime it ran.
	list, _ := z.svc.ListCheckpoints(ctx, name("x"))
	want := map[string]string{"on-84": "mysql@8.4.13", before: "mysql@8.0.46"}
	for _, c := range list {
		if w, ok := want[c.Name]; ok && c.Runtime != w {
			t.Errorf("%s runtime = %q, want %q", c.Name, c.Runtime, w)
		}
		if strings.HasPrefix(c.Name, "before-restore-") && c.Runtime == "" {
			t.Errorf("%s has no runtime", c.Name)
		}
	}
}

func TestRestoreCheckpoint_ランタイムが入っていなければ手順を添えて断る(t *testing.T) {
	z := newUEnv(t)
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err != nil {
		t.Fatal(err)
	}
	before := z.autoNamed(t, "before-upgrade-")
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql84: true}}
	_, err := z.svc.RestoreCheckpoint(ctx, name("x"), before, nil)
	if !errors.Is(err, domain.ErrRuntimeNotInstalled) || !strings.Contains(err.Error(), "runtime install mysql@8.0.46") {
		t.Errorf("err = %v", err)
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady || b.Runtime != mysql84 {
		t.Errorf("branch = %+v", b)
	}
}

func TestRestoreCheckpoint_ランタイムを記録していない古いチェックポイントはベースのランタイムに戻す(t *testing.T) {
	z := newUEnv(t)
	z.checkpoint(t, "old")
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err != nil {
		t.Fatal(err)
	}
	// a checkpoint from before runtimes were recorded
	if err := z.sim.SetProps(ctx, live+"@ckpt-old", map[string]string{domain.PropRuntime: "-"}); err != nil {
		t.Fatal(err)
	}
	z.restore(t, "old")
	if b := z.branchNow(t); b.Runtime != mysql80 {
		t.Errorf("runtime = %s", b.Runtime)
	}
}

func TestResetBranch_ベースの状態とベースのランタイムに戻す(t *testing.T) {
	for _, withCheckpoint := range []bool{true, false} {
		z := newUEnv(t)
		if _, err := z.upgrade(t, dbbranch.UpgradeRequest{Force: true}); err != nil {
			t.Fatal(err)
		}
		b := z.branchNow(t)
		b.LegacyAuth = true
		_ = z.store.PutBranch(ctx, b)
		if !withCheckpoint {
			// without checkpoints the reset is a plain rollback
			for _, n := range z.names(t) {
				if err := z.svc.DeleteCheckpoint(ctx, name("x"), n); err != nil {
					t.Fatal(err)
				}
			}
		}
		z.tick()
		if _, err := z.svc.ResetBranch(ctx, name("x"), nil); err != nil {
			t.Fatalf("checkpoints=%v: %v", withCheckpoint, err)
		}
		b = z.branchNow(t)
		last := z.server.started[len(z.server.started)-1]
		if b.Runtime != mysql80 || b.LegacyAuth || b.Status != domain.BranchReady || last.Runtime != mysql80 {
			t.Errorf("checkpoints=%v: branch = %+v, last start on %s", withCheckpoint, b, last.Runtime)
		}
	}
}

func TestResetBranch_ベースのランタイムが入っていなければ何も変えずに断る(t *testing.T) {
	z := newUEnv(t)
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err != nil {
		t.Fatal(err)
	}
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql84: true}}
	if _, err := z.svc.ResetBranch(ctx, name("x"), nil); !errors.Is(err, domain.ErrRuntimeNotInstalled) {
		t.Errorf("err = %v", err)
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady || b.Runtime != mysql84 {
		t.Errorf("branch = %+v", b)
	}
}

func TestRecreateBranch_作り直したブランチはベースのランタイムで動く(t *testing.T) {
	p := lostPair(t)
	b, _ := p.replica.store.GetBranch(ctx, name("x"))
	b.Runtime, b.LegacyAuth = domain.RuntimeRef{Engine: domain.MySQL, Version: "9.1.0"}, true
	_ = p.replica.store.PutBranch(ctx, b)
	if _, err := p.replica.svc.MarkLost(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := p.replica.svc.RecreateBranch(ctx, name("x"), nil)
	if err != nil || got.Runtime != mysql84 || got.LegacyAuth {
		t.Errorf("branch = %+v, err = %v", got, err)
	}
}

// consistent: after a crash and a restart the branch is ready, runs the
// runtime its data needs and nothing of the upgrade is left half done.
func (z *zenv) upgradeConsistent(t *testing.T, label string) string {
	t.Helper()
	b := z.branchNow(t)
	if b.Status != domain.BranchReady || b.Error != "" {
		t.Errorf("%s: branch = %+v", label, b)
	}
	if !z.server.running["branch-x"] {
		t.Errorf("%s: server not running", label)
	}
	if len(z.server.started) > 0 {
		if last := z.server.started[len(z.server.started)-1]; last.Runtime != b.Runtime {
			t.Errorf("%s: running %s but the branch says %s", label, last.Runtime, b.Runtime)
		}
	}
	if len(z.server.upgraded) > 0 && b.Runtime == mysql84 {
		if last := z.server.upgraded[len(z.server.upgraded)-1]; last.Runtime != mysql84 {
			t.Errorf("%s: upgraded server on %s", label, last.Runtime)
		}
	}
	if p, _ := z.sim.Props(ctx, history); p[domain.PropUpgrade] != "" || p[domain.PropOp] != "" {
		t.Errorf("%s: intent left: %v", label, p)
	}
	if _, ok := z.sim.ds[history+"/new"]; ok {
		t.Errorf("%s: pending clone left", label)
	}
	if z.data() != "v0" {
		t.Errorf("%s: data = %q", label, z.data())
	}
	return b.Runtime.String()
}

func TestUpgradeBranch_どの段階でクラッシュしても再起動後は整合する(t *testing.T) {
	total := 0
	{
		z := newUEnv(t)
		z.crash.steps = 0
		if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err != nil {
			t.Fatal(err)
		}
		total = z.crash.steps
	}
	if total < 8 {
		t.Fatalf("an upgrade makes only %d side effects?", total)
	}
	sawOld, sawNew := false, false
	for k := 1; k <= total; k++ {
		for _, fail := range []bool{false, true} {
			z := newUEnv(t)
			z.server.upFail = nil
			if fail {
				z.server.upFail = errors.New("boom")
			}
			z.crash.steps, z.crash.at = 0, k
			z.tick()
			_, _ = z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: mysql84}, nil)
			z.restart()
			if err := z.svc.Recover(ctx); err != nil {
				t.Fatalf("crash at %d (fail=%v): recover: %v", k, fail, err)
			}
			label := fmt.Sprintf("crash at step %d fail=%v", k, fail)
			switch z.upgradeConsistent(t, label) {
			case "mysql@8.4.13":
				sawNew = true
			default:
				sawOld = true
			}
			z.restart()
			if err := z.svc.Recover(ctx); err != nil {
				t.Fatalf("%s: second recover: %v", label, err)
			}
			z.upgradeConsistent(t, label+", recovered twice")
		}
	}
	if !sawOld || !sawNew {
		t.Errorf("expected both outcomes: old=%v new=%v", sawOld, sawNew)
	}
}

func TestRecover_途中の更新で意図の記録がなければ元の状態に戻す(t *testing.T) {
	z := newUEnv(t)
	b := z.branchNow(t)
	b.Status = domain.BranchUpgrading
	_ = z.store.PutBranch(ctx, b)
	z.server.running = map[string]bool{}
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := z.branchNow(t); got.Status != domain.BranchReady || !z.server.running["branch-x"] {
		t.Errorf("branch = %+v", got)
	}
}

func TestRecover_チェックポイント前に中断した更新は旧ランタイムで再開する(t *testing.T) {
	z := newUEnv(t)
	b := z.branchNow(t)
	b.Status, b.Runtime = domain.BranchUpgrading, mysql84
	_ = z.store.PutBranch(ctx, b)
	// the intent says the checkpoint "gone-yet" was to be saved, but it never was
	_ = z.sim.Create(ctx, history, map[string]string{"canmount": "off"})
	_ = z.sim.SetProps(ctx, history, map[string]string{domain.PropUpgrade: "mysql:8.0.46/mysql:8.4.13/before-upgrade-never"})
	z.server.running = map[string]bool{}
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := z.branchNow(t); got.Status != domain.BranchReady || got.Runtime != mysql80 {
		t.Errorf("branch = %+v", got)
	}
}

func TestCreateBranchUpgraded_作成してすぐ新しいランタイムへ上げる(t *testing.T) {
	z := newUEnv(t)
	br, res, err := z.svc.CreateBranchUpgraded(ctx, dbbranch.CreateBranchRequest{Name: name("y"), Base: name("prod"), Runtime: mysql84}, nil)
	if err != nil || res == nil || res.Status != domain.UpgradeUpgraded || br.Runtime != mysql84 || br.Status != domain.BranchReady {
		t.Fatalf("branch = %+v, result = %+v, err = %v", br, res, err)
	}
	if base, _ := z.store.GetBase(ctx, name("prod")); base.Runtime != mysql80 {
		t.Errorf("the base changed: %+v", base)
	}
	if x := z.branchNow(t); x.Runtime != mysql80 {
		t.Errorf("another branch changed: %+v", x)
	}
}

func TestCreateBranchUpgraded_失敗してもブランチは残りベースのランタイムで動く(t *testing.T) {
	z := newUEnv(t)
	z.server.upFail = errors.New("boom")
	br, res, err := z.svc.CreateBranchUpgraded(ctx, dbbranch.CreateBranchRequest{Name: name("y"), Base: name("prod"), Runtime: mysql84}, nil)
	if err == nil || res == nil || res.Status != domain.UpgradeRolledBack || br.Runtime != mysql80 || br.Status != domain.BranchReady {
		t.Errorf("branch = %+v, result = %+v, err = %v", br, res, err)
	}
}

func TestCreateBranchUpgraded_ランタイムなしなら普通の作成(t *testing.T) {
	z := newUEnv(t)
	br, res, err := z.svc.CreateBranchUpgraded(ctx, dbbranch.CreateBranchRequest{Name: name("y"), Base: name("prod")}, nil)
	if err != nil || res != nil || br.Runtime != mysql80 {
		t.Errorf("branch = %+v, result = %v, err = %v", br, res, err)
	}
}

func TestCreateBranchUpgraded_作る前に道筋とインストールと対象ベースを確かめる(t *testing.T) {
	z := newUEnv(t)
	for label, req := range map[string]dbbranch.CreateBranchRequest{
		"skips LTS":     {Name: name("y"), Base: name("prod"), Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "9.1.0"}},
		"not installed": {Name: name("y"), Base: name("prod"), Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.99"}},
		"no base":       {Name: name("y"), Base: name("nope"), Runtime: mysql84},
	} {
		if _, _, err := z.svc.CreateBranchUpgraded(ctx, req, nil); err == nil {
			t.Errorf("%s: want error", label)
		}
	}
	if _, err := z.store.GetBranch(ctx, name("y")); err == nil {
		t.Error("a refused request created the branch")
	}
	z.clustered().nodes = []domain.Node{{Name: "node1", Protocol: 15}}
	if _, _, err := z.svc.CreateBranchUpgraded(ctx, dbbranch.CreateBranchRequest{Name: name("y"), Base: name("prod"), Runtime: mysql84}, nil); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("old node: %v", err)
	}
}

var (
	pg18 = domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}
	pg19 = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
)

func newPGUEnv(t *testing.T) *zenv {
	t.Helper()
	z := newZEnv(t)
	z.svc.Servers[domain.Postgres] = z.server
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{pg18: true, pg19: true}}
	if _, err := z.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("pgprod"), Runtime: pg18, Dump: "/dumps/pg.sql.gz"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := z.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("pgprod")}, nil); err != nil {
		t.Fatal(err)
	}
	z.sim.write(live, "v0")
	z.j.entries = nil
	return z
}

func TestUpgradeBranch_旧ランタイムが無ければ404を返す(t *testing.T) {
	z := newPGUEnv(t)
	z.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{pg19: true}}
	_, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19, CheckOnly: true}, nil)
	if !errors.Is(err, domain.ErrRuntimeNotInstalled) || !strings.Contains(err.Error(), "runtime install postgres@18.6") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradeBranch_PGメジャー更新はタイムアウト付きで実行し統計更新を後ろで進めて先にreadyにする(t *testing.T) {
	z := newPGUEnv(t)
	z.server.plan = domain.UpgradePlan{Timeout: 20 * time.Minute}
	z.server.upNeedsStats = true

	statsStarted := make(chan struct{})
	statsRelease := make(chan struct{})
	z.server.statsHook = func(context.Context) {
		close(statsStarted)
		<-statsRelease
	}
	changed := make(chan domain.Name, 4)
	z.svc.OnBranchChanged = func(n domain.Name) { changed <- n }

	res, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19, PoolFree: 100 << 30}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.UpgradeUpgraded {
		t.Fatalf("status = %s", res.Status)
	}
	if len(z.server.majorUpgrades) != 1 || z.server.majorUpgrades[0].From != pg18 || len(z.server.upDeadlines) != 1 || !z.server.upDeadlines[0] {
		t.Fatalf("majorUpgrades = %+v deadlines = %v", z.server.majorUpgrades, z.server.upDeadlines)
	}
	// Branch must already be ready with StatsUpdating == true while vacuumdb runs.
	<-statsStarted
	b := z.branchNow(t)
	if b.Status != domain.BranchReady || b.Runtime != pg19 || !b.StatsUpdating || b.StatsWarning != "" {
		t.Fatalf("branch while updating stats = %+v", b)
	}

	close(statsRelease)
	select {
	case n := <-changed:
		if n != name("x") {
			t.Fatalf("changed = %s", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background stats completion")
	}
	b = z.branchNow(t)
	if b.Status != domain.BranchReady || b.StatsUpdating || b.StatsWarning != "" {
		t.Fatalf("branch after stats = %+v", b)
	}
}

func TestUpgradeBranch_統計更新に失敗してもBranchはreadyのままで警告を残す(t *testing.T) {
	z := newPGUEnv(t)
	z.server.upNeedsStats = true
	z.server.statsErr = errors.New("vacuumdb failed")
	var logged []string
	z.svc.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	done := make(chan struct{})
	z.svc.OnBranchChanged = func(domain.Name) { close(done) }

	if _, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stats failure notification")
	}
	b := z.branchNow(t)
	if b.Status != domain.BranchReady || b.StatsUpdating || !strings.Contains(b.StatsWarning, "vacuumdb failed") || len(logged) == 0 {
		t.Fatalf("branch = %+v logged = %v", b, logged)
	}

	// Non-MajorUpgrader fallback and missing MissingStatsUpdater are handled gracefully.
	z2 := newPGUEnv(t)
	z2.server.upNeedsStats = true
	z2.svc.Servers[domain.Postgres] = plainUpgrader{z2.server}
	if _, err := z2.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19}, nil); err != nil {
		t.Fatal(err)
	}
}

type plainUpgrader struct {
	*fakeServer
}

func (p plainUpgrader) UpdateMissingStats(context.Context, domain.Server) error { return nil }

func TestUpgradeBranch_PGメジャー更新は空き容量不足でinsufficient_spaceを出しプロトコル31を待つ(t *testing.T) {
	z := newPGUEnv(t)
	res, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19, CheckOnly: true, PoolFree: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.PreCheck[1].Code != domain.CodeInsufficientSpace || res.PreCheck[1].Level != domain.LevelError {
		t.Errorf("finding = %+v", res.PreCheck[1])
	}

	n := z.clustered()
	n.nodes = []domain.Node{{Name: "node1", Protocol: domain.ProtocolUsers}}
	if _, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19}, nil); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("want ErrClusterUpgrading below protocol 31, got %v", err)
	}
	if res, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19, CheckOnly: true}, nil); err != nil || res.Status != domain.UpgradeChecked {
		t.Errorf("check_only must work during rolling upgrade: %+v %v", res, err)
	}
	n.nodes = []domain.Node{{Name: "node1", Protocol: domain.ProtocolPGMajorUpgrade}}
	if _, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19}, nil); err != nil {
		t.Errorf("protocol 31 should allow PG major upgrade: %v", err)
	}
}

func TestRecover_統計更新中にエージェントが再起動したらバックグラウンド更新をやり直す(t *testing.T) {
	z := newPGUEnv(t)
	b := z.branchNow(t)
	b.Runtime = pg19
	b.StatsUpdating = true
	_ = z.store.PutBranch(ctx, b)
	done := make(chan struct{})
	z.svc.OnBranchChanged = func(domain.Name) { close(done) }

	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovered background stats")
	}
	if got := z.branchNow(t); got.StatsUpdating || len(z.server.statsCalls) == 0 {
		t.Fatalf("after recover: branch = %+v statsCalls = %d", got, len(z.server.statsCalls))
	}
}

func TestResetBranchとRestore_統計更新中に戻すとキャンセルして状態と警告を消す(t *testing.T) {
	z := newPGUEnv(t)
	z.server.upNeedsStats = true
	statsStarted := make(chan struct{})
	statsCancelled := make(chan struct{})
	z.server.statsHook = func(sctx context.Context) {
		close(statsStarted)
		<-sctx.Done()
		close(statsCancelled)
	}
	if _, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19}, nil); err != nil {
		t.Fatal(err)
	}
	<-statsStarted

	got, err := z.svc.ResetBranch(ctx, name("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-statsCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("stats goroutine was not cancelled on ResetBranch")
	}
	if got.Runtime != pg18 || got.StatsUpdating || got.StatsWarning != "" {
		t.Fatalf("after reset = %+v", got)
	}
}

func TestPGMajorUpgrade_RestoreとPauseとRecoverの連携(t *testing.T) {
	// 1. RestoreCheckpoint reverts runtime from pg19 back to pg18 and cancels stats.
	z := newPGUEnv(t)
	z.server.upNeedsStats = true
	started := make(chan struct{})
	cancelled := make(chan struct{})
	z.server.statsHook = func(sctx context.Context) {
		close(started)
		<-sctx.Done()
		close(cancelled)
	}
	res, err := z.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	restored, err := z.svc.RestoreCheckpoint(ctx, name("x"), res.Checkpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-cancelled
	if restored.Runtime != pg18 || restored.StatsUpdating || restored.StatsWarning != "" {
		t.Fatalf("after restore = %+v", restored)
	}

	// 2. Recover clears StatsUpdating on a paused branch.
	b := z.branchNow(t)
	b.Status = domain.BranchPaused
	b.StatsUpdating = true
	_ = z.store.PutBranch(ctx, b)
	if err := z.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := z.branchNow(t); got.StatsUpdating {
		t.Fatalf("paused branch after recover still StatsUpdating: %+v", got)
	}

	// 3. PauseBranch clears StatsUpdating.
	z2 := newPGUEnv(t)
	z2.server.upNeedsStats = true
	z2.server.statsHook = func(sctx context.Context) { <-sctx.Done() }
	if _, err := z2.svc.UpgradeBranch(ctx, dbbranch.UpgradeRequest{Name: name("x"), To: pg19}, nil); err != nil {
		t.Fatal(err)
	}
	paused, err := z2.svc.PauseBranch(ctx, name("x"), nil)
	if err != nil || paused.Status != domain.BranchPaused || paused.StatsUpdating {
		t.Fatalf("paused = %+v, err = %v", paused, err)
	}
}
