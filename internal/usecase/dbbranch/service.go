// Package dbbranch implements the base and branch lifecycles on one node.
package dbbranch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// ErrNoFreePort is returned when every port in the configured range is taken.
var ErrNoFreePort = errors.New("no free port")

// ErrOtherNode is returned for work on a base or branch another node holds;
// the API forwards such requests to that node.
var ErrOtherNode = errors.New("held by another node")

// ServerOverheadMB is what a database server uses beyond its own cache
// (connections, buffers, the binary itself), for placement.
const ServerOverheadMB = 256

// PortRange is the inclusive range of TCP ports handed to database servers.
type PortRange struct{ Min, Max int }

// Reporter receives the current phase of a long operation. It may be nil.
type Reporter func(phase string)

func (r Reporter) report(phase string) {
	if r != nil {
		r(phase)
	}
}

// Service runs bases and branches on this node.
type Service struct {
	Store   domain.Store
	Storage domain.Storage
	// Shared is set when Storage is seen by every node: bases are not
	// copied, branches go on any node, and moved branches are released.
	Shared domain.SharedStorage
	// Maskers write masked copies of bases, by name (from modules).
	Maskers map[string]domain.Masker
	// MaskPolicy governs mask runs (optional, from a paid module).
	MaskPolicy domain.MaskPolicy
	Servers    map[domain.Engine]domain.DBServer
	Runtimes   domain.RuntimeStore
	// Pool is the ZFS pool; datasets live under <Pool>/db-branch.
	Pool string
	// Node is this node's name; it is recorded on everything created here.
	Node string
	// Distro is the distribution this node runs ("ubuntu-26.04"; empty when
	// unknown). Base snapshots record it: PostgreSQL data depends on the
	// system's collation libraries.
	Distro string
	// Nodes lists the cluster members. Nil on a single node.
	Nodes domain.NodeStore
	// ResourceLimits are the operator's resource limits (settings). Nil: none.
	ResourceLimits func() Limits
	// PoolSpace reports the ZFS pool's size and free bytes, for the guard that
	// refuses new data when the pool is nearly full. Nil: no guard.
	PoolSpace func(ctx context.Context) (size, free uint64, err error)
	// HostMemory reports this machine's memory and ZFS ARC cap in MB, for
	// placement on a single node (a cluster's nodes report them in their
	// heartbeats). Nil: unknown, and the memory budget is not enforced.
	HostMemory func() (memoryMB, arcMaxMB uint64)
	// DownAfter is how long without a heartbeat marks a node down.
	DownAfter time.Duration
	Ports     PortRange
	// MemoryMB sizes each server's own cache. Kept small on purpose: branches
	// share base blocks through the ZFS ARC instead.
	MemoryMB int
	Clock    func() time.Time
	Password func() string
	// OpenPassword is the shared password of open branches (AccessOpen).
	OpenPassword func() string

	// Lineage adds the storage operations checkpoints need (rename, promote,
	// properties). Nil where the storage is not this node's own ZFS: checkpoints
	// are then unavailable.
	Lineage domain.CheckpointStorage
	// MaxCheckpoints is the most checkpoints a branch may hold (the
	// branch.max_checkpoints setting). Nil: domain.DefaultMaxCheckpoints.
	MaxCheckpoints func() int

	// ServerOptions asks modules for extra configuration of a branch's server
	// each time its configuration is written (create, start, restore). Nil:
	// none. An error leaves the server without the extras, except when the
	// branch is created or was created to record (Branch.Record): then the
	// server does not start.
	ServerOptions func(ctx context.Context, br domain.Branch, maintenance bool) ([]string, error)
	// RecordNew says, when a branch that did not ask to record is created,
	// whether a module records it all the same (the history module's
	// record_all setting). It is asked once, at creation, and the answer is
	// the branch's Record for good: changing the setting later never changes
	// a branch that exists. Nil: no.
	RecordNew func(ctx context.Context, br domain.Branch) bool
	// AfterRestore lets a module work on a restored branch's data while its
	// server runs in maintenance mode (no network), before anyone can connect.
	// note is what the module gave the restore. Nil: none.
	AfterRestore func(ctx context.Context, br domain.Branch, note string) error

	// Streams and Source copy base snapshots between nodes. Nil on a single node.
	Streams domain.SnapshotStreams
	Source  domain.SnapshotSource

	// Providers are the engines' runtime providers. Optional: used for the
	// optional contracts (live checkpoints: PreFreeze/PostFreeze).
	Providers domain.RuntimeProviders
	// FreezeTimeout bounds a provider's PreFreeze for a live checkpoint
	// (0: 5 seconds). PostFreeze is not bounded by the caller's context.
	FreezeTimeout time.Duration
	// Dumper dumps source databases for scheduled refreshes. Nil: none.
	Dumper domain.SourceDumper
	// Merger performs 3-way logical schema/row diff and apply for `branch merge`.
	Merger domain.LogicalMerger
	// AutoPrune says whether a refresh prunes old snapshots afterwards
	// (the base.prune_snapshots setting). Nil means yes.
	AutoPrune func() bool

	// Follower says which mask runs follow a refresh (optional, from a paid
	// module) and Launcher starts them as operations. Either nil: none.
	Follower domain.RefreshFollower
	Launcher MaskLauncher
	// Logf reports what the service skips on its own. Nil: silent.
	Logf func(format string, args ...any)

	// mounts are the directories OpenLine handed out and not yet closed.
	mounts lineMounts

	// refreshMu guards running, the bases being refreshed now.
	refreshMu sync.Mutex
	running   map[domain.Name]bool

	// limits guards MemoryMB and Ports, which settings change at runtime.
	limits sync.RWMutex
	// replicating keeps one Replicate at a time.
	replicating sync.Mutex
	// mu serializes check-and-reserve steps (names, ports, status changes).
	// Long-running work happens outside it; the status acts as the lock.
	mu sync.Mutex
	// deletingBases are the bases a DeleteBase is removing (guarded by mu).
	deletingBases map[domain.Name]bool
	// reservedPorts holds ephemeral ports (e.g. temp merge clones) guarded by mu.
	reservedPorts map[int]bool
	// mergingDryRun tracks branches currently running `branch merge --dry-run`.
	dryRunMu      sync.Mutex
	mergingDryRun map[domain.Name]bool
}

