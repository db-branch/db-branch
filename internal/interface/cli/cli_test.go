package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/interface/cli"
)

func run(t *testing.T, ctx context.Context, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := cli.App{Version: "1.2.3", Stdout: &out, Stderr: &errOut}
	code = app.Run(ctx, args)
	return code, out.String(), errOut.String()
}

func TestVersion_バージョンを表示する(t *testing.T) {
	code, out, _ := run(t, context.Background(), "version")

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if out != "db-branch 1.2.3\n" {
		t.Errorf("stdout = %q", out)
	}
}

func TestRun_引数なしなら使い方を表示して正常終了する(t *testing.T) {
	code, out, _ := run(t, context.Background())

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("stdout should contain usage, got %q", out)
	}
}

func TestRun_知らないコマンドは失敗する(t *testing.T) {
	code, _, errOut := run(t, context.Background(), "frobnicate")

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero")
	}
	if !strings.Contains(errOut, "frobnicate") {
		t.Errorf("stderr should name the unknown command, got %q", errOut)
	}
}

func TestAgent_停止を指示されたら正常終了する(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	missing := filepath.Join(t.TempDir(), "config.toml")

	code, _, errOut := run(t, ctx, "agent", "--config", missing)

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(errOut, "agent started") || !strings.Contains(errOut, "agent stopped") {
		t.Errorf("stderr should log start and stop, got %q", errOut)
	}
}

func TestAgent_設定ファイルが不正なら起動せず失敗する(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("pol = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := run(t, context.Background(), "agent", "--config", path)

	if code == 0 {
		t.Fatal("exit code = 0, want non-zero")
	}
	if !strings.Contains(errOut, "pol") {
		t.Errorf("stderr should explain the config error, got %q", errOut)
	}
}
