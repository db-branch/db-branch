//go:build integration

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

func TestIntegration_チェックポイントを取り復元しても他は残りリセットとブランチ削除も働く(t *testing.T) {
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
	baseName, branchName := mustName(t, "it-cb-"+id), mustName(t, "it-cr-"+id)

	dump := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(dump, []byte("CREATE DATABASE shop;\nCREATE TABLE shop.t (v INT);\nINSERT INTO shop.t VALUES (0);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: baseName, Runtime: mysql84, Dump: dump}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.DeleteBase(ctx, baseName) })
	br, err := svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{Name: branchName, Base: baseName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.DeleteBranch(ctx, branchName, nil) })

	client := filepath.Join(a.Runtimes.Store.(interface {
		Path(domain.RuntimeRef) string
	}).Path(mysql84), "bin", "mysql")
	query := func(sql string) string {
		t.Helper()
		cmd := exec.Command(client, "-h127.0.0.1", "-P"+strconv.Itoa(br.Port), "-u"+br.User, "--batch", "--skip-column-names", "-e", sql)
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+br.Password)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	sum := func() string { return query("SELECT GROUP_CONCAT(v ORDER BY v) FROM shop.t") }

	query("INSERT INTO shop.t VALUES (1)")
	if _, err := svc.CreateCheckpoint(ctx, branchName, "c1"); err != nil {
		t.Fatal(err)
	}
	query("INSERT INTO shop.t VALUES (2)")
	if _, err := svc.CreateCheckpoint(ctx, branchName, "c2"); err != nil {
		t.Fatal(err)
	}
	query("INSERT INTO shop.t VALUES (3)")

	start := time.Now()
	if _, err := svc.RestoreCheckpoint(ctx, branchName, "c1", nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("restore in %s", time.Since(start).Round(time.Millisecond))
	if got := sum(); got != "0,1" {
		t.Errorf("after restoring c1: %q", got)
	}
	list, err := svc.ListCheckpoints(ctx, branchName)
	if err != nil || len(list) != 3 {
		t.Fatalf("checkpoints = %+v %v", list, err)
	}
	if _, err := svc.RestoreCheckpoint(ctx, branchName, "c2", nil); err != nil {
		t.Fatal(err)
	}
	if got := sum(); got != "0,1,2" {
		t.Errorf("after restoring c2: %q", got)
	}

	if _, err := svc.ResetBranch(ctx, branchName, nil); err != nil {
		t.Fatal(err)
	}
	if got := sum(); got != "0" {
		t.Errorf("after reset: %q", got)
	}
	if list, _ := svc.ListCheckpoints(ctx, branchName); len(list) != 4 {
		t.Errorf("reset must keep the checkpoints: %+v", list)
	}
	if err := svc.DeleteBranch(ctx, branchName, nil); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("zfs", "list", "-H", "-t", "all", "-o", "name").Output()
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, branchName.String()) {
			t.Errorf("left over after the branch was deleted: %s", l)
		}
	}
}
