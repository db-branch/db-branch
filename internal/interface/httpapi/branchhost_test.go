package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

func (f *fakeBranches) RestoreCheckpointWith(_ context.Context, n domain.Name, cp, note string, rep dbbranch.Reporter) (domain.Branch, error) {
	f.record(fmt.Sprintf("restore-with %s %s %q", n, cp, note))
	return domain.Branch{}, f.cpErr
}

func (f *fakeBranches) BranchAdmin(_ context.Context, n domain.Name) (domain.AdminAccess, error) {
	f.record("admin " + n.String())
	sock := "/run/" + n.String() + ".sock"
	return domain.AdminAccess{Engine: domain.MySQL, Socket: sock, User: "root", BinDir: "/rt/bin", LibDir: "/rt/lib",
		Endpoints: map[string]domain.Endpoint{"mysql": {Socket: sock, Port: 3306}}, Env: map[string]string{"MYSQL_UNIX_PORT": sock}}, f.cpErr
}

func (f *fakeBranches) OpenLine(_ context.Context, n domain.Name, line int, cp string) (string, error) {
	f.record(fmt.Sprintf("open %s %d %q", n, line, cp))
	return "/mnt/line", f.cpErr
}

func (f *fakeBranches) CloseLine(_ context.Context, dir string) error {
	f.record("close " + dir)
	return f.cpErr
}

func hostFixture(t *testing.T) (*fixture, ext.BranchHost) {
	f := checkpointFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady, Owner: "alice", Record: true, CreatedAt: t0, Base: name("prod"), Snapshot: "s1"}
	f.branches.branches["anon"] = domain.Branch{Name: name("anon"), Runtime: mysql84, Status: domain.BranchReady, Owner: "anonymous", Access: domain.AccessOpen}
	return f, f.srv.BranchHost()
}

func TestBranchHost_自分のノードのブランチを見せる(t *testing.T) {
	f, h := hostFixture(t)
	b, err := h.Branch(ctx, "x")
	if err != nil || b.Owner != "alice" || b.Engine != "mysql" || b.Version != "8.4.13" || !b.Record || b.Anonymous || b.Status != "ready" || b.Base != "prod" || b.Snapshot != "s1" {
		t.Errorf("branch = %+v %v", b, err)
	}
	if a, _ := h.Branch(ctx, "anon"); !a.Anonymous {
		t.Error("an anonymous branch should say so")
	}
	if _, err := h.Branch(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := h.Branch(ctx, "BAD_NAME"); !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("bad name: %v", err)
	}
	list, err := h.Branches(ctx)
	if err != nil || len(list) != 2 {
		t.Errorf("list = %v %v", list, err)
	}
	f.srv.Cluster = &fakeCluster{}
	f.branches.branches["there"] = domain.Branch{Name: name("there"), Runtime: mysql84, Status: domain.BranchReady, Node: "n2"}
	if _, err := h.Branch(ctx, "there"); !errors.Is(err, dbbranch.ErrOtherNode) {
		t.Errorf("other node: %v", err)
	}
	if list, _ := h.Branches(ctx); len(list) != 2 {
		t.Errorf("another node's branch must not be listed: %v", list)
	}
}

