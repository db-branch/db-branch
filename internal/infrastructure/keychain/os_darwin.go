//go:build darwin

package keychain

import (
	"fmt"
	"os"
	"strings"
)

const macSecurityBin = "/usr/bin/security"

type darwinBackend struct {
	bin  string
	stat func(string) (os.FileInfo, error)
	run  cmdRunner
}

func newOSBackend() Backend {
	return &darwinBackend{bin: macSecurityBin, stat: os.Stat, run: execCmdRunner}
}

// NewDarwinBackendForTest constructs a darwinBackend with injectable hooks.
func NewDarwinBackendForTest(stat func(string) (os.FileInfo, error), run func(name, stdin string, args ...string) (string, string, error)) Backend {
	return &darwinBackend{bin: macSecurityBin, stat: stat, run: run}
}

func (d *darwinBackend) Name() string { return "macOS Keychain" }

func (d *darwinBackend) Available() bool {
	info, err := d.stat(d.bin)
	return err == nil && !info.IsDir()
}

func escapeSecurityArg(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func (d *darwinBackend) Save(profile, token string) error {
	if err := validateTokenInput(profile, token); err != nil {
		return err
	}
	// Run add-generic-password via `/usr/bin/security -i` stdin so the token never appears in argv/ps.
	script := fmt.Sprintf("add-generic-password -U -a %s -s %s -w %s\n",
		escapeSecurityArg(profile), escapeSecurityArg(serviceName), escapeSecurityArg(token))
	_, stderr, err := d.run(d.bin, script, "-i")
	if err != nil {
		return fmt.Errorf("keychain save: %w (%s)", err, strings.TrimSpace(stderr))
	}
	// Verify via direct lookup because `security -i` may exit 0 even on subcommand failure.
	got, err := d.Load(profile)
	if err != nil || got != token {
		return fmt.Errorf("keychain save verification failed: %v", err)
	}
	return nil
}

func (d *darwinBackend) Load(profile string) (string, error) {
	stdout, stderr, err := d.run(d.bin, "", "find-generic-password", "-a", profile, "-s", serviceName, "-w")
	if err != nil {
		if strings.Contains(stderr, "could not be found") || strings.Contains(err.Error(), "exit status 44") {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("keychain load: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return strings.TrimRight(stdout, "\r\n"), nil
}

func (d *darwinBackend) Delete(profile string) error {
	_, stderr, err := d.run(d.bin, "", "delete-generic-password", "-a", profile, "-s", serviceName)
	if err != nil {
		if strings.Contains(stderr, "could not be found") || strings.Contains(err.Error(), "exit status 44") {
			return ErrNotFound
		}
		return fmt.Errorf("keychain delete: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}
