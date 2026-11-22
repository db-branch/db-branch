package dbbranch_test

import (
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestResetBranch_実行中にExtendBranchされたTTLが完了時に消えない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	var extended time.Time
	e.server.onStop = func() {
		e.server.onStop = nil
		b, err := e.svc.ExtendBranch(ctx, name("x"), 72*time.Hour)
		if err != nil {
			t.Error(err)
		}
		extended = *b.ExpiresAt
	}

	if _, err := e.svc.ResetBranch(ctx, name("x"), nil); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetBranch(ctx, name("x"))
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(extended) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, extended)
	}
	if got.Status != domain.BranchReady {
		t.Errorf("status = %s", got.Status)
	}
}

func TestExtendBranch_削除中のブランチは延ばさない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	b, _ := e.store.GetBranch(ctx, name("x"))
	b.Status = domain.BranchDeleting
	_ = e.store.PutBranch(ctx, b)

	if _, err := e.svc.ExtendBranch(ctx, name("x"), time.Hour); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
}

// setStatus puts the branch into status on the store both nodes share.
func setBranchStatus(t *testing.T, p *pair, n string, s domain.BranchStatus) {
	t.Helper()
	b, err := p.origin.store.GetBranch(ctx, name(n))
	if err != nil {
		t.Fatal(err)
	}
	b.Status, b.Error = s, "boom"
	if err := p.origin.store.PutBranch(ctx, b); err != nil {
		t.Fatal(err)
	}
}

func TestMarkLostAndReconcile_一時停止中および失敗ブランチはノード復帰時にreadyへ強制起動されない(t *testing.T) {
	for _, status := range []domain.BranchStatus{domain.BranchPaused, domain.BranchFailed} {
		t.Run(string(status), func(t *testing.T) {
			p := lostPair(t)
			setBranchStatus(t, p, "x", status)
			if lost, err := p.replica.svc.MarkLost(ctx); err != nil || len(lost) != 1 {
				t.Fatalf("MarkLost = %v %v", lost, err)
			}
			if b, _ := p.replica.store.GetBranch(ctx, name("x")); b.Status != domain.BranchLost {
				t.Fatalf("status = %s", b.Status)
			}
			p.origin.j.entries = nil

			if err := p.origin.svc.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			b, _ := p.origin.store.GetBranch(ctx, name("x"))
			if b.Status != status {
				t.Errorf("status = %s, want %s", b.Status, status)
			}
			if status == domain.BranchFailed && b.Error != "boom" {
				t.Errorf("failed branch lost its reason: %q", b.Error)
			}
			if p.origin.j.has("start branch-x") {
				t.Errorf("branch was started: %q", p.origin.j.entries)
			}
		})
	}
}

func TestResetBranch_実行中に消されたブランチを復活させない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.withBranch(t, "x")
	e.server.onStop = func() {
		e.server.onStop = nil
		_ = e.store.DeleteBranch(ctx, name("x"))
	}
	_, _ = e.svc.ResetBranch(ctx, name("x"), nil)
	if _, err := e.store.GetBranch(ctx, name("x")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("branch came back: %v", err)
	}
}
