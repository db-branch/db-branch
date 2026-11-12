package dbbranch_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func TestOpenLine_ライブは自分のディレクトリを退避した線と過去の時点は読み取り専用でマウントする(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1") // line 0 is set aside, line 1 is live

	info, _ := z.sim.Info(ctx, live)
	wantLive := info.Mountpoint
	liveDir, err := z.svc.OpenLine(ctx, name("x"), 1, "")
	if err != nil || liveDir != wantLive || len(z.sim.mounts) != 0 {
		t.Fatalf("live = %q %v mounts=%v", liveDir, err, z.sim.mounts)
	}
	old, err := z.svc.OpenLine(ctx, name("x"), 0, "")
	if err != nil || len(z.sim.mounts) != 1 || !z.j.has("mount ro "+history+"/0") {
		t.Fatalf("old = %q %v mounts=%v", old, err, z.sim.mounts)
	}
	snap, err := z.svc.OpenLine(ctx, name("x"), 0, "c2")
	if err != nil || !z.j.has("mount ro "+history+"/0@ckpt-c2") {
		t.Fatalf("snapshot = %q %v journal=%v", snap, err, z.j.entries)
	}
	for _, d := range []string{old, snap, liveDir} {
		if err := z.svc.CloseLine(ctx, d); err != nil {
			t.Errorf("close %s: %v", d, err)
		}
	}
	if len(z.sim.mounts) != 0 {
		t.Errorf("still mounted: %v", z.sim.mounts)
	}
	if err := z.svc.CloseLine(ctx, old); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
	if err := z.svc.CloseLine(ctx, "/etc"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a directory we did not hand out: %v", err)
	}
}

func TestOpenLine_存在しない線やチェックポイントと保存先の失敗は断る(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	if _, err := z.svc.OpenLine(ctx, name("x"), 9, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("line: %v", err)
	}
	if _, err := z.svc.OpenLine(ctx, name("x"), 0, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("checkpoint: %v", err)
	}
	if _, err := z.svc.OpenLine(ctx, name("nope"), 0, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("branch: %v", err)
	}
	z.sim.failOn = "mount ro"
	z.restore(t, "c1")
	if _, err := z.svc.OpenLine(ctx, name("x"), 0, ""); err == nil {
		t.Error("mount failure: want error")
	}
	if _, err := z.svc.OpenLine(ctx, name("x"), 0, "c2"); err == nil || strings.Contains(err.Error(), "not found") {
		t.Errorf("snapshot mount failure: %v", err)
	}
	z.sim.failOn = ""
	z.sim.readFail = "Info"
	if _, err := z.svc.OpenLine(ctx, name("x"), 1, ""); err == nil {
		t.Error("info failure: want error")
	}
	z.sim.readFail = ""
	e := newEnv(t)
	if _, err := e.svc.OpenLine(ctx, name("x"), 0, ""); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("no lineage: %v", err)
	}
}

func TestBranchAdmin_ブランチのサーバーに管理者として入る方法を返す(t *testing.T) {
	z := newZEnv(t)
	z.branch(t)
	a, err := z.svc.BranchAdmin(ctx, name("x"))
	if err != nil || a.Socket != "/run/branch-x" || a.User != "root" {
		t.Fatalf("admin = %+v %v", a, err)
	}
	if _, err := z.svc.BranchAdmin(ctx, name("nope")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	z.sim.readFail = "Info"
	if _, err := z.svc.BranchAdmin(ctx, name("x")); err == nil {
		t.Error("info failure: want error")
	}
}

func TestOpenLine_作成時の状態はリセット点として開く(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.restore(t, "c1")
	dir, err := z.svc.OpenLine(ctx, name("x"), 0, dbbranch.ResetPoint)
	if err != nil || !z.j.has("mount ro "+live+"@reset") {
		t.Fatalf("dir=%q err=%v journal=%v", dir, err, z.j.entries)
	}
	if err := z.svc.CloseLine(ctx, dir); err != nil {
		t.Error(err)
	}
}

func TestOpenLine_リセット点が無ければ見つからない(t *testing.T) {
	z := newZEnv(t)
	z.history(t)
	z.sim.ds[live].snaps = z.sim.ds[live].snaps[1:] // drop "reset"
	if _, err := z.svc.OpenLine(ctx, name("x"), 0, dbbranch.ResetPoint); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}
