package dbbranch

import (
	"context"
	"errors"

	"github.com/db-branch/db-branch/internal/domain"
)

// recoverBases settles the bases of this node that an agent crash left in a
// transient state: a create that never finished is marked failed, and a
// snapshot or mask that was interrupted goes back to the latest snapshot
// (nothing half-written is kept) with its temporary mask work removed.
func (s *Service) recoverBases(ctx context.Context) error {
	bases, err := s.Store.ListBases(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range bases {
		if !s.owns(b.Node) {
			continue
		}
		switch b.Status {
		case domain.BaseProvisioning:
			errs = append(errs, s.failInterruptedBase(ctx, b))
		case domain.BaseMaintenance:
			errs = append(errs, s.settleInterruptedMaintenance(ctx, b))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) failInterruptedBase(ctx context.Context, b domain.Base) error {
	srv, spec, err := s.baseServer(ctx, b)
	if err != nil {
		return err
	}
	stopErr := srv.Stop(ctx, spec, domain.StopImmediate)
	b.Status, b.Error = domain.BaseFailed, "interrupted by an agent restart; delete and create it again"
	return errors.Join(stopErr, s.Store.PutBase(ctx, b))
}

func (s *Service) settleInterruptedMaintenance(ctx context.Context, b domain.Base) error {
	srv, spec, err := s.baseServer(ctx, b)
	if err != nil {
		return err
	}
	if err := srv.Stop(ctx, spec, domain.StopImmediate); err != nil {
		return err
	}
	if err := s.removeMaskWork(ctx, srv, b); err != nil {
		return err
	}
	latest, ok := b.LatestSnapshot()
	if !ok {
		// Nothing to go back to: what the interrupted load left is not trustworthy.
		return s.markBaseFailed(ctx, b, "interrupted by an agent restart before its first snapshot; delete and create it again")
	}
	if err := s.Storage.Rollback(ctx, domain.Snapshot{Dataset: s.baseDataset(b.Name), Name: latest.Name}); err != nil {
		// E.g. a snapshot taken just before the crash is on disk but was never
		// recorded. The recorded snapshots are intact, so branches still work;
		// say so instead of failing every start.
		return s.markBaseFailed(ctx, b, "interrupted by an agent restart and could not be rolled back to "+latest.Name+": "+err.Error())
	}
	b.Status = domain.BaseReady
	return s.Store.PutBase(ctx, b)
}

func (s *Service) markBaseFailed(ctx context.Context, b domain.Base, why string) error {
	b.Status, b.Error = domain.BaseFailed, why
	return s.Store.PutBase(ctx, b)
}

// removeMaskWork drops the temporary clone and server a mask into b used.
func (s *Service) removeMaskWork(ctx context.Context, srv domain.DBServer, b domain.Base) error {
	id := "mask-" + b.Name.String()
	ds := s.workDataset(id)
	ok, err := s.Storage.Exists(ctx, ds)
	if err != nil || !ok {
		return err
	}
	// The work server runs the source's runtime, which only a mask knows;
	// removing the clone is what frees the space, the unit goes with it.
	return errors.Join(srv.Remove(ctx, domain.Server{ID: id, Runtime: b.Runtime}), s.Storage.Destroy(ctx, ds))
}
