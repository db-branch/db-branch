package backupdir_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/backupdir"
)

func TestDir_書き終えたら現れ所有者だけが読める(t *testing.T) {
	d := backupdir.Dir{Path: filepath.Join(t.TempDir(), "backups")}
	w, err := d.Create("db-branch-n1-20261110T030000Z.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "state")
	if list, _ := d.List(); len(list) != 0 {
		t.Errorf("visible before close: %v", list)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(d.Path, "db-branch-n1-20261110T030000Z.json.gz"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if di, _ := os.Stat(d.Path); di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", di.Mode())
	}
	old := filepath.Join(d.Path, "db-branch-n1-20261109T030000Z.json.gz")
	_ = os.WriteFile(old, []byte("older"), 0o600)
	_ = os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	_ = os.WriteFile(filepath.Join(d.Path, "unrelated.txt"), []byte("x"), 0o600)
	list, err := d.List()
	if err != nil || len(list) != 2 || list[0].Name != "db-branch-n1-20261110T030000Z.json.gz" || list[0].Size != 5 {
		t.Fatalf("%+v %v", list, err)
	}
	r, err := d.Open(list[1].Name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "older" {
		t.Errorf("%q", b)
	}
	if err := d.Remove(list[1].Name); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open("../../etc/passwd"); err == nil {
		t.Error("escaping the directory")
	}
}

func TestDir_ないディレクトリは空で使えない場所は失敗する(t *testing.T) {
	if list, err := (backupdir.Dir{Path: filepath.Join(t.TempDir(), "none")}).List(); err != nil || len(list) != 0 {
		t.Errorf("%v %v", list, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := (backupdir.Dir{Path: filepath.Join(file, "sub")}).Create("x.json.gz"); err == nil {
		t.Error("unusable path")
	}
	if _, err := (backupdir.Dir{Path: file}).List(); err == nil {
		t.Error("a file, not a directory")
	}
}

func TestDir_一時ファイルを作れないか閉じられなければ失敗する(t *testing.T) {
	d := backupdir.Dir{Path: t.TempDir()}
	_ = os.Mkdir(filepath.Join(d.Path, ".b.json.gz.tmp"), 0o700) // a directory in the way
	if _, err := d.Create("b.json.gz"); err == nil {
		t.Error("temporary file blocked")
	}
	w, _ := d.Create("c.json.gz")
	_ = w.Close()
	if err := w.Close(); err == nil {
		t.Error("closed twice")
	}
}

func TestDir_ないファイルはNotFound(t *testing.T) {
	if _, err := (backupdir.Dir{Path: t.TempDir()}).Open("db-branch-n1-20261110T030000Z.json.gz"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestBackupDir_危険なシステムパス設定を拒否し既存ディレクトリを無条件chmodしない(t *testing.T) {
	// Reject dangerous or invalid paths.
	badPaths := []string{
		"",
		"relative/backups",
		"./backups",
		" /var/backups/db-branch",
		"/var/backups/db-branch\x00x",
		"/var/backups/../etc",
		"/",
		"/etc",
		"/etc/db-branch",
		"/etc/db-branch/backups",
		"/usr",
		"/usr/local/backups",
		"/bin",
		"/sbin",
		"/lib",
		"/lib64",
		"/boot",
		"/dev",
		"/proc",
		"/sys",
		"/run",
		"/run/db-branch",
		"/var",
		"/var/lib",
		"/var/log",
		"/var/run",
		"/var/run/db-branch",
		"/var/lock",
		"/var/lock/db-branch",
		"/var/backups",
		"/var/tmp",
		"/tmp",
		"/home",
		"/mnt",
		"/srv",
		"/opt",
		"/root",
		"/root/.ssh",
	}
	for _, p := range badPaths {
		if err := backupdir.ValidatePath(p); err == nil {
			t.Errorf("ValidatePath(%q) = nil, want error", p)
		}
		if _, err := (backupdir.Dir{Path: p}).Create("b.json.gz"); err == nil {
			t.Errorf("Dir{%q}.Create = nil, want error", p)
		}
		if _, err := (backupdir.Dir{Path: p}).List(); err == nil {
			t.Errorf("Dir{%q}.List = nil, want error", p)
		}
		if err := (backupdir.Dir{Path: p}).Remove("b.json.gz"); err == nil {
			t.Errorf("Dir{%q}.Remove = nil, want error", p)
		}
		if _, err := (backupdir.Dir{Path: p}).Open("b.json.gz"); err == nil {
			t.Errorf("Dir{%q}.Open = nil, want error", p)
		}
	}

	// Accept safe absolute paths.
	goodPaths := []string{
		"/var/lib/db-branch/backups",
		"/var/backups/db-branch",
		"/mnt/nfs-backups/db-branch",
		"/srv/db-branch-backups",
		"/opt/backups/db-branch",
		"/data/backups",
	}
	for _, p := range goodPaths {
		if err := backupdir.ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", p, err)
		}
	}

	// Existing directory keeps its permissions (no unconditional chmod 0700).
	existing := filepath.Join(t.TempDir(), "mounted-backups")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := (backupdir.Dir{Path: existing}).Create("db-branch-n1-20261110T030000Z.json.gz")
	if err != nil {
		t.Fatalf("Create in existing dir: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("existing dir mode changed to %v, want 0755", fi.Mode().Perm())
	}

	// Creating a new subdirectory must not alter its existing parent's permissions.
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(parent, 0o755)
	child := filepath.Join(parent, "backups")
	w2, err := (backupdir.Dir{Path: child}).Create("db-branch-n1-20261110T030001Z.json.gz")
	if err != nil {
		t.Fatalf("Create child dir: %v", err)
	}
	_ = w2.Close()
	if pfi, _ := os.Stat(parent); pfi.Mode().Perm() != 0o755 {
		t.Errorf("parent dir mode changed to %v, want 0755", pfi.Mode().Perm())
	}
	if cfi, _ := os.Stat(child); cfi.Mode().Perm() != 0o700 {
		t.Errorf("new child dir mode = %v, want 0700", cfi.Mode().Perm())
	}

	// Symlink directly as backup dir (with and without trailing slash) is rejected on all operations.
	target := filepath.Join(t.TempDir(), "real")
	_ = os.Mkdir(target, 0o755)
	_ = os.WriteFile(filepath.Join(target, "a.json.gz"), []byte("x"), 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, link + "/"} {
		d := backupdir.Dir{Path: p}
		if _, err := d.Create("b.json.gz"); err == nil {
			t.Errorf("Create on symlink %q: want error", p)
		}
		if _, err := d.List(); err == nil {
			t.Errorf("List on symlink %q: want error", p)
		}
		if err := d.Remove("a.json.gz"); err == nil {
			t.Errorf("Remove on symlink %q: want error", p)
		}
		if _, err := d.Open("a.json.gz"); err == nil {
			t.Errorf("Open on symlink %q: want error", p)
		}
	}

	// Invalid file names (., .., empty, slash) are rejected on Create/Remove/Open.
	validDir := backupdir.Dir{Path: existing}
	for _, badName := range []string{"", ".", "..", "a/b.json.gz"} {
		if _, err := validDir.Create(badName); err == nil {
			t.Errorf("Create(%q): want error", badName)
		}
		if err := validDir.Remove(badName); err == nil {
			t.Errorf("Remove(%q): want error", badName)
		}
		if _, err := validDir.Open(badName); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Open(%q): err = %v, want ErrNotFound", badName, err)
		}
	}

	// Symlinked file inside backup dir is rejected by Open/Create (O_EXCL) and ignored by List.
	secretFile := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secretFile, []byte("secret"), 0o600)
	_ = os.Symlink(secretFile, filepath.Join(existing, "evil.json.gz"))
	if _, err := validDir.Open("evil.json.gz"); err == nil {
		t.Error("Open symlinked file: want error")
	}
	if list, err := validDir.List(); err != nil || len(list) != 1 {
		t.Errorf("List should ignore symlinked file: %+v, %v", list, err)
	}
	// Subdirectory and FIFO inside backup dir are rejected by Open without blocking.
	_ = os.Mkdir(filepath.Join(existing, "subdir.json.gz"), 0o700)
	if _, err := validDir.Open("subdir.json.gz"); err == nil {
		t.Error("Open directory entry: want error")
	}
	if err := syscall.Mkfifo(filepath.Join(existing, "pipe.json.gz"), 0o600); err == nil {
		if _, err := validDir.Open("pipe.json.gz"); err == nil {
			t.Error("Open FIFO entry: want error")
		}
	}
	// Dangling symlink as backup dir itself is rejected.
	dangling := filepath.Join(t.TempDir(), "dangling")
	_ = os.Symlink(filepath.Join(t.TempDir(), "nowhere"), dangling)
	if _, err := (backupdir.Dir{Path: dangling}).Create("b.json.gz"); err == nil {
		t.Error("dangling symlink as backup dir: want error")
	}
	_ = os.Symlink(secretFile, filepath.Join(existing, ".tmpattack.json.gz.tmp"))
	if _, err := validDir.Create("tmpattack.json.gz"); err == nil {
		t.Error("Create with pre-planted tmp symlink: want error")
	}
	if b, _ := os.ReadFile(secretFile); string(b) != "secret" {
		t.Errorf("symlink target was modified: %q", b)
	}

	// Open on non-existent directory returns ErrNotFound.
	if _, err := (backupdir.Dir{Path: filepath.Join(t.TempDir(), "missing")}).Open("x.json.gz"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Open on missing dir: %v", err)
	}

	// Symlink ancestor pointing into a forbidden system dir is rejected (both non-existent and existing subdir).
	etcLink := filepath.Join(t.TempDir(), "etc-link")
	if err := os.Symlink("/etc", etcLink); err != nil {
		t.Fatal(err)
	}
	if _, err := (backupdir.Dir{Path: filepath.Join(etcLink, "sub")}).Create("b.json.gz"); err == nil {
		t.Error("symlink ancestor to /etc: want error")
	}
	varLink := filepath.Join(t.TempDir(), "var-link")
	if err := os.Symlink("/var", varLink); err != nil {
		t.Fatal(err)
	}
	if _, err := (backupdir.Dir{Path: filepath.Join(varLink, "tmp")}).Create("b.json.gz"); err == nil {
		t.Error("symlink ancestor to existing /var/tmp: want error")
	}

	// Regular file as backup dir is rejected.
	file := filepath.Join(t.TempDir(), "notadir")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := (backupdir.Dir{Path: file}).Create("b.json.gz"); err == nil {
		t.Error("file as backup dir: want error")
	}
}
