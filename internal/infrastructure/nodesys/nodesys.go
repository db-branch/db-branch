// Package nodesys reports the system a node runs on (distribution, kernel,
// ZFS version) for the cluster view. The answer is cached: none of it changes
// without a reboot or package upgrade, and the heartbeat asks every few seconds.
package nodesys

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/osrelease"
)

// Collector gathers the facts.
type Collector struct {
	// OSRelease is the os-release file and Kernel the file holding the kernel
	// release (/proc/sys/kernel/osrelease).
	OSRelease, Kernel string
	// Run executes a command and returns its output (to run `zfs version`).
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// TTL is how long an answer is reused; Now is the clock.
	TTL time.Duration
	Now func() time.Time

	mu   sync.Mutex
	last domain.NodeSystem
	at   time.Time
}

// Default reads the real system.
func Default(run func(ctx context.Context, name string, args ...string) ([]byte, error)) *Collector {
	return &Collector{OSRelease: osrelease.Path, Kernel: "/proc/sys/kernel/osrelease", Run: run, TTL: time.Hour, Now: time.Now}
}

// Get returns the facts, collecting them again once the cached ones are older
// than TTL. What cannot be read stays empty.
func (c *Collector) Get(ctx context.Context) domain.NodeSystem {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && c.Now().Sub(c.at) < c.TTL {
		return c.last
	}
	var s domain.NodeSystem
	if r, err := osrelease.Read(c.OSRelease); err == nil {
		s.Distro, s.OS = r.Distro, r.Pretty
	}
	s.Kernel = readFirstLine(c.Kernel)
	if out, err := c.Run(ctx, "zfs", "version"); err == nil {
		s.ZFS = ParseZFSVersion(string(out))
	}
	c.last, c.at = s, c.Now()
	return s
}

// ParseZFSVersion reads the output of `zfs version`:
//
//	zfs-2.2.2-0ubuntu9
//	zfs-kmod-2.2.2-0ubuntu9
//
// It returns the userland version, with the kernel module's in parentheses
// when that differs ("2.3.1-1 (kmod 2.2.2-0ubuntu9)"), or "" if there is none.
func ParseZFSVersion(out string) string {
	var user, kmod string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "zfs-kmod-"):
			kmod = strings.TrimPrefix(line, "zfs-kmod-")
		case strings.HasPrefix(line, "zfs-"):
			user = strings.TrimPrefix(line, "zfs-")
		}
	}
	switch {
	case user == "":
		return kmod
	case kmod == "" || kmod == user:
		return user
	}
	return user + " (kmod " + kmod + ")"
}
