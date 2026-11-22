package dbbranch

import (
	"context"
	"fmt"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Limits are the resource limits an operator sets (settings). The zero value
// limits nothing.
type Limits struct {
	// RefQuotaBytes caps what one branch's dataset may reference (ZFS
	// refquota), so a runaway branch cannot fill the pool. 0: no cap.
	RefQuotaBytes uint64
	// MaxBranchesPerUser is the most branches one owner may have. 0: no limit.
	MaxBranchesPerUser int
	// MaxTTL is the longest life a branch may be given; a branch created
	// without a TTL gets it. 0: no limit.
	MaxTTL time.Duration
}

// Pool space kept free: new data is refused below the larger of MinFreeBytes
// and MinFreePercent of the pool, because a full ZFS pool fails every branch.
const (
	MinFreeBytes   = 1 << 30
	MinFreePercent = 10
)

func (s *Service) resourceLimits() Limits {
	if s.ResourceLimits == nil {
		return Limits{}
	}
	return s.ResourceLimits()
}

// checkPoolRoom refuses work that adds data while the pool is nearly full. A
// pool whose size is not known (PoolSpace nil or reporting 0) is not guarded.
func (s *Service) checkPoolRoom(ctx context.Context) error {
	if s.PoolSpace == nil {
		return nil
	}
	size, free, err := s.PoolSpace(ctx)
	if err != nil {
		return err
	}
	if size == 0 {
		return nil
	}
	if min := max(uint64(MinFreeBytes), size/100*MinFreePercent); free < min {
		return fmt.Errorf("%w: pool %s has %d MiB free of %d MiB; at least %d MiB (%d%%, and 1 GiB) must stay free, delete branches or add disk space",
			domain.ErrNoSpace, s.Pool, free>>20, size>>20, min>>20, MinFreePercent)
	}
	return nil
}

// checkTTL applies MaxTTL to a requested TTL and returns the TTL to use.
func (l Limits) checkTTL(ttl time.Duration) (time.Duration, error) {
	if l.MaxTTL <= 0 {
		return ttl, nil
	}
	if ttl == 0 {
		return l.MaxTTL, nil
	}
	if ttl > l.MaxTTL {
		return 0, fmt.Errorf("%w: %s is longer than the maximum %s (setting branch.max_ttl)", domain.ErrInvalidTTL, ttl, l.MaxTTL)
	}
	return ttl, nil
}

// checkBranchCount refuses another branch for an owner at the limit. Caller
// holds s.mu.
func (s *Service) checkBranchCount(ctx context.Context, owner string, l Limits) error {
	if owner == "" || l.MaxBranchesPerUser <= 0 {
		return nil
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, b := range branches {
		if b.Owner == owner {
			n++
		}
	}
	if n >= l.MaxBranchesPerUser {
		return fmt.Errorf("%w: %s already has %d branches (limit %d, setting branch.max_per_user)", domain.ErrQuotaExceeded, owner, n, l.MaxBranchesPerUser)
	}
	return nil
}

// applyQuota sets the refquota on a branch's dataset (a clone does not
// inherit it). Without a limit or without ZFS properties it does nothing.
func (s *Service) applyQuota(ctx context.Context, dataset string) error {
	q := s.resourceLimits().RefQuotaBytes
	if q == 0 || s.Lineage == nil {
		return nil
	}
	return s.Lineage.SetProps(ctx, dataset, map[string]string{"refquota": fmt.Sprint(q)})
}
