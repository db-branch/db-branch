package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// ErrInvalidUpdate is returned for an update that cannot be started.
var ErrInvalidUpdate = errors.New("invalid update")

// ErrRolloutRunning is returned when a rollout is started while one runs.
var ErrRolloutRunning = fmt.Errorf("%w: a rollout is already running", ErrConflict)

// Kinds of update, by how the target compares to the installed version.
const (
	UpdateMinor  = "minor"
	UpdateMajor  = "major"
	UpdateMajor0 = "major0"
)

// UpdateApplicable reports whether an update kind can be applied from the console and API.
func UpdateApplicable(kind string) bool {
	return kind == UpdateMinor || kind == UpdateMajor0
}

// CorePackage is the APT package of db-branch itself.
const CorePackage = "db-branch"

// CompareVersions orders two Debian package versions as dpkg does: -1, 0 or 1.
func CompareVersions(a, b string) int {
	ea, ua, ra := splitVersion(a)
	eb, ub, rb := splitVersion(b)
	if ea != eb {
		return sign(ea - eb)
	}
	if c := verrevcmp(ua, ub); c != 0 {
		return c
	}
	return verrevcmp(ra, rb)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// splitVersion cuts "epoch:upstream-revision".
func splitVersion(v string) (epoch int, upstream, revision string) {
	if e, rest, ok := strings.Cut(v, ":"); ok {
		for _, c := range e {
			if c < '0' || c > '9' {
				return 0, v, ""
			}
			epoch = epoch*10 + int(c-'0')
		}
		v = rest
	}
	if i := strings.LastIndex(v, "-"); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}
	return epoch, v, ""
}

func isDigit(s string, i int) bool { return i < len(s) && s[i] >= '0' && s[i] <= '9' }

// order ranks a non-digit character: "~" sorts before everything, even the end.
func order(s string, i int) int {
	if i >= len(s) || isDigit(s, i) {
		return 0
	}
	c := s[i]
	switch {
	case c == '~':
		return -1
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		return int(c)
	}
	return int(c) + 256
}

func verrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for (i < len(a) && !isDigit(a, i)) || (j < len(b) && !isDigit(b, j)) {
			if oa, ob := order(a, i), order(b, j); oa != ob {
				return sign(oa - ob)
			}
			i++
			j++
		}
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		diff := 0
		for isDigit(a, i) && isDigit(b, j) {
			if diff == 0 {
				diff = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if isDigit(a, i) {
			return 1
		}
		if isDigit(b, j) {
			return -1
		}
		if diff != 0 {
			return sign(diff)
		}
	}
	return 0
}

// Major returns the leading major version number ("1" of "1.2.3", "0" of "0.4.1").
func Major(v string) (string, bool) {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		v = rest
	}
	i := 0
	for isDigit(v, i) {
		i++
	}
	if i == 0 {
		return "", false
	}
	m := strings.TrimLeft(v[:i], "0")
	if m == "" {
		m = "0"
	}
	return m, true
}

// UpdateKind says what moving from one version to another is: UpdateMinor
// (newer, same major), UpdateMajor0 (newer, 0.x to 1.x), UpdateMajor
// (newer, another major) or "" (not newer, or a version without a major).
func UpdateKind(from, to string) string {
	if CompareVersions(to, from) <= 0 {
		return ""
	}
	ef, _, _ := splitVersion(from)
	et, _, _ := splitVersion(to)
	mf, okf := Major(from)
	mt, okt := Major(to)
	switch {
	case !okf || !okt:
		return ""
	case mf == mt:
		return UpdateMinor
	case ef == et && mf == "0" && mt == "1":
		return UpdateMajor0
	}
	return UpdateMajor
}

// PackageVersion is one version of a package the repositories offer.
type PackageVersion struct {
	Version string `json:"version"`
	// Requires is the oldest db-branch the version works with ("" if it names none).
	Requires string `json:"requires,omitempty"`
}

// UpdateCheck is what the repositories offered when an admin last asked.
type UpdateCheck struct {
	At time.Time `json:"at"`
	// By is the node that asked its APT.
	By string `json:"by"`
	// Packages are the offered versions by package, newest first.
	Packages map[string][]PackageVersion `json:"packages"`
}

// RolloutStatus is where a rollout stands.
type RolloutStatus string

const (
	RolloutRunning  RolloutStatus = "running"
	RolloutDone     RolloutStatus = "done"
	RolloutFailed   RolloutStatus = "failed"
	RolloutCanceled RolloutStatus = "canceled"
)

// NodeUpdateState is where one node stands in a rollout.
type NodeUpdateState string

const (
	NodePending  NodeUpdateState = "pending"
	NodeUpdating NodeUpdateState = "updating"
	NodeDone     NodeUpdateState = "done"
	NodeFailed   NodeUpdateState = "failed"
)