func TestBranchHost_チェックポイントの構造を返し作成と復元とリセットを行う(t *testing.T) {
	f, h := hostFixture(t)
	zero := 0
	f.branches.graph = domain.Graph{Live: 1, Head: "c1", Lines: []domain.LineView{{Line: 0}, {Line: 1, Live: true, Parent: &zero, From: "c1"}}}
	g, err := h.Graph(ctx, "x")
	if err != nil || g.Live != 1 || g.Head != "c1" || len(g.Checkpoints) != 2 || g.Lines[0].Parent != ext.NoLine || g.Lines[1].Parent != 0 || g.Lines[1].From != "c1" {
		t.Errorf("graph = %+v %v", g, err)
	}
	if c, err := h.CreateCheckpoint(ctx, "x", "n1"); err != nil || c.Name != "n1" {
		t.Errorf("checkpoint = %+v %v", c, err)
	}
	if err := h.RestoreCheckpoint(ctx, "x", "c1", "line=0"); err != nil {
		t.Error(err)
	}
	if err := h.RestoreCheckpoint(ctx, "x", "@reset", "line=0"); err != nil {
		t.Error(err)
	}
	if got := f.branches.calls[len(f.branches.calls)-2:]; got[0] != `restore-with x c1 "line=0"` || got[1] != `restore-with x @reset "line=0"` {
		t.Errorf("calls = %v", f.branches.calls)
	}
	if err := h.RestoreCheckpoint(ctx, "x", "Bad Name", ""); !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("bad checkpoint: %v", err)
	}
	b := f.branches.branches["x"]
	b.Status = domain.BranchResetting
	f.branches.branches["x"] = b
	if err := h.RestoreCheckpoint(ctx, "x", "c1", ""); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("busy restore: %v", err)
	}
	f.branches.cpErr = errors.New("no zfs")
	for name, call := range map[string]func() error{
		"graph":      func() error { _, err := h.Graph(ctx, "x"); return err },
		"checkpoint": func() error { _, err := h.CreateCheckpoint(ctx, "x", ""); return err },
	} {
		if call() == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestBranchHost_サーバーへの入口と線の読み取りを渡す(t *testing.T) {
	f, h := hostFixture(t)
	db, err := h.DB(ctx, "x")
	if err != nil || db.Engine != "mysql" || db.Socket != "/run/x.sock" || db.BinDir != "/rt/bin" || db.LibDir != "/rt/lib" {
		t.Errorf("db = %+v %v", db, err)
	}
	if db.Endpoints["mysql"] != (ext.Endpoint{Socket: "/run/x.sock", Port: 3306}) || db.Env["MYSQL_UNIX_PORT"] != "/run/x.sock" {
		t.Errorf("endpoints and env were not passed to the module: %+v", db)
	}
	dir, err := h.OpenLine(ctx, "x", 2, "c1")
	if err != nil || dir != "/mnt/line" {
		t.Errorf("open = %q %v", dir, err)
	}
	if err := h.CloseLine(ctx, dir); err != nil {
		t.Error(err)
	}
	f.branches.cpErr = errors.New("busy")
	if _, err := h.DB(ctx, "x"); err == nil {
		t.Error("db: want error")
	}
	if _, err := h.OpenLine(ctx, "x", 0, ""); err == nil {
		t.Error("open: want error")
	}
	if err := h.CloseLine(ctx, dir); err == nil {
		t.Error("close: want error")
	}
	for _, call := range []func() error{
		func() error { _, err := h.DB(ctx, "nope"); return err },
		func() error { _, err := h.OpenLine(ctx, "nope", 0, ""); return err },
		func() error { return h.RestoreCheckpoint(ctx, "nope", "c1", "") },
		func() error { _, err := h.CreateCheckpoint(ctx, "nope", ""); return err },
		func() error { _, err := h.Graph(ctx, "nope"); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("unknown branch: %v", err)
		}
	}
}

func TestBranchHost_初期化前やストレージが対応しない場合は断る(t *testing.T) {
	f, h := hostFixture(t)
	f.srv.Branches = func() (httpapi.Branches, error) { return nil, errors.New("not initialized") }
	if _, err := h.Branches(ctx); err == nil {
		t.Error("branches: want error")
	}
	if err := h.CloseLine(ctx, "/x"); err == nil {
		t.Error("close: want error")
	}
	f.srv.Branches = func() (httpapi.Branches, error) { return plainBranches{f.branches}, nil }
	if _, err := h.Branch(ctx, "x"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("no host service: %v", err)
	}
}

// plainBranches hides the host-only methods of the fake.
type plainBranches struct{ httpapi.Branches }

func TestBranchHost_モジュールの操作を開始から終了まで表示する(t *testing.T) {
	f, h := hostFixture(t)
	id, err := h.StartOperation(ctx, "history.restore", "x")
	if err != nil {
		t.Fatal(err)
	}
	if op, ok := f.ops.Active("branch", "x"); !ok || op.Kind != "history.restore" || op.ID != id {
		t.Fatalf("active = %+v %v", op, ok)
	}
	if err := h.ReportOperation(ctx, id, "replaying"); err != nil {
		t.Fatal(err)
	}
	if op, _ := f.ops.Get(id); op.Phase != "replaying" {
		t.Errorf("phase = %q", op.Phase)
	}
	if err := h.EndOperation(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if op := f.waitOp(t, id); op.State != operation.Succeeded {
		t.Errorf("op = %+v", op)
	}
	if err := h.ReportOperation(ctx, id, "late"); !errors.Is(err, operation.ErrNotFound) {
		t.Errorf("after the end: %v", err)
	}
	if err := h.EndOperation(ctx, id, ""); !errors.Is(err, operation.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
	id2, _ := h.StartOperation(ctx, "history.restore", "x")
	_ = h.EndOperation(ctx, id2, "binlog is gone")
	if op := f.waitOp(t, id2); op.State != operation.Failed || op.Error != "binlog is gone" {
		t.Errorf("failed op = %+v", op)
	}
	if _, err := h.StartOperation(ctx, "", "x"); !errors.Is(err, domain.ErrInvalidName) {
		t.Errorf("no kind: %v", err)
	}
	if _, err := h.StartOperation(ctx, "history.restore", "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown branch: %v", err)
	}
}

func TestBranchHost_終わらない操作は時間切れで失敗にする(t *testing.T) {
	old := httpapi.ModuleOpTimeout
	httpapi.ModuleOpTimeout = 30 * time.Millisecond
	t.Cleanup(func() { httpapi.ModuleOpTimeout = old })
	f, h := hostFixture(t)
	id, _ := h.StartOperation(ctx, "history.restore", "x")
	if op := f.waitOp(t, id); op.State != operation.Failed {
		t.Errorf("op = %+v", op)
	}
}

func TestBranchHost_チームによるブランチ共有の可否を仲介する(t *testing.T) {
	f, h := hostFixture(t)
	sh, ok := h.(ext.BranchSharer)
	if !ok {
		t.Fatal("BranchHost should implement ext.BranchSharer")
	}
	bob := ext.Principal{ID: "token:b", Roles: []string{"developer"}}
	if sh.SharesBranches(ctx, bob, "token:a") {
		t.Error("without policy: want false")
	}
	f.srv.Ext = ext.Module{BranchPolicy: &teamPolicy{}}
	if !sh.SharesBranches(ctx, bob, "token:a") {
		t.Error("teammate should share")
	}
	if sh.SharesBranches(ctx, bob, "anonymous") || sh.SharesBranches(ctx, bob, "") || sh.SharesBranches(ctx, bob, "token:c") {
		t.Error("must not share anonymous, empty, or outsider")
	}
}
