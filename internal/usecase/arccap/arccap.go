// Package arccap keeps a node's ZFS ARC cap in line with the cluster setting
// zfs.arc_max.
package arccap

import (
	"fmt"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
)

// System is the node's memory and the ZFS module parameter.
type System interface {
	MemTotal() (uint64, error)
	// Current is the cap the running kernel uses.
	Current() (uint64, error)
	// Set makes max the cap now and on future boots.
	Set(max uint64) error
	// Conflict is another file that sets the cap at boot ("" if none).
	Conflict() string
}

// Controller applies the setting on this node.
type Controller struct {
	Sys System
	// Initial is the cap `db-branch init` chose (config.toml or a quarter of
	// memory); an empty setting returns to it.
	Initial func() (uint64, error)

	mu      sync.Mutex
	changed bool
}

// Check reports whether raw is acceptable on this node (syntax always; the
// bounds when the node's memory is known).
func (c *Controller) Check(raw string) error {
	l, err := domain.ParseArcLimit(raw)
	if err != nil || l.Empty() {
		return err
	}
	mem, err := c.Sys.MemTotal()
	if err != nil {
		return nil
	}
	_, err = l.Resolve(mem)
	return err
}

// EffectiveMB is the cap the running kernel uses, in MB (0 if unknown).
func (c *Controller) EffectiveMB() uint64 {
	v, err := c.Sys.Current()
	if err != nil {
		return 0
	}
	return v >> 20
}

// Sync makes the node match raw, the value of the setting. Empty leaves the
// node as init made it, or returns it there after a value was removed. The
// error says why the node does not match; call again to retry.
func (c *Controller) Sync(raw string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, err := domain.ParseArcLimit(raw)
	if err != nil {
		return err
	}
	var target uint64
	if l.Empty() {
		if !c.changed {
			return nil
		}
		if target, err = c.Initial(); err != nil {
			return err
		}
	} else {
		mem, err := c.Sys.MemTotal()
		if err != nil {
			return err
		}
		if target, err = l.Resolve(mem); err != nil {
			return err
		}
	}
	cur, err := c.Sys.Current()
	if err != nil {
		return fmt.Errorf("read zfs_arc_max: %w", err)
	}
	if cur != target {
		if err := c.Sys.Set(target); err != nil {
			return err
		}
	}
	c.changed = !l.Empty()
	if other := c.Sys.Conflict(); other != "" && !l.Empty() {
		return fmt.Errorf("applied now, but %s also sets zfs_arc_max and may override it after a reboot", other)
	}
	return nil
}
