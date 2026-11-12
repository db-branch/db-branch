package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// Restoring a checkpoint (and resetting a branch that has checkpoints) swaps
// a clone in as the live dataset instead of rolling back, so the checkpoints
// taken since stay restorable:
//
//  1. record the intent on the history container (what to restore, which
//     line number the new data gets, the name of the automatic checkpoint)
//  2. clone the checkpoint to history/<n>/new (not mounted)
//  3. stop the server, take the automatic checkpoint "before-restore-<time>"
//  4. unmount the live dataset and rename it to history/<n>/<its line>
//  5. rename history/<n>/new to the live name, mount it
//  6. promote it, so the lines set aside are clones of it and can be destroyed
//  7. start the server, clear the intent, destroy lines without checkpoints
//
// Every step looks at what is on disk before it acts, so running the whole
// sequence again after a crash picks up where it stopped. The live dataset
// is missing only between steps 4 and 5, and the sequence always continues
// forward from there.

// What an intent does.
const (
	opRestore = "restore"
	opReset   = "reset"
)

// restoreIntent is the persistent record of a restore in progress.
type restoreIntent struct {
	Op string
	// Source is the snapshot name to clone.
	Source string
	// NewLine is the line number the restored data carries.
	NewLine int
	// Auto is the checkpoint taken before the swap ("" for reset).
	Auto string
}

// head is the checkpoint the restored data descends from.
func (in restoreIntent) head() string {
	if in.Op != opRestore {
		return ""
	}
	n, _ := domain.CheckpointOf(in.Source)
	return n
}

func (s *Service) writeIntent(ctx context.Context, n domain.Name, in restoreIntent) error {
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil {
		return err
	}
	if !ok {
		if err := s.Storage.Create(ctx, root, map[string]string{"canmount": "off"}); err != nil {
			return err
		}
	}
	return s.Lineage.SetProps(ctx, root, map[string]string{
		domain.PropOp: in.Op, domain.PropSource: in.Source, domain.PropNewLine: strconv.Itoa(in.NewLine),
		domain.PropAutoName: orNone(in.Auto),
	})
}

// orNone stands for an empty value, which zfs cannot store.
func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (s *Service) readIntent(ctx context.Context, n domain.Name) (restoreIntent, bool, error) {
	if s.Lineage == nil {
		return restoreIntent{}, false, nil
	}
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return restoreIntent{}, false, err
	}
	p, err := s.Lineage.Props(ctx, root)
	if err != nil {
		return restoreIntent{}, false, err
	}
	line, err := strconv.Atoi(p[domain.PropNewLine])
	if p[domain.PropOp] == "" || p[domain.PropSource] == "" || err != nil {
		return restoreIntent{}, false, nil
	}
	in := restoreIntent{Op: p[domain.PropOp], Source: p[domain.PropSource], NewLine: line, Auto: p[domain.PropAutoName]}
	if in.Auto == "-" {
		in.Auto = ""
	}
	return in, true, nil
}

func (s *Service) clearIntent(ctx context.Context, n domain.Name) error {
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return err
	}
	return s.Lineage.Unset(ctx, root, domain.PropOp, domain.PropSource, domain.PropNewLine, domain.PropAutoName)
}

// RestoreCheckpoint sets the branch back to checkpoint name. Nothing is lost:
// the data as it is now is kept as an automatic checkpoint and every other
// checkpoint stays restorable.
func (s *Service) RestoreCheckpoint(ctx context.Context, n domain.Name, name string, report Reporter) (domain.Branch, error) {
	if err := s.requireLineage(); err != nil {
		return domain.Branch{}, err
	}
	if _, err := domain.ParseCheckpointName(name); err != nil {
		return domain.Branch{}, err
	}
	br, in, err := s.beginRestore(ctx, n, name)
	if err != nil {
		return br, err
	}
	if err := s.writeIntent(ctx, n, in); err != nil {
		br.Status = domain.BranchReady
		return br, errors.Join(err, s.Store.PutBranch(ctx, br))
	}
	return s.runRestore(ctx, br, in, report)
}

