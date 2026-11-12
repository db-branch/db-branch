// Package execx runs external commands without a shell.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Error is returned when a command exits unsuccessfully.
type Error struct {
	Command string
	Stderr  string
	Err     error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v: %s", e.Command, e.Err, e.Stderr) }
func (e *Error) Unwrap() error { return e.Err }

// Runner executes commands directly (no shell), so arguments are never re-parsed.
type Runner struct{}

// Run executes name with args and returns its standard output.
func (r Runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.RunInput(ctx, nil, name, args...)
}

// RunInput is Run with stdin connected to in.
func (r Runner) RunInput(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	err := r.Stream(ctx, in, &stdout, name, args...)
	if err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// Stream runs name with stdin from in (may be nil) and stdout written to
// out as it is produced, for data too large to hold in memory.
func (r Runner) Stream(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
	return r.run(ctx, nil, in, out, name, args...)
}

// StreamEnv is Stream with extra environment variables (NAME=value), which
// keeps secrets off the command line. The error never contains them.
func (r Runner) StreamEnv(ctx context.Context, env []string, out io.Writer, name string, args ...string) error {
	return r.run(ctx, env, nil, out, name, args...)
}

func (Runner) run(ctx context.Context, env []string, in io.Reader, out io.Writer, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &Error{
			Command: strings.Join(append([]string{name}, args...), " "),
			Stderr:  strings.TrimSpace(stderr.String()),
			Err:     err,
		}
	}
	return nil
}
