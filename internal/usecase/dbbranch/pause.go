package dbbranch

import (
	"context"
	"errors"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

// clusterAllowsBranchPause says whether every node knows the paused status.
func (s *Service) clusterAllowsBranchPause(ctx context.Context) error {
	if s.Nodes == nil {
		return nil
	}
	nodes, err := s.Nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	if domain.ClusterProtocol(nodes, domain.ProtocolBranchPause) < domain.ProtocolBranchPause {
		return fmt.Errorf("%w: a branch can only be paused once every node runs the new version", domain.ErrClusterUpgrading)
	}
	return nil
}

// PauseBranch stops the branch's database server cleanly and keeps its data:
// the dataset, the port and the credentials stay, nothing runs and no memory is
// held. ResumeBranch starts it again. Connections are refused meanwhile.
func (s *Service) PauseBranch(ctx context.Context, name domain.Name, report Reporter) (domain.Branch, error) {
	s.cancelStats(name)
	if err := s.clusterAllowsBranchPause(ctx); err != nil {
		return domain.Branch{}, err
	}
	br, err := s.transitionBranch(ctx, name, domain.Branch.CanPause, domain.BranchPausing)
	if err != nil {
		return br, err
	}
	return s.doPause(ctx, br, report)
}

func (s *Service) doPause(ctx context.Context, br domain.Branch, report Reporter) (domain.Branch, error) {
	srv, err := s.server(br.Runtime)
	if err != nil {
		return s.keepRunning(ctx, br, err)
	}
	spec, err := s.branchSpec(ctx, br, s.branchDataset(br.Name), false, false)
	if err != nil {
		return s.keepRunning(ctx, br, err)
	}
	report.report("stopping")
	if err := srv.Stop(ctx, spec, domain.StopClean); err != nil {
		return s.keepRunning(ctx, br, err)
	}
	br.Status, br.Error = domain.BranchPaused, ""
	br.StatsUpdating = false
	return br, s.saveBranch(ctx, br)
}

// keepRunning puts a branch whose pause failed back to ready (and running):
// the data is untouched, so there is nothing to mark failed.
func (s *Service) keepRunning(ctx context.Context, br domain.Branch, cause error) (domain.Branch, error) {
	wctx := context.WithoutCancel(ctx)
	err := s.ensureRunning(wctx, br)
	br.Status = domain.BranchReady
	return br, errors.Join(cause, err, s.saveBranch(wctx, br))
}

// ResumeBranch starts a paused branch's server again. When it cannot start, the
// branch stays paused with the reason, so it can be tried again.
func (s *Service) ResumeBranch(ctx context.Context, name domain.Name, report Reporter) (domain.Branch, error) {
	br, err := s.transitionBranch(ctx, name, domain.Branch.CanResume, domain.BranchResuming, s.checkResumeRoom)
	if err != nil {
		return br, err
	}
	return s.doResume(ctx, br, report)
}

// checkResumeRoom refuses a resume the node's memory budget cannot hold: a
// paused branch takes no budget, so the others may have used it. The branch
// stays paused. Caller holds s.mu.
func (s *Service) checkResumeRoom(ctx context.Context, br domain.Branch) error {
	nodes, err := s.nodes(ctx)
	if err != nil {
		return err
	}
	all, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	var others []domain.Branch
	for _, b := range all {
		if b.Name != br.Name {
			others = append(others, b)
		}
	}
	p := domain.Placement{Nodes: nodes, Branches: others, BranchMB: s.memory() + ServerOverheadMB, Now: s.Clock(), DownAfter: s.DownAfter}
	if !p.NodeFits(br.Node) {
		return fmt.Errorf("%w: node %s has no memory left to resume branch %s; pause or delete another branch first", domain.ErrNoCapacity, br.Node, br.Name)
	}
	return nil
}

func (s *Service) doResume(ctx context.Context, br domain.Branch, report Reporter) (domain.Branch, error) {
	report.report("starting")
	if err := s.ensureRunning(ctx, br); err != nil {
		br.Status, br.Error = domain.BranchPaused, err.Error()
		return br, errors.Join(err, s.saveBranch(context.WithoutCancel(ctx), br))
	}
	br.Status, br.Error = domain.BranchReady, ""
	if err := s.saveBranch(ctx, br); err != nil {
		_ = s.stopBranchImmediate(ctx, br)
		return br, err
	}
	return br, nil
}
