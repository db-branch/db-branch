package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func rt(s string) domain.RuntimeRef {
	r, err := domain.ParseRuntimeRef(s)
	if err != nil {
		panic(err)
	}
	return r
}

func TestCheckUpgradePath_許す道と断る道を見分ける(t *testing.T) {
	cases := []struct {
		from, to string
		want     string // "" = allowed, else a part of the message
	}{
		{"mysql@8.0.46", "mysql@8.4.13", ""},
		{"mysql@8.4.9", "mysql@8.4.13", ""},
		{"mysql@8.4.13", "mysql@9.1.0", ""},
		{"mysql@9.1.0", "mysql@9.2.0", ""},
		{"mysql@8.0.30", "mysql@8.2.0", ""},
		{"mysql@8.0.46", "mysql@9.1.0", "skip the long-term-support series 8.4"},
		{"mysql@8.4.13", "mysql@9.8.0", "skip the long-term-support series 9.7"},
		{"mysql@8.4.13", "mysql@9.7.0", ""},
		{"mysql@8.4.13", "mysql@8.0.46", "downgrade"},
		{"mysql@8.4.13", "mysql@8.4.13", "already runs"},
		{"mysql@8.4.13", "postgres@17.2", "one engine"},
		{"mysql@5.7.44", "mysql@8.0.46", ""},
		{"mysql@5.7.30", "mysql@5.7.44", ""},
		{"mysql@5.6.51", "mysql@5.7.44", ""},
		{"mysql@5.6.40", "mysql@5.6.51", ""},
		{"mysql@5.6.51", "mysql@8.0.46", "skip the long-term-support series 5.7"},
		{"mysql@5.7.44", "mysql@8.4.13", "skip the long-term-support series 8.0"},
		{"mysql@8.0.46", "mysql@5.7.44", "downgrade"},
		{"mysql@5.5.62", "mysql@5.7.44", "too old; upgrades start from MySQL 5.6"},
		{"postgres@17.2", "postgres@17.5", ""},
		{"postgres@17.5", "postgres@17.2", "downgrade"},
		{"postgres@17.2", "postgres@18.1", ""},
		{"postgres@17.12", "postgres@19.0", ""},
		{"postgres@18.7", "postgres@19.0", ""},
		{"postgres@19.0", "postgres@19.1", ""},
		{"postgres@19.1", "postgres@19.0", "downgrade"},
		{"postgres@19.0", "postgres@18.7", "downgrade"},
		{"postgres@9.6.1", "postgres@10.1", ""},
		{"postgres@9.6.1", "postgres@9.6.5", ""},
	}
	for _, c := range cases {
		err := domain.CheckUpgradePath(rt(c.from), rt(c.to))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s -> %s: %v", c.from, c.to, err)
		case c.want != "" && (err == nil || !errors.Is(err, domain.ErrUpgradeRefused) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s -> %s: err = %v, want %q", c.from, c.to, err, c.want)
		}
	}
}

func TestMySQLSeries_シリーズだけを返す(t *testing.T) {
	if got := domain.MySQLSeries("8.4.13-custom"); got != "8.4" {
		t.Errorf("series = %s", got)
	}
}

func TestRuntimeProp_ZFSの値として往復できる(t *testing.T) {
	r := rt("mysql@8.0.46")
	got, ok := domain.ParseRuntimeProp(domain.RuntimeProp(r))
	if !ok || got != r {
		t.Errorf("round trip = %v %v", got, ok)
	}
	for _, bad := range []string{"", "mysql", "oracle:1.0", "mysql:latest"} {
		if _, ok := domain.ParseRuntimeProp(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestHasErrors_エラーが一つでもあれば真(t *testing.T) {
	if domain.HasErrors([]domain.Finding{{Level: domain.LevelWarning}}) {
		t.Error("warning only")
	}
	if !domain.HasErrors([]domain.Finding{{Level: domain.LevelInfo}, {Level: domain.LevelError}}) {
		t.Error("error missed")
	}
}

func TestBranch_復元と更新ができる状態(t *testing.T) {
	ready := domain.Branch{Status: domain.BranchReady}
	if ready.CanRestore() != nil || ready.CanUpgrade() != nil {
		t.Error("ready branch refused")
	}
	kept := domain.Branch{Status: domain.BranchFailed, Error: domain.UpgradeFailedPrefix + "boom"}
	if kept.CanRestore() != nil {
		t.Error("a failed upgrade must allow restore")
	}
	if kept.CanUpgrade() == nil {
		t.Error("a failed branch must not upgrade")
	}
	other := domain.Branch{Status: domain.BranchFailed, Error: "disk"}
	if !errors.Is(other.CanRestore(), domain.ErrConflict) {
		t.Error("another failure must not allow restore")
	}
}

func TestListCheckpoints_取得時のランタイムを返す(t *testing.T) {
	lines := []domain.Line{{Dataset: "d", Live: true, Snapshots: []domain.SnapshotInfo{
		{Name: "ckpt-a", Created: time.Unix(1, 0), Props: map[string]string{domain.PropRuntime: "mysql:8.0.46"}},
		{Name: "ckpt-b", Created: time.Unix(2, 0)},
	}}}
	got := domain.ListCheckpoints(lines, "")
	if got[0].Runtime != "mysql@8.0.46" || got[1].Runtime != "" {
		t.Errorf("runtimes = %q %q", got[0].Runtime, got[1].Runtime)
	}
}

func TestIsPGMajorUpgrade_PostgreSQLのメジャー更新だけを真とする(t *testing.T) {
	if domain.ProtocolPGMajorUpgrade != 31 {
		t.Fatalf("ProtocolPGMajorUpgrade = %d, want 31", domain.ProtocolPGMajorUpgrade)
	}
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"postgres@18.6", "postgres@19.0", true},
		{"postgres@17.12", "postgres@19.0", true},
		{"postgres@18.6", "postgres@18.7", false},
		{"mysql@8.0.46", "mysql@8.4.13", false},
	} {
		if got := domain.IsPGMajorUpgrade(rt(tc.from), rt(tc.to)); got != tc.want {
			t.Errorf("IsPGMajorUpgrade(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}
