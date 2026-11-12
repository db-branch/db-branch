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
