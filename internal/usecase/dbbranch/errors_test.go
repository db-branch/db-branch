package dbbranch_test

import (
	"context"
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

// failingStore wraps the real store and fails the named operation.
type failingStore struct {
	domain.Store
	failOn string
}

var errStore = errors.New("store failed")

func (f *failingStore) ListBases(ctx context.Context) ([]domain.Base, error) {
	if f.failOn == "list-bases" {
		return nil, errStore
	}
	return f.Store.ListBases(ctx)
}

func (f *failingStore) ListBranches(ctx context.Context) ([]domain.Branch, error) {
	if f.failOn == "list-branches" {
		return nil, errStore
	}
	return f.Store.ListBranches(ctx)
}

func TestストアやDBサーバーの失敗はそのまま返す(t *testing.T) {
	cases := map[string]func(e *env) error{
		"port: list bases": func(e *env) error {
			e.svc.Store = &failingStore{Store: e.store, failOn: "list-bases"}
			_, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("x"), Base: name("prod")}, nil)
			return err
		},
		"port: list branches": func(e *env) error {
			e.svc.Store = &failingStore{Store: e.store, failOn: "list-branches"}
			_, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("other"), Runtime: mysql84}, nil)
			return err
		},
		"delete base: list branches": func(e *env) error {
			e.svc.Store = &failingStore{Store: e.store, failOn: "list-branches"}
			return e.svc.DeleteBase(ctx, name("prod"))
		},
		"delete base: remove server": func(e *env) error {
			e.server.failOn = "remove"
			return e.svc.DeleteBase(ctx, name("prod"))
		},
		"expire: list": func(e *env) error {
			e.svc.Store = &failingStore{Store: e.store, failOn: "list-branches"}
			_, err := e.svc.Expire(ctx)
			return err
		},
		"recover: list": func(e *env) error {
			e.svc.Store = &failingStore{Store: e.store, failOn: "list-branches"}
			return e.svc.Recover(ctx)
		},
		"start base: info": func(e *env) error {
			e.storage.failOn = "info"
			return e.svc.StartBase(ctx, name("prod"))
		},
		"stop base: info": func(e *env) error {
			e.storage.failOn = "info"
			return e.svc.StopBase(ctx, name("prod"))
		},
		"snapshot base: info": func(e *env) error {
			e.storage.failOn = "info"
			_, err := e.svc.SnapshotBase(ctx, name("prod"), nil)
			return err
		},
		"delete base: exists": func(e *env) error {
			e.storage.failOn = "exists"
			return e.svc.DeleteBase(ctx, name("prod"))
		},
	}
	for label, op := range cases {
		e := newEnv(t)
		e.withBase(t)
		if err := op(e); err == nil {
			t.Errorf("%s: want error", label)
		}
	}
}

func Test対応していないエンジンのベースやブランチは操作できない(t *testing.T) {
	e := newEnv(t)
	pg := domain.RuntimeRef{Engine: domain.Postgres, Version: "17.2"}
	_ = e.store.PutBase(ctx, domain.Base{Name: name("pg"), Runtime: pg, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s1"}}})
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("pgb"), Base: name("pg"), Runtime: pg, Status: domain.BranchReady})

	checks := map[string]error{
		"start base":  e.svc.StartBase(ctx, name("pg")),
		"stop base":   e.svc.StopBase(ctx, name("pg")),
		"delete base": func() error { _ = e.store.DeleteBranch(ctx, name("pgb")); return e.svc.DeleteBase(ctx, name("pg")) }(),
	}
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("pgb"), Base: name("pg"), Runtime: pg, Status: domain.BranchReady})
	_, checks["reset"] = e.svc.ResetBranch(ctx, name("pgb"), nil)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("pgb"), Base: name("pg"), Runtime: pg, Status: domain.BranchReady})
	checks["delete branch"] = e.svc.DeleteBranch(ctx, name("pgb"), nil)
	_ = e.store.PutBranch(ctx, domain.Branch{Name: name("pgb"), Base: name("pg"), Runtime: pg, Status: domain.BranchReady})
	checks["recover"] = e.svc.Recover(ctx)
	_, checks["create branch"] = e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name("pgc"), Base: name("pg")}, nil)
	for label, err := range checks {
		if !errors.Is(err, domain.ErrUnsupportedEngine) {
			t.Errorf("%s: error = %v, want ErrUnsupportedEngine", label, err)
		}
	}
}

func Testリセットと削除の途中の失敗は失敗状態にする(t *testing.T) {
	cases := map[string]func(e *env) error{
		"reset: info": func(e *env) error {
			e.storage.failOn = "info"
			_, err := e.svc.ResetBranch(ctx, name("x"), nil)
			return err
		},
		"reset: stop": func(e *env) error {
			e.server.failOn = "stop-immediate"
			_, err := e.svc.ResetBranch(ctx, name("x"), nil)
			return err
		},
		"delete: remove": func(e *env) error {
			e.server.failOn = "remove"
			return e.svc.DeleteBranch(ctx, name("x"), nil)
		},
	}
	for label, op := range cases {
		e := newEnv(t)
		e.withBase(t)
		e.withBranch(t, "x")
		if err := op(e); err == nil {
			t.Errorf("%s: want error", label)
		}
		b, _ := e.store.GetBranch(ctx, name("x"))
		if b.Status != domain.BranchFailed {
			t.Errorf("%s: status = %s", label, b.Status)
		}
	}
}

func Test期限切れの削除に失敗しても他は続けてエラーを返す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	for _, n := range []string{"a", "b"} {
		_, _ = e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name(n), Base: name("prod"), TTL: 1}, nil)
	}
	e.now = e.now.Add(1e9)
	e.server.failOn = "remove"

	deleted, err := e.svc.Expire(ctx)
	if err == nil || len(deleted) != 0 {
		t.Errorf("deleted = %v, err = %v", deleted, err)
	}
}

func Test起動中のブランチは復旧時に起動し直さず起動できなければエラーにする(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	if err := e.svc.Recover(ctx); err != nil || e.j.has("start branch-x") {
		t.Errorf("running branch: err=%v journal=%q", err, e.j.entries)
	}
	e.server.running = map[string]bool{}
	e.storage.failOn = "info"
	if err := e.svc.Recover(ctx); err == nil {
		t.Error("info failure: want error")
	}
}

func Test既にあるベース名やランタイム確認の失敗(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.svc.Store = &failingStore{Store: e.store, failOn: "list-bases"}
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("other"), Runtime: mysql84}, nil); !errors.Is(err, errStore) {
		t.Errorf("error = %v", err)
	}
}

func (f *failingStore) SetBaseReplicaNodes(ctx context.Context, n domain.Name, nodes *[]string) error {
	if f.failOn == "set-replicas" {
		return errStore
	}
	return f.Store.SetBaseReplicaNodes(ctx, n, nodes)
}

func (f *failingStore) RemoveReplica(ctx context.Context, n domain.Name, node string) error {
	if f.failOn == "remove-replica" {
		return errStore
	}
	return f.Store.RemoveReplica(ctx, n, node)
}

func (f *failingStore) SetBaseAnonymous(ctx context.Context, n domain.Name, l domain.AnonymousLimits) error {
	if f.failOn == "set-anonymous" {
		return errStore
	}
	return f.Store.SetBaseAnonymous(ctx, n, l)
}
