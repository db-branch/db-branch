package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidAnonymous is returned for anonymous limits that make no sense.
var ErrInvalidAnonymous = errors.New("invalid anonymous limits")

// AnonymousLimits are what a base allows callers without credentials, on
// top of the cluster-wide settings (branch.anonymous_max / _ttl). Which
// bases they can reach at all is still decided by public snapshots.
//
// The zero value inherits: only the cluster-wide settings apply. "Inherit"
// is explicit in each field: MaxBranches nil, MaxTTL 0.
type AnonymousLimits struct {
	// MaxBranches is the most anonymous branches that may exist from this
	// base. nil inherits (the cluster-wide cap alone); 0 allows none even
	// when a snapshot is public.
	MaxBranches *int `json:"max_branches,omitempty"`
	// MaxTTL caps the life of an anonymous branch of this base; the shorter
	// of it and the cluster-wide TTL applies. 0 inherits.
	MaxTTL time.Duration `json:"max_ttl,omitempty"`
}

// Inherits reports whether nothing is set.
func (l AnonymousLimits) Inherits() bool { return l.MaxBranches == nil && l.MaxTTL == 0 }

// Disabled reports whether the base allows no anonymous branches.
func (l AnonymousLimits) Disabled() bool { return l.MaxBranches != nil && *l.MaxBranches == 0 }

// Validate rejects negative values.
func (l AnonymousLimits) Validate() error {
	if l.MaxBranches != nil && *l.MaxBranches < 0 {
		return fmt.Errorf("%w: max_branches must be 0 or more (or inherit)", ErrInvalidAnonymous)
	}
	if l.MaxTTL < 0 {
		return fmt.Errorf("%w: max_ttl must be positive (or inherit)", ErrInvalidAnonymous)
	}
	return nil
}

// EffectiveTTL is the longest life of an anonymous branch of the base given
// the cluster-wide TTL (0: unlimited): the shorter of the two.
func (l AnonymousLimits) EffectiveTTL(cluster time.Duration) time.Duration {
	if l.MaxTTL > 0 && (cluster <= 0 || l.MaxTTL < cluster) {
		return l.MaxTTL
	}
	return cluster
}

// Remaining is how many more anonymous branches the base may get: the
// cluster-wide cap less all anonymous branches (clusterUsed), and the
// base's own cap less the base's (baseUsed), whichever leaves less.
func (l AnonymousLimits) Remaining(clusterMax, clusterUsed, baseUsed int) int {
	n := clusterMax - clusterUsed
	if l.MaxBranches != nil {
		n = min(n, *l.MaxBranches-baseUsed)
	}
	return max(n, 0)
}

// Clone copies the limits so the copy shares no memory.
func (l AnonymousLimits) Clone() AnonymousLimits {
	if l.MaxBranches != nil {
		n := *l.MaxBranches
		l.MaxBranches = &n
	}
	return l
}
