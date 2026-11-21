// Package rtunit writes the systemd drop-in of one instance of an external
// engine (db-branch-rt@<id>.service.d/override.conf). The core, as root,
// writes it from what the engine's module asked for (a LaunchPlan), after
// checking every value: the module runs as an unprivileged user and its
// answers must not be able to add a directive of their own.
package rtunit

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

const (
	// DefaultUnitDir is where transient units and their drop-ins live.
	DefaultUnitDir = "/run/systemd/system"
	// StateRoot holds each instance's configuration and logs.
	StateRoot = "/var/lib/db-branch/instances"
	// DefaultRuntimeRoot holds unpacked external database runtimes.
	DefaultRuntimeRoot = "/var/lib/db-branch/runtimes"
	// RunRoot holds each engine's instances' sockets and pid files: RunRoot/<engine>/<id>.
	RunRoot = "/run/db-branch-rt"

	defaultNoFile = 65535
	minNoFile     = 1024
	maxNoFile     = 1048576
	maxMemoryMB   = 1 << 20
	maxCPUQuota   = 1000000
)

// ErrInvalidUnit is returned for a unit that could not be written safely.
var ErrInvalidUnit = errors.New("invalid runtime unit")

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	safePathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/+-]+$`)

	envEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	argEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
)

// Unit is one instance to run.
type Unit struct {
	ID, Engine, User          string
	Plan                      domain.LaunchPlan
	DataDir, StateDir, RunDir string
	Limits                    domain.ResourceLimits
	// StateRoot and RuntimeRoot are where the agent keeps instances' state
	// and unpacked runtimes (the data directory's instances and runtimes);
	// empty: the defaults below.
	StateRoot, RuntimeRoot string
}

// UnitName is the systemd unit of an instance.
func UnitName(id string) string {
	return "db-branch-rt@" + id + ".service"
}

// DropIn renders override.conf for u.
func DropIn(u Unit) (string, error) {
	if err := validateUnit(u); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# Written by db-branch. Changes are overwritten.\n")
	b.WriteString("[Service]\n")
	fmt.Fprintf(&b, "User=%s\n", u.User)
	fmt.Fprintf(&b, "Group=%s\n", u.User)
	b.WriteString("Slice=db-branch.slice\n")
	b.WriteString("NoNewPrivileges=yes\n")
	b.WriteString("ProtectSystem=strict\n")
	b.WriteString("ProtectHome=yes\n")
	b.WriteString("PrivateTmp=yes\n")
	fmt.Fprintf(&b, "RuntimeDirectory=db-branch-rt/%s/%s\n", u.Engine, u.ID)
	b.WriteString("RuntimeDirectoryMode=0750\n")
	// Kept across stops: the module reads sockets and pid files after a stop to verify it was clean.
	b.WriteString("RuntimeDirectoryPreserve=yes\n")
	b.WriteString("InaccessiblePaths=/run/db-branch /etc/db-branch\n")
	fmt.Fprintf(&b, "ReadWritePaths=%s %s %s\n", u.DataDir, u.StateDir, u.RunDir)
	if u.Plan.ServiceType != "" {
		fmt.Fprintf(&b, "Type=%s\n", u.Plan.ServiceType)
	}
	if u.Plan.NotifyAccess != "" {
		fmt.Fprintf(&b, "NotifyAccess=%s\n", u.Plan.NotifyAccess)
	}
	if u.Plan.KillSignal != "" {
		fmt.Fprintf(&b, "KillSignal=%s\n", u.Plan.KillSignal)
	}
	if u.Plan.KillMode != "" {
		fmt.Fprintf(&b, "KillMode=%s\n", u.Plan.KillMode)
	}
	if u.Plan.TimeoutStopSec > 0 {
		fmt.Fprintf(&b, "TimeoutStopSec=%d\n", u.Plan.TimeoutStopSec)
	}
	noFile := u.Limits.NoFile
	if noFile == 0 {
		noFile = defaultNoFile
	}
	fmt.Fprintf(&b, "LimitNOFILE=%d\n", noFile)
	if u.Limits.MemoryMB > 0 {
		fmt.Fprintf(&b, "MemoryMax=%dM\n", int64(u.Limits.MemoryMB)*2+512)
	}
	if u.Limits.CPUQuota > 0 {
		fmt.Fprintf(&b, "CPUQuota=%d%%\n", u.Limits.CPUQuota)
	}
	if len(u.Plan.Env) > 0 {
		keys := make([]string, 0, len(u.Plan.Env))
		for k := range u.Plan.Env {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "Environment=\"%s=%s\"\n", k, envEscaper.Replace(u.Plan.Env[k]))
		}
	}
	b.WriteString("ExecStart=\n")
	fmt.Fprintf(&b, "ExecStart=%s", u.Plan.Command)
	for _, arg := range u.Plan.Args {
		fmt.Fprintf(&b, " \"%s\"", argEscaper.Replace(arg))
	}
	b.WriteByte('\n')
	return b.String(), nil
}

func validateUnit(u Unit) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidUnit, fmt.Sprintf(format, a...))
	}
	if !idPattern.MatchString(u.ID) {
		return bad("id %q must match %s", u.ID, idPattern)
	}
	if err := domain.ValidRuntimeModuleEngine(u.Engine); err != nil {
		return fmt.Errorf("%w: engine %q: %w", ErrInvalidUnit, u.Engine, err)
	}
	if wantUser := domain.RuntimeUserPrefix + u.Engine; u.User != wantUser {
		return bad("user %q: want %q", u.User, wantUser)
	}
	if err := u.Plan.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidUnit, err)
	}
	stateRoot := rootOrDefault(u.StateRoot, StateRoot)
	if !isCleanSafePath(stateRoot) || stateRoot == "/" {
		return bad("state root %q is not allowed", stateRoot)
	}
	runtimeRoot := rootOrDefault(u.RuntimeRoot, DefaultRuntimeRoot)
	if !isCleanSafePath(runtimeRoot) || runtimeRoot == "/" {
		return bad("runtime root %q is not allowed", runtimeRoot)
	}
	if !isCleanSafePath(u.Plan.Command) {
		return bad("command %q must be a clean safe absolute path", u.Plan.Command)
	}
	runtimePrefix := runtimeRoot + "/" + u.Engine + "/"
	manifestPrefix := domain.ManifestRoot + "/" + u.Engine + "/"
	if !strings.HasPrefix(u.Plan.Command, runtimePrefix) && !strings.HasPrefix(u.Plan.Command, manifestPrefix) {
		return bad("command %q must be under %q or %q", u.Plan.Command, runtimePrefix, manifestPrefix)
	}
	if !isCleanSafePath(u.DataDir) || isForbiddenDataDir(u.DataDir) {
		return bad("data dir %q is not allowed", u.DataDir)
	}
	if wantState := stateRoot + "/" + u.ID; u.StateDir != wantState {
		return bad("state dir %q: want %q", u.StateDir, wantState)
	}
	if wantRun := RunRoot + "/" + u.Engine + "/" + u.ID; u.RunDir != wantRun {
		return bad("run dir %q: want %q", u.RunDir, wantRun)
	}
	if u.Limits.MemoryMB < 0 || u.Limits.MemoryMB > maxMemoryMB {
		return bad("memory limit %d out of range [0, %d]", u.Limits.MemoryMB, maxMemoryMB)
	}
	if u.Limits.CPUQuota < 0 || u.Limits.CPUQuota > maxCPUQuota {
		return bad("cpu quota %d out of range [0, %d]", u.Limits.CPUQuota, maxCPUQuota)
	}
	if u.Limits.NoFile != 0 && (u.Limits.NoFile < minNoFile || u.Limits.NoFile > maxNoFile) {
		return bad("nofile %d out of range [%d, %d]", u.Limits.NoFile, minNoFile, maxNoFile)
	}
	return nil
}

func rootOrDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func isCleanSafePath(p string) bool {
	return safePathPattern.MatchString(p) && path.Clean(p) == p
}

func isForbiddenDataDir(p string) bool {
	if p == "/" || p == "/etc" || p == "/run" {
		return true
	}
	return underOrEqual(p, "/etc/db-branch") || underOrEqual(p, "/run/db-branch")
}

func underOrEqual(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

// Writer puts drop-ins in a directory (DefaultUnitDir when empty).
type Writer struct{ Dir string }

func (w Writer) dir() string {
	if w.Dir != "" {
		return w.Dir
	}
	return DefaultUnitDir
}

// Write validates u and writes its drop-in atomically; it returns the path.
func (w Writer) Write(u Unit) (string, error) {
	content, err := DropIn(u)
	if err != nil {
		return "", err
	}
	dropDir := filepath.Join(w.dir(), UnitName(u.ID)+".d")
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dropDir, "override.conf.*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	err = writeTemp(f, content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	target := filepath.Join(dropDir, "override.conf")
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return target, nil
}

func writeTemp(f *os.File, content string) error {
	for _, fn := range []func() error{
		func() error { _, err := f.WriteString(content); return err },
		func() error { return f.Chmod(0o644) },
		f.Sync,
	} {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes the drop-in directory of an instance (none: no error).
func (w Writer) Remove(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: id %q must match %s", ErrInvalidUnit, id, idPattern)
	}
	return os.RemoveAll(filepath.Join(w.dir(), UnitName(id)+".d"))
}