// SetLimits changes the server memory and the port range at runtime (from
// the cluster settings). New and restarted servers use them.
func (s *Service) SetLimits(memoryMB int, ports PortRange) {
	s.limits.Lock()
	defer s.limits.Unlock()
	s.MemoryMB, s.Ports = memoryMB, ports
}

func (s *Service) memory() int {
	s.limits.RLock()
	defer s.limits.RUnlock()
	return s.MemoryMB
}

func (s *Service) portRange() PortRange {
	s.limits.RLock()
	defer s.limits.RUnlock()
	return s.Ports
}

// ResetSnapshot is the per-branch snapshot taken right after init SQL.
const ResetSnapshot = domain.ResetSnapshot

// BranchUser is the account branches made before branch-named users had;
// Recover gives such branches a user named after them.
const BranchUser = "branch"

func (s *Service) baseDataset(n domain.Name) string { return s.Pool + "/db-branch/bases/" + n.String() }
func (s *Service) branchDataset(n domain.Name) string {
	return s.Pool + "/db-branch/branches/" + n.String()
}

func (s *Service) server(rt domain.RuntimeRef) (domain.DBServer, error) {
	srv, ok := s.Servers[rt.Engine]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrUnsupportedEngine, rt.Engine)
	}
	return srv, nil
}

func (s *Service) spec(ctx context.Context, id, dataset string, rt domain.RuntimeRef, port int) (domain.Server, error) {
	info, err := s.Storage.Info(ctx, dataset)
	if err != nil {
		return domain.Server{}, err
	}
	return domain.Server{ID: id, Runtime: rt, DataDir: info.Mountpoint, Port: port, MemoryMB: s.memory()}, nil
}

// branchSpec is spec for a branch's own server, with the extra configuration
// modules ask for. maintenance starts the server for work on its data only.
func (s *Service) branchSpec(ctx context.Context, br domain.Branch, dataset string, maintenance, strict bool) (domain.Server, error) {
	spec, err := s.spec(ctx, branchServerID(br.Name), dataset, br.Runtime, br.Port)
	if err != nil {
		return spec, err
	}
	spec.Maintenance, spec.LegacyAuth = maintenance, br.LegacyAuth
	if s.ServerOptions == nil {
		if br.Record {
			return spec, fmt.Errorf("%w: the history module is not installed or not loaded", ErrRecordRequired)
		}
		return spec, nil
	}
	extra, err := s.ServerOptions(ctx, br, maintenance)
	switch {
	case err == nil:
		spec.Extra = extra
	case br.Record:
		// A branch records from its creation for good: it must never run
		// unrecorded, or its log would have a gap that a later restore
		// cannot see. This holds for every start: create, recreate, reset,
		// restore and the start after a restart.
		return spec, fmt.Errorf("%w: %v", ErrRecordRequired, err)
	case strict:
		return spec, fmt.Errorf("server options: %w", err)
	case s.Logf != nil:
		s.Logf("branch %s: starting without the options a module asked for: %v", br.Name, err)
	}
	return spec, nil
}