// NodeUpdate is one node's progress.
type NodeUpdate struct {
	State NodeUpdateState `json:"state"`
	// From is the version the node ran when the rollout began.
	From       string    `json:"from,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Rollout moves the cluster's packages to a newer version, one node at a time.
type Rollout struct {
	ID string `json:"id"`
	// Target is the db-branch version every node ends on.
	Target string `json:"target"`
	// Modules are the module packages to upgrade with it, by package.
	Modules map[string]string `json:"modules,omitempty"`
	// Notes are remarks from planning (e.g. a module left out).
	Notes []string `json:"notes,omitempty"`
	// Order lists the nodes in the order they update: followers, the
	// leader (at the start) last.
	Order      []string              `json:"order"`
	Nodes      map[string]NodeUpdate `json:"nodes"`
	Status     RolloutStatus         `json:"status"`
	Error      string                `json:"error,omitempty"`
	By         string                `json:"by,omitempty"`
	StartedAt  time.Time             `json:"started_at"`
	FinishedAt time.Time             `json:"finished_at,omitempty"`
}

// Updating reports whether a node is still running its update.
func (r Rollout) Updating() bool {
	for _, n := range r.Nodes {
		if n.State == NodeUpdating {
			return true
		}
	}
	return false
}

// NewRollout begins a rollout; nodes already on target count as done.
// versions are the nodes' current versions.
func NewRollout(id, target string, order []string, versions map[string]string, at time.Time) Rollout {
	r := Rollout{ID: id, Target: target, Order: slices.Clone(order), Nodes: map[string]NodeUpdate{}, Status: RolloutRunning, StartedAt: at}
	for _, n := range order {
		u := NodeUpdate{State: NodePending, From: versions[n]}
		if CompareVersions(versions[n], target) == 0 {
			u.State = NodeDone
		}
		r.Nodes[n] = u
	}
	return r
}

// Clone copies the rollout so that changes to the copy do not show in r.
func (r Rollout) Clone() Rollout {
	r.Order = slices.Clone(r.Order)
	r.Notes = slices.Clone(r.Notes)
	r.Nodes = maps.Clone(r.Nodes)
	r.Modules = maps.Clone(r.Modules)
	return r
}

// RolloutEvent reports a node's progress (Node set, State the new state)
// or ends the rollout (Node empty, Status set).
type RolloutEvent struct {
	Rollout string          `json:"rollout"`
	Node    string          `json:"node,omitempty"`
	State   NodeUpdateState `json:"state,omitempty"`
	Status  RolloutStatus   `json:"status,omitempty"`
	At      time.Time       `json:"at"`
	Error   string          `json:"error,omitempty"`
}

func rolloutConflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

// Apply records a node's progress. A node may finish after the rollout was
// canceled; a failed node fails a running rollout.
func (r *Rollout) Apply(ev RolloutEvent) error {
	if ev.Rollout != r.ID {
		return rolloutConflict("rollout %s is not the current one", ev.Rollout)
	}
	n, ok := r.Nodes[ev.Node]
	if !ok {
		return rolloutConflict("node %s is not part of the rollout", ev.Node)
	}
	switch {
	case ev.State == NodeUpdating && n.State == NodePending:
		n.StartedAt = ev.At
	case ev.State == NodeDone && n.State == NodeUpdating:
		n.FinishedAt = ev.At
	case ev.State == NodeFailed && n.State == NodeUpdating:
		n.FinishedAt, n.Error = ev.At, ev.Error
		if r.Status == RolloutRunning {
			r.Status, r.FinishedAt, r.Error = RolloutFailed, ev.At, fmt.Sprintf("node %s: %s", ev.Node, ev.Error)
		}
	default:
		return rolloutConflict("node %s cannot go from %s to %s", ev.Node, n.State, ev.State)
	}
	n.State = ev.State
	r.Nodes[ev.Node] = n
	return nil
}

// Record applies an event: a node's progress, or (without a node) the end
// of the rollout.
func (r *Rollout) Record(ev RolloutEvent) error {
	if ev.Node != "" {
		return r.Apply(ev)
	}
	if ev.Rollout != r.ID {
		return rolloutConflict("rollout %s is not the current one", ev.Rollout)
	}
	return r.End(ev.Status, ev.Error, ev.At)
}

// End finishes a running rollout as done, failed or canceled.
func (r *Rollout) End(status RolloutStatus, msg string, at time.Time) error {
	if status != RolloutDone && status != RolloutFailed && status != RolloutCanceled {
		return rolloutConflict("%q does not end a rollout", status)
	}
	if r.Status != RolloutRunning {
		return rolloutConflict("the rollout is %s", r.Status)
	}
	r.Status, r.Error, r.FinishedAt = status, msg, at
	return nil
}

// Complete reports whether every node is done.
func (r Rollout) Complete() bool {
	for _, n := range r.Order {
		if r.Nodes[n].State != NodeDone {
			return false
		}
	}
	return true
}

// Turn names the node that may update now, or "" when none may. A node
// starts only when everyone before it is done, runs the target version
// (as its heartbeat says) and is healthy.
func (r Rollout) Turn(nodes []Node, now time.Time, downAfter time.Duration) string {
	if r.Status != RolloutRunning {
		return ""
	}
	byName := map[string]Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	for _, name := range r.Order {
		switch r.Nodes[name].State {
		case NodeDone:
			n, ok := byName[name]
			if !ok || n.Down(now, downAfter) || CompareVersions(n.Version, r.Target) != 0 {
				return ""
			}
		case NodePending:
			return name
		default:
			return ""
		}
	}
	return ""
}

// UpdateStore keeps the update check and the rollout.
type UpdateStore interface {
	// GetUpdateCheck returns the zero check before any was made.
	GetUpdateCheck(ctx context.Context) (UpdateCheck, error)
	PutUpdateCheck(ctx context.Context, c UpdateCheck) error
	// GetRollout fails with ErrNotFound when there never was one.
	GetRollout(ctx context.Context) (Rollout, error)
	// StartRollout fails with ErrConflict while a rollout runs.
	StartRollout(ctx context.Context, r Rollout) error
	// RecordRollout stores a node's progress or ends the rollout (see RolloutEvent).
	RecordRollout(ctx context.Context, ev RolloutEvent) error
}
