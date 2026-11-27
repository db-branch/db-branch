package execx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/execx"
)

func TestRun_標準出力を返す(t *testing.T) {
	out, err := execx.Runner{}.Run(context.Background(), "sh", "-c", "printf hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("stdout = %q", out)
	}
}

func TestRun_失敗したら標準エラーを含むエラーを返す(t *testing.T) {
	_, err := execx.Runner{}.Run(context.Background(), "sh", "-c", "echo boom >&2; exit 3")

	var ee *execx.Error
	if !errors.As(err, &ee) {
		t.Fatalf("want *execx.Error, got %v", err)
	}
	if ee.Stderr != "boom" {
		t.Errorf("Stderr = %q", ee.Stderr)
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "sh") {
		t.Errorf("error message should name the command and stderr: %q", err.Error())
	}
	if errors.Unwrap(err) == nil {
		t.Error("error should wrap the underlying exec error")
	}
}

func TestRunInput_標準入力を渡して実行する(t *testing.T) {
	out, err := execx.Runner{}.RunInput(context.Background(), strings.NewReader("abc"), "cat")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "abc" {
		t.Errorf("stdout = %q", out)
	}
}

func TestStream_出力を書き込み先へ流す(t *testing.T) {
	var out strings.Builder
	if err := (execx.Runner{}).Stream(context.Background(), strings.NewReader("xyz"), &out, "cat"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "xyz" {
		t.Errorf("out = %q", out.String())
	}
	if err := (execx.Runner{}).Stream(context.Background(), nil, &out, "sh", "-c", "exit 2"); err == nil {
		t.Error("want error")
	}
}

func TestStreamEnv_環境変数を足して実行し標準出力を流す(t *testing.T) {
	var out strings.Builder
	err := execx.Runner{}.StreamEnv(context.Background(), []string{"DBB_SECRET=hunter2"}, &out, "sh", "-c", `printf %s "$DBB_SECRET"`)
	if err != nil || out.String() != "hunter2" {
		t.Fatalf("out = %q, err = %v", out.String(), err)
	}
}

func TestStreamDir_作業ディレクトリを指定して標準出力を流す(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	if err := (execx.Runner{}).StreamDir(context.Background(), dir, &out, "pwd"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(out.String()), dir) {
		t.Fatalf("pwd = %q, want %q", out.String(), dir)
	}
}

func TestRunInputEnv_環境変数と標準入力を渡して実行しエラーに環境変数を含めない(t *testing.T) {
	// Overrides an existing parent environment variable with the same name.
	t.Setenv("DBB_SECRET", "old-parent-value")
	out, err := execx.Runner{}.RunInputEnv(
		context.Background(),
		[]string{"DBB_SECRET=hunter2"},
		strings.NewReader("from-stdin"),
		"sh", "-c", `printf "%s:%s" "$DBB_SECRET" "$(cat)"`,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hunter2:from-stdin" {
		t.Errorf("stdout = %q, want %q", out, "hunter2:from-stdin")
	}

	// nil env and nil stdin work like Run.
	out, err = execx.Runner{}.RunInputEnv(context.Background(), nil, nil, "sh", "-c", "printf ok")
	if err != nil || string(out) != "ok" {
		t.Fatalf("nil env/stdin: out=%q err=%v", out, err)
	}

	// Non-zero exit and missing binary errors never leak secret env values.
	for _, cmd := range [][]string{
		{"sh", "-c", "echo failed >&2; exit 2"},
		{"/nonexistent-binary-db-branch-test"},
	} {
		_, err = execx.Runner{}.RunInputEnv(
			context.Background(),
			[]string{"DBB_SECRET=super-secret-pw"},
			strings.NewReader("bad-input"),
			cmd[0], cmd[1:]...,
		)
		if err == nil {
			t.Fatalf("%v: want error", cmd)
		}
		if strings.Contains(err.Error(), "super-secret-pw") {
			t.Errorf("%v: error leaked secret env: %q", cmd, err.Error())
		}
	}
}
