package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/db-branch/db-branch/internal/domain"
)

func (s *Service) replicasRoot() string { return s.Pool + "/db-branch/replicas" }

func (s *Service) replicaDataset(n domain.Name) string { return s.replicasRoot() + "/" + n.String() }

// sourceDataset is where this node clones the base's snapshots from: its own
// dataset on the base's node (or on shared storage), the received copy
// elsewhere.
func (s *Service) sourceDataset(base domain.Base) string {
	if s.owns(base.Node) || s.Shared != nil {
		return s.baseDataset(base.Name)
	}
	return s.replicaDataset(base.Name)
}

// SendBase streams the base's snapshots after from (all when empty) up to
// to, for another node to replicate.
func (s *Service) SendBase(ctx context.Context, name domain.Name, from, to string, w io.Writer) error {
	base, err := s.Store.GetBase(ctx, name)
	if err != nil {
		return err
	}
	if err := s.checkOwner("base", name, base.Node); err != nil {
		return err
	}
	for _, snap := range []string{from, to} {
		if snap == from && from == "" {
			continue // a full stream
		}
		if _, ok := base.Snapshot(snap); !ok {
			return fmt.Errorf("snapshot %q of base %s: %w", snap, name, domain.ErrNotFound)
		}
	}
	if s.Streams == nil {
		return errors.New("snapshot streams are not available on this node")
	}
	return s.Streams.Send(ctx, s.baseDataset(name), from, to, w)
}

// Replicate brings this node's copies of other nodes' bases up to their
// latest snapshot, records what it holds, and removes copies of deleted
// bases. Errors on one base do not stop the others.
func (s *Service) Replicate(ctx context.Context) error {
	if s.Streams == nil || s.Source == nil {
		return nil
	}
	s.replicating.Lock()
	defer s.replicating.Unlock()
	bases, err := s.Store.ListBases(ctx)
	if err != nil {
		return err
	}
	nodes, _ := s.nodes(ctx) // unknown nodes only loosen the distribution check below
	for _, n := range nodes {
		if n.Name == s.Node && n.IDProblem != "" {
			// Received data would not belong to the user that runs the
			// servers here; wait until the node's ids are fixed.
			return nil
		}
	}
	var errs []error
	for _, b := range bases {
		if s.owns(b.Node) || b.Node == "" {
			continue
		}
		latest, ok := b.LatestSnapshot()
		if !ok || !b.Wants(s.Node) {
			continue
		}
		// A PostgreSQL copy is useless where branches may not run (and the
		// stream would only cost bandwidth): see originDistro.
		if !domain.SameDistro(s.originDistro(b, latest, nodes), s.Distro) {
			continue
		}
		if err := s.replicateBase(ctx, b); err != nil {
			errs = append(errs, fmt.Errorf("replicate base %s from %s: %w", b.Name, b.Node, err))
		}
	}
	errs = append(errs, s.pruneReplicas(ctx, bases))
	return errors.Join(errs...)
}

func (s *Service) replicateBase(ctx context.Context, base domain.Base) error {
	ds := s.replicaDataset(base.Name)
	latest, _ := base.LatestSnapshot()
	held, err := s.heldSnapshots(ctx, ds)
	if err != nil {
		return err
	}
	held = s.dropDeletedCopies(ctx, base, ds, held)
	from := ""
	for i := len(base.Snapshots) - 1; i >= 0; i-- {
		if held[base.Snapshots[i].Name] {
			from = base.Snapshots[i].Name
			break
		}
	}
	if from != latest.Name {
		if err := s.receive(ctx, base, ds, from, latest.Name); err != nil {
			return err
		}
		if held, err = s.heldSnapshots(ctx, ds); err != nil {
			return err
		}
	}
	var errs []error
	for _, snap := range base.Snapshots {
		if held[snap.Name] && !base.HasReplica(snap.Name, s.Node) {
			errs = append(errs, s.Store.AddReplica(ctx, base.Name, snap.Name, s.Node))
		}
	}
	return errors.Join(errs...)
}

// heldSnapshots returns the snapshots of the local copy (none if absent).
func (s *Service) heldSnapshots(ctx context.Context, ds string) (map[string]bool, error) {
	held := map[string]bool{}
	ok, err := s.Storage.Exists(ctx, ds)
	if err != nil || !ok {
		return held, err
	}
	names, err := s.Streams.Snapshots(ctx, ds)
	for _, n := range names {
		held[n] = true
	}
	return held, err
}

func (s *Service) receive(ctx context.Context, base domain.Base, ds, from, to string) error {
	if from == "" {
		// Nothing in common with the base (first copy, or a copy of an older
		// base of the same name): start over.
		if err := s.destroyIfExists(ctx, ds); err != nil {
			return err
		}
		if ok, err := s.Storage.Exists(ctx, s.replicasRoot()); err != nil || !ok {
			if err := errors.Join(err, s.Storage.Create(ctx, s.replicasRoot(), nil)); err != nil {
				return err
			}
		}
	}
	stream, err := s.Source.Open(ctx, base.Node, base.Name, from, to)
	if err != nil {
		return err
	}
	defer stream.Close()
	return s.Streams.Receive(ctx, ds, stream)
}

// pruneReplicas destroys copies of bases that no longer exist, and copies of
// bases that no longer want this node (see dropUnwantedCopy). Deleting a base
// requires that no branch uses it, so no clone depends on its copy.
func (s *Service) pruneReplicas(ctx context.Context, bases []domain.Base) error {
	ok, err := s.Storage.Exists(ctx, s.replicasRoot())
	if err != nil || !ok {
		return err
	}
	names, err := s.Streams.Children(ctx, s.replicasRoot())
	if err != nil {
		return err
	}
	known := map[string]domain.Base{}
	for _, b := range bases {
		known[b.Name.String()] = b
	}
	var errs []error
	for _, n := range names {
		switch b, ok := known[n]; {
		case !ok:
			errs = append(errs, s.Storage.Destroy(ctx, s.replicasRoot()+"/"+n))
		case !b.Wants(s.Node):
			errs = append(errs, s.dropUnwantedCopy(ctx, b))
		}
	}
	return errors.Join(errs...)
}

// dropUnwantedCopy removes this node's copy of a base that no longer lists the
// node. While a branch here still uses the base the copy stays (its ZFS clone
// depends on the snapshot) and the node shows as draining; the record of the
// copy is forgotten first so no new branch is placed on it.
func (s *Service) dropUnwantedCopy(ctx context.Context, base domain.Base) error {
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	for _, br := range branches {
		if br.Base == base.Name && br.Node == s.Node {
			return nil
		}
	}
	if base.HoldsAny(s.Node) {
		if err := s.Store.RemoveReplica(ctx, base.Name, s.Node); err != nil {
			return err
		}
	}
	return s.Storage.Destroy(ctx, s.replicaDataset(base.Name))
}
