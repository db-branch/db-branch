package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// ForkBranchRequest describes a new branch forked from an existing branch.
type ForkBranchRequest struct {
	// Source is the parent branch to fork from.
	Source domain.Name
	// Name is the new child branch name (unique across the cluster).
	Name domain.Name
	// Checkpoint of Source to fork from ("" for current live state, or domain.ResetPoint).
	Checkpoint string
	// Clean stops a ready parent cleanly before taking the live snapshot.
	// Ignored when Checkpoint is set or the parent is already paused.
	Clean bool
	// InitSQL runs once as the child login after cloning; its result is what reset returns to.
	InitSQL string
	// TTL deletes the child branch after this long; zero keeps it until deleted (or MaxTTL).
	TTL time.Duration
	// Node, when set, must match the parent's node (forks stay on the parent's node).
	Node string
	// Owner is the principal ID who manages the child; OwnerName is their display name;
	// Access is domain.AccessPrivate ("") or domain.AccessOpen.
	Owner, OwnerName, Access string
	// Record asks for the child's changes to be recorded by the history module.
	Record bool
}

// clusterAllowsBranchFork reports whether every cluster node speaks ProtocolBranchFork.
func (s *Service) clusterAllowsBranchFork(ctx context.Context) error {
	if s.Nodes == nil {
		return nil
	}
	nodes, err := s.Nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	if domain.ClusterProtocol(nodes, domain.ProtocolBranchFork) < domain.ProtocolBranchFork {
		return fmt.Errorf("%w: a branch can only be forked once every node runs the new version", domain.ErrClusterUpgrading)
	}
	return nil
}

type forkPlan struct {
	parent          domain.Branch
	sourceDS        string
	snapName        string
	takeSnap        bool
	cleanStop       bool
	childRuntime    domain.RuntimeRef
	childLegacyAuth bool
	ttl             time.Duration
}

// planFork validates every precondition of req and resolves the source
// snapshot, runtime and limits. Caller holds s.mu.
func (s *Service) planFork(ctx context.Context, req ForkBranchRequest) (forkPlan, error) {
	if err := s.requireLineage(); err != nil {
		return forkPlan{}, err
	}
	if err := s.clusterAllowsBranchFork(ctx); err != nil {
		return forkPlan{}, err
	}
	if req.Checkpoint != "" && req.Checkpoint != domain.ResetPoint {
		if _, err := domain.ParseCheckpointName(req.Checkpoint); err != nil {
			return forkPlan{}, err
		}
	}
	parent, err := s.Store.GetBranch(ctx, req.Source)
	if err != nil {
		return forkPlan{}, err
	}
	targetNode := parent.Node
	if targetNode == "" {
		targetNode = s.Node
	}
	if req.Node != "" && req.Node != targetNode {
		return forkPlan{}, fmt.Errorf("%w: forked branch %s must be created on parent's node %s, not %s", domain.ErrConflict, req.Name, targetNode, req.Node)
	}
	if err := s.checkOwner("branch", parent.Name, parent.Node); err != nil {
		return forkPlan{}, err
	}
	if err := parent.CanFork(); err != nil {
		return forkPlan{}, err
	}
	if _, err := s.Store.GetBranch(ctx, req.Name); err == nil {
		return forkPlan{}, fmt.Errorf("branch %s: %w", req.Name, domain.ErrAlreadyExists)
	}
	lim := s.resourceLimits()
	ttl, err := lim.checkTTL(req.TTL)
	if err != nil {
		return forkPlan{}, err
	}
	if err := s.checkBranchCount(ctx, req.Owner, lim); err != nil {
		return forkPlan{}, err
	}
	if err := s.checkPoolRoom(ctx); err != nil {
		return forkPlan{}, err
	}
	nodes, err := s.nodes(ctx)
	if err != nil {
		return forkPlan{}, err
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return forkPlan{}, err
	}
	p := domain.Placement{
		Nodes: nodes, Branches: branches, Candidates: []string{targetNode}, Want: targetNode,
		BranchMB: s.memory() + ServerOverheadMB, Now: s.Clock(), DownAfter: s.DownAfter,
	}
	if _, err := p.Choose(); err != nil {
		return forkPlan{}, err
	}
	lines, err := s.family(ctx, parent.Name)
	if err != nil {
		return forkPlan{}, err
	}
	plan := forkPlan{parent: parent, ttl: ttl}
	switch req.Checkpoint {
	case "":
		liveLine, ok := liveOf(lines)
		if !ok {
			return forkPlan{}, fmt.Errorf("branch %s: %w: its dataset is missing", parent.Name, domain.ErrNotFound)
		}
		plan.sourceDS = liveLine.Dataset
		plan.snapName = domain.ForkSnapshotName(req.Name)
		plan.takeSnap = true
		plan.cleanStop = req.Clean && parent.Status == domain.BranchReady
		plan.childRuntime = parent.Runtime
		plan.childLegacyAuth = parent.LegacyAuth
	case domain.ResetPoint:
		ds, ok := domain.FindSnapshot(lines, ResetSnapshot)
		if !ok {
			return forkPlan{}, fmt.Errorf("checkpoint %s of branch %s: %w", req.Checkpoint, parent.Name, domain.ErrNotFound)
		}
		plan.sourceDS = ds
		plan.snapName = ResetSnapshot
		plan.childRuntime = s.runtimeOf(ctx, parent, snapshotProp(lines, ds, ResetSnapshot, domain.PropRuntime))
		plan.childLegacyAuth = parent.ForkLegacyAuth
	default:
		ds, info, ok := domain.FindCheckpoint(lines, req.Checkpoint)
		if !ok {
			return forkPlan{}, fmt.Errorf("checkpoint %s of branch %s: %w", req.Checkpoint, parent.Name, domain.ErrNotFound)
		}
		plan.sourceDS = ds
		plan.snapName = info.Name
		plan.childRuntime = s.runtimeOf(ctx, parent, info.Props[domain.PropRuntime])
		plan.childLegacyAuth = parent.LegacyAuth && plan.childRuntime == parent.Runtime
	}
	if err := s.requireInstalled(ctx, plan.childRuntime); err != nil {
		return forkPlan{}, err
	}
	if _, err := s.server(plan.childRuntime); err != nil {
		return forkPlan{}, err
	}
	return plan, nil
}

