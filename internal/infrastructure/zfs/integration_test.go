//go:build integration

// Integration tests against a real pool. Run as root on a dev VM:
//
//	sudo make integration
package zfs_test

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
)

func TestIntegration_クローンへの変更はリセットで消え元データは変わらない(t *testing.T) {
	pool := os.Getenv("DB_BRANCH_TEST_POOL")
	if pool == "" {
		t.Skip("DB_BRANCH_TEST_POOL is not set")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	root := pool + "/dbb-it-" + hex.EncodeToString(suffix)
	run := execx.Runner{}
	if _, err := run.Run(ctx, "zfs", "create", "-p", root+"/base"); err != nil {
		t.Fatal(err)
	}
	d := zfs.New(run)
	t.Cleanup(func() { _ = d.Destroy(ctx, root) })

	baseInfo, err := d.Info(ctx, root+"/base")
	must(t, err)
	write(t, filepath.Join(baseInfo.Mountpoint, "data"), "base")
	base := domain.Snapshot{Dataset: root + "/base", Name: "s1"}
	must(t, d.Snapshot(ctx, base))

	branch := root + "/branches/feature-x"
	must(t, d.Clone(ctx, base, branch))
	info, err := d.Info(ctx, branch)
	must(t, err)
	reset := domain.Snapshot{Dataset: branch, Name: "reset"}
	must(t, d.Snapshot(ctx, reset))

	write(t, filepath.Join(info.Mountpoint, "data"), "changed")
	must(t, d.Rollback(ctx, reset))

	if got := read(t, filepath.Join(info.Mountpoint, "data")); got != "base" {
		t.Errorf("branch after reset = %q, want base", got)
	}
	if got := read(t, filepath.Join(baseInfo.Mountpoint, "data")); got != "base" {
		t.Errorf("base = %q, want base", got)
	}

	must(t, d.Destroy(ctx, branch))
	if ok, err := d.Exists(ctx, branch); err != nil || ok {
		t.Errorf("Exists after destroy = %v, %v", ok, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	must(t, os.WriteFile(path, []byte(s), 0o600))
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	return string(b)
}
