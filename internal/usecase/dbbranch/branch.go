package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// CreateBranchRequest describes a new branch.
type CreateBranchRequest struct {
	Name domain.Name
	Base domain.Name
	// Snapshot of the base to clone; empty means the latest.
	Snapshot string
	// InitSQL runs once after cloning (e.g. masking); its result is what reset returns to.
	InitSQL string
	// TTL deletes the branch after this long; zero keeps it until deleted.
	TTL time.Duration
	// Node to create the branch on; empty lets PlaceBranch choose.
	Node string
	// Owner manages the branch; Access is domain.AccessPrivate ("") or AccessOpen.
	Owner, Access string
	// Record asks for the branch's changes to be recorded by the history module.
	Record bool
	// Runtime, when set, upgrades the new branch to it right after creation
	// (CreateBranchUpgraded); Force and PoolFree are as in UpgradeRequest.
	Runtime  domain.RuntimeRef
	Force    bool
	PoolFree uint64
}

// PlaceBranch checks the request against the base and chooses the node that
// should create the branch: one holding the snapshot, up, with memory left.
func (s *Service) PlaceBranch(ctx context.Context, req CreateBranchRequest) (string, error) {
	base, snap, err := s.branchSource(ctx, req)
	if err != nil {
		return "", err
	}
	p := domain.Placement{Want: req.Node, BranchMB: s.memory() + ServerOverheadMB, Now: s.Clock(), DownAfter: s.DownAfter}
	if p.Nodes, err = s.nodes(ctx); err != nil {
		return "", err
	}
	p.Candidates = s.holders(base, snap.Name, p.Nodes)
	p.Distro = s.originDistro(base, snap, p.Nodes)
	if p.Branches, err = s.Store.ListBranches(ctx); err != nil {
		return "", err
	}
	node, err := p.Choose()
	if err != nil && base.ReplicaNodes != nil {
		err = fmt.Errorf("%w; base %s is kept only on its node %s and %s (see 'base replicas')", err, base.Name, base.Node, replicaList(*base.ReplicaNodes))
	}
	return node, err
}

func replicaList(nodes []string) string {
	if len(nodes) == 0 {
		return "no other node"
	}
	return strings.Join(nodes, ", ")
}

// nodes lists the cluster's nodes (a single node is always up and has no
// budget on record).
func (s *Service) nodes(ctx context.Context) ([]domain.Node, error) {
	if s.Nodes == nil {
		return []domain.Node{{Name: s.Node, LastSeen: s.Clock(), System: domain.NodeSystem{Distro: s.Distro}}}, nil
	}
	return s.Nodes.ListNodes(ctx)
}

// holders are the nodes that can clone the base's snapshot: every node on
// shared storage, else the base's node and those with a copy that still want
// it (a node draining its copy is not one).
func (s *Service) holders(base domain.Base, snapshot string, nodes []domain.Node) []string {
	if s.Shared != nil {
		out := make([]string, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, n.Name)
		}
		return out
	}
	h := base.Placeable(snapshot)
	if h[0] == "" {
		h[0] = s.Node
	}
	return h
}

// branchSource returns the base and snapshot a new branch would be cloned from.
func (s *Service) branchSource(ctx context.Context, req CreateBranchRequest) (domain.Base, domain.BaseSnapshot, error) {
	base, err := s.Store.GetBase(ctx, req.Base)
	if err != nil {
		return base, domain.BaseSnapshot{}, err
	}
	snap, ok := base.LatestSnapshot()
	if req.Snapshot != "" {
		if snap, ok = base.Snapshot(req.Snapshot); !ok {
			return base, snap, fmt.Errorf("snapshot %s of base %s: %w", req.Snapshot, base.Name, domain.ErrNotFound)
		}
	}
	if !ok {
		return base, snap, fmt.Errorf("%w: base %s has no snapshot yet", domain.ErrConflict, base.Name)
	}
	return base, snap, nil
}