func snapshotProp(lines []domain.Line, dataset, snapName, key string) string {
	for _, l := range lines {
		if l.Dataset != dataset {
			continue
		}
		for _, s := range l.Snapshots {
			if s.Name == snapName {
				return s.Props[key]
			}
		}
	}
	return ""
}

// ValidateFork checks every synchronous precondition of a fork request so the
// API can report errors before accepting a background operation.
func (s *Service) ValidateFork(ctx context.Context, req ForkBranchRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.planFork(ctx, req)
	return err
}

// ForkBranch creates a new independent branch from an existing branch's live
// state or one of its checkpoints.
func (s *Service) ForkBranch(ctx context.Context, req ForkBranchRequest, report Reporter) (domain.Branch, error) {
	plan, child, err := s.beginFork(ctx, req, report)
	if err != nil {
		return domain.Branch{}, err
	}
	if plan.cleanStop {
		childKept, err := s.cloneCleanFork(ctx, plan, child, report)
		if err != nil {
			if childKept {
				return child, err
			}
			return domain.Branch{}, err
		}
	}
	if err := s.finishClonedBranch(ctx, &child, req.InitSQL, report); err != nil {
		return child, s.fail(ctx, child, err)
	}
	child.Status = domain.BranchReady
	if err := s.saveBranch(ctx, child); err != nil {
		_ = s.stopBranchImmediate(ctx, child)
		return child, err
	}
	return child, nil
}

