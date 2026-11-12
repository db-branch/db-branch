package authfile_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/authfile"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

func TestStore_保存した資格情報を0600で読み戻す(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "auth.json")
	s := authfile.Store{Path: path}
	if _, err := s.Load(); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("empty: %v", err)
	}
	c := auth.Credentials{Salt: []byte("salt"), Hash: []byte("hash"), Generation: 3, UpdatedAt: time.Unix(0, 0).UTC()}
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.Generation != 3 || !bytes.Equal(got.Hash, c.Hash) {
		t.Errorf("Load = %+v, %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

func TestStore_壊れたファイルや書けない場所はエラーにする(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "auth.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if _, err := (authfile.Store{Path: broken}).Load(); err == nil {
		t.Error("broken: want error")
	}
	if _, err := (authfile.Store{Path: dir}).Load(); err == nil {
		t.Error("directory: want error")
	}
	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if err := (authfile.Store{Path: filepath.Join(file, "auth.json")}).Save(auth.Credentials{}); err == nil {
		t.Error("save under a file: want error")
	}
}

func TestArgon2_同じ入力なら同じハッシュで塩が違えば別になる(t *testing.T) {
	h := authfile.Argon2{}
	a := h.Hash("password", []byte("salt-1"))
	if !bytes.Equal(a, h.Hash("password", []byte("salt-1"))) || bytes.Equal(a, h.Hash("password", []byte("salt-2"))) || len(a) != 32 {
		t.Errorf("hash = %x", a)
	}
}

func TestStore_置き換え先がディレクトリなら保存に失敗する(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	_ = os.MkdirAll(filepath.Join(path, "x"), 0o755)
	if err := (authfile.Store{Path: path}).Save(auth.Credentials{}); err == nil {
		t.Error("want error")
	}
}

func TestStore_一時ファイルを書けなければ保存に失敗する(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	_ = os.MkdirAll(path+".tmp", 0o755)
	if err := (authfile.Store{Path: path}).Save(auth.Credentials{}); err == nil {
		t.Error("want error")
	}
}
