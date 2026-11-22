package dbbranch_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func (e *env) limits(l dbbranch.Limits) { e.svc.ResourceLimits = func() dbbranch.Limits { return l } }

func (e *env) pool(size, free uint64) {
	e.svc.PoolSpace = func(context.Context) (uint64, uint64, error) { return size, free, nil }
}

func create(e *env, n, owner string, ttl time.Duration) error {
	_, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: name(n), Base: name("prod"), Owner: owner, TTL: ttl}, nil)
	return err
}

func TestCreateBranch_refquotaをブランチのデータセットに設定する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	rec := &setPropsRecorder{}
	e.svc.Lineage = rec
	e.limits(dbbranch.Limits{RefQuotaBytes: 5 << 30})
	if err := create(e, "a", "", 0); err != nil {
		t.Fatal(err)
	}
	if got := rec.sets["tank/db-branch/branches/a"]["refquota"]; got != "5368709120" {
		t.Errorf("refquota = %q, sets = %v", got, rec.sets)
	}
}

func TestCreateBranch_refquotaが無制限なら何も設定しない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	rec := &setPropsRecorder{}
	e.svc.Lineage = rec
	if err := create(e, "a", "", 0); err != nil {
		t.Fatal(err)
	}
	if len(rec.sets) != 0 {
		t.Errorf("sets = %v", rec.sets)
	}
}

func TestCreateBranch_プール残量が1GiB未満なら拒否する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.pool(100<<30, 1<<30-1)
	if err := create(e, "a", "", 0); !errors.Is(err, domain.ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	if _, err := e.store.GetBranch(ctx, name("a")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("nothing is reserved: %v", err)
	}
}

func TestCreateBranch_プール残量が10パーセント未満なら拒否し以上なら通す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.pool(1000<<30, 99<<30)
	if err := create(e, "a", "", 0); !errors.Is(err, domain.ErrNoSpace) {
		t.Fatalf("below 10%%: %v", err)
	}
	e.pool(1000<<30, 100<<30)
	if err := create(e, "a", "", 0); err != nil {
		t.Fatalf("at 10%%: %v", err)
	}
}

func TestCreateBranch_残量が分からないプールでは止めない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.pool(0, 0)
	if err := create(e, "a", "", 0); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBranch_プール残量の取得に失敗したらそのエラーを返す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	boom := errors.New("zfs get failed")
	e.svc.PoolSpace = func(context.Context) (uint64, uint64, error) { return 0, 0, boom }
	if err := create(e, "a", "", 0); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateBranch_ユーザー別の最大ブランチ数を超えたら拒否する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.limits(dbbranch.Limits{MaxBranchesPerUser: 2})
	for _, n := range []string{"a", "b"} {
		if err := create(e, n, "alice", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := create(e, "c", "alice", 0); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("third of alice: %v", err)
	}
	if err := create(e, "d", "bob", 0); err != nil {
		t.Errorf("another user is not affected: %v", err)
	}
	if err := create(e, "e", "", 0); err != nil {
		t.Errorf("a branch without an owner is not limited per user: %v", err)
	}
}

func TestCreateBranch_最大TTLを超える指定は拒否し指定なしは最大TTLにする(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.limits(dbbranch.Limits{MaxTTL: 48 * time.Hour})
	if err := create(e, "a", "", 72*time.Hour); !errors.Is(err, domain.ErrInvalidTTL) {
		t.Fatalf("over the maximum: %v", err)
	}
	if err := create(e, "b", "", 0); err != nil {
		t.Fatal(err)
	}
	b, _ := e.store.GetBranch(ctx, name("b"))
	if b.ExpiresAt == nil || !b.ExpiresAt.Equal(e.now.Add(48*time.Hour)) {
		t.Errorf("expires = %v, want now+48h", b.ExpiresAt)
	}
	if err := create(e, "c", "", 24*time.Hour); err != nil {
		t.Errorf("within the maximum: %v", err)
	}
}

func TestExtendBranch_最大TTLを超える延長は拒否する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "a")
	e.limits(dbbranch.Limits{MaxTTL: 48 * time.Hour})
	if _, err := e.svc.ExtendBranch(ctx, name("a"), 72*time.Hour); !errors.Is(err, domain.ErrInvalidTTL) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.ExtendBranch(ctx, name("a"), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBase_LoadBase_CreateCheckpoint_RefreshBaseもプール残量が足りなければ拒否する(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "a")
	e.svc.Lineage = &setPropsRecorder{}
	e.pool(100<<30, 1<<20)
	if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("other"), Runtime: mysql84, Dump: "/d.sql"}, nil); !errors.Is(err, domain.ErrNoSpace) {
		t.Errorf("CreateBase: %v", err)
	}
	if err := e.svc.LoadBase(ctx, name("prod"), "/d.sql", nil); !errors.Is(err, domain.ErrNoSpace) {
		t.Errorf("LoadBase: %v", err)
	}
	if _, err := e.svc.CreateCheckpoint(ctx, name("a"), "cp"); !errors.Is(err, domain.ErrNoSpace) {
		t.Errorf("CreateCheckpoint: %v", err)
	}
}

func TestRefreshBase_プール残量が足りなければダンプせずに拒否する(t *testing.T) {
	e := newEnv(t)
	d := e.withRefresh(t)
	e.pool(100<<30, 1<<20)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); !errors.Is(err, domain.ErrNoSpace) {
		t.Fatalf("err = %v", err)
	}
	if len(d.jobs) != 0 {
		t.Errorf("dumped %d times", len(d.jobs))
	}
}
