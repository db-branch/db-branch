package dbbranch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

// MaskRequest asks for a masked copy of Source (at Snapshot, the latest
// when empty) to be written into Target, an existing base of the same
// engine, by a masker module with its rules.
type MaskRequest struct {
	Target, Source domain.Name
	Snapshot       string
	Masker         string
	Rules          string
	// By is who asked (recorded with the snapshot).
	By string
}

func (s *Service) workDataset(id string) string { return s.Pool + "/db-branch/work/" + id }

// MaskBase writes a masked copy of the source base into the target base
// and snapshots it. The source snapshot runs on a temporary server of this
// node; the masker reads it and writes into the target's server. If the
// masker, or the mask policy's check, fails, the target goes back to its
// latest snapshot: nothing half-masked is kept.
func (s *Service) MaskBase(ctx context.Context, req MaskRequest, report Reporter) (domain.BaseSnapshot, error) {
	masker, ok := s.Maskers[req.Masker]
	if !ok {
		return domain.BaseSnapshot{}, fmt.Errorf("%w: no masker %q (install its module)", domain.ErrInvalidName, req.Masker)
	}
	if req.Target == req.Source {
		return domain.BaseSnapshot{}, fmt.Errorf("%w: a base cannot be masked into itself", domain.ErrConflict)
	}
	source, err := s.Store.GetBase(ctx, req.Source)
	if err != nil {
		return domain.BaseSnapshot{}, err
	}
	snap, ok := source.LatestSnapshot()
	if req.Snapshot != "" {
		snap, ok = source.Snapshot(req.Snapshot)
	}
	if !ok {
		return domain.BaseSnapshot{}, fmt.Errorf("%w: base %s has no snapshot %q", domain.ErrNotFound, req.Source, req.Snapshot)
	}
	if source, err = s.replicated(ctx, source, snap.Name); err != nil {
		return domain.BaseSnapshot{}, err
	}
	if t, err := s.Store.GetBase(ctx, req.Target); err != nil {
		return domain.BaseSnapshot{}, err
	} else if t.Runtime != source.Runtime {
		return domain.BaseSnapshot{}, fmt.Errorf("%w: %s runs %s and %s runs %s: masking needs the same engine and version", domain.ErrConflict, req.Target, t.Runtime, req.Source, source.Runtime)
	}
	target, err := s.transitionBase(ctx, req.Target, domain.BaseReady, domain.BaseMaintenance)
	if err != nil {
		return domain.BaseSnapshot{}, err
	}
	out, err := s.mask(ctx, req, masker, source, snap.Name, target, report)
	target.Status = domain.BaseReady
	if err == nil {
		target.Snapshots = append(target.Snapshots, out)
	}
	if perr := s.Store.PutBase(context.WithoutCancel(ctx), target); err == nil {
		err = perr
	}
	return out, err
}

// replicated makes sure this node holds the source snapshot a mask reads:
// a base of another node is read from its replica, brought up to date first
// when the snapshot is new (the replication loop may not have got to it).
func (s *Service) replicated(ctx context.Context, source domain.Base, snapshot string) (domain.Base, error) {
	if s.owns(source.Node) || s.Shared != nil || s.Streams == nil || source.HasReplica(snapshot, s.Node) {
		return source, nil
	}
	_ = s.Replicate(ctx) // other bases' failures do not matter here; the check below does
	source, err := s.Store.GetBase(ctx, source.Name)
	if err != nil {
		return source, err
	}
	if !source.HasReplica(snapshot, s.Node) {
		return source, fmt.Errorf("%w: snapshot %s of %s is not replicated to node %s yet", domain.ErrConflict, snapshot, source.Name, s.Node)
	}
	return source, nil
}

