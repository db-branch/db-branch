//go:build windows

package keychain_test

import (
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/keychain"
)

type fakeWinCredOps struct {
	available bool
	creds     map[string][]byte
	writeErr  error
	readErr   error
	delErr    error
}

func (f *fakeWinCredOps) Available() bool { return f.available }
func (f *fakeWinCredOps) Write(target, _ string, blob []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.creds == nil {
		f.creds = map[string][]byte{}
	}
	f.creds[target] = append([]byte(nil), blob...)
	return nil
}
func (f *fakeWinCredOps) Read(target string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	b, ok := f.creds[target]
	if !ok {
		return nil, keychain.ErrNotFound
	}
	return b, nil
}
func (f *fakeWinCredOps) Delete(target string) error {
	if f.delErr != nil {
		return f.delErr
	}
	if _, ok := f.creds[target]; !ok {
		return keychain.ErrNotFound
	}
	delete(f.creds, target)
	return nil
}

func TestWindowsBackend_CredentialManager操作とエラー処理(t *testing.T) {
	ops := &fakeWinCredOps{available: true}
	b := keychain.NewWindowsBackendForTest(ops)
	if !b.Available() || b.Name() == "" {
		t.Fatal("expected available windows backend")
	}
	if err := b.Save("bad/name", "tok"); err == nil {
		t.Error("invalid profile should fail")
	}
	if err := b.Save("prod", "dbb_win_secret"); err != nil {
		t.Fatalf("Save = %v", err)
	}
	if got, err := b.Load("prod"); err != nil || got != "dbb_win_secret" {
		t.Errorf("Load = (%q, %v)", got, err)
	}
	if err := b.Delete("prod"); err != nil {
		t.Errorf("Delete = %v", err)
	}
	if _, err := b.Load("prod"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("Load missing = %v, want ErrNotFound", err)
	}
	ops.writeErr = errors.New("access denied")
	if err := b.Save("prod", "tok"); err == nil {
		t.Error("expected Write error")
	}
	ops.writeErr = nil
	ops.readErr = errors.New("read denied")
	if _, err := b.Load("prod"); err == nil {
		t.Error("expected Read error")
	}
	ops.readErr = nil
	ops.delErr = errors.New("del denied")
	if err := b.Delete("prod"); err == nil {
		t.Error("expected Delete error")
	}
	ops.delErr = nil
	if err := ops.Write("db-branch:empty", "empty", nil); err != nil {
		t.Errorf("empty Write = %v", err)
	}
}