// beginRestore checks the request and marks the branch restoring.
func (s *Service) beginRestore(ctx context.Context, n domain.Name, name string) (domain.Branch, restoreIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.ownBranch(ctx, n)
	if err != nil {
		return br, restoreIntent{}, err
	}
	if err := br.CanCheckpoint(); err != nil {
		return br, restoreIntent{}, err
	}
	lines, err := s.family(ctx, n)
	if err != nil {
		return br, restoreIntent{}, err
	}
	if _, _, ok := domain.FindCheckpoint(lines, name); !ok {
		return br, restoreIntent{}, fmt.Errorf("checkpoint %s of branch %s: %w", name, n, domain.ErrNotFound)
	}
	if err := domain.CheckLimit(n.String(), len(domain.ListCheckpoints(lines, "")), s.maxCheckpoints(), true); err != nil {
		return br, restoreIntent{}, err
	}
	in := restoreIntent{
		Op: opRestore, Source: domain.CheckpointSnapshot(name), NewLine: domain.NextLine(lines),
		Auto: domain.UniqueCheckpointName(lines, domain.AutoCheckpointName("restore", s.Clock())),
	}
	br.Status = domain.BranchRestoring
	return br, in, s.Store.PutBranch(ctx, br)
}

// runRestore does the work of in and settles the branch: ready when it
// succeeds; back to ready with nothing changed when it failed before the
// swap began; failed (and finished on the next start) when it failed later.
func (s *Service) runRestore(ctx context.Context, br domain.Branch, in restoreIntent, report Reporter) (domain.Branch, error) {
	err := s.continueRestore(ctx, br, in, report)
	if err != nil {
		if s.beforeSwap(ctx, br.Name, in) {
			return br, errors.Join(err, s.abortRestore(ctx, br, in))
		}
		return br, s.fail(ctx, br, fmt.Errorf("%s interrupted, it is finished when the agent restarts: %w", in.Op, err))
	}
	br.Status, br.Error = domain.BranchReady, ""
	if in.Op == opReset {
		now := s.Clock()
		br.ResetAt = &now
	}
	return br, s.Store.PutBranch(ctx, br)
}

// swapped reports whether the live dataset already holds the restored data.
func (s *Service) swapped(ctx context.Context, n domain.Name, in restoreIntent) (live, exists bool, err error) {
	ds := s.branchDataset(n)
	if exists, err = s.Storage.Exists(ctx, ds); err != nil || !exists {
		return false, exists, err
	}
	p, err := s.Lineage.Props(ctx, ds)
	return p[domain.PropLine] == strconv.Itoa(in.NewLine), true, err
}

// beforeSwap reports whether nothing has been moved yet.
func (s *Service) beforeSwap(ctx context.Context, n domain.Name, in restoreIntent) bool {
	done, exists, err := s.swapped(ctx, n, in)
	return err == nil && exists && !done
}

func (s *Service) continueRestore(ctx context.Context, br domain.Branch, in restoreIntent, report Reporter) error {
	srv, err := s.server(br.Runtime)
	if err != nil {
		return err
	}
	liveDS := s.branchDataset(br.Name)
	done, exists, err := s.swapped(ctx, br.Name, in)
	if err != nil {
		return err
	}
	if !done {
		if err := s.setAside(ctx, br, in, srv, exists, report); err != nil {
			return err
		}
	}
	if err := s.Lineage.SetProps(ctx, liveDS, map[string]string{"canmount": "on", domain.PropLine: strconv.Itoa(in.NewLine)}); err != nil {
		return err
	}
	if head := in.head(); head != "" {
		err = s.Lineage.SetProps(ctx, liveDS, map[string]string{domain.PropHead: head})
	} else {
		err = s.Lineage.Unset(ctx, liveDS, domain.PropHead)
	}
	if err != nil {
		return err
	}
	if err := s.Lineage.Mount(ctx, liveDS); err != nil {
		return err
	}
	report.report("promoting")
	if err := s.promoteOwn(ctx, br.Name); err != nil {
		return err
	}
	spec, err := s.spec(ctx, branchServerID(br.Name), liveDS, br.Runtime, br.Port)
	if err != nil {
		return err
	}
	report.report("starting")
	if err := srv.PrepareClone(ctx, spec); err != nil {
		return err
	}
	if running, _ := srv.Running(ctx, spec); !running {
		if err := srv.Start(ctx, spec); err != nil {
			return err
		}
	}
	if err := s.clearIntent(ctx, br.Name); err != nil {
		return err
	}
	s.collectLogged(ctx, br.Name)
	return nil
}

