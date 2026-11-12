// Package svcids implements idalign.Host on a real machine: it reads and
// changes the db-branch user and group and the ownership of their files.
package svcids

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/idalign"
)

// Account is the name of both the system user and its group.
const Account = "db-branch"

// Runner runs external commands.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Host changes the db-branch account of this machine.
type Host struct {
	Run Runner
	// Systemd is whether services are managed by systemd (false: nothing to stop).
	Systemd bool
	// Pool is the ZFS pool whose db-branch datasets are re-owned ("": none).
	Pool string
	// Dirs are directory trees to re-own; Globs are patterns of more.
	Dirs  []string
	Globs []string

	// Chown, LookupUserID and LookupGroupID default to the operating system's;
	// tests replace them.
	Chown         func(path string, uid, gid int) error
	LookupUser    func(name string) (*user.User, error)
	LookupGroup   func(name string) (*user.Group, error)
	LookupUserID  func(id string) (*user.User, error)
	LookupGroupID func(id string) (*user.Group, error)
}

var _ idalign.Host = (*Host)(nil)

// Local returns the ids of the db-branch user and group.
func (h *Host) Local(context.Context) (domain.ServiceIDs, error) {
	lookUser, lookGroup := h.LookupUser, h.LookupGroup
	if lookUser == nil {
		lookUser = user.Lookup
	}
	if lookGroup == nil {
		lookGroup = user.LookupGroup
	}
	u, err := lookUser(Account)
	if err != nil {
		return domain.ServiceIDs{}, err
	}
	g, err := lookGroup(Account)
	if err != nil {
		return domain.ServiceIDs{}, err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(g.Gid)
	if err1 != nil || err2 != nil {
		return domain.ServiceIDs{}, fmt.Errorf("non-numeric ids for %s: %q %q", Account, u.Uid, g.Gid)
	}
	return domain.ServiceIDs{UID: uid, GID: gid}, nil
}

// UserWithUID returns the user that has uid ("" when it is free).
func (h *Host) UserWithUID(uid int) string {
	look := h.LookupUserID
	if look == nil {
		look = user.LookupId
	}
	if u, err := look(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return ""
}

// GroupWithGID returns the group that has gid ("" when it is free).
func (h *Host) GroupWithGID(gid int) string {
	look := h.LookupGroupID
	if look == nil {
		look = user.LookupGroupId
	}
	if g, err := look(strconv.Itoa(gid)); err == nil {
		return g.Name
	}
	return ""
}

// StopServices stops the agent and the database servers it started.
func (h *Host) StopServices(ctx context.Context) error {
	if !h.Systemd {
		return nil
	}
	if _, err := h.Run.Run(ctx, "systemctl", "stop", "db-branch.service"); err != nil {
		return err
	}
	out, err := h.Run.Run(ctx, "systemctl", "list-units", "--plain", "--no-legend", "--all", "--type=service",
		"db-branch-mysqld@*.service", "db-branch-postgres@*.service")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			if _, err := h.Run.Run(ctx, "systemctl", "stop", f[0]); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetIDs runs groupmod and usermod.
func (h *Host) SetIDs(ctx context.Context, from, to domain.ServiceIDs) error {
	var args []string
	if from.UID != to.UID {
		args = append(args, "-u", strconv.Itoa(to.UID))
	}
	if from.GID != to.GID {
		if _, err := h.Run.Run(ctx, "groupmod", "-g", strconv.Itoa(to.GID), Account); err != nil {
			return err
		}
		args = append(args, "-g", strconv.Itoa(to.GID))
	}
	if len(args) == 0 {
		return nil
	}
	_, err := h.Run.Run(ctx, "usermod", append(args, Account)...)
	return err
}

// Reown gives every file under the configured trees and the pool's dataset
// mounts that belongs to the old uid or gid to the new one (symbolic links
// themselves, never what they point to).
func (h *Host) Reown(ctx context.Context, from, to domain.ServiceIDs) error {
	roots := append([]string(nil), h.Dirs...)
	for _, g := range h.Globs {
		m, _ := filepath.Glob(g) // only a malformed pattern fails
		roots = append(roots, m...)
	}
	mounts, err := h.mounts(ctx)
	if err != nil {
		return err
	}
	chown := h.Chown
	if chown == nil {
		chown = os.Lchown
	}
	for _, root := range append(roots, mounts...) {
		if _, err := os.Lstat(root); err != nil {
			continue // not there on this node
		}
		if err := reown(root, from, to, chown); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) mounts(ctx context.Context) ([]string, error) {
	if h.Pool == "" {
		return nil, nil
	}
	out, err := h.Run.Run(ctx, "zfs", "list", "-H", "-o", "mountpoint", "-r", h.Pool+"/db-branch")
	if err != nil {
		return nil, fmt.Errorf("listing dataset mounts: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); strings.HasPrefix(p, "/") {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func reown(root string, from, to domain.ServiceIDs, chown func(string, int, int) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		uid, gid := int(st.Uid), int(st.Gid)
		nu, ng := uid, gid
		if uid == from.UID {
			nu = to.UID
		}
		if gid == from.GID {
			ng = to.GID
		}
		if nu == uid && ng == gid {
			return nil
		}
		return chown(path, nu, ng)
	})
}

// OwnerCheck returns a check that a path belongs to the db-branch user
// (want returns its uid). A path owned by someone else fails with
// domain.ErrOwnership and an explanation; a missing path is left to the
// server to report.
func OwnerCheck(want func() (int, error)) func(path string) error {
	return func(path string) error {
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		uid, err := want()
		if err != nil || int(st.Uid) == uid {
			return nil
		}
		return fmt.Errorf("%w: %s is owned by uid %d but the database server runs as uid %d; %s",
			domain.ErrOwnership, path, st.Uid, uid, domain.OwnershipHint)
	}
}