// beginFork validates, allocates a port and inserts the child reservation.
// For non-clean forks it also snapshots (if needed) and clones atomically
// under s.mu so a concurrent parent reset/delete cannot race the clone.
func (s *Service) beginFork(ctx context.Context, req ForkBranchRequest, report Reporter) (forkPlan, domain.Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, err := s.planFork(ctx, req)
	if err != nil {
		return forkPlan{}, domain.Branch{}, err
	}
	port, err := s.allocatePort(ctx)
	if err != nil {
		return forkPlan{}, domain.Branch{}, err
	}
	child := domain.Branch{
		Name: req.Name, Base: plan.parent.Base, Snapshot: plan.parent.Snapshot,
		Runtime: plan.childRuntime, Status: domain.BranchProvisioning, Port: port,
		Node: s.Node, User: req.Name.String(), Password: s.Password(),
		CreatedAt: s.Clock(), InitSQL: req.InitSQL, Owner: req.Owner, OwnerName: req.OwnerName,
		Access: domain.AccessPrivate, Record: req.Record, LegacyAuth: plan.childLegacyAuth,
		ParentBranch: plan.parent.Name, ParentSnapshot: plan.snapName,
		ForkRuntime: plan.childRuntime, ForkLegacyAuth: plan.childLegacyAuth,
	}
	if !child.Record && s.RecordNew != nil {
		child.Record = s.RecordNew(ctx, child)
	}
	if req.Access == domain.AccessOpen {
		child.Access = domain.AccessOpen
		if s.OpenPassword != nil && s.OpenPassword() != "" {
			child.Password = s.OpenPassword()
		}
	}
	child.SetTTL(s.Clock(), plan.ttl)

	if plan.cleanStop {
		parent := plan.parent
		parent.Status = domain.BranchForking
		if err := s.Store.PutBranch(ctx, parent); err != nil {
			return forkPlan{}, domain.Branch{}, err
		}
		if err := s.Store.InsertBranch(ctx, child); err != nil {
			parent.Status = domain.BranchReady
			_ = s.Store.PutBranch(context.WithoutCancel(ctx), parent)
			return forkPlan{}, domain.Branch{}, err
		}
		plan.parent = parent
		return plan, child, nil
	}

	if err := s.Store.InsertBranch(ctx, child); err != nil {
		return forkPlan{}, domain.Branch{}, err
	}
	snap := domain.Snapshot{Dataset: plan.sourceDS, Name: plan.snapName}
	if plan.takeSnap {
		report.report("snapshotting")
		snapTaken := false
		take := func() error {
			if err := s.Storage.Snapshot(ctx, snap); err != nil {
				return err
			}
			snapTaken = true
			return nil
		}
		if plan.parent.Status == domain.BranchReady {
			err = s.withLiveFreeze(ctx, plan.parent, plan.sourceDS, take)
		} else {
			err = take()
		}
		if err != nil {
			var drop domain.Snapshot
			if snapTaken {
				drop = snap
			}
			return forkPlan{}, domain.Branch{}, s.cancelForkReservationLocked(ctx, child, drop, err)
		}
	}
	report.report("cloning")
	if err := s.Storage.Clone(ctx, snap, s.branchDataset(child.Name)); err != nil {
		var drop domain.Snapshot
		if plan.takeSnap {
			drop = snap
		}
		return forkPlan{}, domain.Branch{}, s.cancelForkReservationLocked(ctx, child, drop, err)
	}
	return plan, child, nil
}

// cloneCleanFork stops a ready parent cleanly, snapshots and clones it, and
// brings the parent back to ready before the child starts. The returned bool
// reports whether the child record remains in the store (true once cloning has
// succeeded).
func (s *Service) cloneCleanFork(ctx context.Context, plan forkPlan, child domain.Branch, report Reporter) (bool, error) {
	parent := plan.parent
	restoreParent := func(cause error) (bool, error) {
		wctx := context.WithoutCancel(ctx)
		if err := s.ensureRunning(wctx, parent); err != nil {
			return false, errors.Join(cause, err, s.markFailed(wctx, parent, err))
		}
		parent.Status, parent.Error = domain.BranchReady, ""
		if err := s.saveBranch(wctx, parent); err != nil {
			_ = s.stopBranchImmediate(wctx, parent)
			return false, errors.Join(cause, err)
		}
		s.collectIfSettled(wctx, parent.Name)
		return false, cause
	}
	cancelChild := func(drop domain.Snapshot, cause error) error {
		s.mu.Lock()
		err := s.cancelForkReservationLocked(ctx, child, drop, cause)
		s.mu.Unlock()
		return err
	}

	parentSrv, err := s.server(parent.Runtime)
	if err != nil {
		return restoreParent(cancelChild(domain.Snapshot{}, err))
	}
	parentSpec, err := s.branchSpec(ctx, parent, plan.sourceDS, false, false)
	if err != nil {
		return restoreParent(cancelChild(domain.Snapshot{}, err))
	}
	report.report("stopping")
	if err := parentSrv.Stop(ctx, parentSpec, domain.StopClean); err != nil {
		return restoreParent(cancelChild(domain.Snapshot{}, err))
	}
	snap := domain.Snapshot{Dataset: plan.sourceDS, Name: plan.snapName}
	report.report("snapshotting")
	if err := s.Storage.Snapshot(ctx, snap); err != nil {
		return restoreParent(cancelChild(domain.Snapshot{}, err))
	}
	report.report("cloning")
	if err := s.Storage.Clone(ctx, snap, s.branchDataset(child.Name)); err != nil {
		return restoreParent(cancelChild(snap, err))
	}
	wctx := context.WithoutCancel(ctx)
	if err := s.ensureRunning(wctx, parent); err != nil {
		return true, errors.Join(err, s.markFailed(wctx, parent, err), s.markFailed(wctx, child, err))
	}
	parent.Status, parent.Error = domain.BranchReady, ""
	if err := s.saveBranch(wctx, parent); err != nil {
		_ = s.stopBranchImmediate(wctx, parent)
		return true, errors.Join(err, s.markFailed(wctx, child, err))
	}
	s.collectIfSettled(wctx, parent.Name)
	return true, nil
}

