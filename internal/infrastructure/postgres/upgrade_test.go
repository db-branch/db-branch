package postgres_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestCheckUpgrade_同じメジャーなら情報だけ返す(t *testing.T) {
	for _, tc := range []struct {
		pgVersion string
		to        string
	}{
		{"17\n", "17.5"},
		{"19\n", "19.1"},
	} {
		f := newFixture(t)
		_ = os.WriteFile(filepath.Join(f.spec.DataDir, "PG_VERSION"), []byte(tc.pgVersion), 0o644)
		plan, err := f.srv.CheckUpgrade(ctx, f.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: tc.to}, domain.Login{})
		if err != nil || len(plan.Findings) != 1 || plan.Findings[0].Level != domain.LevelInfo {
			t.Errorf("%s -> %s: plan = %+v, err = %v", tc.pgVersion, tc.to, plan, err)
		}
	}
}

func TestCheckUpgrade_データのメジャーが違えばエラーにする(t *testing.T) {
	for _, tc := range []struct {
		pgVersion string
		to        string
		wantOld   string
	}{
		{"16\n", "17.5", "PostgreSQL 16"},
		{"18\n", "19.0", "PostgreSQL 18"},
	} {
		f := newFixture(t)
		_ = os.WriteFile(filepath.Join(f.spec.DataDir, "PG_VERSION"), []byte(tc.pgVersion), 0o644)
		plan, _ := f.srv.CheckUpgrade(ctx, f.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: tc.to}, domain.Login{})
		if !domain.HasErrors(plan.Findings) || !strings.Contains(plan.Findings[0].Message, tc.wantOld) || !strings.Contains(plan.Findings[0].Message, "postgres@"+tc.to) {
			t.Errorf("%s -> %s: plan = %+v", tc.pgVersion, tc.to, plan)
		}
	}
}

func TestCheckUpgrade_PG_VERSIONが読めなければエラー(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.CheckUpgrade(ctx, f.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "17.5"}, domain.Login{}); err == nil {
		t.Error("want error")
	}
}

func TestStartUpgraded_起動してジャーナルを添える(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{}); err != nil {
		t.Fatal(err)
	}
}
