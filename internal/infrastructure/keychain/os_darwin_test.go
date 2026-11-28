//go:build darwin

package keychain_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/keychain"
)

func TestDarwinBackend_セキュリティコマンドで保存読込削除(t *testing.T) {
	var stored string
	var sawStdin string
	run := func(name, stdin string, args ...string) (string, string, error) {
		if name != "/usr/bin/security" {
			t.Fatalf("cmd = %q, want /usr/bin/security", name)
		}
		if len(args) == 1 && args[0] == "-i" {
			sawStdin = stdin
			if strings.Contains(stdin, "fail-save") {
				return "", "denied", errors.New("exit status 36")
			}
			if idx := strings.Index(stdin, "-w "); idx >= 0 {
				raw := strings.TrimSpace(stdin[idx+3:])
				raw = strings.TrimPrefix(raw, `"`)
				raw = strings.TrimSuffix(raw, `"`)
				raw = strings.ReplaceAll(raw, `\"`, `"`)
				raw = strings.ReplaceAll(raw, `\\`, `\`)
				stored = raw
			}
			return "", "", nil
		}
		switch args[0] {
		case "find-generic-password":
			if stored == "" {
				return "", "The specified item could not be found in the keychain.", errors.New("exit status 44")
			}
			return stored + "\n", "", nil
		case "delete-generic-password":
			if stored == "" {
				return "", "could not be found", errors.New("exit status 44")
			}
			stored = ""
			return "", "", nil
		}
		return "", "", errors.New("unexpected args")
	}
	selfInfo, err := os.Stat("/usr/bin/security")
	b := keychain.NewDarwinBackendForTest(func(string) (os.FileInfo, error) { return selfInfo, err }, run)
	if !b.Available() || b.Name() == "" {
		t.Fatal("expected available darwin backend")
	}
	if err := b.Save("prod", `tok\"en`); err != nil {
		t.Fatalf("Save = %v", err)
	}
	if !strings.Contains(sawStdin, "add-generic-password") {
		t.Errorf("expected security -i script, got %q", sawStdin)
	}
	got, err := b.Load("prod")
	if err != nil || got != `tok\"en` {
		t.Errorf("Load = (%q, %v)", got, err)
	}
	if err := b.Delete("prod"); err != nil {
		t.Errorf("Delete = %v", err)
	}
	if _, err := b.Load("prod"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("Load after delete = %v, want ErrNotFound", err)
	}
	if err := b.Delete("prod"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("Delete missing = %v, want ErrNotFound", err)
	}
	if err := b.Save("fail-save", "tok"); err == nil {
		t.Error("expected Save error")
	}
	if err := b.Save("bad/profile", "tok"); err == nil {
		t.Error("expected profile validation error")
	}
	if err := b.Save("prod", "tok\x07bell"); err == nil {
		t.Error("control char in token should fail")
	}

	bErr := keychain.NewDarwinBackendForTest(func(string) (os.FileInfo, error) { return nil, errors.New("missing") },
		func(string, string, ...string) (string, string, error) {
			return "", "perm denied", errors.New("exit status 1")
		})
	if bErr.Available() {
		t.Error("missing security binary should report unavailable")
	}
	if _, err := bErr.Load("prod"); err == nil || !strings.Contains(err.Error(), "perm denied") {
		t.Errorf("Load err = %v", err)
	}
	if err := bErr.Delete("prod"); err == nil || !strings.Contains(err.Error(), "perm denied") {
		t.Errorf("Delete err = %v", err)
	}

	bMismatch := keychain.NewDarwinBackendForTest(nil, func(_ string, _ string, args ...string) (string, string, error) {
		if args[0] == "-i" {
			return "", "", nil
		}
		return "wrong-tok\n", "", nil
	})
	if err := bMismatch.Save("prod", "want-tok"); err == nil || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("expected descriptive Save verification error, got %v", err)
	}

	// Non-ASCII tokens are hex-encoded by `security find-generic-password -w`, so darwinBackend
	// must refuse them before touching the keychain (allowing Store to fall back to the 0600 file).
	securityCalled := false
	bNonASCII := keychain.NewDarwinBackendForTest(nil, func(_ string, _ string, _ ...string) (string, string, error) {
		securityCalled = true
		return "", "", nil
	})
	if err := bNonASCII.Save("prod", "tok-日本語"); err == nil {
		t.Error("non-ASCII token should fail on macOS keychain")
	}
	if securityCalled {
		t.Error("non-ASCII token should be rejected before invoking security")
	}

	// Printable ASCII metacharacters must round-trip cleanly through escapeSecurityArg.
	for _, tok := range []string{`with space`, `with'single`, `with$dollar`, `with#hash`, `semi;colon`, `-leading-dash`, `a\"b\\c`} {
		stored = ""
		if err := b.Save("prod", tok); err != nil {
			t.Fatalf("Save(%q) = %v", tok, err)
		}
		if got, err := b.Load("prod"); err != nil || got != tok {
			t.Fatalf("Load(%q) = (%q, %v)", tok, got, err)
		}
	}
}