// setAside is steps 2 to 5: clone, stop, automatic checkpoint, swap.
func (s *Service) setAside(ctx context.Context, br domain.Branch, in restoreIntent, srv domain.DBServer, liveExists bool, report Reporter) error {
	liveDS, pending := s.branchDataset(br.Name), s.pendingDataset(br.Name)
	havePending, err := s.Storage.Exists(ctx, pending)
	if err != nil {
		return err
	}
	if !havePending {
		report.report("cloning")
		lines, err := s.family(ctx, br.Name)
		if err != nil {
			return err
		}
		ds, ok := domain.FindSnapshot(lines, in.Source)
		if !ok {
			return fmt.Errorf("snapshot %s of branch %s: %w", in.Source, br.Name, domain.ErrNotFound)
		}
		props := map[string]string{"canmount": "off", domain.PropLine: strconv.Itoa(in.NewLine)}
		if err := s.Lineage.CloneWith(ctx, domain.Snapshot{Dataset: ds, Name: in.Source}, pending, props); err != nil {
			return err
		}
	}
	if liveExists {
		if err := s.hideLive(ctx, br, in, srv, report); err != nil {
			return err
		}
	}
	if !liveExists {
		report.report("swapping")
	}
	return s.Lineage.Rename(ctx, pending, liveDS)
}

// hideLive stops the server, takes the automatic checkpoint and moves the
// live dataset into the history container.
func (s *Service) hideLive(ctx context.Context, br domain.Branch, in restoreIntent, srv domain.DBServer, report Reporter) error {
	liveDS := s.branchDataset(br.Name)
	props, err := s.Lineage.Props(ctx, liveDS)
	if err != nil {
		return err
	}
	line := lineNumber(props)
	spec, err := s.spec(ctx, branchServerID(br.Name), liveDS, br.Runtime, br.Port)
	if err != nil {
		return err
	}
	report.report("stopping")
	if in.Op == opReset {
		// The state is thrown away, so there is no point in a clean shutdown.
		err = srv.Stop(ctx, spec, domain.StopImmediate)
	} else if err = srv.Stop(ctx, spec, domain.StopClean); err != nil {
		err = srv.Stop(ctx, spec, domain.StopImmediate)
	}
	if err != nil {
		return err
	}
	if in.Auto != "" {
		report.report("checkpointing")
		if err := s.ensureAuto(ctx, liveDS, in.Auto, line); err != nil {
			return err
		}
	}
	report.report("swapping")
	if err := s.Lineage.SetProps(ctx, liveDS, map[string]string{"canmount": "off"}); err != nil {
		return err
	}
	return s.Lineage.Rename(ctx, liveDS, s.lineDataset(br.Name, line))
}

// ensureAuto takes the automatic checkpoint unless an earlier attempt did,
// and labels it either way.
func (s *Service) ensureAuto(ctx context.Context, dataset, name string, line int) error {
	snaps, err := s.Lineage.SnapshotInfos(ctx, dataset)
	if err != nil {
		return err
	}
	snap := domain.Snapshot{Dataset: dataset, Name: domain.CheckpointSnapshot(name)}
	for _, x := range snaps {
		if x.Name == snap.Name {
			return s.labelCheckpoint(ctx, snap, line, true)
		}
	}
	return s.takeCheckpoint(ctx, domain.Line{Dataset: dataset, Props: map[string]string{domain.PropLine: strconv.Itoa(line)}}, name, true)
}