// CreateBranch clones a base snapshot, prepares the branch and takes its reset point.
func (s *Service) CreateBranch(ctx context.Context, req CreateBranchRequest, report Reporter) (domain.Branch, error) {
	br, err := s.reserveBranch(ctx, req)
	if err != nil {
		return domain.Branch{}, err
	}
	if err := s.buildBranch(ctx, &br, req.InitSQL, report); err != nil {
		return br, s.fail(ctx, br, err)
	}
	br.Status = domain.BranchReady
	return br, s.Store.PutBranch(ctx, br)
}

func (s *Service) reserveBranch(ctx context.Context, req CreateBranchRequest) (domain.Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Store.GetBranch(ctx, req.Name); err == nil {
		return domain.Branch{}, fmt.Errorf("branch %s: %w", req.Name, domain.ErrAlreadyExists)
	}
	node, err := s.PlaceBranch(ctx, req)
	if err != nil {
		return domain.Branch{}, err
	}
	if node != s.Node {
		return domain.Branch{}, fmt.Errorf("%w: branch %s belongs on node %s", ErrOtherNode, req.Name, node)
	}
	base, snap, err := s.branchSource(ctx, req)
	if err != nil {
		return domain.Branch{}, err
	}
	port, err := s.allocatePort(ctx)
	if err != nil {
		return domain.Branch{}, err
	}
	br := domain.Branch{
		Name: req.Name, Base: base.Name, Snapshot: snap.Name, Runtime: base.Runtime,
		Status: domain.BranchProvisioning, Port: port, Node: s.Node, User: req.Name.String(), Password: s.Password(),
		CreatedAt: s.Clock(), InitSQL: req.InitSQL, Owner: req.Owner, Access: domain.AccessPrivate, Record: req.Record,
	}
	if !br.Record && s.RecordNew != nil {
		// The one moment it can be decided: never again after this.
		br.Record = s.RecordNew(ctx, br)
	}
	if req.Access == domain.AccessOpen {
		br.Access = domain.AccessOpen
		if s.OpenPassword != nil && s.OpenPassword() != "" {
			br.Password = s.OpenPassword()
		}
	}
	br.SetTTL(s.Clock(), req.TTL)
	return br, s.Store.InsertBranch(ctx, br)
}

func (s *Service) buildBranch(ctx context.Context, br *domain.Branch, initSQL string, report Reporter) error {
	srv, err := s.server(br.Runtime)
	if err != nil {
		return err
	}
	base, err := s.Store.GetBase(ctx, br.Base)
	if err != nil {
		return err
	}
	ds := s.branchDataset(br.Name)
	report.report("cloning")
	if err := s.Storage.Clone(ctx, domain.Snapshot{Dataset: s.sourceDataset(base), Name: br.Snapshot}, ds); err != nil {
		return err
	}
	spec, err := s.branchSpec(ctx, *br, ds, false, true)
	if err != nil {
		return err
	}
	if err := srv.PrepareClone(ctx, spec); err != nil {
		return err
	}
	report.report("starting")
	if err := srv.Start(ctx, spec); err != nil {
		return err
	}
	report.report("init_sql")
	if err := srv.CreateUser(ctx, spec, br.User, br.Password); err != nil {
		return err
	}
	if initSQL != "" {
		// As the branch login, not the admin: the SQL comes from whoever asked for the branch.
		if err := srv.ExecAs(ctx, spec, br.User, br.Password, initSQL); err != nil {
			return fmt.Errorf("init sql: %w", err)
		}
	}
	report.report("stopping")
	if err := srv.Stop(ctx, spec, domain.StopClean); err != nil {
		return err
	}
	report.report("snapshotting")
	if err := s.Storage.Snapshot(ctx, domain.Snapshot{Dataset: ds, Name: ResetSnapshot}); err != nil {
		return err
	}
	report.report("starting")
	return srv.Start(ctx, spec)
}

