package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ServiceIDs are the numeric ids of the db-branch system user and group. The
// package creates them with whatever ids are free on the machine, but bases
// travel between nodes as ZFS streams that keep numeric owners, so every node
// of a cluster must use the same ones (see docs/design.md, "Service user ids").
type ServiceIDs struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// Valid reports whether both ids are set (the zero value means "not known").
func (s ServiceIDs) Valid() bool { return s.UID > 0 && s.GID > 0 }

func (s ServiceIDs) String() string { return fmt.Sprintf("uid %d / gid %d", s.UID, s.GID) }

// IDProblem explains how a node's db-branch ids differ from the cluster's;
// it is empty when they match or the cluster has not recorded any (nothing to
// compare with). local is invalid when the node has no db-branch user or group.
func IDProblem(local, cluster ServiceIDs) string {
	if !cluster.Valid() || local == cluster {
		return ""
	}
	if !local.Valid() {
		return fmt.Sprintf("the db-branch user or group is missing on this node; the cluster uses %s", cluster)
	}
	return fmt.Sprintf("the db-branch user and group are %s here but %s in the cluster; "+
		"data received from other nodes would not be owned by the user that runs the servers "+
		"(fix: run `db-branch cluster align-ids` as root on this node)", local, cluster)
}

// ErrNodeIDMismatch is returned when a node whose db-branch ids differ from
// the cluster's was asked to hold a branch or replica. It is a conflict (409).
func ErrNodeIDMismatch(node, problem string) error {
	return fmt.Errorf("%w: node %s cannot take branches or replicas: %s", ErrConflict, node, problem)
}

// ErrOwnership is wrapped by the error of a database server whose data
// directory belongs to another user than the one the server runs as.
var ErrOwnership = errors.New("data directory ownership mismatch")

// OwnershipHint is appended to errors about a data directory the server's
// user cannot use.
const OwnershipHint = "the data directory belongs to another user than the one the database server runs as; " +
	"the node's db-branch uid/gid probably differ from the cluster's (see `db-branch cluster status`)"

// LooksLikeOwnershipError reports whether a server's log says it could not
// access its files for lack of permission (EACCES).
func LooksLikeOwnershipError(log string) bool {
	return strings.Contains(log, "Permission denied") || strings.Contains(log, "errno 13")
}

// StartFailure is the error of a database server that stopped while starting:
// an ownership problem is marked as such so the branch fails for good with a
// clear reason instead of waiting for a server that cannot come up.
func StartFailure(id, log string) error {
	if LooksLikeOwnershipError(log) {
		return fmt.Errorf("%w: server %s stopped while starting: %s: %s", ErrOwnership, id, OwnershipHint, log)
	}
	return fmt.Errorf("server %s stopped while starting: %s", id, log)
}
