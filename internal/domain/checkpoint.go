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

// ResetPoint names the branch's creation state where a checkpoint is asked
// for (restore by a module, OpenLine). It cannot clash with a checkpoint name.
const ResetPoint = "@reset"

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
	// MaxAutoCheckpoints is how many automatic checkpoints (before-upgrade, and
	// the before-restore ones of older versions) a branch keeps; one that adds
	// another deletes the oldest.
	MaxAutoCheckpoints = 5
)

// ZFS user properties of the checkpoint feature. They are set on the snapshot
// itself (never relied on through inheritance) except where noted.
const (
	// PropLine is the line number: on a checkpoint, the line it was taken on;
	// on a dataset, the line it holds.
	PropLine = "db-branch:line"
	// PropAuto is "on" for checkpoints the system took (before an upgrade, or
	// before a restore in older versions).
	PropAuto = "db-branch:auto"
	// PropHead (on the live dataset) names the checkpoint the current state descends from.
	PropHead = "db-branch:head"
	// PropOp, PropSource, PropNewLine and PropAutoName (on the history
	// container) record a restore in progress so that it can be finished.
	// PropNote carries a module's note for the restore (see ext.RestoreFinisher).
	PropNote = "db-branch:note"
	// PropRuntime (on a checkpoint) is the runtime the branch ran when it was
	// taken, as RuntimeProp writes it; restoring the checkpoint switches back
	// to it. On the history container it is the runtime an interrupted restore
	// ends on. PropUpgrade (history container) records an upgrade in progress.
	PropRuntime = "db-branch:runtime"
	PropUpgrade = "db-branch:upgrade"
	// PropFrom and PropParent (on a line's dataset) say which snapshot the
	// line was cloned from by a restore ("ckpt-x" or "reset") and the line
	// that snapshot was taken on.
	PropFrom     = "db-branch:from"
	PropParent   = "db-branch:parent"
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
	// Runtime is what the branch ran when it was taken ("" before this was recorded).
	Runtime string `json:"runtime,omitempty"`
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
			rt := ""
			if r, ok := ParseRuntimeProp(s.Props[PropRuntime]); ok {
				rt = r.String()
			}
			out = append(out, Checkpoint{Name: n, CreatedAt: s.Created, Size: s.Used, Line: line, Auto: s.Props[PropAuto] == "on", Current: n == head, Runtime: rt})
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
		return fmt.Errorf("%w: branch %s has %d checkpoints of its own (limit %d, automatic checkpoints not counted); delete one first or raise the branch.max_checkpoints setting", ErrConflict, branch, manual, max)
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
	// MountReadOnly mounts a dataset, or dataset@snapshot, read-only in a
	// new directory (even one a restore set aside, which is never mounted
	// otherwise) and returns the directory. Unmount undoes it.
	MountReadOnly(ctx context.Context, target string) (string, error)
	Unmount(ctx context.Context, dir string) error
}

// CanRestore reports whether a checkpoint may be restored: while the branch is
// ready, or failed by an upgrade that was kept for investigation.
func (b Branch) CanRestore() error {
	if b.Status == BranchFailed && strings.HasPrefix(b.Error, UpgradeFailedPrefix) {
		return nil
	}
	return b.CanCheckpoint()
}

// CanUpgrade reports whether an upgrade may start.
func (b Branch) CanUpgrade() error { return b.CanCheckpoint() }

// CanCheckpoint reports whether checkpoints may be taken, listed changed or
// restored now: only while the branch is ready.
func (b Branch) CanCheckpoint() error {
	if b.Status != BranchReady {
		return fmt.Errorf("%w: branch %s is %s", ErrConflict, b.Name, b.Status)
	}
	return nil
}

// LineView is a line of history as the API shows it: the structure the
// console draws as lanes.
type LineView struct {
	Line int  `json:"line"`
	Live bool `json:"live"`
	// Parent is the line the checkpoint this line started from belongs to
	// (nil for the first line, and for lines made before this was recorded).
	Parent *int `json:"parent"`
	// From is the checkpoint the line was restored from, "reset" for the
	// branch's creation state, and "" for the first line.
	From string `json:"from"`
}

// LineViews lists the lines of the family by number (the restore in
// progress is not a line yet). A line whose dataset a later restore promoted
// away is known by its checkpoints, which carry where it came from.
func LineViews(lines []Line) []LineView {
	byLine := map[int]LineView{}
	origin := func(v *LineView, props map[string]string) {
		if from := props[PropFrom]; from != "" && v.From == "" {
			v.From = from
			if n, ok := CheckpointOf(from); ok {
				v.From = n
			}
			if p, err := strconv.Atoi(props[PropParent]); err == nil {
				v.Parent = &p
			}
		}
	}
	for _, l := range lines {
		if l.Pending {
			continue
		}
		for _, s := range l.Snapshots {
			if _, ok := CheckpointOf(s.Name); !ok {
				continue
			}
			n := lineNumber(s.Props)
			v := byLine[n]
			v.Line = n
			origin(&v, s.Props)
			byLine[n] = v
		}
	}
	for _, l := range lines {
		if l.Pending {
			continue
		}
		n := lineNumber(l.Props)
		v := byLine[n]
		v.Line, v.Live = n, l.Live
		origin(&v, l.Props)
		byLine[n] = v
	}
	out := make([]LineView, 0, len(byLine))
	for _, v := range byLine {
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// NewestCheckpointOfLine finds the newest checkpoint taken on a line and the
// dataset that holds it now (the data of a line whose own dataset is gone).
func NewestCheckpointOfLine(lines []Line, line int) (string, SnapshotInfo, bool) {
	var best SnapshotInfo
	dataset, found := "", false
	for _, l := range lines {
		for _, s := range l.Snapshots {
			if _, ok := CheckpointOf(s.Name); !ok || lineNumber(s.Props) != line {
				continue
			}
			if !found || s.Created.After(best.Created) {
				dataset, best, found = l.Dataset, s, true
			}
		}
	}
	return dataset, best, found
}

func lineNumber(props map[string]string) int {
	n, _ := strconv.Atoi(props[PropLine])
	return n
}

// SnapshotLine is the line a snapshot belongs to: the one a checkpoint was
// taken on; the reset point belongs to the first line.
func SnapshotLine(lines []Line, name string) int {
	for _, l := range lines {
		for _, s := range l.Snapshots {
			if s.Name == name {
				return lineNumber(s.Props)
			}
		}
	}
	return 0
}

// Graph is a branch's history as the console draws it.
type Graph struct {
	Checkpoints []Checkpoint
	Lines       []LineView
	// Live is the number of the line the branch runs on; Head the
	// checkpoint its data descends from ("" when none).
	Live int
	Head string
}
