package dbbranch

import (
	"context"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

// RefreshUser is who a mask run started by a refresh is recorded as.
const RefreshUser = "refresh"

// MaskLauncher starts mask runs in the background (as operations).
type MaskLauncher interface {
	// Busy: something is already running on the target base (anywhere in
	// the cluster).
	Busy(target domain.Name) bool
	// Start begins the run on node, the one holding the target.
	Start(node string, req MaskRequest) error
}

// followRefresh asks the refresh follower which mask runs go with the
// snapshot a refresh just took, and starts each as its own run. A target
// that is busy or unknown is skipped (and logged): a follow-up
// never fails the refresh and never runs twice into one target.
func (s *Service) followRefresh(ctx context.Context, base domain.Name, snapshot string) {
	if s.Follower == nil || s.Launcher == nil {
		return
	}
	runs, err := s.Follower.AfterRefresh(ctx, base, snapshot)
	if err != nil {
		s.logf("asking what follows the refresh of %s: %v", base, err)
		return
	}
	for _, r := range runs {
		if err := s.followRun(ctx, base, snapshot, r); err != nil {
			s.logf("skipping the mask of %q after the refresh of %s: %v", r.Target, base, err)
		}
	}
}

func (s *Service) followRun(ctx context.Context, base domain.Name, snapshot string, r domain.MaskRun) error {
	target, err := domain.ParseName(r.Target)
	if err != nil {
		return err
	}
	if target == base {
		return fmt.Errorf("a base cannot be masked into itself")
	}
	b, err := s.Store.GetBase(ctx, target)
	if err != nil {
		return err
	}
	if s.Launcher.Busy(target) {
		return fmt.Errorf("%w: base %s is busy", domain.ErrConflict, target)
	}
	return s.Launcher.Start(b.Node, MaskRequest{Target: target, Source: base, Snapshot: snapshot, Masker: r.Masker, By: RefreshUser})
}

func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
