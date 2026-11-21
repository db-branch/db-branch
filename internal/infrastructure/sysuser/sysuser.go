// Package sysuser ensures isolated, non-login OS system users for runtime modules.
package sysuser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
)

const (
	nonexistentHome   = "/nonexistent"
	nologinShell      = "/usr/sbin/nologin"
	defaultPasswdPath = "/etc/passwd"
)

// Runner runs external commands directly without a shell.
// execx.Runner satisfies this interface.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

var _ Runner = execx.Runner{}

// ErrUnsafeUser is returned when an existing OS user or group violates the
// isolation requirements for a runtime module account.
var ErrUnsafeUser = errors.New("unsafe system user")

// Users manages runtime module OS accounts ("db-branch-rt-<engine>").
type Users struct {
	// Run executes groupadd and useradd without a shell (default: execx.Runner{}).
	Run Runner
	// Lookup resolves an OS user by name (default: user.Lookup).
	Lookup func(username string) (*user.User, error)
	// LookupGroup resolves an OS group by name (default: user.LookupGroup).
	LookupGroup func(name string) (*user.Group, error)
	// PasswdPath is the passwd file consulted to verify an existing user's
	// login shell when LookupShell is nil (default: "/etc/passwd").
	PasswdPath string
	// LookupShell returns an existing user's login shell. When nil, PasswdPath is parsed.
	LookupShell func(username string) (string, error)
}

func (u *Users) runner() Runner {
	if u.Run != nil {
		return u.Run
	}
	return execx.Runner{}
}

func (u *Users) lookupUser(name string) (*user.User, error) {
	if u.Lookup != nil {
		return u.Lookup(name)
	}
	return user.Lookup(name)
}

func (u *Users) lookupGroup(name string) (*user.Group, error) {
	if u.LookupGroup != nil {
		return u.LookupGroup(name)
	}
	return user.LookupGroup(name)
}

func (u *Users) lookupShell(username string) (string, error) {
	if u.LookupShell != nil {
		return u.LookupShell(username)
	}
	path := u.PasswdPath
	if path == "" {
		path = defaultPasswdPath
	}
	return lookupShellFromPasswd(path, username)
}

// EnsureSystemUser idempotently ensures that the isolated OS user and group
// for username exist. Only "db-branch-rt-<engine>" names whose engine satisfies
// domain.ValidRuntimeModuleEngine are accepted.
func (u *Users) EnsureSystemUser(ctx context.Context, username string) (created bool, uid, gid uint32, err error) {
	engine, ok := strings.CutPrefix(username, domain.RuntimeUserPrefix)
	if !ok {
		return false, 0, 0, fmt.Errorf("%w: system user %q must start with %q", domain.ErrInvalidModule, username, domain.RuntimeUserPrefix)
	}
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return false, 0, 0, fmt.Errorf("%w: system user %q has invalid engine %q", err, username, engine)
	}

	run := u.runner()
	existing, err := u.lookupUser(username)
	if err == nil {
		uid, gid, err = u.verifyExistingUser(username, existing)
		if err != nil {
			return false, 0, 0, err
		}
		return false, uid, gid, nil
	}
	var unknown user.UnknownUserError
	if !errors.As(err, &unknown) {
		return false, 0, 0, fmt.Errorf("lookup user %q: %w", username, err)
	}

	if grp, gErr := u.lookupGroup(username); gErr != nil {
		var unknownGroup user.UnknownGroupError
		if !errors.As(gErr, &unknownGroup) {
			return false, 0, 0, fmt.Errorf("lookup group %q: %w", username, gErr)
		}
		if _, err := run.Run(ctx, "groupadd", "--system", username); err != nil {
			return false, 0, 0, fmt.Errorf("create system group %q: %w", username, err)
		}
	} else {
		ggid, err := strconv.ParseUint(grp.Gid, 10, 32)
		if err != nil {
			return false, 0, 0, fmt.Errorf("%w: group %q has non-numeric gid %q", ErrUnsafeUser, username, grp.Gid)
		}
		if ggid == 0 {
			return false, 0, 0, fmt.Errorf("%w: group %q must not have gid 0", ErrUnsafeUser, username)
		}
	}

	if _, err := run.Run(ctx, "useradd",
		"--system",
		"--gid", username,
		"--no-create-home",
		"--home-dir", nonexistentHome,
		"--shell", nologinShell,
		username,
	); err != nil {
		return false, 0, 0, fmt.Errorf("create system user %q: %w", username, err)
	}

	createdUser, err := u.lookupUser(username)
	if err != nil {
		return false, 0, 0, fmt.Errorf("verify created user %q: %w", username, err)
	}
	uid, gid, err = parseNonRootIDs(username, createdUser)
	if err != nil {
		return false, 0, 0, err
	}
	return true, uid, gid, nil
}

func (u *Users) verifyExistingUser(username string, existing *user.User) (uint32, uint32, error) {
	uid, gid, err := parseNonRootIDs(username, existing)
	if err != nil {
		return 0, 0, err
	}
	if existing.HomeDir != nonexistentHome {
		return 0, 0, fmt.Errorf("%w: existing user %q has home directory %q (want %q)", ErrUnsafeUser, username, existing.HomeDir, nonexistentHome)
	}
	shell, err := u.lookupShell(username)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: cannot verify login shell of existing user %q: %v", ErrUnsafeUser, username, err)
	}
	if !isNoLoginShell(shell) {
		return 0, 0, fmt.Errorf("%w: existing user %q has interactive login shell %q (want nologin or false)", ErrUnsafeUser, username, shell)
	}
	return uid, gid, nil
}

func isNoLoginShell(shell string) bool {
	switch shell {
	case "/usr/sbin/nologin", "/sbin/nologin", "/usr/bin/false", "/bin/false":
		return true
	default:
		return false
	}
}

func lookupShellFromPasswd(passwdPath, username string) (string, error) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 7 {
			continue
		}
		if fields[0] == username {
			return fields[6], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("user %q not found in %s", username, passwdPath)
}

func parseNonRootIDs(username string, usr *user.User) (uint32, uint32, error) {
	uid, err := strconv.ParseUint(usr.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: user %q has non-numeric uid %q", ErrUnsafeUser, username, usr.Uid)
	}
	gid, err := strconv.ParseUint(usr.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: user %q has non-numeric gid %q", ErrUnsafeUser, username, usr.Gid)
	}
	if uid == 0 {
		return 0, 0, fmt.Errorf("%w: user %q must not have uid 0", ErrUnsafeUser, username)
	}
	if gid == 0 {
		return 0, 0, fmt.Errorf("%w: user %q must not have gid 0", ErrUnsafeUser, username)
	}
	return uint32(uid), uint32(gid), nil
}
