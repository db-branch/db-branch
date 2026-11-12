package nodesys_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/nodesys"
)

func file(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGet_OSとカーネルとZFSの版を集める(t *testing.T) {
	c := &nodesys.Collector{
		OSRelease: file(t, "PRETTY_NAME=\"Ubuntu 26.04.1 LTS\"\nID=ubuntu\nVERSION_ID=\"26.04\"\n"),
		Kernel:    file(t, "7.0.0-14-generic\n"),
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "zfs" || len(args) != 1 || args[0] != "version" {
				t.Errorf("ran %s %v", name, args)
			}
			return []byte("zfs-2.4.0-1ubuntu1\nzfs-kmod-2.4.0-1ubuntu1\n"), nil
		},
		TTL: time.Hour, Now: time.Now,
	}
	got := c.Get(context.Background())
	if got.Distro != "ubuntu-26.04" || got.OS != "Ubuntu 26.04.1 LTS" || got.Kernel != "7.0.0-14-generic" || got.ZFS != "2.4.0-1ubuntu1" {
		t.Errorf("got %+v", got)
	}
}

func TestGet_読めないものは空のままにする(t *testing.T) {
	c := &nodesys.Collector{
		OSRelease: filepath.Join(t.TempDir(), "none"), Kernel: filepath.Join(t.TempDir(), "none"),
		Run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no zfs") },
		TTL: time.Hour, Now: time.Now,
	}
	if got := c.Get(context.Background()); got.Distro != "" || got.Kernel != "" || got.ZFS != "" {
		t.Errorf("got %+v", got)
	}
}

func TestGet_TTLの間は取り直さない(t *testing.T) {
	runs, now := 0, time.Now()
	c := &nodesys.Collector{
		OSRelease: filepath.Join(t.TempDir(), "none"), Kernel: filepath.Join(t.TempDir(), "none"),
		Run: func(context.Context, string, ...string) ([]byte, error) {
			runs++
			return []byte("zfs-2.2.2\n"), nil
		},
		TTL: time.Minute, Now: func() time.Time { return now },
	}
	c.Get(context.Background())
	now = now.Add(30 * time.Second)
	c.Get(context.Background())
	if runs != 1 {
		t.Errorf("runs = %d, want 1", runs)
	}
	now = now.Add(time.Minute)
	c.Get(context.Background())
	if runs != 2 {
		t.Errorf("runs = %d, want 2 after the TTL", runs)
	}
}

func TestDefault_実際のパスを使う(t *testing.T) {
	c := nodesys.Default(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("x") })
	if c.OSRelease != "/etc/os-release" || c.Kernel != "/proc/sys/kernel/osrelease" || c.TTL != time.Hour || c.Now == nil {
		t.Errorf("%+v", c)
	}
}

func TestParseZFSVersion(t *testing.T) {
	for in, want := range map[string]string{
		"zfs-2.2.2-0ubuntu9\nzfs-kmod-2.2.2-0ubuntu9\n": "2.2.2-0ubuntu9",
		"zfs-2.3.1-1\nzfs-kmod-2.2.2-0ubuntu9":          "2.3.1-1 (kmod 2.2.2-0ubuntu9)",
		"zfs-kmod-2.2.2\n":                              "2.2.2",
		"zfs-2.2.2\n":                                   "2.2.2",
		"":                                              "",
		"The ZFS modules are not loaded.\n":             "",
	} {
		if got := nodesys.ParseZFSVersion(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
