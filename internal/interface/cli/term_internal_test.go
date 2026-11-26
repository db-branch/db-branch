package cli

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/term"
)

func TestTermReadSecretWith_TTYはx_termで読み非TTYはreadLineにフォールバックする(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	_, _ = pw.WriteString("pipe-secret\n")
	_ = pw.Close()

	// Non-TTY (*os.File pipe): falls back to readLine without invoking readPassword.
	var pwCalled atomic.Bool
	got, err := termReadSecret(pr)
	if err != nil || got != "pipe-secret" {
		t.Fatalf("pipe termReadSecret = (%q, %v)", got, err)
	}

	// Non-*os.File reader: falls back to readLine.
	got, err = termReadSecretWith(
		strings.NewReader("reader-secret\r\n"),
		func(int) bool { pwCalled.Store(true); return true },
		term.GetState,
		term.Restore,
		func(int) ([]byte, error) { pwCalled.Store(true); return nil, nil },
	)
	if err != nil || got != "reader-secret" || pwCalled.Load() {
		t.Fatalf("reader fallback = (%q, %v, called=%v)", got, err, pwCalled.Load())
	}

	// TTY (*os.File where isTerminal is true): uses readPassword and handles errors and length limits.
	pr2, pw2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr2.Close()
	_ = pw2.Close()

	got, err = termReadSecretWith(
		pr2,
		func(int) bool { return true },
		func(int) (*term.State, error) { return &term.State{}, nil },
		func(int, *term.State) error { return nil },
		func(int) ([]byte, error) { return []byte("tty-secret\r"), nil },
	)
	if err != nil || got != "tty-secret" {
		t.Fatalf("tty termReadSecretWith = (%q, %v)", got, err)
	}

	if _, err := termReadSecretWith(
		pr2,
		func(int) bool { return true },
		func(int) (*term.State, error) { return nil, errors.New("no state") },
		term.Restore,
		func(int) ([]byte, error) { return nil, errors.New("read failed") },
	); err == nil {
		t.Error("expected readPassword error")
	}

	if _, err := termReadSecretWith(
		pr2,
		func(int) bool { return true },
		func(int) (*term.State, error) { return nil, errors.New("no state") },
		term.Restore,
		func(int) ([]byte, error) { return []byte(strings.Repeat("a", maxInputLineBytes+1)), nil },
	); err == nil {
		t.Error("expected oversized tty secret to fail")
	}

	tooLong := strings.NewReader(strings.Repeat("a", maxInputLineBytes+1) + "\n")
	if _, err := readLine(tooLong); err == nil {
		t.Error("oversized line should fail")
	}
}