// fail marks the branch failed with cause and returns cause.
func (s *Service) fail(ctx context.Context, br domain.Branch, cause error) error {
	return errors.Join(cause, s.markFailed(ctx, br, cause))
}

func (s *Service) markFailed(ctx context.Context, br domain.Branch, cause error) error {
	br.Status, br.Error = domain.BranchFailed, cause.Error()
	return s.Store.PutBranch(ctx, br)
}

func (s *Service) transitionBranch(ctx context.Context, name domain.Name, check func(domain.Branch) error, to domain.BranchStatus) (domain.Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.Store.GetBranch(ctx, name)
	if err != nil {
		return br, err
	}
	if err := s.checkOwner("branch", br.Name, br.Node); err != nil {
		return br, err
	}
	if err := check(br); err != nil {
		return br, err
	}
	br.Status = to
	return br, s.Store.PutBranch(ctx, br)
}

// ResetBranch discards every change since the reset point.
func (s *Service) ResetBranch(ctx context.Context, name domain.Name, report Reporter) (domain.Branch, error) {
	br, err := s.transitionBranch(ctx, name, domain.Branch.CanReset, domain.BranchResetting)
	if err != nil {
		return br, err
	}
	return s.doReset(ctx, br, report)
}

func (s *Service) doReset(ctx context.Context, br domain.Branch, report Reporter) (domain.Branch, error) {
	if in, swap, err := s.resetBySwap(ctx, br); err != nil {
		// Nothing was touched: the branch is still what it was.
		br.Status = domain.BranchReady
		return br, errors.Join(err, s.Store.PutBranch(ctx, br))
	} else if swap {
		return s.runRestore(ctx, br, in, report)
	}
	srv, err := s.server(br.Runtime)
	if err != nil {
		return br, s.fail(ctx, br, err)
	}
	ds := s.branchDataset(br.Name)
	spec, err := s.branchSpec(ctx, br, ds, false, false)
	if err != nil {
		return br, s.fail(ctx, br, err)
	}
	// The current state is thrown away, so there is no point in a clean shutdown.
	report.report("stopping")
	if err := srv.Stop(ctx, spec, domain.StopImmediate); err != nil {
		return br, s.fail(ctx, br, err)
	}
	report.report("rolling_back")
	if err := s.Storage.Rollback(ctx, domain.Snapshot{Dataset: ds, Name: ResetSnapshot}); err != nil {
		return br, s.fail(ctx, br, err)
	}
	if s.Lineage != nil {
		if err := s.Lineage.Unset(ctx, ds, domain.PropHead); err != nil {
			return br, s.fail(ctx, br, err)
		}
	}
	// The reset point is the base's state, made by the base's runtime.
	if rt := s.runtimeOf(ctx, br, ""); rt != br.Runtime || br.LegacyAuth {
		br.Runtime, br.LegacyAuth = rt, false
		if err := s.Store.PutBranch(ctx, br); err != nil {
			return br, s.fail(ctx, br, err)
		}
		if srv, err = s.server(br.Runtime); err != nil {
			return br, s.fail(ctx, br, err)
		}
		if spec, err = s.branchSpec(ctx, br, ds, false, false); err != nil {
			return br, s.fail(ctx, br, err)
		}
	}
	report.report("starting")
	if err := srv.Start(ctx, spec); err != nil {
		return br, s.fail(ctx, br, err)
	}
	if err := s.putLoginRight(ctx, br, srv, spec); err != nil {
		return br, s.fail(ctx, br, err)
	}
	now := s.Clock()
	br.Status, br.ResetAt, br.Error = domain.BranchReady, &now, ""
	return br, s.Store.PutBranch(ctx, br)
}

// DeleteBranch removes the branch's server, dataset and record. A lost
// branch may be deleted from any node: it is marked deleting, and its node
// finishes the job when it returns.
func (s *Service) DeleteBranch(ctx context.Context, name domain.Name, report Reporter) error {
	if done, err := s.abandonLost(ctx, name); done || err != nil {
		return err
	}
	br, err := s.transitionBranch(ctx, name, domain.Branch.CanDelete, domain.BranchDeleting)
	if err != nil {
		return err
	}
	return s.doDelete(ctx, br, report)
}

