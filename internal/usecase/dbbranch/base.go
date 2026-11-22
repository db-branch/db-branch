package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// CreateBaseRequest describes a new base.
type CreateBaseRequest struct {
	Name    domain.Name
	Runtime domain.RuntimeRef
	// Dump is a SQL dump on this node (plain or .gz). Empty creates an empty base.
	Dump string
	// Description says what the base holds. Optional.
	Description string
	// ReplicaNodes limits the other nodes that keep a copy: nil means every
	// node, empty the creating node only.
	ReplicaNodes *[]string
}

// MaxDescription is the longest base description, in bytes.
const MaxDescription = 1000

func cleanDescription(d string) (string, error) {
	d = strings.TrimSpace(d)
	if len(d) > MaxDescription {
		return "", fmt.Errorf("the description is %d bytes; at most %d", len(d), MaxDescription)
	}
	return d, nil
}

// DescribeBase sets what the base holds.
func (s *Service) DescribeBase(ctx context.Context, name domain.Name, description string) (domain.Base, error) {
	d, err := cleanDescription(description)
	if err != nil {
		return domain.Base{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return base, err
	}
	base.Description = d
	return base, s.Store.PutBase(ctx, base)
}

// CreateBase creates a base, loads the dump and takes the first consistent snapshot.
// The base server is left stopped.
func (s *Service) CreateBase(ctx context.Context, req CreateBaseRequest, report Reporter) (domain.Base, error) {
	if err := s.checkPoolRoom(ctx); err != nil {
		return domain.Base{}, err
	}
	srv, err := s.server(req.Runtime)
	if err != nil {
		return domain.Base{}, err
	}
	base, err := s.reserveBase(ctx, req)
	if err != nil {
		return domain.Base{}, err
	}
	if err := s.buildBase(ctx, srv, &base, req.Dump, report); err != nil {
		base.Status, base.Error = domain.BaseFailed, err.Error()
		_ = s.Store.PutBase(context.WithoutCancel(ctx), base)
		return base, err
	}
	base.Status = domain.BaseReady
	return base, s.Store.PutBase(ctx, base)
}

func (s *Service) reserveBase(ctx context.Context, req CreateBaseRequest) (domain.Base, error) {
	desc, err := cleanDescription(req.Description)
	if err != nil {
		return domain.Base{}, err
	}
	replicas, err := s.normalizeReplicaNodes(ctx, s.Node, s.ownDistro(req.Runtime.Engine), req.ReplicaNodes)
	if err != nil {
		return domain.Base{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Store.GetBase(ctx, req.Name); err == nil {
		return domain.Base{}, fmt.Errorf("base %s: %w", req.Name, domain.ErrAlreadyExists)
	}
	if ok, err := s.Runtimes.Has(ctx, req.Runtime); err != nil || !ok {
		return domain.Base{}, errors.Join(fmt.Errorf("%s: %w", req.Runtime, domain.ErrRuntimeNotInstalled), err)
	}
	port, err := s.allocatePort(ctx)
	if err != nil {
		return domain.Base{}, err
	}
	base := domain.Base{Name: req.Name, Runtime: req.Runtime, Status: domain.BaseProvisioning, Port: port, Node: s.Node, CreatedAt: s.Clock(), Description: desc}
	if err := s.Store.InsertBase(ctx, base); err != nil {
		return base, err
	}
	if replicas != nil {
		// A separate command: insert_base predates the setting.
		if err := s.Store.SetBaseReplicaNodes(ctx, base.Name, replicas); err != nil {
			return base, errors.Join(err, s.Store.DeleteBase(ctx, base.Name))
		}
		base.ReplicaNodes = replicas
	}
	return base, nil
}

// SetReplicaNodes chooses which nodes keep a copy of the base besides its own
// (nil: every node; empty: none). Nodes dropped from the list prune their copy
// once no branch there uses it; added nodes start pulling.
func (s *Service) SetReplicaNodes(ctx context.Context, name domain.Name, nodes *[]string) (domain.Base, error) {
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return base, err
	}
	all, err := s.nodes(ctx)
	if err != nil {
		return base, err
	}
	snap, _ := base.LatestSnapshot()
	norm, err := s.normalizeReplicaNodes(ctx, base.Node, s.originDistro(base, snap, all), nodes)
	if err != nil {
		return base, err
	}
	if err := s.Store.SetBaseReplicaNodes(ctx, name, norm); err != nil {
		return base, err
	}
	return s.Store.GetBase(ctx, name)
}

// normalizeReplicaNodes checks that every listed node exists and returns the
// list sorted, without duplicates and without the base's own node (implied).
// distro, when set, is the distribution a collation-bound base was made on:
// a listed node known to run another is refused.
func (s *Service) normalizeReplicaNodes(ctx context.Context, origin, distro string, nodes *[]string) (*[]string, error) {
	if nodes == nil {
		return nil, nil
	}
	all, err := s.nodes(ctx)
	if err != nil {
		return nil, err
	}
	exist, have := map[string]bool{}, map[string]string{}
	for _, n := range all {
		exist[n.Name], have[n.Name] = true, n.System.Distro
	}
	seen := map[string]bool{}
	out := []string{}
	for _, n := range *nodes {
		n = strings.TrimSpace(n)
		switch {
		case !exist[n]:
			return nil, fmt.Errorf("%w: node %q does not exist", domain.ErrInvalidReplicas, n)
		case n == origin || seen[n]:
		case !domain.SameDistro(distro, have[n]):
			return nil, domain.ErrDistroMismatch(n, have[n], distro)
		default:
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return &out, nil
}

func (s *Service) buildBase(ctx context.Context, srv domain.DBServer, base *domain.Base, dump string, report Reporter) error {
	ds := s.baseDataset(base.Name)
	report.report("creating")
	if err := s.Storage.Create(ctx, ds, engineProps(base.Runtime)); err != nil {
		return err
	}
	spec, err := s.spec(ctx, baseServerID(base.Name), ds, base.Runtime, base.Port)
	if err != nil {
		return err
	}
	report.report("initializing")
	if err := srv.Initialize(ctx, spec); err != nil {
		return err
	}
	started := false
	defer func() {
		if started {
			_ = srv.Stop(context.WithoutCancel(ctx), spec, domain.StopImmediate)
		}
	}()
	if err := srv.Start(ctx, spec); err != nil {
		return err
	}
	started = true
	if dump != "" {
		report.report("loading")
		if err := srv.Load(ctx, spec, dump); err != nil {
			return err
		}
	}
	snap, err := s.cleanSnapshot(ctx, srv, spec, ds, report)
	if err != nil {
		return err
	}
	started = false
	base.Snapshots = append(base.Snapshots, snap)
	return nil
}

// cleanSnapshot stops the server cleanly and snapshots its dataset. Branches
// cloned from such a snapshot start without crash recovery.
func (s *Service) cleanSnapshot(ctx context.Context, srv domain.DBServer, spec domain.Server, ds string, report Reporter) (domain.BaseSnapshot, error) {
	started := false
	defer func() {
		if started {
			stopCtx := context.WithoutCancel(ctx)
			if serr := srv.Stop(stopCtx, spec, domain.StopClean); serr != nil {
				_ = srv.Stop(stopCtx, spec, domain.StopImmediate)
			}
		}
	}()
	// Statistics first, so branches of the snapshot plan their queries well.
	// A stopped server is started for it (and stopped again below).
	if u, ok := srv.(domain.StatsUpdater); ok {
		if running, _ := srv.Running(ctx, spec); !running {
			report.report("starting")
			if err := srv.Start(ctx, spec); err != nil {
				return domain.BaseSnapshot{}, err
			}
			started = true
		}
		report.report("analyzing")
		if err := u.UpdateStats(ctx, spec); err != nil {
			return domain.BaseSnapshot{}, fmt.Errorf("updating the statistics of %s: %w", spec.ID, err)
		}
	}
	report.report("stopping")
	if err := srv.Stop(ctx, spec, domain.StopClean); err != nil {
		return domain.BaseSnapshot{}, err
	}
	started = false
	report.report("snapshotting")
	snap := domain.BaseSnapshot{Name: s.snapshotName(), CreatedAt: s.Clock(), Distro: s.Distro}
	if err := s.Storage.Snapshot(ctx, domain.Snapshot{Dataset: ds, Name: snap.Name}); err != nil {
		return domain.BaseSnapshot{}, err
	}
	return snap, nil
}

// SnapshotBase takes a new consistent snapshot of a ready base. A running base
// server is stopped for the snapshot and started again afterwards.
func (s *Service) SnapshotBase(ctx context.Context, name domain.Name, report Reporter) (domain.BaseSnapshot, error) {
	base, err := s.transitionBase(ctx, name, domain.BaseReady, domain.BaseMaintenance)
	if err != nil {
		return domain.BaseSnapshot{}, err
	}
	snap, err := s.snapshotRunningBase(ctx, base, report)
	if err == nil {
		base.Snapshots = append(base.Snapshots, snap)
	}
	base.Status = domain.BaseReady
	if perr := s.Store.PutBase(ctx, base); err == nil {
		err = perr
	}
	return snap, err
}

func (s *Service) snapshotRunningBase(ctx context.Context, base domain.Base, report Reporter) (domain.BaseSnapshot, error) {
	srv, spec, err := s.baseServer(ctx, base)
	if err != nil {
		return domain.BaseSnapshot{}, err
	}
	wasRunning, _ := srv.Running(ctx, spec)
	snap, err := s.cleanSnapshot(ctx, srv, spec, s.baseDataset(base.Name), report)
	if errors.Is(err, domain.ErrUncleanShutdown) {
		return snap, err
	}
	if wasRunning {
		if running, _ := srv.Running(ctx, spec); !running {
			report.report("restarting")
			err = errors.Join(err, srv.Start(ctx, spec))
		}
	}
	return snap, err
}

func (s *Service) baseServer(ctx context.Context, base domain.Base) (domain.DBServer, domain.Server, error) {
	if err := s.checkOwner("base", base.Name, base.Node); err != nil {
		return nil, domain.Server{}, err
	}
	srv, err := s.server(base.Runtime)
	if err != nil {
		return nil, domain.Server{}, err
	}
	spec, err := s.spec(ctx, baseServerID(base.Name), s.baseDataset(base.Name), base.Runtime, base.Port)
	return srv, spec, err
}

func (s *Service) transitionBase(ctx context.Context, name domain.Name, from, to domain.BaseStatus) (domain.Base, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return base, err
	}
	if err := s.checkOwner("base", base.Name, base.Node); err != nil {
		return base, err
	}
	if base.Status != from {
		return base, fmt.Errorf("%w: base %s is %s", domain.ErrConflict, name, base.Status)
	}
	base.Status = to
	return base, s.Store.PutBase(ctx, base)
}

// StartBase starts the base server, e.g. to load more data before the next snapshot.
func (s *Service) StartBase(ctx context.Context, name domain.Name) error {
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return err
	}
	srv, spec, err := s.baseServer(ctx, base)
	if err != nil {
		return err
	}
	return srv.Start(ctx, spec)
}

// StopBase stops the base server cleanly.
func (s *Service) StopBase(ctx context.Context, name domain.Name) error {
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return err
	}
	srv, spec, err := s.baseServer(ctx, base)
	if err != nil {
		return err
	}
	return srv.Stop(ctx, spec, domain.StopClean)
}

// DeleteBase removes a base that no branch uses. A base in the middle of a
// snapshot, mask or create is refused, and while the delete runs no branch
// can be made from the base.
func (s *Service) DeleteBase(ctx context.Context, name domain.Name) error {
	base, err := s.reserveBaseDelete(ctx, name)
	if err != nil {
		return err
	}
	defer s.endBaseDelete(name)
	srv, err := s.server(base.Runtime)
	if err != nil {
		return err
	}
	if err := srv.Remove(ctx, domain.Server{ID: baseServerID(name), Runtime: base.Runtime}); err != nil {
		return err
	}
	if err := s.destroyIfExists(ctx, s.baseDataset(name)); err != nil {
		return err
	}
	return s.Store.DeleteBase(ctx, name)
}

func (s *Service) destroyIfExists(ctx context.Context, ds string) error {
	ok, err := s.Storage.Exists(ctx, ds)
	if err != nil || !ok {
		return err
	}
	return s.Storage.Destroy(ctx, ds)
}

// GetBase returns one base.
func (s *Service) GetBase(ctx context.Context, name domain.Name) (domain.Base, error) {
	return s.Store.GetBase(ctx, name)
}

// ListBases returns all bases.
func (s *Service) ListBases(ctx context.Context) ([]domain.Base, error) {
	return s.Store.ListBases(ctx)
}

// SetAnonymous sets what callers without credentials may do with the base:
// how many branches they may hold from it and for how long (the zero value
// inherits the cluster-wide settings). It does not make a snapshot public.
func (s *Service) SetAnonymous(ctx context.Context, name domain.Name, l domain.AnonymousLimits) (domain.Base, error) {
	if err := l.Validate(); err != nil {
		return domain.Base{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Store.GetBase(ctx, name); err != nil {
		return domain.Base{}, err
	}
	if err := s.Store.SetBaseAnonymous(ctx, name, l); err != nil {
		return domain.Base{}, err
	}
	return s.Store.GetBase(ctx, name)
}

// reserveBaseDelete checks, under the service lock, that nothing is using the
// base and marks it as being deleted.
func (s *Service) reserveBaseDelete(ctx context.Context, name domain.Name) (domain.Base, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return base, err
	}
	if err := s.checkOwner("base", name, base.Node); err != nil {
		return base, err
	}
	if base.Status == domain.BaseProvisioning || base.Status == domain.BaseMaintenance || s.deletingBases[name] {
		return base, fmt.Errorf("%w: base %s is %s", domain.ErrConflict, name, base.Status)
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return base, err
	}
	for _, b := range branches {
		if b.Base == name {
			return base, fmt.Errorf("%w: base %s is used by branch %s", domain.ErrInUse, name, b.Name)
		}
	}
	if s.deletingBases == nil {
		s.deletingBases = map[domain.Name]bool{}
	}
	s.deletingBases[name] = true
	return base, nil
}

func (s *Service) endBaseDelete(name domain.Name) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deletingBases, name)
}
