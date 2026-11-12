package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/app"
	"github.com/db-branch/db-branch/internal/domain"
)

type fakeZFS struct {
	domain.Storage
	pools   map[string]bool
	created []string
}

func (f *fakeZFS) Exists(_ context.Context, ds string) (bool, error) { return f.pools[ds], nil }
func (f *fakeZFS) Create(_ context.Context, ds string, _ map[string]string) error {
	f.created = append(f.created, ds)
	f.pools[ds] = true
	return nil
}

type fakeARC struct {
	applied uint64
	err     error
}

func (f *fakeARC) DefaultMax() (uint64, error)    { return 3 << 30, f.err }
func (f *fakeARC) Apply(n uint64) (string, error) { f.applied = n; return "ARC limited", f.err }

func newNode(t *testing.T) (app.Node, *fakeZFS, *fakeARC, *int, *strings.Builder) {
	z := &fakeZFS{pools: map[string]bool{"tank": true}}
	arc := &fakeARC{}
	restarts := 0
	var notes strings.Builder
	n := app.Node{
		Storage:             z,
		ARC:                 arc,
		InitialPasswordFile: filepath.Join(t.TempDir(), "etc", "initial-admin-password"),
		Restart:             func(context.Context) error { restarts++; return nil },
		Notes:               &notes,
	}
	return n, z, arc, &restarts, &notes
}

func TestInit_データセットとARCと初期パスワードと証明書を用意してエージェントを再起動する(t *testing.T) {
	n, z, arc, restarts, notes := newNode(t)
	c := cfg(t)

	pw, err := n.Init(context.Background(), c, "tank")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) < 20 {
		t.Errorf("password = %q", pw)
	}
	if len(z.created) != 1 || z.created[0] != "tank/db-branch" {
		t.Errorf("created = %v", z.created)
	}
	if arc.applied != 3<<30 || !strings.Contains(notes.String(), "ARC limited") {
		t.Errorf("arc = %d notes = %q", arc.applied, notes.String())
	}
	saved, _ := os.ReadFile(n.InitialPasswordFile)
	if strings.TrimSpace(string(saved)) != pw {
		t.Errorf("saved password = %q", saved)
	}
	if info, _ := os.Stat(n.InitialPasswordFile); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "tls", "cert.pem")); err != nil {
		t.Errorf("certificate: %v", err)
	}
	if *restarts != 1 {
		t.Errorf("restarts = %d", *restarts)
	}
	a, _ := app.New(c)
	if svc, err := a.Branches(); err != nil || svc.Pool != "tank" {
		t.Errorf("pool not recorded: %v", err)
	}

	again, err := n.Init(context.Background(), c, "tank")
	if err != nil || again != "" {
		t.Errorf("second init should keep the password: %q, %v", again, err)
	}
}

func TestInit_設定のARC上限を優先する(t *testing.T) {
	n, _, arc, _, _ := newNode(t)
	c := cfg(t)
	c.ZFS.ArcMaxMB = 2048
	if _, err := n.Init(context.Background(), c, "tank"); err != nil {
		t.Fatal(err)
	}
	if arc.applied != 2048<<20 {
		t.Errorf("applied = %d", arc.applied)
	}
}

func TestInit_プールがなければ何もせず失敗する(t *testing.T) {
	n, z, _, restarts, _ := newNode(t)
	_, err := n.Init(context.Background(), cfg(t), "nope")
	if err == nil || !strings.Contains(err.Error(), "zpool") || len(z.created) != 0 || *restarts != 0 {
		t.Errorf("err = %v created = %v restarts = %d", err, z.created, *restarts)
	}
}

func TestInit_途中の失敗はエラーにする(t *testing.T) {
	n, _, arc, _, _ := newNode(t)
	arc.err = errors.New("sysfs read-only")
	if _, err := n.Init(context.Background(), cfg(t), "tank"); err == nil {
		t.Error("ARC failure: want error")
	}
	n, _, _, _, _ = newNode(t)
	n.Restart = func(context.Context) error { return errors.New("systemctl failed") }
	if _, err := n.Init(context.Background(), cfg(t), "tank"); err == nil {
		t.Error("restart failure: want error")
	}
	n, _, _, _, _ = newNode(t)
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	n.InitialPasswordFile = filepath.Join(file, "pw")
	if _, err := n.Init(context.Background(), cfg(t), "tank"); err == nil {
		t.Error("password file failure: want error")
	}
}

func TestResetPassword_新しいパスワードで初期パスワードのファイルは消す(t *testing.T) {
	n, _, _, _, _ := newNode(t)
	c := cfg(t)
	first, _ := n.Init(context.Background(), c, "tank")

	pw, err := n.ResetPassword(c)
	if err != nil || pw == "" || pw == first {
		t.Fatalf("ResetPassword = %q, %v", pw, err)
	}
	if _, err := os.Stat(n.InitialPasswordFile); !os.IsNotExist(err) {
		t.Errorf("initial password file should be removed: %v", err)
	}
}
