package domain

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A checkpoint is a ZFS snapshot of a branch's live dataset, taken while the
// server runs. Restoring one never destroys the others: the branch's current
// data is set aside as a hidden "line" (a dataset of its own) and a clone of
// the checkpoint becomes the live dataset. See docs/checkpoints.md.

// ResetSnapshot is the per-branch snapshot taken right after init SQL.
const ResetSnapshot = "reset"

const (
	// CheckpointPrefix starts the ZFS snapshot name of every checkpoint.
	CheckpointPrefix = "ckpt-"
	// GonePrefix names a deleted checkpoint that cannot be destroyed yet
	// because a clone depends on it; it is destroyed with its last clone.
	GonePrefix = "gone-"
	// MaxCheckpointNameLength keeps snapshot names short.
	MaxCheckpointNameLength = 40
	// DefaultMaxCheckpoints is the default of the branch.max_checkpoints setting.
	DefaultMaxCheckpoints = 20
	// MaxAutoCheckpoints is how many automatic (before-restore) checkpoints a
	// branch keeps; a restore that adds another deletes the oldest one.
	MaxAutoCheckpoints = 5
)

// ZFS user properties of the checkpoint feature. They are set on the snapshot
// itself (never relied on through inheritance) except where noted.
const (
	// PropLine is the line number: on a checkpoint, the line it was taken on;
	// on a dataset, the line it holds.
	PropLine = "db-branch:line"
	// PropAuto is "on" for checkpoints the system took (before a restore).
	PropAuto = "db-branch:auto"
	// PropHead (on the live dataset) names the checkpoint the current state descends from.
	PropHead = "db-branch:head"
	// PropOp, PropSource, PropNewLine and PropAutoName (on the history
	// container) record a restore in progress so that it can be finished.
	PropOp       = "db-branch:op"
	PropSource   = "db-branch:source"
	PropNewLine  = "db-branch:newline"
	PropAutoName = "db-branch:autoname"
)

var checkpointNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// ParseCheckpointName validates a checkpoint name: lowercase letters, digits
// and hyphens, up to MaxCheckpointNameLength.
func ParseCheckpointName(s string) (string, error) {
	if len(s) > MaxCheckpointNameLength || !checkpointNamePattern.MatchString(s) {
		return "", fmt.Errorf("%w %q: use 1-%d lowercase letters, digits or hyphens (not at the ends)", ErrInvalidName, s, MaxCheckpointNameLength)
	}
	return s, nil
}

// DefaultCheckpointName is the name of a checkpoint taken without one: the UTC time.
func DefaultCheckpointName(t time.Time) string { return t.UTC().Format("20060102-150405") }

// AutoCheckpointName is the name of the checkpoint the system takes before
// an action ("restore").
func AutoCheckpointName(action string, t time.Time) string {
	return "before-" + action + "-" + DefaultCheckpointName(t)
}

// CheckpointSnapshot is the ZFS snapshot name of checkpoint name.
func CheckpointSnapshot(name string) string { return CheckpointPrefix + name }

// CheckpointOf returns the checkpoint a snapshot name stands for.
func CheckpointOf(snapshot string) (string, bool) {
	n, ok := strings.CutPrefix(snapshot, CheckpointPrefix)
	return n, ok && n != ""
}

// SnapshotInfo is a snapshot with what the checkpoint feature needs to know.
type SnapshotInfo struct {
	Name    string
	Created time.Time
	// Used is the space only this snapshot holds.
	Used uint64
	// Clones are the datasets cloned from it.
	Clones []string
	// Props are its own (not inherited) user properties.
	Props map[string]string
}

// Line is one dataset of a branch's family: the live dataset, a hidden line
// kept for its checkpoints, or the half-made clone of a restore.
type Line struct {
	Dataset string
	Live    bool
	// Pending is the clone a restore is still preparing.
	Pending   bool
	Props     map[string]string
	Snapshots []SnapshotInfo
}

// Checkpoint is what the API shows.
type Checkpoint struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Size is the space only this checkpoint holds (freed when it is deleted).
	Size uint64 `json:"size"`
	// Line is the line of history it was taken on; 0 is the branch's first.
	Line int `json:"line"`
	// Auto marks checkpoints taken by the system before a restore.
	Auto bool `json:"auto"`
	// Current marks the checkpoint the branch's data descends from.
	Current bool `json:"current"`
}

