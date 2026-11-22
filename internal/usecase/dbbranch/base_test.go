package dbbranch_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func TestCreateBase_初期化しダンプを入れ正常停止してからスナップショットを取る(t *testing.T) {
	e := newEnv(t)
	var phases []string

	b, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84, Dump: "/d.sql"},
		func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"zfs create tank/db-branch/bases/prod",
		"init base-prod mysql@8.4.13 port=40000",
		"start base-prod",
		"load base-prod /d.sql",
		"analyze base-prod",
		"stop base-prod clean",
		"zfs snapshot tank/db-branch/bases/prod@s20261109120000",
	}
	if !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal:\n got %q\nwant %q", e.j.entries, want)
	}
	if b.Status != domain.BaseReady || len(b.Snapshots) != 1 || b.Snapshots[0].Name != "s20261109120000" {
		t.Errorf("base = %+v", b)
	}
	if !reflect.DeepEqual(phases, []string{"creating", "initializing", "loading", "analyzing", "stopping", "snapshotting"}) {
		t.Errorf("phases = %q", phases)
	}
	stored, _ := e.store.GetBase(ctx, name("prod"))
	if !reflect.DeepEqual(stored, b) {
		t.Errorf("stored %+v", stored)
	}
}

func TestCreateBase_ダンプなしなら空のベースを作る(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("empty"), Runtime: mysql84}, nil); err != nil {
		t.Fatal(err)
	}
	for _, entry := range e.j.entries {
		if len(entry) > 4 && entry[:4] == "load" {
			t.Errorf("unexpected %q", entry)
		}
	}
}

func TestCreateBase_前提を満たさなければ何もせずに失敗する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	cases := map[string]struct {
		req  dbbranch.CreateBaseRequest
		want error
	}{
		"duplicate":          {dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84}, domain.ErrAlreadyExists},
		"runtime missing":    {dbbranch.CreateBaseRequest{Name: name("x"), Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "9.0.0"}}, domain.ErrRuntimeNotInstalled},
		"engine unsupported": {dbbranch.CreateBaseRequest{Name: name("y"), Runtime: domain.RuntimeRef{Engine: domain.Postgres, Version: "17.2"}}, domain.ErrUnsupportedEngine},
	}
	e.svc.Runtimes = fakeRuntimes{installed: map[domain.RuntimeRef]bool{mysql84: true, {Engine: domain.Postgres, Version: "17.2"}: true}}
	for label, c := range cases {
		e.j.entries = nil
		if _, err := e.svc.CreateBase(ctx, c.req, nil); !errors.Is(err, c.want) {
			t.Errorf("%s: error = %v, want %v", label, err, c.want)
		}
		if len(e.j.entries) != 0 {
			t.Errorf("%s: did work: %q", label, e.j.entries)
		}
	}
}

func TestCreateBase_途中で失敗したら失敗状態と理由を残す(t *testing.T) {
	for _, op := range []string{"create", "info", "init", "start", "load", "analyze", "stop-clean", "snapshot"} {
		e := newEnv(t)
		e.storage.failOn = map[string]string{"create": "create", "info": "info", "snapshot": "snapshot"}[op]
		if e.storage.failOn == "" {
			e.server.failOn = op
		}
		_, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84, Dump: "/d.sql"}, nil)
		if err == nil {
			t.Errorf("%s: want error", op)
			continue
		}
		b, _ := e.store.GetBase(ctx, name("prod"))
		if b.Status != domain.BaseFailed || b.Error == "" {
			t.Errorf("%s: base = %+v", op, b)
		}
		if op == "stop-clean" && e.storage.snaps["tank/db-branch/bases/prod@s20261109120000"] {
			t.Error("must not snapshot after an unclean shutdown")
		}
	}
}

func TestSnapshotBase_動いていたベースは止めて取ってから再開する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.server.running["base-prod"] = true
	e.now = e.now.Add(time.Hour)

	snap, err := e.svc.SnapshotBase(ctx, name("prod"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Name != "s20261109130000" {
		t.Errorf("snapshot = %+v", snap)
	}
	want := []string{"analyze base-prod", "stop base-prod clean", "zfs snapshot tank/db-branch/bases/prod@s20261109130000", "start base-prod"}
	if !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal %q", e.j.entries)
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	if b.Status != domain.BaseReady || len(b.Snapshots) != 2 {
		t.Errorf("base = %+v", b)
	}
}

func TestSnapshotBase_止まっていたベースは統計を更新するため一度起こして止めたままにする(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)

	if _, err := e.svc.SnapshotBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"start base-prod", "analyze base-prod", "stop base-prod clean", "zfs snapshot tank/db-branch/bases/prod@s20261109120000"}
	if !reflect.DeepEqual(e.j.entries, want) {
		t.Errorf("journal %q", e.j.entries)
	}
	if e.server.running["base-prod"] {
		t.Error("the base must be stopped again")
	}
}

func TestSnapshotBase_止まっていたベースが起動できなければ取らない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.server.failOn = "start"
	if _, err := e.svc.SnapshotBase(ctx, name("prod"), nil); err == nil {
		t.Fatal("want error")
	}
	if e.j.has("zfs snapshot tank/db-branch/bases/prod@s20261109120000") {
		t.Errorf("journal %q", e.j.entries)
	}
}

