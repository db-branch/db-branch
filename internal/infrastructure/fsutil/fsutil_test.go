package fsutil_test

import (
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
