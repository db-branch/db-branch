package backupdir_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
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
