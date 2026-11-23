package mysql_test

import (
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/mysql"
)

func TestResetCloneHistory_8_4以降はRESET_BINARY_LOGS_AND_GTIDSでそれ未満はRESET_MASTERを実行する(t *testing.T) {
	var _ domain.CloneHistoryResetter = (*mysql.Server)(nil)

	for _, tc := range []struct {
		version string
		want    string
	}{
		{"8.4.13", "RESET BINARY LOGS AND GTIDS"},
		{"9.6.0", "RESET BINARY LOGS AND GTIDS"},
		{"8.0.46", "RESET MASTER"},
		{"5.7.44", "RESET MASTER"},
		{"5.6.51", "RESET MASTER"},
	} {
		f := newFixture(t).with(tc.version)
		if err := f.srv.ResetCloneHistory(ctx, f.spec); err != nil {
			t.Fatalf("%s: ResetCloneHistory: %v", tc.version, err)
		}
		if len(f.sys.stdin) != 1 || !strings.Contains(f.sys.stdin[0], tc.want) {
			t.Errorf("%s: stdin = %q, want %q", tc.version, f.sys.stdin, tc.want)
		}
	}
}

func TestDropUser_親ブランチユーザーを安全に削除しrootや不正名を拒否する(t *testing.T) {
	var _ domain.CloneUserCleaner = (*mysql.Server)(nil)

	for _, tc := range []struct {
		version string
		want    string
	}{
		{"8.4.13", "DROP USER IF EXISTS 'parent'@'%'"},
		{"5.7.44", "DROP USER IF EXISTS 'parent'@'%'"},
		{"5.6.51", "DELETE FROM mysql.user WHERE User = 'parent' AND Host = '%'; FLUSH PRIVILEGES"},
	} {
		f := newFixture(t).with(tc.version)
		if err := f.srv.DropUser(ctx, f.spec, "parent"); err != nil {
			t.Fatalf("%s: DropUser: %v", tc.version, err)
		}
		if len(f.sys.stdin) != 1 || !strings.Contains(f.sys.stdin[0], tc.want) {
			t.Errorf("%s: stdin = %q, want %q", tc.version, f.sys.stdin, tc.want)
		}
	}

	f := newFixture(t).with("8.4.13")
	for _, bad := range []string{"", "bad\x00user", "root"} {
		if err := f.srv.DropUser(ctx, f.spec, bad); err == nil {
			t.Errorf("DropUser(%q): want error", bad)
		}
	}
}
