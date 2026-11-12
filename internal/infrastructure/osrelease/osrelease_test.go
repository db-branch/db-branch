package osrelease_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/osrelease"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDistro_IDとVERSION_IDからディストリビューション名を作る(t *testing.T) {
	for body, want := range map[string]string{
		"NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"26.04\"\n":         "ubuntu-26.04",
		"# comment\nID=ubuntu\nVERSION_ID=\"24.04\"\nID_LIKE=debian": "ubuntu-24.04",
		"ID='debian'\nVERSION_ID='13'\n":                             "debian-13",
		"ID=Ubuntu\nVERSION_ID=24.04\n":                              "ubuntu-24.04",
	} {
		got, err := osrelease.Distro(write(t, body))
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", body, got, err, want)
		}
	}
}

func TestDistro_読めないか欠けているときはエラー(t *testing.T) {
	if _, err := osrelease.Distro(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("missing file: want error")
	}
	if _, err := osrelease.Distro(write(t, "NAME=x\nID=ubuntu\n")); err == nil {
		t.Error("no VERSION_ID: want error")
	}
	if _, err := osrelease.Distro(write(t, "VERSION_ID=1\n")); err == nil {
		t.Error("no ID: want error")
	}
}
