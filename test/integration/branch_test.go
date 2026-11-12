//go:build integration

// End-to-end test against real ZFS, systemd and mysqld. Run as root on a dev VM
// with the db-branch package and mysql@8.4.13 installed:
//
//	sudo make integration
package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/app"
	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

var mysql84 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}

func mustName(t *testing.T, s string) domain.Name {
	t.Helper()
	n, err := domain.ParseName(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntegration_ブランチを作って書き換えリセットで戻る(t *testing.T) {
	pool := os.Getenv("DB_BRANCH_TEST_POOL")
	if pool == "" {
		t.Skip("DB_BRANCH_TEST_POOL is not set")
	}
	ctx := context.Background()
	cfg := config.Default()
	cfg.Pool = pool
	a, err := app.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := a.Branches()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Runtimes.Store.Has(ctx, mysql84); !ok {
		t.Skip("mysql@8.4.13 is not installed: sudo db-branch runtime install mysql@8.4.13")
	}

	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	id := hex.EncodeToString(suffix)
	baseName, branchName := mustName(t, "it-base-"+id), mustName(t, "it-br-"+id)

	dump := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(dump, []byte(`
CREATE DATABASE shop;
CREATE TABLE shop.users (id INT PRIMARY KEY, email VARCHAR(64));
INSERT INTO shop.users VALUES (1, 'alice@example.com'), (2, 'bob@example.com');
`), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: baseName, Runtime: mysql84, Dump: dump}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.DeleteBase(ctx, baseName) })
	t.Logf("base created in %s", time.Since(start).Round(time.Millisecond))

	start = time.Now()
	br, err := svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{
		Name: branchName, Base: baseName,
		InitSQL: "UPDATE shop.users SET email = CONCAT('user', id, '@masked.invalid')",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.DeleteBranch(ctx, branchName, nil) })
	t.Logf("branch created in %s", time.Since(start).Round(time.Millisecond))

	client := filepath.Join(a.Runtimes.Store.(interface {
		Path(domain.RuntimeRef) string
	}).Path(mysql84), "bin", "mysql")
	query := func(sql string) string {
		t.Helper()
		cmd := exec.Command(client, "-h127.0.0.1", "-P"+strconv.Itoa(br.Port), "-u"+br.User,
			"--batch", "--skip-column-names", "-e", sql)
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+br.Password)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if got := query("SELECT email FROM shop.users WHERE id = 1"); got != "user1@masked.invalid" {
		t.Errorf("init sql not applied: %q", got)
	}
	query("INSERT INTO shop.users VALUES (3, 'carol@example.com'); DELETE FROM shop.users WHERE id = 1")

	start = time.Now()
	if _, err := svc.ResetBranch(ctx, branchName, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("branch reset in %s", time.Since(start).Round(time.Millisecond))

	if got := query("SELECT COUNT(*), MIN(email) FROM shop.users"); got != "2\tuser1@masked.invalid" {
		t.Errorf("after reset: %q", got)
	}

	log, _ := os.ReadFile(filepath.Join(cfg.DataDir, "instances", "branch-"+branchName.String(), "error.log"))
	for _, bad := range []string{"not shut down normally", "Starting crash recovery", "crash recovery"} {
		if strings.Contains(string(log), bad) {
			t.Errorf("branch ran crash recovery (%q found in error log)", bad)
		}
	}
}
