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
	// Nodes lists the cluster members. Nil on a single node.
	Nodes domain.NodeStore
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

	// Streams and Source copy base snapshots between nodes. Nil on a single node.
	Streams domain.SnapshotStreams
	Source  domain.SnapshotSource

	// Dumper dumps source databases for scheduled refreshes. Nil: none.
	Dumper domain.SourceDumper
	// AutoPrune says whether a refresh prunes old snapshots afterwards
	// (the base.prune_snapshots setting). Nil means yes.
	AutoPrune func() bool

	// Follower says which mask runs follow a refresh (optional, from a paid
	// module) and Launcher starts them as operations. Either nil: none.
	Follower domain.RefreshFollower
	Launcher MaskLauncher
	// Logf reports what the service skips on its own. Nil: silent.
	Logf func(format string, args ...any)

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
	used := map[int]bool{}
	bases, err := s.Store.ListBases(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range bases {
		if s.owns(b.Node) {
			used[b.Port] = true
		}
	}
	branches, err := s.Store.ListBranches(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range branches {
		if s.owns(b.Node) {
			used[b.Port] = true
		}
	}
	ports := s.portRange()
	for p := ports.Min; p <= ports.Max; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("%w in %d-%d", ErrNoFreePort, ports.Min, ports.Max)
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
