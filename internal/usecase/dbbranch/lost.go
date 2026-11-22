package dbbranch

import (
	"context"
	"errors"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

func (s *Service) branchesRoot() string { return s.Pool + "/db-branch/branches" }

// MarkLost marks the branches of nodes that stopped answering as lost, so
// clients and the console see it and they can be recreated elsewhere.
// Branches being deleted stay so: their node finishes when it returns.
func (s *Service) MarkLost(ctx context.Context) ([]domain.Name, error) {
	if s.Nodes == nil {
		return nil, nil
	}
	nodes, err := s.Nodes.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	now := s.Clock()
	down := map[string]domain.Node{}
	for _, n := range nodes {
		if n.Down(now, s.DownAfter) {
			down[n.Name] = n
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return nil, err
	}
	var lost []domain.Name
	var errs []error
	for _, b := range branches {
		n, ok := down[b.Node]
		if !ok || b.Status == domain.BranchLost || b.Status == domain.BranchDeleting {
			continue
		}
		b.Status = domain.BranchLost
		b.Error = fmt.Sprintf("node %s stopped answering at %s", n.Name, n.LastSeen.UTC().Format("2006-01-02T15:04:05Z"))
		if err := s.Store.PutBranch(ctx, b); err != nil {
			errs = append(errs, err)
			continue
		}
		lost = append(lost, b.Name)
	}
	return lost, errors.Join(errs...)
}

// PlaceRecreate chooses a node, other than the lost one, that holds the
// branch's snapshot and can take it.
func (s *Service) PlaceRecreate(ctx context.Context, name domain.Name) (string, error) {
	return s.placeRecreate(ctx, name, "")
}

func (s *Service) placeRecreate(ctx context.Context, name domain.Name, want string) (string, error) {
	br, err := s.Store.GetBranch(ctx, name)
	if err != nil {
		return "", err
	}
	base, err := s.Store.GetBase(ctx, br.Base)
	if err != nil {
		return "", err
	}
	p := domain.Placement{Want: want, BranchMB: s.memory() + ServerOverheadMB, Now: s.Clock(), DownAfter: s.DownAfter}
	if p.Nodes, err = s.nodes(ctx); err != nil {
		return "", err
	}
	snap, _ := base.Snapshot(br.Snapshot)
	p.Distro = s.originDistro(base, snap, p.Nodes)
	for _, n := range s.holders(base, br.Snapshot, p.Nodes) {
		if n != br.Node {
			p.Candidates = append(p.Candidates, n)
		}
	}
	if p.Branches, err = s.Store.ListBranches(ctx); err != nil {
		return "", err
	}
	return p.Choose()
}

// RecreateBranch builds a lost branch again on this node from the same base
// snapshot, with the same name, credentials, expiry and init SQL. Changes
// made on the lost node since the last reset are gone with it.
func (s *Service) RecreateBranch(ctx context.Context, name domain.Name, report Reporter) (domain.Branch, error) {
	br, err := s.claimLost(ctx, name)
	if err != nil {
		return br, err
	}
	if err := s.dropSharedCopy(ctx, br); err != nil {
		return br, s.fail(ctx, br, err)
	}
	if err := s.buildBranch(ctx, &br, br.InitSQL, report); err != nil {
		return br, s.fail(ctx, br, err)
	}
	br.Status = domain.BranchReady
	if err := s.Store.PutBranch(ctx, br); err != nil {
		_ = s.stopBranchImmediate(ctx, br)
		return br, err
	}
	return br, nil
}

// dropSharedCopy destroys the lost branch's volume on shared storage before
// it is cloned again: its server may still be running on the lost node, and
// two servers must never share one data directory.
func (s *Service) dropSharedCopy(ctx context.Context, br domain.Branch) error {
	if s.Shared == nil {
		return nil
	}
	ds := s.branchDataset(br.Name)
	ok, err := s.Storage.Exists(ctx, ds)
	if err != nil || !ok {
		return err
	}
	return s.Storage.Destroy(ctx, ds)
}

// claimLost moves a lost branch's record to this node.
func (s *Service) claimLost(ctx context.Context, name domain.Name) (domain.Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.Store.GetBranch(ctx, name)
	if err != nil {
		return br, err
	}
	if err := br.CanRecreate(); err != nil {
		return br, err
	}
	if _, err := s.placeRecreate(ctx, name, s.Node); err != nil {
		return br, err
	}
	port, err := s.allocatePort(ctx)
	if err != nil {
		return br, err
	}
	// The branch is built again from its base snapshot, so it runs the base's runtime.
	br.Runtime, br.LegacyAuth = s.runtimeOf(ctx, br, ""), false
	br.Node, br.Port, br.Status, br.Error, br.ResetAt = s.Node, port, domain.BranchProvisioning, "", nil
	return br, s.Store.PutBranch(ctx, br)
}

// Reconcile runs on every node after it was away: its own lost branches are
// started and marked ready again, and local copies of branches recreated on
// another node meanwhile are removed. Datasets without any record are left
// alone; a missing record is not proof that the data is unwanted.
func (s *Service) Reconcile(ctx context.Context) error {
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	var errs []error
	byName := map[string]domain.Branch{}
	for _, b := range branches {
		byName[b.Name.String()] = b
		if !s.owns(b.Node) {
			continue
		}
		if b.Status == domain.BranchDeleting && b.Error == abandoned {
			errs = append(errs, s.doDelete(ctx, b, nil))
			continue
		}
		if b.Status != domain.BranchLost {
			continue
		}
		if err := s.reconcileLost(ctx, b); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, s.removeMoved(ctx, byName))
	return errors.Join(errs...)
}

func (s *Service) reconcileLost(ctx context.Context, b domain.Branch) error {
	srv, spec, started, err := s.startServerIfDown(ctx, b)
	if err != nil {
		return err
	}
	stopIfStarted := func() {
		if started {
			_ = srv.Stop(context.WithoutCancel(ctx), spec, domain.StopImmediate)
		}
	}
	if err := s.releaseFreeze(ctx, b); err != nil {
		stopIfStarted()
		return err
	}
	if err := s.putLoginRight(ctx, b, srv, spec); err != nil {
		stopIfStarted()
		return err
	}
	if err := s.resume(ctx, b.Name); err != nil {
		stopIfStarted()
		return err
	}
	return nil
}

// resume marks a lost branch of this node ready again, unless it changed meanwhile.
func (s *Service) resume(ctx context.Context, name domain.Name) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.Store.GetBranch(ctx, name)
	if err != nil || b.Status != domain.BranchLost || !s.owns(b.Node) {
		return err
	}
	b.Status, b.Error = domain.BranchReady, ""
	return s.Store.PutBranch(ctx, b)
}

func (s *Service) removeMoved(ctx context.Context, byName map[string]domain.Branch) error {
	if s.Shared != nil {
		return s.releaseMoved(ctx, byName)
	}
	if s.Streams == nil {
		return nil
	}
	ok, err := s.Storage.Exists(ctx, s.branchesRoot())
	if err != nil || !ok {
		return err
	}
	names, err := s.Streams.Children(ctx, s.branchesRoot())
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		rec, ok := byName[n]
		if !ok || s.owns(rec.Node) {
			continue
		}
		if srv, err := s.server(rec.Runtime); err == nil {
			errs = append(errs, srv.Remove(ctx, domain.Server{ID: branchServerID(rec.Name), Runtime: rec.Runtime}))
		}
		errs = append(errs, s.destroyBranchData(ctx, rec.Name))
	}
	return errors.Join(errs...)
}

// releaseMoved stops this node's servers of branches that now run on another
// node and unmounts their volumes, which stay: on shared storage they are
// the moved branches' data or were already replaced.
func (s *Service) releaseMoved(ctx context.Context, byName map[string]domain.Branch) error {
	names, err := s.Shared.Mounted(ctx, s.branchesRoot())
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		rec, ok := byName[n]
		if !ok || s.owns(rec.Node) {
			continue
		}
		if srv, err := s.server(rec.Runtime); err == nil {
			errs = append(errs, srv.Remove(ctx, domain.Server{ID: branchServerID(rec.Name), Runtime: rec.Runtime}))
		}
		errs = append(errs, s.Shared.Release(ctx, s.branchesRoot()+"/"+n))
	}
	return errors.Join(errs...)
}