// ListCheckpoints returns the checkpoints of every line, oldest first.
func ListCheckpoints(lines []Line, head string) []Checkpoint {
	out := []Checkpoint{}
	for _, l := range lines {
		for _, s := range l.Snapshots {
			n, ok := CheckpointOf(s.Name)
			if !ok {
				continue
			}
			line, _ := strconv.Atoi(s.Props[PropLine])
			out = append(out, Checkpoint{Name: n, CreatedAt: s.Created, Size: s.Used, Line: line, Auto: s.Props[PropAuto] == "on", Current: n == head})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// FindCheckpoint returns the dataset that holds checkpoint name now.
func FindCheckpoint(lines []Line, name string) (string, SnapshotInfo, bool) {
	for _, l := range lines {
		for _, s := range l.Snapshots {
			if s.Name == CheckpointSnapshot(name) {
				return l.Dataset, s, true
			}
		}
	}
	return "", SnapshotInfo{}, false
}

// FindSnapshot returns the dataset holding the snapshot called name.
func FindSnapshot(lines []Line, name string) (string, bool) {
	for _, l := range lines {
		for _, s := range l.Snapshots {
			if s.Name == name {
				return l.Dataset, true
			}
		}
	}
	return "", false
}

// UniqueCheckpointName returns base, or base-2, base-3... when taken.
func UniqueCheckpointName(lines []Line, base string) string {
	name := base
	for i := 2; ; i++ {
		if _, _, taken := FindCheckpoint(lines, name); !taken {
			return name
		}
		name = base + "-" + strconv.Itoa(i)
	}
}

// NextLine is the number the next line gets: one above the highest in use.
func NextLine(lines []Line) int {
	most := 0
	see := func(props map[string]string) {
		if n, err := strconv.Atoi(props[PropLine]); err == nil && n > most {
			most = n
		}
	}
	for _, l := range lines {
		see(l.Props)
		for _, s := range l.Snapshots {
			see(s.Props)
		}
	}
	return most + 1
}

// ErrCheckpointsUnavailable is the answer where the node's storage has no
// lineage support (module or shared storage): checkpoints cannot work there.
var ErrCheckpointsUnavailable = fmt.Errorf("%w: checkpoints are not available on this storage (they need this node's own ZFS, not module or shared storage)", ErrConflict)

// CheckLimit refuses a manual checkpoint when the branch holds max of them
// already. The automatic checkpoints taken before a restore are not counted.
func CheckLimit(branch string, manual, max int) error {
	if manual >= max {
		return fmt.Errorf("%w: branch %s has %d checkpoints of its own (limit %d, automatic before-restore checkpoints not counted); delete one first or raise the branch.max_checkpoints setting", ErrConflict, branch, manual, max)
	}
	return nil
}

// CountManual counts the checkpoints people took (not the automatic ones).
func CountManual(list []Checkpoint) int {
	n := 0
	for _, c := range list {
		if !c.Auto {
			n++
		}
	}
	return n
}

// AutoToRotate returns the automatic checkpoints to delete so that only the
// newest keep remain, oldest first. list may be in any order.
func AutoToRotate(list []Checkpoint, keep int) []Checkpoint {
	var autos []Checkpoint
	for _, c := range list {
		if c.Auto {
			autos = append(autos, c)
		}
	}
	sort.SliceStable(autos, func(i, j int) bool {
		if !autos[i].CreatedAt.Equal(autos[j].CreatedAt) {
			return autos[i].CreatedAt.Before(autos[j].CreatedAt)
		}
		return autos[i].Name < autos[j].Name
	})
	if len(autos) <= keep {
		return nil
	}
	return autos[:len(autos)-keep]
}

// SpaceWarning says so when the space a branch uses (its data and its
// history) is more than half of what the pool has free. A checkpoint is not
// refused for it; free 0 means unknown.
func SpaceWarning(used, free uint64) string {
	if free == 0 || used <= free/2 {
		return ""
	}
	return "This branch (with its checkpoints) uses more than half of the pool's free space; checkpoints keep the data they protect, so free space may run out. Delete checkpoints you no longer need."
}

// Collection is what garbage collection removes.
type Collection struct {
	Snapshots []Snapshot
	Datasets  []string
}

// Garbage decides what to destroy now: deleted checkpoints no clone depends
// on, and hidden lines that hold no checkpoint, no reset point and nothing a
// clone depends on. Removing a line may free its origin, so callers repeat
// until nothing is left.
func Garbage(lines []Line) Collection {
	var c Collection
	for _, l := range lines {
		if l.Pending {
			continue
		}
		hold := l.Live
		for _, s := range l.Snapshots {
			_, isCheckpoint := CheckpointOf(s.Name)
			switch {
			case strings.HasPrefix(s.Name, GonePrefix) && len(s.Clones) == 0:
				c.Snapshots = append(c.Snapshots, Snapshot{Dataset: l.Dataset, Name: s.Name})
			case isCheckpoint || s.Name == ResetSnapshot || len(s.Clones) > 0:
				hold = true
			}
		}
		if !hold {
			c.Datasets = append(c.Datasets, l.Dataset)
		}
	}
	// A line destroyed with its snapshots needs no separate snapshot destroy.
	kept := c.Snapshots[:0]
	for _, s := range c.Snapshots {
		if !contains(c.Datasets, s.Dataset) {
			kept = append(kept, s)
		}
	}
	c.Snapshots = kept
	return c
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// DestroyOrder lists every dataset of the family so that clones come before
// the datasets they were cloned from. It refuses when a snapshot has a clone
// outside the family: removing a branch must never touch anything else.
func DestroyOrder(lines []Line) ([]string, error) {
	in := map[string]bool{}
	for _, l := range lines {
		in[l.Dataset] = true
	}
	dependents := map[string]int{}
	parent := map[string]string{}
	for _, l := range lines {
		for _, s := range l.Snapshots {
			for _, c := range s.Clones {
				if !in[c] {
					return nil, fmt.Errorf("%w: %s@%s has a clone, %s, outside this branch", ErrInUse, l.Dataset, s.Name, c)
				}
				if c != l.Dataset && parent[c] == "" {
					parent[c] = l.Dataset
					dependents[l.Dataset]++
				}
			}
		}
	}
	var order []string
	done := map[string]bool{}
	for len(order) < len(lines) {
		progressed := false
		for _, l := range lines {
			if done[l.Dataset] || dependents[l.Dataset] > 0 {
				continue
			}
			done[l.Dataset] = true
			order = append(order, l.Dataset)
			if p := parent[l.Dataset]; p != "" {
				dependents[p]--
			}
			progressed = true
		}
		if !progressed {
			return nil, fmt.Errorf("%w: clones of this branch's datasets form a cycle", ErrInUse)
		}
	}
	return order, nil
}

// CheckpointStorage is the storage a branch's checkpoints need beyond Storage:
// renaming, promoting and properties.
type CheckpointStorage interface {
	// Datasets lists the datasets directly under parent, by full name.
	Datasets(ctx context.Context, parent string) ([]string, error)
	// SnapshotInfos lists the snapshots directly on dataset, oldest first.
	SnapshotInfos(ctx context.Context, dataset string) ([]SnapshotInfo, error)
	// Props returns the user properties set on dataset itself (not inherited).
	Props(ctx context.Context, dataset string) (map[string]string, error)
	// SetProps sets properties on a dataset or snapshot in one step.
	SetProps(ctx context.Context, target string, props map[string]string) error
	// Unset removes user properties from a dataset.
	Unset(ctx context.Context, dataset string, keys ...string) error
	// CloneWith clones origin into dataset with the given properties.
	CloneWith(ctx context.Context, origin Snapshot, dataset string, props map[string]string) error
	// Rename moves dataset to a new name, creating parents.
	Rename(ctx context.Context, from, to string) error
	// RenameSnapshot gives a snapshot another name.
	RenameSnapshot(ctx context.Context, s Snapshot, name string) error
	// Promote reverses the clone relationship: the snapshots the clone's
	// origin dataset took up to the origin move to the clone.
	Promote(ctx context.Context, dataset string) error
	// Origin returns the snapshot dataset was cloned from, or "".
	Origin(ctx context.Context, dataset string) (string, error)
	// Mount mounts a dataset that is not mounted (a no-op when it is).
	Mount(ctx context.Context, dataset string) error
}

// CanCheckpoint reports whether checkpoints may be taken, listed changed or
// restored now: only while the branch is ready.
func (b Branch) CanCheckpoint() error {
	if b.Status != BranchReady {
		return fmt.Errorf("%w: branch %s is %s", ErrConflict, b.Name, b.Status)
	}
	return nil
}
