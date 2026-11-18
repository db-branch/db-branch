package dbbranch_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func TestCreateBranch_クローンしてユーザーを作り正常停止してリセット点を取ってから起動する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	var phases []string

	b, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{
		Name: name("feature-x"), Base: name("prod"), InitSQL: "UPDATE users SET email = 'x'", TTL: 72 * time.Hour,
	}, func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"zfs clone tank/db-branch/bases/prod@s20261109120000 tank/db-branch/branches/feature-x",
		"prepare branch-feature-x",
		"start branch-feature-x",
		"exec branch-feature-x",
		"exec branch-feature-x",
		"stop branch-feature-x clean",
		"zfs snapshot tank/db-branch/branches/feature-x@reset",
		"start branch-feature-x",
	}
	if !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal:\n got %q\nwant %q", e.j.entries, want)
	}
	if !reflect.DeepEqual(phases, []string{"cloning", "starting", "init_sql", "stopping", "snapshotting", "starting"}) {
		t.Errorf("phases = %q", phases)
	}
	sql := e.server.sql["branch-feature-x"]
	// The branch's own user is named after it, so the proxy can route by user name.
	if !strings.Contains(sql[0], "CREATE USER IF NOT EXISTS 'feature-x'@'%' IDENTIFIED BY 'pw1'") || sql[1] != "UPDATE users SET email = 'x'" {
		t.Errorf("sql = %q", sql)
	}
	if b.Status != domain.BranchReady || b.Port != 40001 || b.User != "feature-x" || b.Password != "pw1" ||
		b.Snapshot != "s20261109120000" || b.Runtime != mysql84 || !b.ExpiresAt.Equal(e.now.Add(72*time.Hour)) {
		t.Errorf("branch = %+v", b)
	}
}

func TestCreateBranch_スナップショットを指定できる(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.now = e.now.Add(time.Hour)
	_, _ = e.svc.SnapshotBase(ctx, name("prod"), nil)

	b, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("old"), Base: name("prod"), Snapshot: "s20261109120000"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Snapshot != "s20261109120000" {
		t.Errorf("snapshot = %s", b.Snapshot)
	}
	latest, _ := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("new"), Base: name("prod")}, nil)
	if latest.Snapshot != "s20261109130000" {
		t.Errorf("default snapshot = %s", latest.Snapshot)
	}
}

func TestCreateBranch_前提を満たさなければ何もせずに失敗する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "taken")
	_ = e.store.PutBase(ctx, domain.Base{Name: name("empty"), Runtime: mysql84, Status: domain.BaseReady})
	cases := map[string]struct {
		req  dbbranch.CreateBranchRequest
		want error
	}{
		"duplicate":        {dbbranch.CreateBranchRequest{Name: name("taken"), Base: name("prod")}, domain.ErrAlreadyExists},
		"no base":          {dbbranch.CreateBranchRequest{Name: name("a"), Base: name("nope")}, domain.ErrNotFound},
		"no such snapshot": {dbbranch.CreateBranchRequest{Name: name("b"), Base: name("prod"), Snapshot: "s1"}, domain.ErrNotFound},
		"base w/o snaps":   {dbbranch.CreateBranchRequest{Name: name("c"), Base: name("empty")}, domain.ErrConflict},
	}
	for label, c := range cases {
		e.j.entries = nil
		if _, err := e.svc.CreateBranch(ctx, c.req, nil); !errors.Is(err, c.want) {
			t.Errorf("%s: error = %v, want %v", label, err, c.want)
		}
		if len(e.j.entries) != 0 {
			t.Errorf("%s: did work %q", label, e.j.entries)
		}
	}
}

