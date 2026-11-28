package authfile_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

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

func TestArgon2_PHC形式でハッシュしレガシー生バイトと不正文字列を検証できる(t *testing.T) {
	h := authfile.Argon2{}
	salt := []byte("salt-1-16-bytes!")
	a := h.Hash("password", salt)
	if !bytes.Equal(a, h.Hash("password", salt)) || bytes.Equal(a, h.Hash("password", []byte("salt-2-16-bytes!"))) {
		t.Fatalf("hash = %q", a)
	}
	if !strings.HasPrefix(string(a), "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("not PHC argon2id format: %q", a)
	}
	if !h.Verify("password", nil, a) {
		t.Error("Verify PHC failed for valid password")
	}
	if h.Verify("wrong", nil, a) {
		t.Error("Verify PHC accepted wrong password")
	}
	if h.NeedsRehash(a) {
		t.Error("current PHC params should not need rehash")
	}

	// Legacy raw 32-byte key (no PHC prefix) still verifies and requests rehash.
	legacy := h.HashLegacy("password", salt)
	if len(legacy) != 32 {
		t.Fatalf("legacy len = %d, want 32", len(legacy))
	}
	if !h.Verify("password", salt, legacy) || h.Verify("wrong", salt, legacy) {
		t.Error("Verify legacy failed")
	}
	if !h.NeedsRehash(legacy) {
		t.Error("legacy raw hash must report NeedsRehash")
	}

	// Non-default PHC params verify using their embedded parameters and report NeedsRehash.
	customKey := argon2.IDKey([]byte("password"), salt, 1, 8192, 1, 32)
	customPHC := []byte("$argon2id$v=19$m=8192,t=1,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(customKey))
	if !h.Verify("password", nil, customPHC) || h.Verify("wrong", nil, customPHC) {
		t.Error("Verify custom-param PHC failed")
	}
	if !h.NeedsRehash(customPHC) {
		t.Error("outdated PHC params must report NeedsRehash")
	}

	// Legacy raw 32-byte key (even if first byte happens to be '$') verifies and requests rehash.
	legacyDollar := append([]byte("$"), make([]byte, 31)...)
	if h.Verify("password", salt, legacyDollar) {
		t.Error("non-matching legacy hash starting with '$' should be false")
	}
	// Verify a 32-byte raw hash whose first byte is '$' still takes legacy verification path.
	for i := 0; i < 2000; i++ {
		candSalt := []byte("dollar-salt-" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
		raw := h.HashLegacy("password", candSalt)
		if raw[0] == '$' {
			if !h.Verify("password", candSalt, raw) || !h.NeedsRehash(raw) {
				t.Fatalf("legacy 32-byte hash starting with '$' failed verification")
			}
			break
		}
	}

	// Malformed PHC strings are rejected safely.
	for _, bad := range []string{
		"",
		"$argon2id$v=18$m=19456,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=0,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=8,t=1,p=64$AAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=19456,t=0,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=0$AAAA$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$!bad!$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$AAAA$!bad!",
		"$argon2id$v=19$m=19456,t=2,p=1$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$AAAA$AAAA$extra",
		"$argon2id$v=19$x=1,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$AA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$AAAAAA$AA",
	} {
		if h.Verify("password", nil, []byte(bad)) {
			t.Errorf("malformed PHC %q should be rejected", bad)
		}
		if !h.NeedsRehash([]byte(bad)) {
			t.Errorf("malformed PHC %q should report NeedsRehash", bad)
		}
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

func TestStore_Stampはファイル変更で即座に変わり削除でErrNoCredentialsを返す(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s := authfile.Store{Path: path}
	if _, err := s.Stamp(); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("missing stamp err = %v, want ErrNoCredentials", err)
	}
	if err := s.Save(auth.Credentials{Hash: []byte("h1"), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	st1, err := s.Stamp()
	if err != nil || st1 == "" {
		t.Fatalf("Stamp 1 = (%q, %v)", st1, err)
	}
	st1Again, err := s.Stamp()
	if err != nil || st1Again != st1 {
		t.Fatalf("Stamp unchanged = (%q, %v), want %q", st1Again, err, st1)
	}
	if err := s.Save(auth.Credentials{Hash: []byte("h2"), Generation: 2}); err != nil {
		t.Fatal(err)
	}
	st2, err := s.Stamp()
	if err != nil || st2 == "" || st2 == st1 {
		t.Fatalf("Stamp after Save = (%q, %v), want != %q", st2, err, st1)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stamp(); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("after remove stamp err = %v, want ErrNoCredentials", err)
	}
	file := filepath.Join(t.TempDir(), "notadir")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := (authfile.Store{Path: filepath.Join(file, "auth.json")}).Stamp(); err == nil || errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("stat under regular file should return non-ErrNoCredentials error, got %v", err)
	}
}
