package membership

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Roster is the Raft configuration as the remover edits it.
type Roster interface {
	IsLeader() bool
	Servers(ctx context.Context) ([]Server, error)
	// RemoveServer takes a member out of the configuration (leader only).
	RemoveServer(ctx context.Context, id string) error
}

// Catalog is the cluster state a removal reads and cleans up.
type Catalog interface {
	ListBases(ctx context.Context) ([]domain.Base, error)
	ListBranches(ctx context.Context) ([]domain.Branch, error)
	ListNodes(ctx context.Context) ([]domain.Node, error)
	DeleteBranch(ctx context.Context, name domain.Name) error
	DeleteBase(ctx context.Context, name domain.Name) error
	RemoveReplica(ctx context.Context, base domain.Name, node string) error
	// SetReplicaNodes sets the nodes a base keeps copies on (the list excludes
	// the base's own node); nil would mean every node, so callers pass a list.
	SetReplicaNodes(ctx context.Context, base domain.Name, nodes []string) error
	DeleteNode(ctx context.Context, name string) error
}

// Remover takes a node out of the cluster for good (a failed or retired node).
type Remover struct {
	Roster  Roster
	Catalog Catalog
	// Self is this node's name: the leader cannot remove itself.
	Self      string
	Clock     func() time.Time
	DownAfter time.Duration
}

// RemoveReport says what a forced removal cleaned up.
type RemoveReport struct {
	// Branches are the branches that lived on the node (their records go).
	Branches []string `json:"branches"`
	// Bases are the bases whose origin was on the node (their records go).
	Bases []string `json:"bases"`
	// Replicas are the bases that kept a copy on the node (the reference goes).
	Replicas []string `json:"replicas"`
	// Orphaned are the forgotten bases that still have copies on other nodes;
	// those datasets stay on disk there, no longer managed.
	Orphaned []string `json:"orphaned"`
}

