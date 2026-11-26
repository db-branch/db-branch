package keychain_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/keychain"
)

type fakeBackend struct {
	name      string
	available bool
	tokens    map[string]string
	saveErr   error
	loadErr   error
	delErr    error
}

func (f *fakeBackend) Name() string    { return f.name }
func (f *fakeBackend) Available() bool { return f.available }
func (f *fakeBackend) Save(profile, token string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.tokens == nil {
		f.tokens = map[string]string{}
	}
	f.tokens[profile] = token
	return nil
}
func (f *fakeBackend) Load(profile string) (string, error) {
	if f.loadErr != nil {
		return "", f.loadErr
	}
	tok, ok := f.tokens[profile]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return tok, nil
}
func (f *fakeBackend) Delete(profile string) error {
	if f.delErr != nil {
		return f.delErr
	}
	if _, ok := f.tokens[profile]; !ok {
		return keychain.ErrNotFound
	}
	delete(f.tokens, profile)
	return nil
}

func TestStore_OSキーチェーン優先とフォールバック警告(t *testing.T) {
	credPath := filepath.Join(t.TempDir(), "credentials.toml")

	// 1. OS backend available and succeeds -> storage "keychain", no warning, removes stale file entry.
	osB := &fakeBackend{name: "keychain", available: true, tokens: map[string]string{}}
	fileB := keychain.NewFileBackend(credPath)
	if err := fileB.Save("prod", "stale-tok"); err != nil {
		t.Fatal(err)
	}
	st := keychain.NewWithBackends(osB, fileB)
	storage, warn, err := st.Save("prod", "dbb_123_secret")
	if err != nil {
		t.Fatalf("Save = %v", err)
	}
	if storage != "keychain" || warn != "" {
		t.Errorf("got (%q, %q), want (keychain, empty)", storage, warn)
	}
	if got, err := st.Load("prod"); err != nil || got != "dbb_123_secret" {
		t.Errorf("Load = (%q, %v)", got, err)
	}
	if _, err := fileB.Load("prod"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("stale file token should be cleared, got err=%v", err)
	}

	// 2. OS backend fails -> falls back to 0600 file with warning.
	osFail := &fakeBackend{name: "keychain", available: true, saveErr: errors.New("locked")}
	stFail := keychain.NewWithBackends(osFail, fileB)
	storage, warn, err = stFail.Save("staging", "dbb_456_secret")
	if err != nil {
		t.Fatalf("fallback Save = %v", err)
	}
	if storage != "file" || !strings.Contains(warn, "0600") {
		t.Errorf("got (%q, %q), want file + 0600 warning", storage, warn)
	}
	info, err := os.Stat(credPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cred file stat = (%v, %v)", info.Mode().Perm(), err)
	}
	if got, err := stFail.Load("staging"); err != nil || got != "dbb_456_secret" {
		t.Errorf("Load from fallback = (%q, %v)", got, err)
	}

	// 3. Delete clears both and is idempotent on missing.
	if err := st.Delete("prod"); err != nil {
		t.Errorf("Delete prod = %v", err)
	}
	if err := stFail.Delete("staging"); err != nil {
		t.Errorf("Delete staging = %v", err)
	}
	if err := st.Delete("never-existed"); err != nil {
		t.Errorf("Delete missing = %v", err)
	}
	if _, err := st.Load("prod"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("Load after delete = %v, want ErrNotFound", err)
	}
}

