package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRunInteractiveCommand_実行と終了コードとenvフィルタ(t *testing.T) {
	var out, errOut bytes.Buffer
	code, err := runInteractiveCommand("go", []string{"version"}, os.Environ(), strings.NewReader(""), &out, &errOut)
	if err != nil || code != 0 || !strings.Contains(out.String(), "go version") {
		t.Fatalf("go version = (%d, %v, %q)", code, err, out.String())
	}
	code, err = runInteractiveCommand("go", []string{"nonexistent-subcommand-xyz"}, nil, strings.NewReader(""), &out, &errOut)
	if err == nil || code == 0 {
		t.Errorf("expected non-zero exit for bad subcommand, got (%d, %v)", code, err)
	}
	code, err = runInteractiveCommand("nonexistent-binary-xyz-123", nil, nil, strings.NewReader(""), &out, &errOut)
	if err == nil || code != 1 {
		t.Errorf("missing binary = (%d, %v)", code, err)
	}

	// Verify SIGINT disposition inside child is not SIG_IGN (e.g. a shell/subcommand is interruptible).
	if _, err := os.Stat("/bin/sh"); err == nil {
		code, err = runInteractiveCommand("/bin/sh", []string{"-c", "kill -INT $$"}, nil, strings.NewReader(""), &out, &errOut)
		if err == nil || code == 0 {
			t.Errorf("child killed by SIGINT should exit non-zero without killing test parent, got (%d, %v)", code, err)
		}
	}

	filtered := filterEnv([]string{"PATH=/usr/bin", "PGPASSWORD=secret", "PGPASSFILE=/tmp/p", "HOME=/home/a"}, "PGPASSFILE", "PGPASSWORD")
	if len(filtered) != 2 || filtered[0] != "PATH=/usr/bin" || filtered[1] != "HOME=/home/a" {
		t.Errorf("filtered = %v", filtered)
	}
	if got := sanitizeFilename("..."); got != "branch" {
		t.Errorf("sanitizeFilename(...) = %q", got)
	}
	if got := maskDSN("mysql://u@host:3306/db", ""); got != "mysql://u@host:3306/db" {
		t.Errorf("maskDSN without pw = %q", got)
	}
	if got := maskDSN("://bad", "pw"); got != "://bad" {
		t.Errorf("maskDSN invalid url = %q", got)
	}
}
