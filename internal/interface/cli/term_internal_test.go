package cli

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTermReadSecretWith_エコー無効化と復元と行長上限(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	_, _ = pw.WriteString("my-secret\n")
	_ = pw.Close()

	var restored atomic.Bool
	got, err := termReadSecretWith(pr, func(*os.File) (func(), bool) {
		return func() { restored.Store(true) }, true
	})
	if err != nil || got != "my-secret" {
		t.Fatalf("got = (%q, %v)", got, err)
	}
	if !restored.Load() {
		t.Error("expected echo restore callback to be called")
	}

	tooLong := strings.NewReader(strings.Repeat("a", maxInputLineBytes+1) + "\n")
	if _, err := readLine(tooLong); err == nil {
		t.Error("oversized line should fail")
	}
}