func TestCreateBranch_ポートが尽きたら失敗する(t *testing.T) {
	e := newEnv(t)
	e.svc.Ports = dbbranch.PortRange{Min: 40000, Max: 40002}
	e.withBase(t) // uses 40000
	e.withBranch(t, "a")
	e.withBranch(t, "b")

	if _, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("c"), Base: name("prod")}, nil); !errors.Is(err, dbbranch.ErrNoFreePort) {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateBranch_削除したブランチのポートは再利用する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "a")
	_ = e.svc.DeleteBranch(ctx, name("a"), nil)

	b := e.withBranch(t, "b")
	if b.Port != 40001 {
		t.Errorf("port = %d", b.Port)
	}
}

func TestCreateBranch_途中で失敗したら失敗状態と理由を残す(t *testing.T) {
	for _, op := range []string{"clone", "info", "prepare", "start", "exec", "stop-clean", "snapshot"} {
		e := newEnv(t)
		e.withBase(t)
		switch op {
		case "clone", "info", "snapshot":
			e.storage.failOn = op
		default:
			e.server.failOn = op
		}
		if _, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil); err == nil {
			t.Errorf("%s: want error", op)
			continue
		}
		b, _ := e.store.GetBranch(ctx, name("x"))
		if b.Status != domain.BranchFailed || b.Error == "" {
			t.Errorf("%s: branch = %+v", op, b)
		}
	}
}

func TestResetBranch_即時停止してリセット点に戻し起動する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "feature-x")
	e.now = e.now.Add(time.Minute)

	b, err := e.svc.ResetBranch(ctx, name("feature-x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The data comes back from the reset point: the login is put right again after the start.
	want := []string{"stop branch-feature-x immediate", "zfs rollback tank/db-branch/branches/feature-x@reset", "start branch-feature-x", "exec branch-feature-x"}
	if !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal %q", e.j.entries)
	}
	if b.Status != domain.BranchReady || b.ResetAt == nil || !b.ResetAt.Equal(e.now) {
		t.Errorf("branch = %+v", b)
	}
}

func TestResetBranch_準備完了でなければ失敗する(t *testing.T) {
	e := newEnv(t)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("busy"), Status: domain.BranchProvisioning})
	if _, err := e.svc.ResetBranch(ctx, name("busy"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("busy: %v", err)
	}
	if _, err := e.svc.ResetBranch(ctx, name("nope"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestResetBranch_失敗したら失敗状態にする(t *testing.T) {
	for _, op := range []string{"rollback", "start"} {
		e := newEnv(t)
		e.withBase(t)
		e.withBranch(t, "x")
		if op == "rollback" {
			e.storage.failOn = op
		} else {
			e.server.failOn = op
		}
		if _, err := e.svc.ResetBranch(ctx, name("x"), nil); err == nil {
			t.Errorf("%s: want error", op)
		}
		b, _ := e.store.GetBranch(ctx, name("x"))
		if b.Status != domain.BranchFailed {
			t.Errorf("%s: status %s", op, b.Status)
		}
	}
}

func TestDeleteBranch_サーバーとデータセットと記録を消す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "feature-x")

	if err := e.svc.DeleteBranch(ctx, name("feature-x"), nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"remove branch-feature-x", "zfs destroy tank/db-branch/branches/feature-x"}
	if !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal %q", e.j.entries)
	}
	if _, err := e.store.GetBranch(ctx, name("feature-x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("still stored: %v", err)
	}
}

func TestDeleteBranch_失敗したブランチも消せてデータセットがなければ飛ばす(t *testing.T) {
	e := newEnv(t)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("broken"), Runtime: mysql84, Status: domain.BranchFailed})

	if err := e.svc.DeleteBranch(ctx, name("broken"), nil); err != nil {
		t.Fatal(err)
	}
	if e.j.has("zfs destroy tank/db-branch/branches/broken") {
		t.Errorf("should skip missing dataset: %q", e.j.entries)
	}
}

func TestDeleteBranch_消せなければ失敗状態にして記録を残す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	e.storage.failOn = "destroy"

	if err := e.svc.DeleteBranch(ctx, name("x"), nil); err == nil {
		t.Fatal("want error")
	}
	b, err := e.store.GetBranch(ctx, name("x"))
	if err != nil || b.Status != domain.BranchFailed {
		t.Errorf("branch = %+v, %v", b, err)
	}
}

