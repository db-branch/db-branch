package dbbranch_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// failingRuntimes cannot tell what is installed.
type failingRuntimes struct{ fakeRuntimes }

func (failingRuntimes) Has(context.Context, domain.RuntimeRef) (bool, error) {
	return false, errors.New("runtime store unreadable")
}

// putFails refuses to store a branch in one status.
type putFails struct {
	domain.Store
	status domain.BranchStatus
}

func (s putFails) PutBranch(ctx context.Context, b domain.Branch) error {
	if b.Status == s.status {
		return errors.New("store unavailable")
	}
	return s.Store.PutBranch(ctx, b)
}

func TestRequireInstalled_ランタイムの一覧が読めなければそのエラーを返す(t *testing.T) {
	z := newUEnv(t)
	z.svc.Runtimes = failingRuntimes{}
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true}); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("err = %v", err)
	}
	z.svc.Runtimes = nil // no store: nothing to check
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true}); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestUpgradeBranch_エンジンが無ければ断る(t *testing.T) {
	z := newUEnv(t)
	z.svc.Servers = map[domain.Engine]domain.DBServer{}
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); !errors.Is(err, domain.ErrUnsupportedEngine) {
		t.Errorf("err = %v", err)
	}
}

func TestUpgradeBranch_ノード一覧が読めなければ始めない(t *testing.T) {
	z := newUEnv(t)
	z.clustered().err = errors.New("raft down")
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err == nil || !strings.Contains(err.Error(), "raft down") {
		t.Errorf("err = %v", err)
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady {
		t.Errorf("status = %s", b.Status)
	}
}

func TestUpgradeBranch_事前確認の途中で読み取りに失敗したらブランチを戻す(t *testing.T) {
	z := newUEnv(t)
	z.server.failOn = "start"
	z.server.running["branch-x"] = false
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %v", err)
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady {
		t.Errorf("status = %s", b.Status)
	}
	z.server.failOn = ""
	z.sim.readFail = "Info"
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err == nil {
		t.Error("want error")
	}
}

// infoFailsFrom makes the nth and later Info calls fail.
type infoFailsFrom struct {
	domain.Storage
	n, calls int
}

func (s *infoFailsFrom) Info(ctx context.Context, ds string) (domain.DatasetInfo, error) {
	s.calls++
	if s.calls >= s.n {
		return domain.DatasetInfo{}, errors.New("info failed")
	}
	return s.Storage.Info(ctx, ds)
}

func TestUpgradeBranch_停止の前に状態を読めなければ何も変えずに戻す(t *testing.T) {
	// Info is read by the space estimate, the running check, the pre-check and then the setup.
	z := newUEnv(t)
	z.svc.Storage = &infoFailsFrom{Storage: z.sim, n: 4}
	_, err := z.upgrade(t, dbbranch.UpgradeRequest{})
	if err == nil || !strings.Contains(err.Error(), "info failed") {
		t.Fatalf("err = %v", err)
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady || b.Runtime != mysql80 || z.j.has("stop branch-x clean") {
		t.Errorf("branch = %+v", b)
	}
}

func TestUpgradeBranch_意図の記録に失敗したら何も変えずに戻す(t *testing.T) {
	z := newUEnv(t)
	z.sim.failOn = "zfs set db-branch:upgrade="
	res, err := z.upgrade(t, dbbranch.UpgradeRequest{})
	if err == nil || res.Status != domain.UpgradeAborted {
		t.Fatalf("result = %+v err = %v", res, err)
	}
	if b := z.branchNow(t); b.Status != domain.BranchReady || b.Runtime != mysql80 {
		t.Errorf("branch = %+v", b)
	}
}

func TestUpgradeBranch_成功後に意図を消せなければ失敗として返す(t *testing.T) {
	z := newUEnv(t)
	z.sim.failOn = "zfs inherit [db-branch:upgrade]"
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err == nil {
		t.Error("want error")
	}
}

func TestUpgradeBranch_成功後に状態を保存できなければ失敗として返す(t *testing.T) {
	z := newUEnv(t)
	z.svc.Store = putFails{z.svc.Store, domain.BranchReady}
	if _, err := z.upgrade(t, dbbranch.UpgradeRequest{}); err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Errorf("err = %v", err)
	}
}

func TestUpgradeBranch_ロールバックの準備に失敗したら手で戻せる失敗状態にする(t *testing.T) {
	cases := map[string]func(z *zenv){
		"old runtime gone": func(z *zenv) {
			z.svc.Runtimes.(fakeRuntimes).installed[mysql80] = false
		},
		"store refuses restoring": func(z *zenv) {
			z.svc.Store = putFails{z.svc.Store, domain.BranchRestoring}
		},
		"intent cannot be cleared": func(z *zenv) {
			z.sim.failOn = "zfs inherit [db-branch:upgrade]"
		},
		"intent cannot be written": func(z *zenv) {
			z.sim.failOn = "zfs set db-branch:autoname"
		},
	}
	for label, break_ := range cases {
		z := newUEnv(t)
		z.server.upFail = errors.New("boom")
		break_(z)
		res, err := z.upgrade(t, dbbranch.UpgradeRequest{})
		b := z.branchNow(t)
		if err == nil || res.Status != domain.UpgradeFailed || b.Status != domain.BranchFailed || !strings.HasPrefix(b.Error, domain.UpgradeFailedPrefix) {
			t.Errorf("%s: result = %+v branch = %+v err = %v", label, res, b, err)
		}
	}
}

func TestRecover_中断した更新の後始末で読み取りに失敗したらエラーを返す(t *testing.T) {
	for _, op := range []string{"Exists", "Props"} {
		z := newUEnv(t)
		b := z.branchNow(t)
		b.Status = domain.BranchUpgrading
		_ = z.store.PutBranch(ctx, b)
		_ = z.sim.Create(ctx, history, map[string]string{"canmount": "off"})
		z.sim.readFail = op
		if err := z.svc.Recover(ctx); err == nil {
			t.Errorf("%s: want error", op)
		}
	}
}

func TestRecover_中断した更新で古いサーバーを起動できなければエラーを返す(t *testing.T) {
	z := newUEnv(t)
	b := z.branchNow(t)
	b.Status = domain.BranchUpgrading
	_ = z.store.PutBranch(ctx, b)
	z.server.running = map[string]bool{}
	z.server.failOn = "start"
	if err := z.svc.Recover(ctx); err == nil {
		t.Error("want error")
	}
	// with an intent whose checkpoint was never saved
	_ = z.sim.Create(ctx, history, map[string]string{"canmount": "off"})
	_ = z.sim.SetProps(ctx, history, map[string]string{domain.PropUpgrade: "mysql:8.0.46/mysql:8.4.13/before-upgrade-never"})
	if err := z.svc.Recover(ctx); err == nil {
		t.Error("want error")
	}
	z.server.failOn = ""
	z.sim.failOn = "zfs inherit [db-branch:upgrade]"
	if err := z.svc.Recover(ctx); err == nil {
		t.Error("an intent that cannot be cleared must be reported")
	}
}

func TestRecover_中断した更新のチェックポイントを読めなければエラーを返す(t *testing.T) {
	z := newUEnv(t)
	b := z.branchNow(t)
	b.Status = domain.BranchUpgrading
	_ = z.store.PutBranch(ctx, b)
	_ = z.sim.Create(ctx, history, map[string]string{"canmount": "off"})
	_ = z.sim.SetProps(ctx, history, map[string]string{domain.PropUpgrade: "mysql:8.0.46/mysql:8.4.13/before-upgrade-never"})
	z.sim.failRead = z.sim.reads + 4
	if err := z.svc.Recover(ctx); err == nil {
		t.Error("want error")
	}
}

func TestParseUpgradeIntent_壊れた記録は無視する(t *testing.T) {
	z := newUEnv(t)
	b := z.branchNow(t)
	b.Status = domain.BranchUpgrading
	_ = z.store.PutBranch(ctx, b)
	_ = z.sim.Create(ctx, history, map[string]string{"canmount": "off"})
	for _, bad := range []string{"x", "mysql:8.0.46/mysql:8.4.13/", "nope/mysql:8.4.13/c"} {
		_ = z.sim.SetProps(ctx, history, map[string]string{domain.PropUpgrade: bad})
		if err := z.svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if got := z.branchNow(t); got.Status != domain.BranchReady {
			t.Errorf("%q: status = %s", bad, got.Status)
		}
		b.Status = domain.BranchUpgrading
		_ = z.store.PutBranch(ctx, b)
	}
}

func TestCreateBranchUpgraded_作成に失敗したらそのエラーを返す(t *testing.T) {
	z := newUEnv(t)
	_, res, err := z.svc.CreateBranchUpgraded(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod"), Runtime: mysql84}, nil)
	if !errors.Is(err, domain.ErrAlreadyExists) || res != nil {
		t.Errorf("res = %v err = %v", res, err)
	}
}

func TestUpgradeBranch_数バイトの空きでも見積もりを書く(t *testing.T) {
	z := newUEnv(t)
	res, _ := z.upgrade(t, dbbranch.UpgradeRequest{CheckOnly: true, PoolFree: 500})
	if !strings.Contains(res.PreCheck[1].Message, "has 500 B") {
		t.Errorf("finding = %+v", res.PreCheck[1])
	}
}

func TestRestoreCheckpoint_ベースが無くてもランタイム未記録のチェックポイントは今のランタイムのまま(t *testing.T) {
	z := newUEnv(t)
	z.checkpoint(t, "old")
	_ = z.sim.SetProps(ctx, live+"@ckpt-old", map[string]string{domain.PropRuntime: "-"})
	_ = z.store.DeleteBase(ctx, name("prod"))
	z.restore(t, "old")
	if b := z.branchNow(t); b.Runtime != mysql80 {
		t.Errorf("runtime = %s", b.Runtime)
	}
}