func TestFileBackend_読み書き削除と不正入力(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "credentials.toml")
	fb := keychain.NewFileBackend(path)

	if _, err := fb.Load("missing"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("missing file Load = %v, want ErrNotFound", err)
	}
	if err := fb.Save("bad/name", "tok"); err == nil {
		t.Error("invalid profile name should fail")
	}
	if err := fb.Save("default", "tok\nnewline"); err == nil {
		t.Error("newline in token should fail")
	}
	if err := fb.Save("default", "tok1"); err != nil {
		t.Fatal(err)
	}
	if err := fb.Save("other", "tok2"); err != nil {
		t.Fatal(err)
	}
	if got, err := fb.Load("default"); err != nil || got != "tok1" {
		t.Errorf("Load default = (%q, %v)", got, err)
	}
	if err := fb.Delete("default"); err != nil {
		t.Fatal(err)
	}
	if _, err := fb.Load("default"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("deleted Load = %v", err)
	}
	if got, err := fb.Load("other"); err != nil || got != "tok2" {
		t.Errorf("Load other = (%q, %v)", got, err)
	}
	// Corrupted file returns error.
	if err := os.WriteFile(path, []byte("broken =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fb.Load("other"); err == nil {
		t.Error("corrupted file Load should fail")
	}
	if err := fb.Save("other", "tok3"); err == nil {
		t.Error("Save over corrupted file should fail")
	}
	if err := fb.Delete("other"); err == nil {
		t.Error("Delete over corrupted file should fail")
	}
	if err := fb.Delete("bad/name"); err == nil {
		t.Error("Delete invalid profile should fail")
	}
	if _, err := fb.Load("bad/name"); err == nil {
		t.Error("Load invalid profile should fail")
	}
}

func TestStore_デフォルト生成とエラー伝搬(t *testing.T) {
	st := keychain.New(filepath.Join(t.TempDir(), "credentials.toml"))
	if _, _, err := st.Save("bad/name", "tok"); err == nil {
		t.Error("Save invalid profile should fail")
	}
	if _, _, err := st.Save("default", ""); err == nil {
		t.Error("Save empty token should fail")
	}
	if _, err := st.Load("bad/name"); err == nil {
		t.Error("Load invalid profile should fail")
	}
	if err := st.Delete("bad/name"); err == nil {
		t.Error("Delete invalid profile should fail")
	}

	// OS backend returns real error on Load, falls back to file if present, else returns OS error.
	fb := keychain.NewFileBackend(filepath.Join(t.TempDir(), "credentials.toml"))
	osErr := &fakeBackend{name: "keychain", available: true, loadErr: errors.New("locked"), delErr: errors.New("del-fail")}
	stErr := keychain.NewWithBackends(osErr, fb)
	if _, err := stErr.Load("default"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("Load err = %v", err)
	}
	if err := fb.Save("default", "from-file"); err != nil {
		t.Fatal(err)
	}
	if got, err := stErr.Load("default"); err != nil || got != "from-file" {
		t.Errorf("Load fallback on OS error = (%q, %v)", got, err)
	}
	if err := stErr.Delete("default"); err == nil || !strings.Contains(err.Error(), "del-fail") {
		t.Errorf("Delete err = %v", err)
	}
	if !fb.Available() {
		t.Error("FileBackend.Available should be true")
	}
	// Save fails when fallback file cannot be written (directory at file path).
	dir := t.TempDir()
	stBadFile := keychain.NewWithBackends(&fakeBackend{available: false}, keychain.NewFileBackend(dir))
	if _, _, err := stBadFile.Save("default", "tok"); err == nil {
		t.Error("Save to directory path should fail")
	}
	// Delete ignores ErrNotFound from OS and file backends.
	if err := stBadFile.Delete("default"); err == nil {
		t.Error("Delete on directory path should fail")
	}
	// ExecCmdRunnerForTest exercises real process execution cleanly.
	out, _, err := keychain.ExecCmdRunnerForTest("go", "", "version")
	if err != nil || !strings.Contains(out, "go version") {
		t.Errorf("ExecCmdRunnerForTest(go version) = (%q, %v)", out, err)
	}
	if _, _, err := keychain.ExecCmdRunnerForTest("go", "ignored", "help"); err != nil {
		t.Errorf("ExecCmdRunnerForTest with stdin = %v", err)
	}
}

