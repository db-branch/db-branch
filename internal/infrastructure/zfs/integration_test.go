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

// The swap a restore does, on a real pool: clone a checkpoint, set the live
// dataset aside, rename the clone into place, promote it. Every checkpoint
// stays restorable and the data directory path never changes.
func TestIntegration_チェックポイントの復元は入れ替えと昇格で他を失わない(t *testing.T) {
	pool := os.Getenv("DB_BRANCH_TEST_POOL")
	if pool == "" {
		t.Skip("DB_BRANCH_TEST_POOL is not set")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	root := pool + "/dbb-it-" + hex.EncodeToString(suffix)
	run := execx.Runner{}
	d := zfs.New(run)
	live, hidden := root+"/branches/x", root+"/history/x"
	must(t, d.Create(ctx, live, nil))
	t.Cleanup(func() { _ = d.Destroy(ctx, root) })
	must(t, d.Create(ctx, hidden, map[string]string{"canmount": "off"}))
	info, err := d.Info(ctx, live)
	must(t, err)
	data := filepath.Join(info.Mountpoint, "data")

	write(t, data, "v1")
	c1 := domain.Snapshot{Dataset: live, Name: "ckpt-c1"}
	must(t, d.Snapshot(ctx, c1))
	must(t, d.SetProps(ctx, c1.String(), map[string]string{domain.PropLine: "0", domain.PropAuto: "off"}))
	write(t, data, "v2")
	must(t, d.Snapshot(ctx, domain.Snapshot{Dataset: live, Name: "ckpt-c2"}))
	write(t, data, "v3")

	// Restore c1.
	must(t, d.CloneWith(ctx, c1, hidden+"/new", map[string]string{"canmount": "off", domain.PropLine: "1"}))
	must(t, d.SetProps(ctx, live, map[string]string{"canmount": "off"}))
	must(t, d.Rename(ctx, live, hidden+"/0"))
	must(t, d.Rename(ctx, hidden+"/new", live))
	must(t, d.SetProps(ctx, live, map[string]string{"canmount": "on"}))
	must(t, d.Mount(ctx, live))
	must(t, d.Mount(ctx, live)) // mounted already: not an error
	origin, err := d.Origin(ctx, live)
	must(t, err)
	if origin != hidden+"/0@ckpt-c1" {
		t.Fatalf("origin = %q", origin)
	}
	must(t, d.Promote(ctx, live))

	if got := read(t, data); got != "v1" {
		t.Errorf("live after the restore = %q at %s, want v1 at the same path", got, data)
	}
	if o, err := d.Origin(ctx, live); err != nil || o != "" {
		t.Errorf("origin after promote = %q %v", o, err)
	}
	snaps, err := d.SnapshotInfos(ctx, live)
	must(t, err)
	if len(snaps) != 1 || snaps[0].Name != "ckpt-c1" || snaps[0].Props[domain.PropLine] != "0" || len(snaps[0].Clones) != 1 {
		t.Errorf("live snapshots = %+v", snaps)
	}
	old, err := d.SnapshotInfos(ctx, hidden+"/0")
	must(t, err)
	if len(old) != 1 || old[0].Name != "ckpt-c2" {
		t.Errorf("the old line keeps c2: %+v", old)
	}
	names, err := d.Datasets(ctx, hidden)
	must(t, err)
	if len(names) != 1 || names[0] != hidden+"/0" {
		t.Errorf("history = %v", names)
	}

	// c2 is still restorable: clone it, and the old line can be destroyed once it is not needed.
	must(t, d.CloneWith(ctx, domain.Snapshot{Dataset: hidden + "/0", Name: "ckpt-c2"}, hidden+"/probe", map[string]string{"canmount": "off"}))
	if err := d.Destroy(ctx, hidden+"/0"); err == nil {
		t.Error("destroying a line with a clone depending on it must fail")
	}
	must(t, d.Destroy(ctx, hidden+"/probe"))
	must(t, d.RenameSnapshot(ctx, domain.Snapshot{Dataset: live, Name: "ckpt-c1"}, "gone-1"))
	must(t, d.Destroy(ctx, hidden+"/0"))
	must(t, d.DestroySnapshot(ctx, domain.Snapshot{Dataset: live, Name: "gone-1"}))
}