func (s *Service) doDelete(ctx context.Context, br domain.Branch, report Reporter) error {
	srv, err := s.server(br.Runtime)
	if err != nil {
		return s.fail(ctx, br, err)
	}
	report.report("stopping")
	if err := srv.Remove(ctx, domain.Server{ID: branchServerID(br.Name), Runtime: br.Runtime}); err != nil {
		return s.fail(ctx, br, err)
	}
	report.report("destroying")
	if err := s.destroyBranchData(ctx, br.Name); err != nil {
		return s.fail(ctx, br, err)
	}
	return s.Store.DeleteBranch(ctx, br.Name)
}

// ExtendBranch moves the expiry to now+ttl.
func (s *Service) ExtendBranch(ctx context.Context, name domain.Name, ttl time.Duration) (domain.Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.Store.GetBranch(ctx, name)
	if err != nil {
		return br, err
	}
	if err := br.Extend(s.Clock(), ttl); err != nil {
		return br, err
	}
	return br, s.Store.PutBranch(ctx, br)
}

// Expire deletes branches whose TTL has run out and returns their names.
func (s *Service) Expire(ctx context.Context) ([]domain.Name, error) {
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []domain.Name
	var errs []error
	for _, b := range branches {
		if !s.owns(b.Node) || !b.Expired(s.Clock()) || b.CanDelete() != nil {
			continue
		}
		if err := s.DeleteBranch(ctx, b.Name, nil); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted = append(deleted, b.Name)
	}
	return deleted, errors.Join(errs...)
}

