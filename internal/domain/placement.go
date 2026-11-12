package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrNoCapacity is returned when no node can take a new branch.
var ErrNoCapacity = errors.New("no node can take the branch")

// Placement chooses the node for a new branch: a node that is up, holds the
// base snapshot (Candidates) and has memory left, preferring the one with the
// fewest branches.
type Placement struct {
	Nodes      []Node
	Branches   []Branch
	Candidates []string
	// BranchMB is what one branch server is expected to use.
	BranchMB  int
	Now       time.Time
	DownAfter time.Duration
	// Want is a node the caller asked for; empty lets Choose decide.
	Want string
	// Distro is the distribution the base snapshot was made on, set for
	// engines whose data depends on the OS (see CollationBound): only nodes
	// of the same distribution qualify. Empty: any node.
	Distro string
}

// CollationBound reports whether an engine's data files depend on the
// operating system's collation libraries (glibc locale data, ICU). PostgreSQL
// uses them for text ordering, so indexes built on one distribution release
// can be wrong on another. MySQL ships its own collations in the runtime.
func CollationBound(e Engine) bool { return e == Postgres }

// SameDistro reports whether a node on have may run data made on want. A side
// that is not known (nodes of older versions do not report it) does not
// block: it cannot be shown to differ.
func SameDistro(want, have string) bool { return want == "" || have == "" || want == have }

// ErrDistroMismatch explains why a PostgreSQL base cannot be used on a node
// of another distribution. It is a conflict (HTTP 409).
func ErrDistroMismatch(node, have, want string) error {
	return fmt.Errorf("%w: node %s runs %s but the PostgreSQL base was made on %s; "+
		"its data depends on the operating system's collation libraries (glibc locale data, ICU), "+
		"and a different release can leave text indexes corrupt or give wrong query results; "+
		"use a node running %s, or dump and reload the data into a base made on %s",
		ErrConflict, node, have, want, want, have)
}

// MemoryShare is the part of a node's memory branches may use; the rest is
// left to the OS and the ZFS ARC.
const MemoryShare = 0.75

func (p Placement) fits(n Node, count int) bool {
	if n.Down(p.Now, p.DownAfter) {
		return false
	}
	if n.MemoryMB == 0 || p.BranchMB <= 0 {
		return true
	}
	return float64((count+1)*p.BranchMB) <= n.BranchBudgetMB()
}

// BranchBudgetMB is the memory branch servers may use on the node: its
// memory minus the ZFS ARC cap, but never more than MemoryShare of it (so a
// smaller cap does not squeeze the OS). Nodes that do not report the cap get
// MemoryShare alone.
func (n Node) BranchBudgetMB() float64 {
	budget := float64(n.MemoryMB) * MemoryShare
	if n.ArcMaxMB > 0 {
		budget = min(budget, float64(n.MemoryMB)-float64(n.ArcMaxMB))
	}
	return max(budget, 0)
}

// Choose returns the chosen node's name.
func (p Placement) Choose() (string, error) {
	count := map[string]int{}
	for _, b := range p.Branches {
		count[b.Node]++
	}
	nodes := map[string]Node{}
	for _, n := range p.Nodes {
		nodes[n.Name] = n
	}
	var ok []string
	for _, c := range p.Candidates {
		if n, found := nodes[c]; found && p.fits(n, count[c]) && (p.Want == "" || p.Want == c) {
			ok = append(ok, c)
		}
	}
	if p.Distro != "" {
		var same []string
		for _, c := range ok {
			if SameDistro(p.Distro, nodes[c].System.Distro) {
				same = append(same, c)
			}
		}
		if len(same) == 0 && len(ok) > 0 {
			return "", ErrDistroMismatch(ok[0], nodes[ok[0]].System.Distro, p.Distro)
		}
		ok = same
	}
	if len(ok) == 0 {
		if p.Want != "" {
			return "", fmt.Errorf("%w: node %s is down, full or lacks the snapshot (holders: %s)", ErrNoCapacity, p.Want, strings.Join(p.Candidates, ", "))
		}
		return "", fmt.Errorf("%w: every node holding the snapshot (%s) is down or full", ErrNoCapacity, strings.Join(p.Candidates, ", "))
	}
	sort.Slice(ok, func(i, j int) bool {
		if count[ok[i]] != count[ok[j]] {
			return count[ok[i]] < count[ok[j]]
		}
		return ok[i] < ok[j]
	})
	return ok[0], nil
}

// ErrInvalidReplicas is returned for a replica node list naming an unknown node.
var ErrInvalidReplicas = errors.New("invalid replica nodes")
