package postgres_test

import (
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/postgres"
)

func TestDropUser_親ロールの所有物を移譲し権限剥奪して削除し予約ロールや不正名を拒否する(t *testing.T) {
	var _ domain.CloneUserCleaner = (*postgres.Server)(nil)

	f := newFixture(t)
	f.sys.outputs = map[string]string{
		"SELECT datname FROM pg_database": "postgres\napp\n",
	}
	if err := f.srv.DropUser(ctx, f.spec, "parent"); err != nil {
		t.Fatalf("DropUser: %v", err)
	}
	// Runs REASSIGN OWNED + DROP OWNED in postgres and app, then REVOKE + DROP ROLE in postgres.
	if len(f.sys.stdin) != 3 {
		t.Fatalf("stdin len = %d, want 3: %q", len(f.sys.stdin), f.sys.stdin)
	}
	for i := 0; i < 2; i++ {
		if !strings.Contains(f.sys.stdin[i], `REASSIGN OWNED BY "parent" TO "db_branch_owner"`) ||
			!strings.Contains(f.sys.stdin[i], `DROP OWNED BY "parent"`) {
			t.Errorf("stdin[%d] = %q", i, f.sys.stdin[i])
		}
	}
	if !strings.Contains(f.sys.stdin[2], `REVOKE "db_branch_owner" FROM "parent"`) ||
		!strings.Contains(f.sys.stdin[2], `DROP ROLE "parent"`) {
		t.Errorf("stdin[2] = %q", f.sys.stdin[2])
	}

	for _, bad := range []string{"", "bad\x00user", "bad\nuser", postgres.Superuser, postgres.OwnerRole} {
		if err := f.srv.DropUser(ctx, f.spec, bad); err == nil {
			t.Errorf("DropUser(%q): want error", bad)
		}
	}

	// Database listing error propagates.
	f2 := newFixture(t)
	f2.sys.failOn = "SELECT datname"
	if err := f2.srv.DropUser(ctx, f2.spec, "parent"); err == nil {
		t.Error("want databases error")
	}

	// Per-database REASSIGN/DROP OWNED error propagates.
	f3 := newFixture(t)
	f3.sys.outputs = map[string]string{"SELECT datname FROM pg_database": "app\n"}
	f3.sys.failOn = "-d app"
	if err := f3.srv.DropUser(ctx, f3.spec, "parent"); err == nil || !strings.Contains(err.Error(), "database app") {
		t.Errorf("per-db error = %v, want database app", err)
	}
}