// Recover brings branches back to a steady state after the agent restarts:
// ready branches are started (paused ones are left stopped), interrupted
// resets, deletes, pauses and resumes are finished,
// and interrupted creates are marked failed.
func (s *Service) Recover(ctx context.Context) error {
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, br := range branches {
		if !s.owns(br.Node) {
			continue
		}
		switch br.Status {
		case domain.BranchReady:
			started, err := s.startIfDown(ctx, br)
			if errors.Is(err, ErrRecordRequired) {
				// Say why it is down; the next start retries (resumeFailedRestore).
				err = errors.Join(err, s.markFailed(ctx, br, err))
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			errs = append(errs, s.ownUser(ctx, br))
			if !started {
				errs = append(errs, s.hardenRunning(ctx, br))
			}
			errs = append(errs, s.releaseFreeze(ctx, br))
		case domain.BranchResetting:
			_, err := s.doReset(ctx, br, nil)
			errs = append(errs, err)
		case domain.BranchRestoring:
			errs = append(errs, s.resumeRestore(ctx, br))
		case domain.BranchPausing:
			_, err := s.doPause(ctx, br, nil)
			errs = append(errs, err)
		case domain.BranchResuming:
			_, err := s.doResume(ctx, br, nil)
			errs = append(errs, err)
		case domain.BranchUpgrading:
			errs = append(errs, s.resumeUpgrade(ctx, br))
		case domain.BranchFailed:
			errs = append(errs, s.resumeFailedRestore(ctx, br))
		case domain.BranchDeleting:
			errs = append(errs, s.doDelete(ctx, br, nil))
		case domain.BranchProvisioning:
			errs = append(errs, s.markFailed(ctx, br, errors.New("interrupted by an agent restart; delete and create it again")))
		}
	}
	return errors.Join(errs...)
}

// hardenRunning puts the login of a branch right when the agent starts and
// finds the branch's server running already.
func (s *Service) hardenRunning(ctx context.Context, br domain.Branch) error {
	srv, err := s.server(br.Runtime)
	if err != nil {
		return err
	}
	spec, err := s.branchSpec(ctx, br, s.branchDataset(br.Name), false, false)
	if err != nil {
		return err
	}
	return s.putLoginRight(ctx, br, srv, spec)
}

func (s *Service) ensureRunning(ctx context.Context, br domain.Branch) error {
	_, err := s.startIfDown(ctx, br)
	return err
}

// startIfDown starts the branch's server when it is not running (and puts the
// login right then), and says whether it did.
func (s *Service) startIfDown(ctx context.Context, br domain.Branch) (started bool, err error) {
	srv, err := s.server(br.Runtime)
	if err != nil {
		return false, err
	}
	spec, err := s.branchSpec(ctx, br, s.branchDataset(br.Name), false, false)
	if err != nil {
		return false, err
	}
	if ok, _ := srv.Running(ctx, spec); ok {
		return false, nil
	}
	if err := srv.Start(ctx, spec); err != nil {
		return false, err
	}
	return true, s.putLoginRight(ctx, br, srv, spec)
}

// putLoginRight makes the branch's login what clients get now (see
// DBServer.CreateUser). A branch made before that has the old login in its
// data, and a reset or a restore brings that data back, so this runs whenever
// a branch server starts and when the agent starts.
func (s *Service) putLoginRight(ctx context.Context, br domain.Branch, srv domain.DBServer, spec domain.Server) error {
	if br.User == "" {
		return nil
	}
	return srv.CreateUser(ctx, spec, br.User, br.Password)
}

// GetBranch returns one branch.
func (s *Service) GetBranch(ctx context.Context, name domain.Name) (domain.Branch, error) {
	return s.Store.GetBranch(ctx, name)
}

// ListBranches returns all branches.
func (s *Service) ListBranches(ctx context.Context) ([]domain.Branch, error) {
	return s.Store.ListBranches(ctx)
}

// BranchStorage reports how much space a branch uses on its own and shares with its base.
func (s *Service) BranchStorage(ctx context.Context, name domain.Name) (domain.DatasetInfo, error) {
	info, err := s.Storage.Info(ctx, s.branchDataset(name))
	if err != nil {
		return info, err
	}
	return s.historyUsage(ctx, name, info), nil
}

// RuntimeInUse reports whether a base or branch this node runs uses ref.
// Copies of other nodes' bases do not count: they never run a server.
func (s *Service) RuntimeInUse(ctx context.Context, ref domain.RuntimeRef) (bool, error) {
	bases, err := s.Store.ListBases(ctx)
	if err != nil {
		return false, err
	}
	for _, b := range bases {
		if b.Runtime == ref && s.owns(b.Node) {
			return true, nil
		}
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return false, err
	}
	for _, b := range branches {
		if b.Runtime == ref && s.owns(b.Node) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) abandonLost(ctx context.Context, name domain.Name) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.Store.GetBranch(ctx, name)
	if err != nil || br.Status != domain.BranchLost || s.owns(br.Node) {
		return false, nil
	}
	br.Status, br.Error = domain.BranchDeleting, abandoned
	return true, s.Store.PutBranch(ctx, br)
}

// abandoned marks a delete requested while the branch's node was away; that
// node finishes it in Recover or Reconcile.
const abandoned = "deleted while its node was unreachable"

// ownUser gives a branch made before branch-named users one, so the proxy
// can route to it by user name. The shared user keeps working.
func (s *Service) ownUser(ctx context.Context, br domain.Branch) error {
	if br.User != BranchUser {
		return nil
	}
	srv, err := s.server(br.Runtime)
	if err != nil {
		return err
	}
	spec, err := s.spec(ctx, branchServerID(br.Name), s.branchDataset(br.Name), br.Runtime, br.Port)
	if err != nil {
		return err
	}
	if err := srv.CreateUser(ctx, spec, br.Name.String(), br.Password); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.Store.GetBranch(ctx, br.Name)
	if err != nil || cur.User != BranchUser {
		return err
	}
	cur.User = br.Name.String()
	return s.Store.PutBranch(ctx, cur)
}
