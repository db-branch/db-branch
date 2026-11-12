// Package idalign makes a node's db-branch user and group use the ids the
// cluster agreed on. Bases travel between nodes as ZFS streams that keep
// numeric owners, so a node whose db-branch user has other ids could not
// start servers on cloned data.
package idalign

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// ErrIDTaken is returned when a cluster id is in use by another account on
// this node, so the db-branch account cannot take it over.
var ErrIDTaken = errors.New("cluster id is used by another account")

// Host is the machine whose accounts and files are changed.
type Host interface {
	// Local returns the ids of the node's db-branch user and group.
	Local(ctx context.Context) (domain.ServiceIDs, error)
	// UserWithUID and GroupWithGID return the name of the account that has
	// the id, or "" when it is free.
	UserWithUID(uid int) string
	GroupWithGID(gid int) string
	// StopServices stops the agent and every database server it manages, so
	// nothing runs as the db-branch user while its ids change.
	StopServices(ctx context.Context) error
	// Reown gives the files db-branch owns (data directory, configuration,
	// runtime directories, ZFS dataset mounts) from the old ids to the new.
	Reown(ctx context.Context, from, to domain.ServiceIDs) error
	// SetIDs changes the db-branch user and group (groupmod and usermod).
	SetIDs(ctx context.Context, from, to domain.ServiceIDs) error
}

// Result says what Align did.
type Result struct {
	// Changed is false when the ids already matched (or the cluster has none).
	Changed  bool
	From, To domain.ServiceIDs
}

// Align moves the node's db-branch user and group to the cluster's ids. If
// an id is used by another account locally it changes nothing and returns an
// ErrIDTaken that tells the administrator what to do. The files are re-owned
// before the account changes, so a failure in between is repaired by running
// Align again.
func Align(ctx context.Context, h Host, cluster domain.ServiceIDs) (Result, error) {
	if !cluster.Valid() {
		return Result{}, nil
	}
	local, err := h.Local(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("the db-branch user and group must exist on this node (install the db-branch package first): %w", err)
	}
	res := Result{From: local, To: cluster}
	if local == cluster {
		return res, nil
	}
	if err := conflicts(h, local, cluster); err != nil {
		return res, err
	}
	if err := h.StopServices(ctx); err != nil {
		return res, fmt.Errorf("stopping db-branch services: %w", err)
	}
	if err := h.Reown(ctx, local, cluster); err != nil {
		return res, fmt.Errorf("re-owning files from %s to %s (run the command again after fixing the cause): %w", local, cluster, err)
	}
	if err := h.SetIDs(ctx, local, cluster); err != nil {
		return res, fmt.Errorf("changing the db-branch user and group to %s (run the command again after fixing the cause): %w", cluster, err)
	}
	res.Changed = true
	return res, nil
}

func conflicts(h Host, local, cluster domain.ServiceIDs) error {
	var msgs []string
	if local.UID != cluster.UID {
		if who := h.UserWithUID(cluster.UID); who != "" {
			msgs = append(msgs, fmt.Sprintf("uid %d belongs to the user %q on this node", cluster.UID, who))
		}
	}
	if local.GID != cluster.GID {
		if who := h.GroupWithGID(cluster.GID); who != "" {
			msgs = append(msgs, fmt.Sprintf("gid %d belongs to the group %q on this node", cluster.GID, who))
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s. The cluster uses %s for db-branch. Give that account another id first "+
		"(for example `usermod -u <free-id> <name>` or `groupmod -g <free-id> <name>`, then chown its files), "+
		"or re-create this node on a machine where those ids are free, and run the command again. Nothing was changed",
		ErrIDTaken, strings.Join(msgs, "; "), cluster)
}