// ErrRecordRequired: the branch was created to record its changes, which it
// does from creation to deletion, so it does not start without the module
// that records (missing, disabled or without a licence).
var ErrRecordRequired = errors.New("this branch records its changes (decided when it was created) and does not start without recording")

func baseServerID(n domain.Name) string   { return "base-" + n.String() }
func branchServerID(n domain.Name) string { return "branch-" + n.String() }

// owns reports whether this node is responsible for something held by node.
// Records without a node belong to a single node, but to nobody in a cluster
// until Adopt finds their dataset.
func (s *Service) owns(node string) bool {
	return node == s.Node || (node == "" && s.Nodes == nil)
}

func (s *Service) checkOwner(kind string, n domain.Name, node string) error {
	if s.owns(node) {
		return nil
	}
	return fmt.Errorf("%w: %s %s is on node %s", ErrOtherNode, kind, n, node)
}

// allocatePort returns the lowest port not used by any base or branch on this
// node. Caller holds s.mu.
func (s *Service) allocatePort(ctx context.Context) (int, error) {
	ports, err := s.allocatePorts(ctx, 1)
	if err != nil {
		return 0, err
	}
	return ports[0], nil
}

// allocatePorts returns count distinct free ports not used by any base or
// branch on this node. Caller holds s.mu (or synchronizes concurrently).
func (s *Service) allocatePorts(ctx context.Context, count int) ([]int, error) {
	used := map[int]bool{}
	bases, err := s.Store.ListBases(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range bases {
		if s.owns(b.Node) {
			used[b.Port] = true
		}
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range branches {
		if s.owns(b.Node) {
			used[b.Port] = true
		}
	}
	for p := range s.reservedPorts {
		used[p] = true
	}
	pr := s.portRange()
	out := make([]int, 0, count)
	for p := pr.Min; p <= pr.Max && len(out) < count; p++ {
		if !used[p] {
			used[p] = true
			out = append(out, p)
		}
	}
	if len(out) < count {
		return nil, fmt.Errorf("%w in %d-%d", ErrNoFreePort, pr.Min, pr.Max)
	}
	return out, nil
}

// reservePorts allocates count free ports and holds them in s.reservedPorts
// until the returned release function is called (safe to call multiple times).
func (s *Service) reservePorts(ctx context.Context, count int) ([]int, func(), error) {
	s.mu.Lock()
	ports, err := s.allocatePorts(ctx, count)
	if err == nil {
		if s.reservedPorts == nil {
			s.reservedPorts = map[int]bool{}
		}
		for _, p := range ports {
			s.reservedPorts[p] = true
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, func() {}, err
	}
	var once sync.Once
	return ports, func() {
		once.Do(func() {
			s.mu.Lock()
			for _, p := range ports {
				delete(s.reservedPorts, p)
			}
			s.mu.Unlock()
		})
	}, nil
}

func (s *Service) snapshotName() string { return "s" + s.Clock().UTC().Format("20060102150405") }

// DatasetProperties apply to <pool>/db-branch and are inherited by every base and branch.
// recordsize matches the 16K InnoDB page (and suits PostgreSQL better than 8K for compression).
var DatasetProperties = map[string]string{
	"compression": "lz4",
	"atime":       "off",
	"xattr":       "sa",
	"recordsize":  "16K",
}

// Init creates <pool>/db-branch. It is safe to run again.
func (s *Service) Init(ctx context.Context) error {
	root := s.Pool + "/db-branch"
	ok, err := s.Storage.Exists(ctx, root)
	if err != nil || ok {
		return err
	}
	return s.Storage.Create(ctx, root, DatasetProperties)
}

// Adopt claims bases and branches recorded without a node whose dataset is on
// this node (state from before nodes were tracked, or imported into a cluster).
func (s *Service) Adopt(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	claim := func(node string, ds string, put func() error) {
		if node != "" {
			return
		}
		ok, err := s.Storage.Exists(ctx, ds)
		if err != nil || !ok {
			errs = append(errs, err)
			return
		}
		errs = append(errs, put())
	}
	bases, err := s.Store.ListBases(ctx)
	if err != nil {
		return err
	}
	for _, b := range bases {
		claim(b.Node, s.baseDataset(b.Name), func() error { b.Node = s.Node; return s.Store.PutBase(ctx, b) })
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return err
	}
	for _, b := range branches {
		claim(b.Node, s.branchDataset(b.Name), func() error { b.Node = s.Node; return s.Store.PutBranch(ctx, b) })
	}
	return errors.Join(errs...)
}
