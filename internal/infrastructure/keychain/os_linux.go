//go:build linux

package keychain

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type linuxBackend struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	run      cmdRunner
}

func newOSBackend() Backend {
	return &linuxBackend{getenv: os.Getenv, lookPath: exec.LookPath, run: execCmdRunner}
}

// NewLinuxBackendForTest constructs a linuxBackend with injectable hooks.
func NewLinuxBackendForTest(getenv func(string) string, lookPath func(string) (string, error), run func(name, stdin string, args ...string) (string, string, error)) Backend {
	return &linuxBackend{getenv: getenv, lookPath: lookPath, run: run}
}

func (l *linuxBackend) Name() string { return "Secret Service" }

func (l *linuxBackend) Available() bool {
	if l.getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	_, err := l.lookPath("secret-tool")
	return err == nil
}

func (l *linuxBackend) Save(profile, token string) error {
	if err := validateTokenInput(profile, token); err != nil {
		return err
	}
	_, stderr, err := l.run("secret-tool", token, "store",
		"--label", fmt.Sprintf("db-branch (%s)", profile),
		"service", serviceName, "profile", profile)
	if err != nil {
		return fmt.Errorf("secret-tool store: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}

func (l *linuxBackend) Load(profile string) (string, error) {
	stdout, stderr, err := l.run("secret-tool", "", "lookup", "service", serviceName, "profile", profile)
	if err != nil {
		// secret-tool lookup exits 1 with empty stdout and empty stderr when not found.
		// Non-empty stderr indicates DBus/keyring failure and must not be masked as ErrNotFound.
		if strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == "" {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("secret-tool lookup: %w (%s)", err, strings.TrimSpace(stderr))
	}
	tok := strings.TrimRight(stdout, "\r\n")
	if tok == "" {
		return "", ErrNotFound
	}
	return tok, nil
}

func (l *linuxBackend) Delete(profile string) error {
	_, stderr, err := l.run("secret-tool", "", "clear", "service", serviceName, "profile", profile)
	if err != nil {
		return fmt.Errorf("secret-tool clear: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}