func TestSnapshotBase_正常停止できなければ取らずに元の状態へ戻す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.server.running["base-prod"] = true
	e.server.failOn = "stop-clean"
	e.now = e.now.Add(time.Hour)

	if _, err := e.svc.SnapshotBase(ctx, name("prod"), nil); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Fatalf("error = %v", err)
	}
	if e.j.has("zfs snapshot tank/db-branch/bases/prod@s20261109130000") {
		t.Error("must not snapshot")
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	if b.Status != domain.BaseReady || len(b.Snapshots) != 1 {
		t.Errorf("base = %+v", b)
	}
}

func TestSnapshotBase_スナップショットに失敗しても再開して元の状態へ戻す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.server.running["base-prod"] = true
	e.storage.failOn = "snapshot"

	if _, err := e.svc.SnapshotBase(ctx, name("prod"), nil); err == nil {
		t.Fatal("want error")
	}
	if !e.j.has("start base-prod") {
		t.Errorf("should restart: %q", e.j.entries)
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	if b.Status != domain.BaseReady {
		t.Errorf("status = %s", b.Status)
	}
}

func TestSnapshotBase_準備中や存在しないベースは失敗する(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SnapshotBase(ctx, name("nope"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	_ = e.store.PutBase(ctx, domain.Base{Name: name("busy"), Runtime: mysql84, Status: domain.BaseMaintenance})
	if _, err := e.svc.SnapshotBase(ctx, name("busy"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("busy: %v", err)
	}
}

func TestStartStopBase_ベースのサーバーを起動停止する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)

	if err := e.svc.StartBase(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.StopBase(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.j.entries, []string{"start base-prod", "stop base-prod clean"}) {
		t.Errorf("journal %q", e.j.entries)
	}
	if err := e.svc.StartBase(ctx, name("nope")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if err := e.svc.StopBase(ctx, name("nope")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestDeleteBase_ブランチが使っていれば消さない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "feature-x")

	if err := e.svc.DeleteBase(ctx, name("prod")); !errors.Is(err, domain.ErrInUse) {
		t.Fatalf("error = %v, want ErrInUse", err)
	}
}

func TestDeleteBase_サーバーとデータセットと記録を消す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)

	if err := e.svc.DeleteBase(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.j.entries, []string{"remove base-prod", "zfs destroy tank/db-branch/bases/prod"}) {
		t.Errorf("journal %q", e.j.entries)
	}
	if _, err := e.store.GetBase(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("still stored: %v", err)
	}
	if err := e.svc.DeleteBase(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestDeleteBase_データセットを消せなければ記録を残す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.storage.failOn = "destroy"

	if err := e.svc.DeleteBase(ctx, name("prod")); err == nil {
		t.Fatal("want error")
	}
	if _, err := e.store.GetBase(ctx, name("prod")); err != nil {
		t.Errorf("record should remain: %v", err)
	}
}

func TestListBases_一覧と取得(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	list, err := e.svc.ListBases(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListBases = %v, %v", list, err)
	}
	if _, err := e.svc.GetBase(ctx, name("prod")); err != nil {
		t.Error(err)
	}
}

func TestInit_ベースとブランチの親データセットをDB向けの設定で作る(t *testing.T) {
	e := newEnv(t)
	var props map[string]string
	e.svc.Storage = &recordingStorage{fakeStorage: e.storage, props: &props}

	if err := e.svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.j.has("zfs create tank/db-branch") {
		t.Errorf("journal %q", e.j.entries)
	}
	want := map[string]string{"compression": "lz4", "atime": "off", "xattr": "sa", "recordsize": "16K"}
	if !reflect.DeepEqual(props, want) {
		t.Errorf("props = %v", props)
	}
	if err := e.svc.Init(ctx); err != nil {
		t.Errorf("second init should be a no-op: %v", err)
	}
}

type recordingStorage struct {
	*fakeStorage
	props *map[string]string
}

func (r *recordingStorage) Create(ctx context.Context, ds string, props map[string]string) error {
	*r.props = props
	return r.fakeStorage.Create(ctx, ds, props)
}

func TestCreateBaseとSnapshotBase_途中で失敗したとき起動済みサーバーを放置しない(t *testing.T) {
	// CreateBase: Load, UpdateStats, or StopClean failure must stop the base server.
	for _, op := range []string{"load", "analyze", "stop-clean"} {
		e := newEnv(t)
		e.server.failOn = op
		if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84, Dump: "/d.sql"}, nil); err == nil {
			t.Fatalf("CreateBase(%s): want error", op)
		}
		if e.server.running["base-prod"] {
			t.Errorf("CreateBase(%s): base server left running: %q", op, e.j.entries)
		}
	}

	// SnapshotBase on a stopped base: if UpdateStats or StopClean fails after starting it, stop it again.
	for _, op := range []string{"analyze", "stop-clean"} {
		e := newEnv(t)
		e.withBase(t)
		e.server.failOn = op
		if _, err := e.svc.SnapshotBase(ctx, name("prod"), nil); err == nil {
			t.Fatalf("SnapshotBase(%s): want error", op)
		}
		if e.server.running["base-prod"] {
			t.Errorf("SnapshotBase(%s): stopped base left running: %q", op, e.j.entries)
		}
	}

	// SnapshotBase on a running base: if UpdateStats fails, do not start it a second time.
	er := newEnv(t)
	er.withBase(t)
	er.server.running["base-prod"] = true
	er.server.failOn = "analyze"
	if _, err := er.svc.SnapshotBase(ctx, name("prod"), nil); err == nil {
		t.Fatal("SnapshotBase running (analyze): want error")
	}
	if er.j.has("start base-prod") {
		t.Errorf("SnapshotBase restarted an already-running base: %q", er.j.entries)
	}
}
