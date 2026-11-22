package fsutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/fsutil"
)

func TestWriteFresh_既存のシンボリックリンクを辿らず置き換える(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	_ = os.WriteFile(target, []byte("keep"), 0o600)
	path := filepath.Join(dir, "env")
	_ = os.Symlink(target, path)

	if err := fsutil.WriteFresh(path, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep" {
		t.Errorf("link target was written: %q", got)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o640 {
		t.Errorf("path = %v, %v", info, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteFresh_既存のファイルを作り直す(t *testing.T) {
	path := filepath.Join(t.TempDir(), "my.cnf")
	_ = os.WriteFile(path, []byte("old"), 0o600)
	if err := fsutil.WriteFresh(path, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteFresh_書けなければ失敗する(t *testing.T) {
	if err := fsutil.WriteFresh(filepath.Join(t.TempDir(), "no", "dir", "f"), nil, 0o640); err == nil {
		t.Error("want error")
	}
	dir := filepath.Join(t.TempDir(), "d")
	_ = os.MkdirAll(filepath.Join(dir, "x"), 0o755)
	if err := fsutil.WriteFresh(dir, nil, 0o640); err == nil {
		t.Error("non-empty directory in the way: want error")
	}
}

func TestWriteFresh_残った一時ファイルは消せなければ失敗する(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	_ = os.MkdirAll(filepath.Join(path+".new", "x"), 0o755)
	if err := fsutil.WriteFresh(path, nil, 0o640); err == nil {
		t.Error("want error")
	}
}

func TestWriteFresh_書き込みに失敗したら一時ファイルを残さない(t *testing.T) {
	defer fsutil.SetWrite(func(*os.File, []byte) (int, error) { return 0, errors.New("disk full") })()
	path := filepath.Join(t.TempDir(), "env")
	if err := fsutil.WriteFresh(path, []byte("x"), 0o640); err == nil {
		t.Fatal("want error")
	}
	for _, p := range []string{path, path + ".new"} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s remains", p)
		}
	}
}