func (s *Service) mask(ctx context.Context, req MaskRequest, masker domain.Masker, source domain.Base, snapshot string, target domain.Base, report Reporter) (domain.BaseSnapshot, error) {
	srv, tspec, err := s.runningBase(ctx, target)
	if err != nil {
		return domain.BaseSnapshot{}, err
	}
	report.report("starting the source copy")
	id := "mask-" + target.Name.String()
	work, cleanup, err := s.workServer(ctx, srv, id, source, snapshot)
	defer func() { _ = cleanup(context.WithoutCancel(ctx)) }()
	if err != nil {
		return domain.BaseSnapshot{}, err
	}
	plan := domain.MaskPlan{Source: source.Name, SourceSnapshot: snapshot, Target: target.Name, Masker: req.Masker, Rules: req.Rules, By: req.By,
		SourceDB: srv.Admin(work), TargetDB: srv.Admin(tspec)}
	if s.MaskPolicy != nil {
		if plan.Rules, err = s.MaskPolicy.PrepareMask(ctx, plan); err != nil {
			return domain.BaseSnapshot{}, err
		}
	}
	report.report("masking")
	if _, err := masker.Mask(ctx, domain.MaskJob{Source: plan.SourceDB, Target: plan.TargetDB, Rules: plan.Rules}); err != nil {
		return domain.BaseSnapshot{}, errors.Join(fmt.Errorf("masking %s into %s: %w", source.Name, target.Name, err), s.revert(ctx, srv, tspec, target))
	}
	if s.MaskPolicy != nil {
		report.report("verifying")
		if err := s.MaskPolicy.VerifyMask(ctx, plan); err != nil {
			return domain.BaseSnapshot{}, errors.Join(fmt.Errorf("checking the masked %s: %w", target.Name, err), s.revert(ctx, srv, tspec, target))
		}
	}
	_ = cleanup(ctx)
	out, err := s.cleanSnapshot(ctx, srv, tspec, s.baseDataset(target.Name), report)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256([]byte(plan.Rules))
	out.Mask = &domain.MaskInfo{Source: source.Name, SourceSnapshot: snapshot, Masker: req.Masker, RulesSHA256: hex.EncodeToString(sum[:]), By: req.By}
	report.report("restarting")
	err = srv.Start(ctx, tspec)
	if s.MaskPolicy != nil {
		err = errors.Join(err, s.MaskPolicy.MaskRecorded(ctx, plan, out.Name))
	}
	return out, err
}

// workServer clones the source snapshot and starts a temporary server on it.
// cleanup stops it and destroys the clone; it is safe to call twice.
func (s *Service) workServer(ctx context.Context, srv domain.DBServer, id string, source domain.Base, snapshot string) (domain.Server, func(context.Context) error, error) {
	ds, done := s.workDataset(id), false
	cleanup := func(ctx context.Context) error {
		if done {
			return nil
		}
		done = true
		ok, err := s.Storage.Exists(ctx, ds)
		if err != nil || !ok {
			return err
		}
		return errors.Join(srv.Remove(ctx, domain.Server{ID: id, Runtime: source.Runtime}), s.Storage.Destroy(ctx, ds))
	}
	if err := s.Storage.Clone(ctx, domain.Snapshot{Dataset: s.sourceDataset(source), Name: snapshot}, ds); err != nil {
		return domain.Server{}, cleanup, err
	}
	port, err := s.allocatePort(ctx)
	if err != nil {
		return domain.Server{}, cleanup, err
	}
	spec, err := s.spec(ctx, id, ds, source.Runtime, port)
	if err == nil {
		err = srv.PrepareClone(ctx, spec)
	}
	if err == nil {
		err = srv.Start(ctx, spec)
	}
	return spec, cleanup, err
}

// revert puts the target back to its latest snapshot and starts it again.
func (s *Service) revert(ctx context.Context, srv domain.DBServer, spec domain.Server, target domain.Base) error {
	latest, ok := target.LatestSnapshot()
	if !ok {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	return errors.Join(srv.Stop(ctx, spec, domain.StopImmediate), s.Storage.Rollback(ctx, domain.Snapshot{Dataset: s.baseDataset(target.Name), Name: latest.Name}), srv.Start(ctx, spec))
}
