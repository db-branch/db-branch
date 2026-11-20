package runtimegrpc

import (
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// LookupUser finds the numeric uid and gid of an OS user.
type LookupUser func(name string) (uid, gid uint32, err error)

// SystemUser is the LookupUser over the system's user database.
func SystemUser(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return 0, 0, err
	}
	return uint32(uid), uint32(gid), nil
}

// Command builds the process of a verified module: its binary, run as the
// module's own OS user (db-branch-rt-<engine>, never root and never in group
// 0), in "/", with a minimal environment: the agent's environment holds
// secrets the module has no use for.
func Command(m domain.RuntimeManifest, lookup LookupUser) (*exec.Cmd, error) {
	name := m.RunAsUser
	if name == "" {
		name = "db-branch-rt-" + m.Engine
	}
	uid, gid, err := lookup(name)
	if err != nil {
		return nil, fmt.Errorf("runtime module %s: OS user %s: %w", m.Engine, name, err)
	}
	if uid == 0 || gid == 0 {
		return nil, fmt.Errorf("runtime module %s: OS user %s must not be root (uid %d, gid %d)", m.Engine, name, uid, gid)
	}
	cmd := exec.Command(m.Binary)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	return cmd, nil
}

// Start launches cmd as the module of m and accepts it only when its
// Describe() answer equals the signed manifest. A module that differs is
// stopped.
func Start(cmd *exec.Cmd, m domain.RuntimeManifest) (*Provider, func(), error) {
	p, stop, err := Launch(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime module %s: %w", m.Engine, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec, err := p.Describe(ctx)
	if err == nil {
		err = m.CheckDescribe(spec)
	}
	if err != nil {
		stop()
		return nil, nil, err
	}
	return p, stop, nil
}
