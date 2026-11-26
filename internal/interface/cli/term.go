package cli

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

const maxInputLineBytes = 16 * 1024

// readLine reads a single line ending in '\n' or EOF without over-reading buffered bytes.
func readLine(r io.Reader) (string, error) {
	var buf []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			if b[0] == '\n' {
				break
			}
			if len(buf) >= maxInputLineBytes {
				return "", errors.New("input line exceeds maximum length")
			}
			buf = append(buf, b[0])
		}
		if err != nil {
			if len(buf) == 0 {
				return "", err
			}
			break
		}
	}
	return strings.TrimRight(string(buf), "\r"), nil
}

func termReadSecret(r io.Reader) (string, error) {
	return termReadSecretWith(r, term.IsTerminal, term.GetState, term.Restore, term.ReadPassword)
}

func termReadSecretWith(
	r io.Reader,
	isTerminal func(int) bool,
	getState func(int) (*term.State, error),
	restore func(int, *term.State) error,
	readPassword func(int) ([]byte, error),
) (string, error) {
	if f, ok := r.(*os.File); ok && f != nil {
		fd := int(f.Fd())
		if isTerminal(fd) {
			if oldState, err := getState(fd); err == nil {
				sigCh := make(chan os.Signal, 1)
				signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
				done := make(chan struct{})
				defer func() {
					signal.Stop(sigCh)
					close(done)
				}()
				go func() {
					select {
					case <-sigCh:
						_ = restore(fd, oldState)
					case <-done:
					}
				}()
			}
			b, err := readPassword(fd)
			if err != nil {
				return "", err
			}
			if len(b) > maxInputLineBytes {
				return "", errors.New("input line exceeds maximum length")
			}
			return strings.TrimRight(string(b), "\r"), nil
		}
	}
	return readLine(r)
}