// abortRestore undoes a restore that failed before anything was moved: the
// branch is as it was, running, and the half-made clone is gone.
func (s *Service) abortRestore(ctx context.Context, br domain.Branch, in restoreIntent) error {
	liveDS := s.branchDataset(br.Name)
	var errs []error
	if ok, err := s.Storage.Exists(ctx, s.pendingDataset(br.Name)); err != nil {
		errs = append(errs, err)
	} else if ok {
		errs = append(errs, s.Storage.Destroy(ctx, s.pendingDataset(br.Name)))
	}
	if in.Auto != "" {
		// Nothing was lost: it only saved the state the branch still has.
		_ = s.Storage.DestroySnapshot(ctx, domain.Snapshot{Dataset: liveDS, Name: domain.CheckpointSnapshot(in.Auto)})
	}
	errs = append(errs, s.Lineage.SetProps(ctx, liveDS, map[string]string{"canmount": "on"}), s.Lineage.Mount(ctx, liveDS), s.ensureRunning(ctx, br))
	if err := errors.Join(errs...); err != nil {
		// The intent stays: the restore is finished when the agent restarts.
		return s.fail(ctx, br, err)
	}
	if err := s.clearIntent(ctx, br.Name); err != nil {
		return s.fail(ctx, br, err)
	}
	br.Status, br.Error = domain.BranchReady, ""
	return s.Store.PutBranch(ctx, br)
}

// resumeRestore finishes a restore the agent was interrupted in.
func (s *Service) resumeRestore(ctx context.Context, br domain.Branch) error {
	in, ok, err := s.readIntent(ctx, br.Name)
	if err != nil {
		return err
	}
	if !ok {
		// Interrupted before anything was written: the branch is as it was.
		if err := s.ensureRunning(ctx, br); err != nil {
			return err
		}
		br.Status, br.Error = domain.BranchReady, ""
		return s.Store.PutBranch(ctx, br)
	}
	_, err = s.runRestore(ctx, br, in, nil)
	return err
}

// resumeFailedRestore finishes a restore that failed after the swap began:
// the branch is failed, but the intent is still recorded.
func (s *Service) resumeFailedRestore(ctx context.Context, br domain.Branch) error {
	in, ok, err := s.readIntent(ctx, br.Name)
	if err != nil || !ok {
		return err
	}
	_, err = s.runRestore(ctx, br, in, nil)
	return err
}

// resetBySwap decides how to reset: by rolling back when the live dataset has
// only its reset point, else (it holds checkpoints, or the reset point lives
// on a line set aside) by swapping a clone of the reset point in, which keeps
// the checkpoints. An interrupted swap is resumed.
func (s *Service) resetBySwap(ctx context.Context, br domain.Branch) (restoreIntent, bool, error) {
	if s.Lineage == nil {
		return restoreIntent{}, false, nil
	}
	if in, ok, err := s.readIntent(ctx, br.Name); err != nil || (ok && in.Op == opReset) {
		return in, ok, err
	}
	lines, err := s.family(ctx, br.Name)
	if err != nil {
		return restoreIntent{}, false, err
	}
	if l, ok := liveOf(lines); ok && len(l.Snapshots) == 1 && l.Snapshots[0].Name == ResetSnapshot {
		return restoreIntent{}, false, nil
	}
	in := restoreIntent{Op: opReset, Source: ResetSnapshot, NewLine: domain.NextLine(lines)}
	return in, true, s.writeIntent(ctx, br.Name, in)
}

// promoteOwn promotes the live dataset for as long as it was cloned from one
// of the branch's own lines, so that it owns the whole history and every
// line set aside is a clone of it (and can be destroyed on its own). It never
// promotes across the branch's own boundary: a live dataset cloned from its
// base keeps that origin, or the base would become a clone of the branch.
func (s *Service) promoteOwn(ctx context.Context, n domain.Name) error {
	liveDS, own := s.branchDataset(n), s.historyRoot(n)+"/"
	for range 1024 {
		origin, err := s.Lineage.Origin(ctx, liveDS)
		if err != nil || !strings.HasPrefix(origin, own) {
			return err
		}
		if err := s.Lineage.Promote(ctx, liveDS); err != nil {
			return err
		}
	}
	return fmt.Errorf("promoting %s: its origin chain does not end", liveDS)
}