// cancelForkReservationLocked rolls back a fork that failed before cloning
// finished: drops any freshly taken fork snapshot and removes the child
// reservation (or marks it failed if removing it fails, so it stays deletable).
// Caller holds s.mu.
func (s *Service) cancelForkReservationLocked(ctx context.Context, child domain.Branch, drop domain.Snapshot, cause error) error {
	wctx := context.WithoutCancel(ctx)
	if drop.Dataset != "" {
		_ = s.Storage.DestroySnapshot(wctx, drop)
	}
	if derr := s.Store.DeleteBranch(wctx, child.Name); derr != nil {
		child.Status, child.Error = domain.BranchFailed, cause.Error()
		_ = s.Store.PutBranch(wctx, child)
		return errors.Join(cause, derr)
	}
	return cause
}

// checkNoForkChildren refuses deleting a parent branch while any forked child
// still exists. Caller holds s.mu.
func (s *Service) checkNoForkChildren(ctx context.Context, br domain.Branch) error {
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	var children []string
	for _, b := range branches {
		if b.ParentBranch == br.Name {
			children = append(children, b.Name.String())
		}
	}
	if len(children) == 0 {
		return nil
	}
	sort.Strings(children)
	return fmt.Errorf("%w: branch %s has forked child branch(es): %s; delete them first", domain.ErrInUse, br.Name, strings.Join(children, ", "))
}

// dropInheritedUsers removes the parent branch's login (and any legacy shared
// "branch" login) from a freshly cloned child so the parent's credentials
// cannot bypass the child's private access or owner boundary.
func (s *Service) dropInheritedUsers(ctx context.Context, br domain.Branch, srv domain.DBServer, spec domain.Server) error {
	cleaner, ok := srv.(domain.CloneUserCleaner)
	if !ok {
		return nil
	}
	for _, u := range []string{br.ParentBranch.String(), BranchUser} {
		if u == "" || u == br.User {
			continue
		}
		if err := cleaner.DropUser(ctx, spec, u); err != nil {
			return fmt.Errorf("drop inherited user %s: %w", u, err)
		}
	}
	return nil
}

func isLineageTransition(st domain.BranchStatus) bool {
	switch st {
	case domain.BranchUpgrading, domain.BranchRestoring, domain.BranchResetting, domain.BranchForking:
		return true
	}
	return false
}

// collectIfSettled runs lineage GC for branch n once it is no longer in a
// lineage-mutating transition, reaping any orphaned fork snapshots whose
// cleanup was deferred while n was transitioning.
func (s *Service) collectIfSettled(ctx context.Context, n domain.Name) {
	if s.Lineage == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.Store.GetBranch(ctx, n)
	if err != nil || isLineageTransition(br.Status) {
		return
	}
	s.collectLogged(ctx, n)
}

// cleanupParentFork destroys a child's dedicated fork-<child> snapshot on the
// parent's lineage (wherever it lives now) and runs parent GC so gone-*
// snapshots and retired lines are reclaimed immediately. If the parent is in a
// lineage transition (upgrade/restore/reset/fork), cleanup is deferred to the
// parent's collectIfSettled when its transition completes.
func (s *Service) cleanupParentFork(ctx context.Context, child domain.Branch) {
	if child.ParentBranch == (domain.Name{}) || s.Lineage == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, err := s.Store.GetBranch(ctx, child.ParentBranch)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			s.logf("branch %s: reading parent %s: %v", child.Name, child.ParentBranch, err)
		}
		return
	}
	if isLineageTransition(parent.Status) {
		return
	}
	if domain.IsForkSnapshot(child.ParentSnapshot) {
		lines, err := s.family(ctx, child.ParentBranch)
		if err != nil {
			s.logf("branch %s: reading parent %s lineage: %v", child.Name, child.ParentBranch, err)
		} else if ds, ok := domain.FindSnapshot(lines, child.ParentSnapshot); ok {
			if err := s.Storage.DestroySnapshot(ctx, domain.Snapshot{Dataset: ds, Name: child.ParentSnapshot}); err != nil {
				s.logf("branch %s: destroying parent fork snapshot %s@%s: %v", child.Name, ds, child.ParentSnapshot, err)
			}
		}
	}
	s.collectLogged(ctx, child.ParentBranch)
}

// recoverForking finishes recovery of a parent branch interrupted during a
// --clean fork: ensures it is running, unfrozen, hardened and marked ready.
func (s *Service) recoverForking(ctx context.Context, br domain.Branch) error {
	if err := s.recoverRunning(ctx, br, false); err != nil {
		return err
	}
	br.Status, br.Error = domain.BranchReady, ""
	if err := s.saveBranch(ctx, br); err != nil {
		_ = s.stopBranchImmediate(ctx, br)
		return err
	}
	s.collectIfSettled(ctx, br.Name)
	return nil
}
