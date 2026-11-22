package dbbranch_test

import (
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func setBaseStatus(t *testing.T, e *env, n string, s domain.BaseStatus) {
	t.Helper()
	b, err := e.store.GetBase(ctx, name(n))
	if err != nil {
		t.Fatal(err)
	}
	b.Status = s
	if err := e.store.PutBase(ctx, b); err != nil {
		t.Fatal(err)
	}
}

func TestRecover_provisioningで固着したベースは理由つきでfailedにする(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	setBaseStatus(t, e, "prod", domain.BaseProvisioning)

	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	if b.Status != domain.BaseFailed || b.Error == "" {
		t.Errorf("base = %+v", b)
	}
}

func TestRecover_maintenanceで固着したベースは最新スナップショットへ戻しmaskのワークを掃除してreadyにする(t *testing.T) {
	e := newEnv(t)
	base := e.withBase(t)
	setBaseStatus(t, e, "prod", domain.BaseMaintenance)
	work := "tank/db-branch/work/mask-prod"
	e.storage.datasets[work] = true

	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	if b.Status != domain.BaseReady {
		t.Errorf("status = %s", b.Status)
	}
	if !e.j.has("zfs rollback tank/db-branch/bases/prod@" + base.Snapshots[0].Name) {
		t.Errorf("no rollback: %q", e.j.entries)
	}
	if !e.j.has("zfs destroy " + work) {
		t.Errorf("work not destroyed: %q", e.j.entries)
	}
}

func TestRecover_他ノードのベースには手を出さない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	b, _ := e.store.GetBase(ctx, name("prod"))
	b.Status, b.Node = domain.BaseMaintenance, "node2"
	_ = e.store.PutBase(ctx, b)
	e.svc.Nodes = nil
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ = e.store.GetBase(ctx, name("prod"))
	if b.Status != domain.BaseMaintenance {
		t.Errorf("status = %s", b.Status)
	}
}

func TestDeleteBase_maintenance中のベースは拒否し何も消さない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	setBaseStatus(t, e, "prod", domain.BaseMaintenance)

	if err := e.svc.DeleteBase(ctx, name("prod")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if len(e.j.entries) != 0 {
		t.Errorf("journal %q", e.j.entries)
	}
	if _, err := e.store.GetBase(ctx, name("prod")); err != nil {
		t.Errorf("record should remain: %v", err)
	}
}

func TestDeleteBase_リフレッシュ設定も消す(t *testing.T) {
	e := newEnv(t)
	e.withRefresh(t)

	if err := e.svc.DeleteBase(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetRefresh(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("refresh should be gone: %v", err)
	}
}

func TestDeleteBase_削除の最中はそのベースからブランチを作れない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	inRemove, release := make(chan struct{}), make(chan struct{})
	e.server.onRemove = func() { close(inRemove); <-release }
	done := make(chan error, 1)
	go func() { done <- e.svc.DeleteBase(ctx, name("prod")) }()
	<-inRemove

	_, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil)
	close(release)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("create during delete = %v, want ErrConflict", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
