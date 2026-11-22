package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/db-branch/db-branch/internal/domain"
)

// readyBase reads a base this node holds that is not being changed. The
// caller holds s.mu, so the record it writes back is current.
func (s *Service) readyBase(ctx context.Context, name domain.Name) (domain.Base, error) {
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return base, err
	}
	if err := s.checkOwner("base", name, base.Node); err != nil {
		return base, err
	}
	if base.Status != domain.BaseReady {
		return base, fmt.Errorf("%w: base %s is %s", domain.ErrConflict, name, base.Status)
	}
	return base, nil
}

// SetSnapshotPublic lets anyone (without logging in) branch from a snapshot, or stops that.
func (s *Service) SetSnapshotPublic(ctx context.Context, name domain.Name, snapshot string, public bool) (domain.Base, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, err := s.readyBase(ctx, name)
	if err != nil {
		return base, err
	}
	if !base.SetPublic(snapshot, public) {
		return base, fmt.Errorf("snapshot %s of base %s: %w", snapshot, name, domain.ErrNotFound)
	}
	return base, s.Store.PutBase(ctx, base)
}

func usedBy(branches []domain.Branch, base domain.Name, snapshot string) (domain.Name, bool) {
	for _, b := range branches {
		if b.Base == base && b.Snapshot == snapshot {
			return b.Name, true
		}
	}
	return domain.Name{}, false
}

// DeleteSnapshot removes a snapshot of a base that no branch uses. The
// latest snapshot stays (it is what new branches start from). Copies on
// other nodes are removed by their next replication.
func (s *Service) DeleteSnapshot(ctx context.Context, name domain.Name, snapshot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteSnapshot(ctx, name, snapshot)
}

func (s *Service) deleteSnapshot(ctx context.Context, name domain.Name, snapshot string) error {
	base, err := s.readyBase(ctx, name)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(base.Snapshots, func(x domain.BaseSnapshot) bool { return x.Name == snapshot })
	if i < 0 {
		return fmt.Errorf("snapshot %s of base %s: %w", snapshot, name, domain.ErrNotFound)
	}
	if i == len(base.Snapshots)-1 {
		return fmt.Errorf("%w: %s is the latest snapshot of base %s; take a newer one first", domain.ErrConflict, snapshot, name)
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	if b, ok := usedBy(branches, name, snapshot); ok {
		return fmt.Errorf("%w: snapshot %s of base %s is used by branch %s", domain.ErrInUse, snapshot, name, b)
	}
	// Withdraw it first so no new branch starts from it, then check again
	// for one that started in the meantime.
	removed := base.Snapshots[i]
	base.Snapshots = slices.Delete(slices.Clone(base.Snapshots), i, i+1)
	if err := s.Store.PutBase(ctx, base); err != nil {
		return err
	}
	if branches, err = s.Store.ListBranches(ctx); err == nil {
		if b, ok := usedBy(branches, name, snapshot); ok {
			base.Snapshots = slices.Insert(base.Snapshots, i, removed)
			return errors.Join(fmt.Errorf("%w: snapshot %s of base %s is used by branch %s", domain.ErrInUse, snapshot, name, b),
				s.Store.PutBase(ctx, base))
		}
	}
	return s.Storage.DestroySnapshot(ctx, domain.Snapshot{Dataset: s.baseDataset(name), Name: snapshot})
}

// PruneSnapshots deletes, on the bases this node holds, every snapshot older
// than the latest that no branch uses, once each node holding a copy of it
// also holds the latest (so replication continues incrementally). A pruned
// public snapshot passes its publicity to the latest.
func (s *Service) PruneSnapshots(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bases, err := s.Store.ListBases(ctx)
	if err != nil {
		return nil, err
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return nil, err
	}
	var pruned []string
	var errs []error
	for _, base := range bases {
		if !s.owns(base.Node) || base.Status != domain.BaseReady || len(base.Snapshots) < 2 {
			continue
		}
		latest, _ := base.LatestSnapshot()
		public := false
		for _, snap := range base.Snapshots[:len(base.Snapshots)-1] {
			if _, used := usedBy(branches, base.Name, snap.Name); used || !s.copiesCaughtUp(base, snap.Name, latest.Name) {
				continue
			}
			if err := s.deleteSnapshot(ctx, base.Name, snap.Name); err != nil {
				errs = append(errs, err)
				continue
			}
			public = public || snap.Public
			pruned = append(pruned, base.Name.String()+"@"+snap.Name)
		}
		if public && !latest.Public {
			b, err := s.Store.GetBase(ctx, base.Name)
			if err == nil && b.SetPublic(latest.Name, true) {
				err = s.Store.PutBase(ctx, b)
			}
			errs = append(errs, err)
		}
	}
	return pruned, errors.Join(errs...)
}

func (s *Service) copiesCaughtUp(base domain.Base, old, latest string) bool {
	for _, n := range base.Replicas[old] {
		if !base.HasReplica(latest, n) {
			return false
		}
	}
	return true
}

// ExecBase runs SQL as the base server's local admin, starting the server
// if it is stopped, to change the data before the next snapshot.
func (s *Service) ExecBase(ctx context.Context, name domain.Name, sql string) error {
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return err
	}
	if base.Status != domain.BaseReady {
		return fmt.Errorf("%w: base %s is %s", domain.ErrConflict, name, base.Status)
	}
	srv, spec, err := s.runningBase(ctx, base)
	if err != nil {
		return err
	}
	return srv.Exec(ctx, spec, sql)
}

// runningBase returns the base's server, started if it was stopped.
func (s *Service) runningBase(ctx context.Context, base domain.Base) (domain.DBServer, domain.Server, error) {
	srv, spec, err := s.baseServer(ctx, base)
	if err != nil {
		return nil, spec, err
	}
	if running, err := srv.Running(ctx, spec); err != nil {
		return nil, spec, err
	} else if !running {
		if err := srv.Start(ctx, spec); err != nil {
			return nil, spec, err
		}
	}
	return srv, spec, nil
}

// LoadBase loads a SQL dump (plain or .gz, a file on this node) into the
// base's server. The base is in maintenance meanwhile, so no snapshot stops
// the server halfway; its snapshots and branches are untouched.
func (s *Service) LoadBase(ctx context.Context, name domain.Name, dump string, report Reporter) error {
	if dump == "" {
		return errors.New("no dump to load")
	}
	if err := s.checkPoolRoom(ctx); err != nil {
		return err
	}
	base, err := s.transitionBase(ctx, name, domain.BaseReady, domain.BaseMaintenance)
	if err != nil {
		return err
	}
	defer func() {
		base.Status = domain.BaseReady
		_ = s.Store.PutBase(context.WithoutCancel(ctx), base)
	}()
	report.report("starting")
	srv, spec, err := s.runningBase(ctx, base)
	if err != nil {
		return err
	}
	report.report("loading")
	return srv.Load(ctx, spec, dump)
}

// dropDeletedCopies destroys this node's copies of snapshots the base no
// longer has (they were deleted on its node), so the next incremental
// stream starts from a snapshot both sides keep. Copies still cloned by a
// branch here stay (zfs refuses), and are retried later.
func (s *Service) dropDeletedCopies(ctx context.Context, base domain.Base, ds string, held map[string]bool) map[string]bool {
	out := map[string]bool{}
	for name := range held {
		if _, ok := base.Snapshot(name); ok {
			out[name] = true
			continue
		}
		if err := s.Storage.DestroySnapshot(ctx, domain.Snapshot{Dataset: ds, Name: name}); err != nil {
			out[name] = true
		}
	}
	return out
}
