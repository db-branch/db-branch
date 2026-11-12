// Package update checks the package repositories for newer db-branch
// versions and rolls a minor update out to the cluster one node at a time.
//
// The rollout lives in the cluster's replicated state. Every node runs Tick:
// the leader supervises (it marks a node done once the node's heartbeat shows
// the target version, and fails a node that takes too long), and the node
// whose turn it is runs apt on itself. Nobody has to command a node: whose
// turn it is follows from the replicated state (domain.Rollout.Turn).
package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Packages is APT on this node.
type Packages interface {
	// Refresh downloads the db-branch repository's package lists.
	Refresh(ctx context.Context) error
	Installed(ctx context.Context, prefix string) (map[string]string, error)
	// Versions lists the versions of pkg the lists offer.
	Versions(ctx context.Context, pkg string) ([]domain.PackageVersion, error)
	// Upgrade moves packages (package -> version) up to those versions. The
	// package's own restart of the agent may cut it short.
	Upgrade(ctx context.Context, versions map[string]string) error
}

// Members is the Raft view of a clustered node.
type Members interface {
	// Leader is the leader's node name ("" if there is none).
	Leader() string
	// Voters is the number of voting members.
	Voters() int
	// Transfer hands leadership to another member.
	Transfer() error
}

// Defaults.
const (
	DefaultTimeout = 15 * time.Minute
	// CheckEvery is the interval of the optional automatic check.
	CheckEvery  = 24 * time.Hour
	maxVersions = 30
)

// Service checks for updates and runs rollouts.
type Service struct {
	Store    domain.UpdateStore
	Modules  domain.ModuleStore
	Packages Packages
	// Nodes returns the cluster's nodes with their heartbeats (a single
	// node lists itself).
	Nodes func(ctx context.Context) ([]domain.Node, error)
	// Self is this node's name.
	Self string
	// Cluster is nil on a single node.
	Cluster Members
	Clock   func() time.Time
	// DownAfter is how long without a heartbeat marks a node down.
	DownAfter time.Duration
	// Timeout is how long one node may take (default DefaultTimeout).
	Timeout time.Duration
	// NewID makes rollout ids (default: random).
	NewID func() string
	// Changed is called after this node changed something others should see
	// (the console is told through Raft in a cluster). Optional.
	Changed func()
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *Service) changed() {
	if s.Changed != nil {
		s.Changed()
	}
}

func (s *Service) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultTimeout
}

func (s *Service) leader() string {
	if s.Cluster == nil {
		return s.Self
	}
	return s.Cluster.Leader()
}

// Candidate is a version a package could move to.
type Candidate struct {
	Version string `json:"version"`
	// Kind is domain.UpdateMinor or domain.UpdateMajor.
	Kind string `json:"kind"`
	// Requires is the oldest db-branch the version works with.
	Requires string `json:"requires,omitempty"`
}

// PackageView is a package with the newer versions on offer.
type PackageView struct {
	Name string `json:"name"`
	// Installed is the oldest version in the cluster.
	Installed string `json:"installed"`
	// Mixed: the nodes do not all run Installed.
	Mixed     bool        `json:"mixed"`
	Available []Candidate `json:"available"`
}

// NodeView is a node's versions and its part in the rollout.
type NodeView struct {
	Name    string             `json:"name"`
	Leader  bool               `json:"leader"`
	Down    bool               `json:"down"`
	Version string             `json:"version"`
	Modules map[string]string  `json:"modules"`
	Update  *domain.NodeUpdate `json:"update,omitempty"`
}

// View is what the console and CLI show.
type View struct {
	CheckedAt time.Time     `json:"checked_at"`
	CheckedBy string        `json:"checked_by"`
	Nodes     []NodeView    `json:"nodes"`
	Packages  []PackageView `json:"packages"`
	// LatestMinor is the newest db-branch the console can update to ("" if none).
	LatestMinor string          `json:"latest_minor"`
	Rollout     *domain.Rollout `json:"rollout"`
}