// Remove takes node out of the Raft configuration and forgets it. It refuses
// when that would leave the voters without a healthy majority, and, unless
// force is set, while the node still holds bases, copies or branches. Force
// forgets those records (the data on the node is not touched) but keeps a base
// whose origin was on the node while branches on other nodes still derive from
// it. Every step is idempotent, so a failed removal can be run again.
func (r *Remover) Remove(ctx context.Context, node string, force bool) (RemoveReport, error) {
	rep := RemoveReport{Branches: []string{}, Bases: []string{}, Replicas: []string{}, Orphaned: []string{}}
	if !r.Roster.IsLeader() {
		return rep, domain.ErrNoLeader
	}
	if node == r.Self {
		return rep, fmt.Errorf("%w: %s leads the cluster; run this on another node (or after another node took over)", domain.ErrConflict, node)
	}
	servers, err := r.Roster.Servers(ctx)
	if err != nil {
		return rep, err
	}
	nodes, err := r.Catalog.ListNodes(ctx)
	if err != nil {
		return rep, err
	}
	seen := map[string]time.Time{}
	for _, n := range nodes {
		seen[n.Name] = n.LastSeen
	}
	inRaft := slices.ContainsFunc(servers, func(s Server) bool { return s.ID == node })
	if _, known := seen[node]; !known && !inRaft {
		return rep, fmt.Errorf("node %s: %w", node, domain.ErrNotFound)
	}
	if err := r.checkQuorum(servers, seen, node); err != nil {
		return rep, err
	}
	bases, err := r.Catalog.ListBases(ctx)
	if err != nil {
		return rep, err
	}
	branches, err := r.Catalog.ListBranches(ctx)
	if err != nil {
		return rep, err
	}
	var gone []domain.Branch
	for _, b := range branches {
		if b.Node == node {
			gone = append(gone, b)
			rep.Branches = append(rep.Branches, b.Name.String())
		}
	}
	var origins, copies, listed []domain.Base
	for _, b := range bases {
		switch {
		case b.Node == node:
			origins = append(origins, b)
			rep.Bases = append(rep.Bases, b.Name.String())
			if hasOtherCopies(b, node) {
				rep.Orphaned = append(rep.Orphaned, b.Name.String())
			}
		case b.HoldsAny(node):
			copies = append(copies, b)
			rep.Replicas = append(rep.Replicas, b.Name.String())
		}
		// Naming the node in the list of copy nodes is a setting, not data:
		// when nothing else holds the removal back, it is dropped without force.
		if b.Node != node && b.ReplicaNodes != nil && slices.Contains(*b.ReplicaNodes, node) {
			listed = append(listed, b)
		}
	}
	if len(gone)+len(origins)+len(copies) > 0 && !force {
		return RemoveReport{}, fmt.Errorf("%w: node %s still holds %s; move or delete them, or remove the node with force to forget them", domain.ErrInUse, node, rep.describe())
	}
	// A base that others derive from stays: its branches' clones need it.
	for _, b := range origins {
		var dependents []string
		for _, br := range branches {
			if br.Base == b.Name && br.Node != node {
				dependents = append(dependents, br.Name.String())
			}
		}
		if len(dependents) > 0 {
			return RemoveReport{}, fmt.Errorf("%w: base %s of node %s is the base of branches on other nodes (%s); delete them first", domain.ErrInUse, b.Name, node, strings.Join(dependents, ", "))
		}
	}
	// Copies first: forgetting them needs the newest protocol of the three, so
	// a cluster that is still upgrading refuses before anything else is gone.
	for _, b := range copies {
		if err := r.Catalog.RemoveReplica(ctx, b.Name, node); err != nil {
			return RemoveReport{}, err
		}
	}
	for _, b := range listed {
		rest := slices.DeleteFunc(slices.Clone(*b.ReplicaNodes), func(n string) bool { return n == node })
		if err := r.Catalog.SetReplicaNodes(ctx, b.Name, rest); err != nil {
			return RemoveReport{}, err
		}
	}
	for _, b := range gone {
		if err := r.Catalog.DeleteBranch(ctx, b.Name); err != nil {
			return RemoveReport{}, err
		}
	}
	for _, b := range origins {
		if err := r.Catalog.DeleteBase(ctx, b.Name); err != nil {
			return RemoveReport{}, err
		}
	}
	if inRaft {
		if err := r.Roster.RemoveServer(ctx, node); err != nil {
			return RemoveReport{}, err
		}
	}
	if err := r.Catalog.DeleteNode(ctx, node); err != nil {
		return RemoveReport{}, err
	}
	return rep, nil
}

// checkQuorum refuses to remove a voter when the voters left would not have a
// healthy majority.
func (r *Remover) checkQuorum(servers []Server, seen map[string]time.Time, node string) error {
	now := r.Clock()
	voters, healthy := 0, 0
	for _, s := range servers {
		if !s.Voter || s.ID == node {
			continue
		}
		voters++
		if at, ok := seen[s.ID]; ok && now.Sub(at) <= r.DownAfter {
			healthy++
		}
	}
	isVoter := slices.ContainsFunc(servers, func(s Server) bool { return s.ID == node && s.Voter })
	if isVoter && healthy < majority(voters) {
		return fmt.Errorf("%w: removing voter %s would leave %d healthy of %d voters, short of a majority", domain.ErrConflict, node, healthy, voters)
	}
	return nil
}

// hasOtherCopies reports whether a node other than the origin and node holds a
// copy of b.
func hasOtherCopies(b domain.Base, node string) bool {
	for _, nodes := range b.Replicas {
		if slices.ContainsFunc(nodes, func(n string) bool { return n != node && n != b.Node }) {
			return true
		}
	}
	return false
}

func (r RemoveReport) describe() string {
	var parts []string
	if len(r.Bases) > 0 {
		parts = append(parts, "the origin of bases "+strings.Join(r.Bases, ", "))
	}
	if len(r.Replicas) > 0 {
		parts = append(parts, "copies of bases "+strings.Join(r.Replicas, ", "))
	}
	if len(r.Branches) > 0 {
		parts = append(parts, "branches "+strings.Join(r.Branches, ", "))
	}
	return strings.Join(parts, "; ")
}
