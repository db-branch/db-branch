//go:build linux

package keychain_test

import (
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/keychain"
)

func TestLinuxBackend_SecretToolで保存読込削除(t *testing.T) {
	stored := ""
	run := func(name, stdin string, args ...string) (string, string, error) {
		if name != "secret-tool" {
			t.Fatalf("cmd = %q", name)
		}
		switch args[0] {
		case "store":
			if stdin == "fail" {
				return "", "dbus error", errors.New("exit status 1")
			}
			stored = stdin
			return "", "", nil
		case "lookup":
			if stored == "" {
				return "", "", errors.New("exit status 1")
			}
			return stored + "\n", "", nil
		case "clear":
			stored = ""
			return "", "", nil
		}
		return "", "", errors.New("unexpected")
	}
	noDBus := keychain.NewLinuxBackendForTest(func(string) string { return "" }, func(s string) (string, error) { return "/usr/bin/" + s, nil }, run)
	if noDBus.Available() {
		t.Error("without DBUS_SESSION_BUS_ADDRESS should not be available")
	}
	b := keychain.NewLinuxBackendForTest(func(k string) string {
		if k == "DBUS_SESSION_BUS_ADDRESS" {
			return "unix:path=/run/user/1000/bus"
		}
		return ""
	}, func(s string) (string, error) { return "/usr/bin/" + s, nil }, run)
	if !b.Available() || b.Name() == "" {
		t.Fatal("expected available linux backend")
	}
	if err := b.Save("prod", "dbb_123_secret"); err != nil {
		t.Fatalf("Save = %v", err)
	}
	if got, err := b.Load("prod"); err != nil || got != "dbb_123_secret" {
		t.Errorf("Load = (%q, %v)", got, err)
	}
	if err := b.Delete("prod"); err != nil {
		t.Errorf("Delete = %v", err)
	}
	if _, err := b.Load("prod"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("Load after clear = %v", err)
	}
	if err := b.Save("prod", "fail"); err == nil {
		t.Error("expected store failure")
	}
	if err := b.Save("bad/name", "tok"); err == nil {
		t.Error("expected invalid profile error")
	}
	// Non-empty stderr on exit status 1 must not be swallowed as ErrNotFound.
	bErr := keychain.NewLinuxBackendForTest(func(string) string { return "unix:x" },
		func(string) (string, error) { return "", errors.New("not found") },
		func(_ string, _ string, args ...string) (string, string, error) {
			if args[0] == "lookup" {
				return "", "dbus fail", errors.New("exit status 1")
			}
			return "", "clear fail", errors.New("exit status 2")
		})
	if bErr.Available() {
		t.Error("missing secret-tool should report unavailable")
	}
	if _, err := bErr.Load("prod"); err == nil {
		t.Error("lookup error should fail")
	}
	if err := bErr.Delete("prod"); err == nil {
		t.Error("clear error should fail")
	}
	bEmpty := keychain.NewLinuxBackendForTest(nil, nil, func(string, string, ...string) (string, string, error) {
		return "\n", "", nil
	})
	if _, err := bEmpty.Load("prod"); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("empty lookup = %v, want ErrNotFound", err)
	}
}
