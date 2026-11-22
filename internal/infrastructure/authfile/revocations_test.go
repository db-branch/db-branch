package authfile_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/authfile"
)

func TestRevocations_ログアウトを再起動しても覚え期限の過ぎた記録は片付ける(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "revoked.json")
	now := time.Unix(1000, 0)
	r := authfile.NewRevocations(path)
	if r.Revoked("a") {
		t.Fatal("nothing revoked yet")
	}
	if err := r.Revoke("a", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := r.Revoke("b", now.Add(2*time.Hour), now.Add(90*time.Minute)); err != nil { // a has expired
		t.Fatal(err)
	}
	again := authfile.NewRevocations(path) // a restart
	if again.Revoked("a") || !again.Revoked("b") {
		t.Errorf("after restart: a=%v b=%v", again.Revoked("a"), again.Revoked("b"))
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("stat = %v, %v", info, err)
	}
}

func TestRevocations_壊れたファイルは空として扱い書けなければ失敗する(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if authfile.NewRevocations(bad).Revoked("x") {
		t.Error("garbage file must read as empty")
	}
	blocked := filepath.Join(dir, "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	now := time.Unix(1000, 0)
	if err := authfile.NewRevocations(filepath.Join(blocked, "r.json")).Revoke("a", now.Add(time.Hour), now); err == nil {
		t.Error("want error when the directory cannot be made")
	}
}