func (s *Service) rollout(ctx context.Context) (*domain.Rollout, error) {
	r, err := s.Store.GetRollout(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// wanted lists the wanted modules by name.
func (s *Service) wanted(ctx context.Context) ([]domain.ModuleSpec, error) {
	specs, err := s.Modules.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	out := specs[:0:0]
	for _, m := range specs {
		if m.Wanted {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func candidates(installed string, offered []domain.PackageVersion) []Candidate {
	out := []Candidate{}
	for _, v := range offered {
		if kind := domain.UpdateKind(installed, v.Version); kind != "" {
			out = append(out, Candidate{Version: v.Version, Kind: kind, Requires: v.Requires})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return domain.CompareVersions(out[i].Version, out[j].Version) > 0 })
	return out
}

// actualModules is, per module, the lowest version the nodes run (from their
// heartbeats) and whether they differ. The recorded spec is only the version
// the cluster wants; it can lag behind what is installed.
func actualModules(nodes []domain.Node) (lowest map[string]string, mixed map[string]bool) {
	lowest, mixed = map[string]string{}, map[string]bool{}
	for _, n := range nodes {
		for _, m := range n.Modules {
			name, ver, _ := strings.Cut(m, " ")
			cur, ok := lowest[name]
			switch {
			case !ok:
				lowest[name] = ver
			case domain.CompareVersions(ver, cur) != 0:
				mixed[name] = true
				if domain.CompareVersions(ver, cur) < 0 {
					lowest[name] = ver
				}
			}
		}
	}
	return lowest, mixed
}

// withActual returns the wanted modules with the versions the nodes run.
func withActual(specs []domain.ModuleSpec, lowest map[string]string) []domain.ModuleSpec {
	out := slices.Clone(specs)
	for i := range out {
		if v, ok := lowest[out[i].Name]; ok {
			out[i].Version = v
		}
	}
	return out
}

// View shows the nodes, what the last check found and the rollout.
func (s *Service) View(ctx context.Context) (View, error) {
	check, err := s.Store.GetUpdateCheck(ctx)
	if err != nil {
		return View{}, err
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return View{}, err
	}
	specs, err := s.wanted(ctx)
	if err != nil {
		return View{}, err
	}
	r, err := s.rollout(ctx)
	if err != nil {
		return View{}, err
	}
	v := View{CheckedAt: check.At, CheckedBy: check.By, Nodes: []NodeView{}, Rollout: r}
	now, leader := s.now(), s.leader()
	core, coreMixed := "", false
	for _, n := range nodes {
		nv := NodeView{Name: n.Name, Leader: n.Name == leader && s.Cluster != nil, Down: n.Down(now, s.DownAfter), Version: n.Version, Modules: map[string]string{}}
		for _, m := range n.Modules {
			name, ver, _ := strings.Cut(m, " ")
			nv.Modules[name] = ver
		}
		if r != nil {
			if u, ok := r.Nodes[n.Name]; ok {
				nv.Update = &u
			}
		}
		v.Nodes = append(v.Nodes, nv)
		if core != "" && domain.CompareVersions(n.Version, core) != 0 {
			coreMixed = true
		}
		if core == "" || domain.CompareVersions(n.Version, core) < 0 {
			core = n.Version
		}
	}
	sort.Slice(v.Nodes, func(i, j int) bool { return v.Nodes[i].Name < v.Nodes[j].Name })
	v.Packages = append(v.Packages, PackageView{Name: domain.CorePackage, Installed: core, Mixed: coreMixed, Available: candidates(core, check.Packages[domain.CorePackage])})
	for _, c := range v.Packages[0].Available {
		if c.Kind == domain.UpdateMinor {
			v.LatestMinor = c.Version
			break
		}
	}
	lowest, mixed := actualModules(nodes)
	for _, m := range withActual(specs, lowest) {
		v.Packages = append(v.Packages, PackageView{Name: m.Package(), Installed: m.Version, Mixed: mixed[m.Name], Available: candidates(m.Version, check.Packages[m.Package()])})
	}
	return v, nil
}

// Check refreshes this node's package lists (db-branch's repository only) and
// records the versions on offer for db-branch and every db-branch-* package.
// It runs on the node that was asked: the repository is the same for the
// whole cluster, the result is replicated for every console, and a refresh on
// every node would multiply the traffic. Network access happens only here.
func (s *Service) Check(ctx context.Context) (View, error) {
	if err := s.Packages.Refresh(ctx); err != nil {
		return View{}, fmt.Errorf("refreshing the package lists: %w", err)
	}
	installed, err := s.Packages.Installed(ctx, domain.CorePackage)
	if err != nil {
		return View{}, err
	}
	specs, err := s.wanted(ctx)
	if err != nil {
		return View{}, err
	}
	names := map[string]bool{domain.CorePackage: true}
	for pkg := range installed {
		if strings.HasPrefix(pkg, domain.CorePackage+"-") {
			names[pkg] = true
		}
	}
	for _, m := range specs {
		names[m.Package()] = true
	}
	check := domain.UpdateCheck{At: s.now(), By: s.Self, Packages: map[string][]domain.PackageVersion{}}
	for pkg := range names {
		versions, err := s.Packages.Versions(ctx, pkg)
		if err != nil {
			return View{}, fmt.Errorf("versions of %s: %w", pkg, err)
		}
		if len(versions) > maxVersions {
			versions = versions[:maxVersions]
		}
		check.Packages[pkg] = versions
	}
	if err := s.Store.PutUpdateCheck(ctx, check); err != nil {
		return View{}, err
	}
	s.changed()
	return s.View(ctx)
}

// CheckDue runs a check when enabled, the last one is a day old and this node
// leads (one node asks for the whole cluster).
func (s *Service) CheckDue(ctx context.Context, enabled bool) error {
	if !enabled || s.leader() != s.Self {
		return nil
	}
	last, err := s.Store.GetUpdateCheck(ctx)
	if err != nil || s.now().Sub(last.At) < CheckEvery {
		return err
	}
	_, err = s.Check(ctx)
	return err
}

// planModules picks, for each wanted module, its newest version of the same
// major that works with the target core. A newer one that needs a newer core
// is skipped with a note.
func planModules(check domain.UpdateCheck, specs []domain.ModuleSpec, target string) (map[string]string, []string) {
	plan := map[string]string{}
	var notes []string
	for _, m := range specs {
		offered := slices.Clone(check.Packages[m.Package()])
		sort.SliceStable(offered, func(i, j int) bool { return domain.CompareVersions(offered[i].Version, offered[j].Version) > 0 })
		var skipped *domain.PackageVersion
		for i, c := range offered {
			if domain.UpdateKind(m.Version, c.Version) != domain.UpdateMinor {
				continue
			}
			if c.Requires != "" && domain.CompareVersions(c.Requires, target) > 0 {
				if skipped == nil {
					skipped = &offered[i]
				}
				continue
			}
			plan[m.Package()] = c.Version
			break
		}
		if skipped != nil {
			stays := m.Version
			if v, ok := plan[m.Package()]; ok {
				stays = v
			}
			notes = append(notes, fmt.Sprintf("%s %s needs db-branch %s or newer, so it stays at %s", m.Package(), skipped.Version, skipped.Requires, stays))
		}
	}
	return plan, notes
}

func (s *Service) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalidUpdate, fmt.Sprintf(format, args...))
}

// Apply starts a rollout to target. It refuses when one runs, when a node is
// down, and when target is not a minor update (newer, same major) for every
// node that is not on it yet.
func (s *Service) Apply(ctx context.Context, target, by string) (domain.Rollout, error) {
	check, err := s.Store.GetUpdateCheck(ctx)
	if err != nil {
		return domain.Rollout{}, err
	}
	if !slices.ContainsFunc(check.Packages[domain.CorePackage], func(v domain.PackageVersion) bool { return v.Version == target }) {
		return domain.Rollout{}, invalid("db-branch %s is not on offer; check for updates first", target)
	}
	if cur, err := s.rollout(ctx); err != nil {
		return domain.Rollout{}, err
	} else if cur != nil && cur.Status == domain.RolloutRunning {
		return domain.Rollout{}, fmt.Errorf("%w (%s)", domain.ErrRolloutRunning, cur.ID)
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return domain.Rollout{}, err
	}
	leader := s.leader()
	if leader == "" {
		return domain.Rollout{}, domain.ErrNoLeader
	}
	now := s.now()
	versions, pending := map[string]string{}, 0
	for _, n := range nodes {
		if n.Down(now, s.DownAfter) {
			return domain.Rollout{}, fmt.Errorf("%w: node %s is down or unreachable", domain.ErrConflict, n.Name)
		}
		versions[n.Name] = n.Version
		switch kind := domain.UpdateKind(n.Version, target); {
		case domain.CompareVersions(n.Version, target) == 0:
		case kind == domain.UpdateMajor:
			return domain.Rollout{}, invalid("%s is a major update for node %s (%s); a major update is done by hand, see docs/updates.md", target, n.Name, n.Version)
		case kind == "":
			return domain.Rollout{}, invalid("%s is not newer than %s on node %s", target, n.Version, n.Name)
		default:
			pending++
		}
	}
	if pending == 0 {
		return domain.Rollout{}, invalid("every node already runs %s", target)
	}
	order := make([]string, 0, len(nodes))
	for name := range versions {
		if name != leader {
			order = append(order, name)
		}
	}
	sort.Strings(order)
	if _, ok := versions[leader]; ok {
		order = append(order, leader)
	}
	specs, err := s.wanted(ctx)
	if err != nil {
		return domain.Rollout{}, err
	}
	lowest, _ := actualModules(nodes)
	specs = withActual(specs, lowest)
	r := domain.NewRollout(s.newID(), target, order, versions, now)
	r.By = by
	r.Modules, r.Notes = planModules(check, specs, target)
	if err := s.Store.StartRollout(ctx, r); err != nil {
		return domain.Rollout{}, err
	}
	s.changed()
	return r, nil
}

// Cancel stops a running rollout: nodes that have not started will not. A node
// that is updating finishes.
func (s *Service) Cancel(ctx context.Context) error {
	r, err := s.rollout(ctx)
	if err != nil {
		return err
	}
	if r == nil || r.Status != domain.RolloutRunning {
		return fmt.Errorf("%w: no rollout is running", domain.ErrConflict)
	}
	if err := s.Store.RecordRollout(ctx, domain.RolloutEvent{Rollout: r.ID, Status: domain.RolloutCanceled, At: s.now(), Error: "canceled"}); err != nil {
		return err
	}
	s.changed()
	return nil
}

// Rolled returns the module versions the latest rollout installed, by
// package: module sync leaves a node that runs one of them alone instead of
// putting the older recorded version back.
func (s *Service) Rolled(ctx context.Context) map[string]string {
	if r, err := s.rollout(ctx); err == nil && r != nil {
		return r.Modules
	}
	return nil
}

// Tick does this node's part of a rollout: supervise (leader) and update itself
// (when it is its turn). Call it every few seconds.
func (s *Service) Tick(ctx context.Context) error {
	r, err := s.rollout(ctx)
	if err != nil || r == nil || r.Status != domain.RolloutRunning {
		return err
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return err
	}
	if s.leader() == s.Self {
		if err := s.supervise(ctx, nodes); err != nil {
			return err
		}
		if r, err = s.rollout(ctx); err != nil || r == nil {
			return err
		}
	}
	return s.execute(ctx, *r, nodes)
}

func (s *Service) record(ctx context.Context, ev domain.RolloutEvent) error {
	ev.At = s.now()
	if err := s.Store.RecordRollout(ctx, ev); err != nil {
		return err
	}
	s.changed()
	return nil
}

// supervise (leader only) moves nodes from updating to done or failed, and
// ends the rollout when every node is done.
func (s *Service) supervise(ctx context.Context, nodes []domain.Node) error {
	r, err := s.rollout(ctx)
	if err != nil || r == nil {
		return err
	}
	byName := map[string]domain.Node{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	now := s.now()
	for _, name := range r.Order {
		n, ok := byName[name]
		st := r.Nodes[name]
		switch {
		case !ok:
			return s.record(ctx, domain.RolloutEvent{Rollout: r.ID, Status: domain.RolloutFailed, Error: fmt.Sprintf("node %s left the cluster", name)})
		case st.State != domain.NodeUpdating:
		case domain.CompareVersions(n.Version, r.Target) == 0 && !n.Down(now, s.DownAfter):
			if err := s.record(ctx, domain.RolloutEvent{Rollout: r.ID, Node: name, State: domain.NodeDone}); err != nil {
				return err
			}
		case now.Sub(st.StartedAt) > s.timeout():
			msg := fmt.Sprintf("node did not report version %s within %s", r.Target, s.timeout())
			return s.record(ctx, domain.RolloutEvent{Rollout: r.ID, Node: name, State: domain.NodeFailed, Error: msg})
		}
	}
	cur, err := s.rollout(ctx)
	if err != nil || cur == nil || cur.Status != domain.RolloutRunning {
		return err
	}
	if !cur.Complete() {
		return s.failIfStalled(ctx, *cur, byName)
	}
	return s.finish(ctx, *cur)
}

// failIfStalled fails a rollout whose next node has not started within the
// node limit of the previous node's completion (or of the rollout's start):
// an earlier node went down after it was done, the next node is down, or its
// agent does nothing.
func (s *Service) failIfStalled(ctx context.Context, r domain.Rollout, byName map[string]domain.Node) error {
	since, next := r.StartedAt, ""
	for _, name := range r.Order {
		st := r.Nodes[name]
		if st.State == domain.NodeDone {
			if st.FinishedAt.After(since) {
				since = st.FinishedAt
			}
			continue
		}
		if st.State == domain.NodePending {
			next = name
		}
		break
	}
	now := s.now()
	if next == "" || now.Sub(since) <= s.timeout() {
		return nil
	}
	reason := fmt.Sprintf("node %s did not start within %s", next, s.timeout())
	if n := byName[next]; n.Down(now, s.DownAfter) {
		reason = fmt.Sprintf("node %s is down; it did not start within %s", next, s.timeout())
	}
	for _, name := range r.Order {
		if name == next {
			break
		}
		if n := byName[name]; n.Down(now, s.DownAfter) || domain.CompareVersions(n.Version, r.Target) != 0 {
			reason = fmt.Sprintf("node %s is not healthy on %s, so %s did not start within %s", name, r.Target, next, s.timeout())
			break
		}
	}
	return s.record(ctx, domain.RolloutEvent{Rollout: r.ID, Status: domain.RolloutFailed, Error: reason})
}

// finish records the module versions the rollout installed as the cluster's
// wanted ones, then ends the rollout.
func (s *Service) finish(ctx context.Context, r domain.Rollout) error {
	specs, err := s.wanted(ctx)
	if err != nil {
		return err
	}
	for _, m := range specs {
		if v, ok := r.Modules[m.Package()]; ok && m.Version != v {
			m.Version = v
			if err := s.Modules.PutModule(ctx, m); err != nil {
				return err
			}
		}
	}
	return s.record(ctx, domain.RolloutEvent{Rollout: r.ID, Status: domain.RolloutDone})
}

// execute runs apt on this node when it is its turn. A leader first hands
// leadership over (it does not restart while leading).
func (s *Service) execute(ctx context.Context, r domain.Rollout, nodes []domain.Node) error {
	if r.Turn(nodes, s.now(), s.DownAfter) != s.Self {
		return nil
	}
	if s.Cluster != nil && s.Cluster.Leader() == s.Self && s.Cluster.Voters() > 1 {
		if err := s.Cluster.Transfer(); err != nil {
			return fmt.Errorf("handing over leadership before updating: %w", err)
		}
		return nil
	}
	// Recorded before apt runs: the package's own restart ends this process.
	if err := s.record(ctx, domain.RolloutEvent{Rollout: r.ID, Node: s.Self, State: domain.NodeUpdating}); err != nil {
		return err
	}
	versions := map[string]string{domain.CorePackage: r.Target}
	for pkg, v := range r.Modules {
		versions[pkg] = v
	}
	if err := s.Packages.Upgrade(ctx, versions); err != nil && ctx.Err() == nil {
		return s.record(ctx, domain.RolloutEvent{Rollout: r.ID, Node: s.Self, State: domain.NodeFailed, Error: err.Error()})
	}
	return nil
}
