package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/db-branch/db-branch/internal/domain"
)

// Checkpoints are ZFS snapshots of a branch's live dataset, taken while the
// server runs. Layout, per branch x:
//
//	<pool>/db-branch/branches/x       the live dataset (the server's data directory)
//	<pool>/db-branch/history/x        container for the branch's other lines (no mount)
//	<pool>/db-branch/history/x/<n>    a line set aside by a restore, kept for its checkpoints
//	<pool>/db-branch/history/x/new    the clone of a restore that is being swapped in
//
// A branch that never took a checkpoint has only the live dataset, as before.

func (s *Service) historyRoot(n domain.Name) string {
	return s.Pool + "/db-branch/history/" + n.String()
}

func (s *Service) lineDataset(n domain.Name, line int) string {
	return s.historyRoot(n) + "/" + strconv.Itoa(line)
}

func (s *Service) pendingDataset(n domain.Name) string { return s.historyRoot(n) + "/new" }

func (s *Service) maxCheckpoints() int {
	if s.MaxCheckpoints != nil {
		return s.MaxCheckpoints()
	}
	return domain.DefaultMaxCheckpoints
}

func (s *Service) requireLineage() error {
	if s.Lineage == nil {
		return fmt.Errorf("%w: checkpoints need this node's own ZFS storage", domain.ErrConflict)
	}
	return nil
}

// ownBranch returns the branch this node holds.
func (s *Service) ownBranch(ctx context.Context, n domain.Name) (domain.Branch, error) {
	br, err := s.Store.GetBranch(ctx, n)
	if err != nil {
		return br, err
	}
	return br, s.checkOwner("branch", br.Name, br.Node)
}

func (s *Service) readLine(ctx context.Context, dataset string) (domain.Line, error) {
	props, err := s.Lineage.Props(ctx, dataset)
	if err != nil {
		return domain.Line{}, err
	}
	snaps, err := s.Lineage.SnapshotInfos(ctx, dataset)
	return domain.Line{Dataset: dataset, Props: props, Snapshots: snaps}, err
}

// family reads the live dataset and every line set aside for branch n.
func (s *Service) family(ctx context.Context, n domain.Name) ([]domain.Line, error) {
	var lines []domain.Line
	liveDS := s.branchDataset(n)
	live, err := s.Storage.Exists(ctx, liveDS)
	if err != nil {
		return nil, err
	}
	if live {
		l, err := s.readLine(ctx, liveDS)
		if err != nil {
			return nil, err
		}
		l.Live = true
		lines = append(lines, l)
	}
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return lines, err
	}
	names, err := s.Lineage.Datasets(ctx, root)
	if err != nil {
		return nil, err
	}
	for _, ds := range names {
		l, err := s.readLine(ctx, ds)
		if err != nil {
			return nil, err
		}
		l.Pending = ds == s.pendingDataset(n)
		lines = append(lines, l)
	}
	return lines, nil
}

func liveOf(lines []domain.Line) (domain.Line, bool) {
	for _, l := range lines {
		if l.Live {
			return l, true
		}
	}
	return domain.Line{}, false
}

func lineNumber(props map[string]string) int {
	n, _ := strconv.Atoi(props[domain.PropLine])
	return n
}

// ListCheckpoints returns the checkpoints of every line of branch n, oldest first.
func (s *Service) ListCheckpoints(ctx context.Context, n domain.Name) ([]domain.Checkpoint, error) {
	if err := s.requireLineage(); err != nil {
		return nil, err
	}
	if _, err := s.ownBranch(ctx, n); err != nil {
		return nil, err
	}
	lines, err := s.family(ctx, n)
	if err != nil {
		return nil, err
	}
	head := ""
	if l, ok := liveOf(lines); ok {
		head = l.Props[domain.PropHead]
	}
	return domain.ListCheckpoints(lines, head), nil
}