func TestDeleteBranch_処理中や存在しないブランチは失敗する(t *testing.T) {
	e := newEnv(t)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("busy"), Status: domain.BranchResetting})
	if err := e.svc.DeleteBranch(ctx, name("busy"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("busy: %v", err)
	}
	if err := e.svc.DeleteBranch(ctx, name("nope"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestExtendBranch_今からTTL分に延ばす(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	e.now = e.now.Add(time.Hour)

	b, err := e.svc.ExtendBranch(ctx, name("x"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !b.ExpiresAt.Equal(e.now.Add(24 * time.Hour)) {
		t.Errorf("expires = %v", b.ExpiresAt)
	}
	if _, err := e.svc.ExtendBranch(ctx, name("x"), 0); !errors.Is(err, domain.ErrInvalidTTL) {
		t.Errorf("zero ttl: %v", err)
	}
	if _, err := e.svc.ExtendBranch(ctx, name("nope"), time.Hour); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestExpire_期限切れのブランチだけを消す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	_, _ = e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("short"), Base: name("prod"), TTL: time.Hour}, nil)
	_, _ = e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("long"), Base: name("prod"), TTL: 48 * time.Hour}, nil)
	_, _ = e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("forever"), Base: name("prod")}, nil)
	e.now = e.now.Add(2 * time.Hour)

	deleted, err := e.svc.Expire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0].String() != "short" {
		t.Errorf("deleted = %v", deleted)
	}
	list, _ := e.svc.ListBranches(ctx)
	if len(list) != 2 {
		t.Errorf("remaining = %v", list)
	}
}

func TestRecover_エージェント再起動後に中断した処理を整える(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "ready")
	e.withBranch(t, "resetting")
	e.withBranch(t, "deleting")
	put := func(n string, s domain.BranchStatus) {
		b, _ := e.store.GetBranch(ctx, name(n))
		b.Status = s
		_ = e.store.PutBranch(ctx, b)
	}
	put("resetting", domain.BranchResetting)
	put("deleting", domain.BranchDeleting)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("half"), Runtime: mysql84, Status: domain.BranchProvisioning})
	e.server.running = map[string]bool{}

	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.j.has("start branch-ready") {
		t.Errorf("ready branch should be started: %q", e.j.entries)
	}
	if !e.j.has("zfs rollback tank/db-branch/branches/resetting@reset") {
		t.Errorf("reset should be resumed: %q", e.j.entries)
	}
	if _, err := e.store.GetBranch(ctx, name("deleting")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("delete should be resumed: %v", err)
	}
	half, _ := e.store.GetBranch(ctx, name("half"))
	if half.Status != domain.BranchFailed || half.Error == "" {
		t.Errorf("interrupted create = %+v", half)
	}
	r, _ := e.store.GetBranch(ctx, name("resetting"))
	if r.Status != domain.BranchReady {
		t.Errorf("resetting -> %s", r.Status)
	}
}

func TestBranchServer_ブランチのサーバー情報(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")

	got, err := e.svc.GetBranch(ctx, name("x"))
	if err != nil || got.Name.String() != "x" {
		t.Fatalf("GetBranch = %+v, %v", got, err)
	}
	info, err := e.svc.BranchStorage(ctx, name("x"))
	if err != nil || info.Used != 1<<20 {
		t.Errorf("BranchStorage = %+v, %v", info, err)
	}
}

func TestSetLimits_メモリとポートの範囲を動かしながら変えられる(t *testing.T) {
	e := newEnv(t)
	e.withBase(t) // port 40000
	e.svc.SetLimits(512, dbbranch.PortRange{Min: 41000, Max: 41001})
	b := e.withBranch(t, "x")
	if b.Port != 41000 {
		t.Errorf("port = %d", b.Port)
	}
	if e.server.memory["branch-x"] != 512 {
		t.Errorf("memory = %d", e.server.memory["branch-x"])
	}
}