// CheckpointGraph returns the checkpoints and the structure of the lines
// they are on: which line is live, and from which line and checkpoint each
// other line was restored.
func (s *Service) CheckpointGraph(ctx context.Context, n domain.Name) (domain.Graph, error) {
	if err := s.requireLineage(); err != nil {
		return domain.Graph{}, err
	}
	if _, err := s.ownBranch(ctx, n); err != nil {
		return domain.Graph{}, err
	}
	lines, err := s.family(ctx, n)
	if err != nil {
		return domain.Graph{}, err
	}
	head, live := "", 0
	if l, ok := liveOf(lines); ok {
		head, live = l.Props[domain.PropHead], lineNumber(l.Props)
	}
	return domain.Graph{Checkpoints: domain.ListCheckpoints(lines, head), Lines: domain.LineViews(lines), Live: live, Head: head}, nil
}

// CreateCheckpoint takes a checkpoint of the running branch. The server
// keeps running: the data directory and its write-ahead log are in one
// dataset, so the snapshot is what a crash would leave, which the database
// recovers from at once. An empty name is chosen from the time.
func (s *Service) CreateCheckpoint(ctx context.Context, n domain.Name, name string) (domain.Checkpoint, error) {
	if err := s.requireLineage(); err != nil {
		return domain.Checkpoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.ownBranch(ctx, n)
	if err != nil {
		return domain.Checkpoint{}, err
	}
	if err := br.CanCheckpoint(); err != nil {
		return domain.Checkpoint{}, err
	}
	lines, err := s.family(ctx, n)
	if err != nil {
		return domain.Checkpoint{}, err
	}
	if name == "" {
		name = domain.UniqueCheckpointName(lines, domain.DefaultCheckpointName(s.Clock()))
	} else if name, err = domain.ParseCheckpointName(name); err != nil {
		return domain.Checkpoint{}, err
	}
	if _, _, taken := domain.FindCheckpoint(lines, name); taken {
		return domain.Checkpoint{}, fmt.Errorf("checkpoint %s of branch %s: %w", name, n, domain.ErrAlreadyExists)
	}
	if err := domain.CheckLimit(n.String(), len(domain.ListCheckpoints(lines, "")), s.maxCheckpoints(), false); err != nil {
		return domain.Checkpoint{}, err
	}
	liveLine, ok := liveOf(lines)
	if !ok {
		return domain.Checkpoint{}, fmt.Errorf("branch %s: %w: its dataset is missing", n, domain.ErrNotFound)
	}
	if err := s.takeCheckpoint(ctx, liveLine, name, false); err != nil {
		return domain.Checkpoint{}, err
	}
	if err := s.Lineage.SetProps(ctx, liveLine.Dataset, map[string]string{domain.PropHead: name}); err != nil {
		return domain.Checkpoint{}, err
	}
	lines, err = s.family(ctx, n)
	if err != nil {
		return domain.Checkpoint{}, err
	}
	for _, c := range domain.ListCheckpoints(lines, name) {
		if c.Name == name {
			return c, nil
		}
	}
	return domain.Checkpoint{}, fmt.Errorf("checkpoint %s: %w", name, domain.ErrNotFound)
}

// takeCheckpoint snapshots line's dataset as checkpoint name and records the
// line it belongs to. A snapshot that was made but cannot be labelled is removed.
func (s *Service) takeCheckpoint(ctx context.Context, line domain.Line, name string, auto bool) error {
	snap := domain.Snapshot{Dataset: line.Dataset, Name: domain.CheckpointSnapshot(name)}
	if err := s.Storage.Snapshot(ctx, snap); err != nil {
		return err
	}
	return s.labelCheckpoint(ctx, snap, line.Props, auto)
}

// labelCheckpoint records on the snapshot the line it belongs to, and where
// that line came from: a line that a later restore promotes away loses its
// dataset, and the checkpoints are all that is left to draw it from.
func (s *Service) labelCheckpoint(ctx context.Context, snap domain.Snapshot, lineProps map[string]string, auto bool) error {
	flag := "off"
	if auto {
		flag = "on"
	}
	props := map[string]string{domain.PropLine: strconv.Itoa(lineNumber(lineProps)), domain.PropAuto: flag}
	for _, k := range []string{domain.PropFrom, domain.PropParent} {
		if v := lineProps[k]; v != "" {
			props[k] = v
		}
	}
	if err := s.Lineage.SetProps(ctx, snap.String(), props); err != nil {
		return errors.Join(err, s.Storage.DestroySnapshot(ctx, snap))
	}
	return nil
}

// DeleteCheckpoint removes one checkpoint. Other checkpoints are untouched.
// A checkpoint a restored line was cloned from cannot be destroyed while that
// line lives; it is renamed out of sight and destroyed with the line.
func (s *Service) DeleteCheckpoint(ctx context.Context, n domain.Name, name string) error {
	if err := s.requireLineage(); err != nil {
		return err
	}
	if _, err := domain.ParseCheckpointName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	br, err := s.ownBranch(ctx, n)
	if err != nil {
		return err
	}
	if err := br.CanCheckpoint(); err != nil {
		return err
	}
	lines, err := s.family(ctx, n)
	if err != nil {
		return err
	}
	ds, info, ok := domain.FindCheckpoint(lines, name)
	if !ok {
		return fmt.Errorf("checkpoint %s of branch %s: %w", name, n, domain.ErrNotFound)
	}
	snap := domain.Snapshot{Dataset: ds, Name: info.Name}
	if len(info.Clones) > 0 {
		err = s.Lineage.RenameSnapshot(ctx, snap, fmt.Sprintf("%s%d", domain.GonePrefix, s.Clock().UnixNano()))
	} else {
		err = s.Storage.DestroySnapshot(ctx, snap)
	}
	if err != nil {
		return err
	}
	s.collectLogged(ctx, n)
	return nil
}

func (s *Service) collectLogged(ctx context.Context, n domain.Name) {
	if err := s.collect(ctx, n); err != nil && s.Logf != nil {
		s.Logf("branch %s: cleaning up checkpoints: %v", n, err)
	}
}

// collect destroys the lines that hold no checkpoint any more and the
// deleted checkpoints nothing depends on. Removing a line can free what it
// was cloned from, so it repeats until nothing is left.
func (s *Service) collect(ctx context.Context, n domain.Name) error {
	for range 64 {
		lines, err := s.family(ctx, n)
		if err != nil {
			return err
		}
		g := domain.Garbage(lines)
		if len(g.Snapshots)+len(g.Datasets) == 0 {
			return s.dropEmptyRoot(ctx, n, lines)
		}
		for _, snap := range g.Snapshots {
			if err := s.Storage.DestroySnapshot(ctx, snap); err != nil {
				return err
			}
		}
		for _, ds := range g.Datasets {
			if err := s.Storage.Destroy(ctx, ds); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropEmptyRoot removes the history container once no line is left in it.
func (s *Service) dropEmptyRoot(ctx context.Context, n domain.Name, lines []domain.Line) error {
	root := s.historyRoot(n)
	for _, l := range lines {
		if !l.Live {
			return nil
		}
	}
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return err
	}
	if _, pending, _ := s.readIntent(ctx, n); pending {
		return nil
	}
	return s.Storage.Destroy(ctx, root)
}

// destroyBranchData removes everything a branch owns on this node: the live
// dataset, every line set aside, the restore in progress and all checkpoints.
// Datasets are destroyed one by one, clones before the datasets they were
// cloned from, so no zfs destroy -R is needed; it refuses when a snapshot of
// the branch has a clone elsewhere.
func (s *Service) destroyBranchData(ctx context.Context, n domain.Name) error {
	if s.Lineage == nil {
		return s.destroyIfExists(ctx, s.branchDataset(n))
	}
	lines, err := s.family(ctx, n)
	if err != nil {
		return err
	}
	order, err := domain.DestroyOrder(lines)
	if err != nil {
		return err
	}
	for _, ds := range order {
		if err := s.Storage.Destroy(ctx, ds); err != nil {
			return err
		}
	}
	root := s.historyRoot(n)
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || !ok {
		return err
	}
	return s.Storage.Destroy(ctx, root)
}

// historyUsage adds the space of the lines set aside to a branch's own.
func (s *Service) historyUsage(ctx context.Context, n domain.Name, info domain.DatasetInfo) domain.DatasetInfo {
	if s.Lineage == nil {
		return info
	}
	root := s.historyRoot(n)
	if ok, err := s.Storage.Exists(ctx, root); err != nil || !ok {
		return info
	}
	if h, err := s.Storage.Info(ctx, root); err == nil {
		info.Used += h.Used
	}
	return info
}
