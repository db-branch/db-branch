package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

var (
	// ErrNotFound is returned when a base or branch does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists is returned when creating a base or branch whose name is taken.
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict is returned when an operation does not fit the current status.
	ErrConflict = errors.New("conflict")
	// ErrInvalidTTL is returned for a non-positive TTL extension.
	ErrInvalidTTL = errors.New("invalid ttl")
	// ErrInUse is returned when deleting something others still depend on.
	ErrInUse = errors.New("in use")
	// ErrUnsupportedEngine is returned when no DBServer is available for an engine.
	ErrUnsupportedEngine = errors.New("unsupported engine")
	// ErrUncleanShutdown is returned when a clean stop cannot be confirmed; no snapshot may be taken.
	ErrUncleanShutdown = errors.New("unclean shutdown")
)

// BaseStatus is the lifecycle state of a base.
type BaseStatus string

const (
	BaseProvisioning BaseStatus = "provisioning"
	BaseReady        BaseStatus = "ready"
	// BaseMaintenance: the base server is stopped while a snapshot is taken.
	BaseMaintenance BaseStatus = "maintenance"
	BaseFailed      BaseStatus = "failed"
)

// BaseSnapshot is a consistent snapshot of a base, taken after a clean shutdown.
type BaseSnapshot struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Public lets anyone, without logging in, branch from this snapshot.
	Public bool `json:"public,omitempty"`
	// Mask is set when the snapshot holds a masked copy of another base.
	Mask *MaskInfo `json:"mask,omitempty"`
	// Distro is the distribution key of the node the snapshot was taken on
	// (empty for snapshots from before it was recorded). PostgreSQL data
	// depends on that system's collation libraries; see CollationDistro.
	Distro string `json:"distro,omitempty"`
}

// Base is a dataset branches are cloned from.
type Base struct {
	Name    Name       `json:"name"`
	Runtime RuntimeRef `json:"runtime"`
	Status  BaseStatus `json:"status"`
	Port    int        `json:"port"`
	// Node is the node that holds the base's origin dataset and runs its server.
	Node      string         `json:"node,omitempty"`
	Snapshots []BaseSnapshot `json:"snapshots"`
	// Replicas lists, by snapshot name, the other nodes that hold a copy.
	// Stores keep it across PutBase; only AddReplica changes it.
	Replicas map[string][]string `json:"replicas,omitempty"`
	// ReplicaNodes limits the other nodes that keep a copy: nil means every
	// node (the default); an empty list means the base's node only. Stores
	// keep it across PutBase; only SetReplicaNodes changes it.
	ReplicaNodes *[]string `json:"replica_nodes,omitempty"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// Description says what the base holds, for people choosing one.
	Description string `json:"description,omitempty"`
	// Anonymous limits the branches callers without credentials may make from
	// this base (zero: inherit the cluster-wide settings). Stores keep it
	// across PutBase; only SetBaseAnonymous changes it.
	Anonymous AnonymousLimits `json:"anonymous,omitzero"`
}

// LatestSnapshot returns the newest snapshot.
func (b Base) LatestSnapshot() (BaseSnapshot, bool) {
	if len(b.Snapshots) == 0 {
		return BaseSnapshot{}, false
	}
	return b.Snapshots[len(b.Snapshots)-1], true
}

// Holders returns the nodes that hold snapshot: the base's own node first,
// then the replicas.
func (b Base) Holders(snapshot string) []string {
	return append([]string{b.Node}, b.Replicas[snapshot]...)
}

// HasReplica reports whether node holds a copy of snapshot.
func (b Base) HasReplica(snapshot, node string) bool {
	for _, n := range b.Replicas[snapshot] {
		if n == node {
			return true
		}
	}
	return false
}

// AddReplica records that node holds snapshot; it is idempotent.
func (b *Base) AddReplica(snapshot, node string) {
	if b.HasReplica(snapshot, node) {
		return
	}
	if b.Replicas == nil {
		b.Replicas = map[string][]string{}
	}
	b.Replicas[snapshot] = append(b.Replicas[snapshot], node)
}

// Wants reports whether node should keep a copy of the base: the base's own
// node always does, the others when ReplicaNodes is unset or lists them.
func (b Base) Wants(node string) bool {
	if b.ReplicaNodes == nil || node == b.Node {
		return true
	}
	for _, n := range *b.ReplicaNodes {
		if n == node {
			return true
		}
	}
	return false
}

// Placeable returns the nodes a branch of snapshot may be cloned on: those
// that hold it and still want it (a node being drained is left out).
func (b Base) Placeable(snapshot string) []string {
	var out []string
	for _, n := range b.Holders(snapshot) {
		if b.Wants(n) {
			out = append(out, n)
		}
	}
	return out
}

// HoldsAny reports whether node holds a copy of any snapshot other than as
// the base's own node.
func (b Base) HoldsAny(node string) bool {
	for _, nodes := range b.Replicas {
		for _, n := range nodes {
			if n == node {
				return true
			}
		}
	}
	return false
}

// Draining reports whether node still holds a copy it no longer wants.
func (b Base) Draining(node string) bool { return !b.Wants(node) && b.HoldsAny(node) }

// RemoveReplica forgets every copy node held.
func (b *Base) RemoveReplica(node string) {
	out := make(map[string][]string, len(b.Replicas))
	for snap, nodes := range b.Replicas {
		for _, n := range nodes {
			if n != node {
				out[snap] = append(out[snap], n)
			}
		}
	}
	b.Replicas = out
}

// Snapshot returns the snapshot called name.
func (b Base) Snapshot(name string) (BaseSnapshot, bool) {
	for _, s := range b.Snapshots {
		if s.Name == name {
			return s, true
		}
	}
	return BaseSnapshot{}, false
}

// Who may connect to a branch.
const (
	AccessPrivate = "private"
	AccessOpen    = "open"
)

// ValidAccess reports whether a is an access mode ("" means private).
func ValidAccess(a string) bool { return a == "" || a == AccessPrivate || a == AccessOpen }

// Open reports whether anyone may connect to the branch.
func (b Branch) Open() bool { return b.Access == AccessOpen }

// SetPublic marks snapshot as public or not; false if there is none.
func (b *Base) SetPublic(snapshot string, public bool) bool {
	for i := range b.Snapshots {
		if b.Snapshots[i].Name == snapshot {
			b.Snapshots[i].Public = public
			return true
		}
	}
	return false
}

// BranchStatus is the lifecycle state of a branch.
type BranchStatus string

const (
	BranchProvisioning BranchStatus = "provisioning"
	BranchReady        BranchStatus = "ready"
	BranchResetting    BranchStatus = "resetting"
	// BranchRestoring: a checkpoint is being restored (see checkpoint.go).
	BranchRestoring BranchStatus = "restoring"
	BranchDeleting  BranchStatus = "deleting"
	BranchFailed    BranchStatus = "failed"
	// BranchLost: the node holding the branch is gone.
	BranchLost BranchStatus = "lost"
	// BranchUpgrading: the branch is moving to another runtime (see upgrade.go).
	BranchUpgrading BranchStatus = "upgrading"
	// BranchPausing, BranchPaused, BranchResuming: the branch's server is being
	// stopped, is stopped with the data kept (nothing runs, no memory is held),
	// or is being started again (see pause.go).
	BranchPausing  BranchStatus = "pausing"
	BranchPaused   BranchStatus = "paused"
	BranchResuming BranchStatus = "resuming"
)

// Branch is a writable clone of a base snapshot served by its own database server.
type Branch struct {
	Name     Name         `json:"name"`
	Base     Name         `json:"base"`
	Snapshot string       `json:"snapshot"`
	Runtime  RuntimeRef   `json:"runtime"`
	Status   BranchStatus `json:"status"`
	Port     int          `json:"port"`
	// Node is the node that holds the branch's dataset and runs its server.
	Node     string `json:"node,omitempty"`
	User     string `json:"user"`
	Password string `json:"password"`
	// Owner is who manages the branch ("" for branches made before owners).
	Owner string `json:"owner,omitempty"`
	// Access says who may connect: AccessPrivate (a random password for the
	// owner) or AccessOpen (the cluster's shared password, for anyone).
	Access    string     `json:"access,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// InitSQL is kept so the branch can be recreated on another node.
	InitSQL string `json:"init_sql,omitempty"`
	// Record asks for the branch's changes to be recorded (the history
	// module honours it; without the module it has no effect).
	Record bool `json:"record,omitempty"`
	// LegacyAuth keeps MySQL 8.4's mysql_native_password plugin loaded for a
	// branch upgraded with accounts that use it.
	LegacyAuth bool       `json:"legacy_auth,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
}

// Expired reports whether the branch's TTL has run out at now.
func (b Branch) Expired(now time.Time) bool {
	return b.ExpiresAt != nil && !now.Before(*b.ExpiresAt)
}

// SetTTL sets the expiry to now+ttl, or removes it when ttl is zero.
func (b *Branch) SetTTL(now time.Time, ttl time.Duration) {
	if ttl <= 0 {
		b.ExpiresAt = nil
		return
	}
	t := now.Add(ttl)
	b.ExpiresAt = &t
}

// Extend moves the expiry to now+ttl.
func (b *Branch) Extend(now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: %s must be positive", ErrInvalidTTL, ttl)
	}
	b.SetTTL(now, ttl)
	return nil
}

// CanReset reports whether a reset may start.
func (b Branch) CanReset() error {
	if b.Status != BranchReady {
		return fmt.Errorf("%w: branch %s is %s", ErrConflict, b.Name, b.Status)
	}
	return nil
}

// CanDelete reports whether a delete may start.
func (b Branch) CanDelete() error {
	switch b.Status {
	case BranchReady, BranchPaused, BranchFailed, BranchLost:
		return nil
	}
	return fmt.Errorf("%w: branch %s is %s", ErrConflict, b.Name, b.Status)
}

// CanPause reports whether the branch's server may be stopped (keeping the data).
func (b Branch) CanPause() error {
	if b.Status != BranchReady {
		return fmt.Errorf("%w: branch %s is %s; only a ready branch can be paused", ErrConflict, b.Name, b.Status)
	}
	return nil
}

// CanResume reports whether the branch's server may be started again.
func (b Branch) CanResume() error {
	if b.Status != BranchPaused {
		return fmt.Errorf("%w: branch %s is %s; only a paused branch can be resumed", ErrConflict, b.Name, b.Status)
	}
	return nil
}

// CanRecreate reports whether the branch may be built again elsewhere: only
// when the node holding it is gone.
func (b Branch) CanRecreate() error {
	if b.Status != BranchLost {
		return fmt.Errorf("%w: branch %s is %s; only a lost branch can be recreated", ErrConflict, b.Name, b.Status)
	}
	return nil
}

// DSN is the connection URL for clients reaching the branch at host.
func (b Branch) DSN(host string) string {
	u := url.URL{
		Scheme: string(b.Runtime.Engine),
		User:   url.UserPassword(b.User, b.Password),
		Host:   net.JoinHostPort(host, strconv.Itoa(b.Port)),
		Path:   "/",
	}
	if b.Runtime.Engine == Postgres {
		u.Path = "/postgres"
	}
	return u.String()
}

// Store persists bases and branches. Single node: a local file; cluster: Raft.
type Store interface {
	GetBase(ctx context.Context, name Name) (Base, error)
	ListBases(ctx context.Context) ([]Base, error)
	PutBase(ctx context.Context, b Base) error
	// AddReplica records that node holds a copy of the base's snapshot.
	AddReplica(ctx context.Context, base Name, snapshot, node string) error
	// RemoveReplica forgets the copies node held of the base.
	RemoveReplica(ctx context.Context, base Name, node string) error
	// SetBaseReplicaNodes sets Base.ReplicaNodes (nil: every node).
	SetBaseReplicaNodes(ctx context.Context, base Name, nodes *[]string) error
	// SetBaseAnonymous sets Base.Anonymous (the zero value: inherit).
	SetBaseAnonymous(ctx context.Context, base Name, l AnonymousLimits) error
	// InsertBase stores a new base, or fails with ErrAlreadyExists.
	InsertBase(ctx context.Context, b Base) error
	DeleteBase(ctx context.Context, name Name) error

	GetBranch(ctx context.Context, name Name) (Branch, error)
	ListBranches(ctx context.Context) ([]Branch, error)
	PutBranch(ctx context.Context, b Branch) error
	// InsertBranch stores a new branch, or fails with ErrAlreadyExists.
	InsertBranch(ctx context.Context, b Branch) error
	DeleteBranch(ctx context.Context, name Name) error
}

// StopMode chooses how a database server is stopped.
type StopMode int

const (
	// StopClean flushes everything to disk and verifies the shutdown was clean,
	// so a snapshot taken afterwards starts without crash recovery.
	StopClean StopMode = iota
	// StopImmediate kills the server; used when the data is about to be discarded.
	StopImmediate
)

// Server describes one database server instance (a base's or a branch's).
type Server struct {
	// ID is unique per node, e.g. "base-prod" or "branch-feature-x".
	ID      string
	Runtime RuntimeRef
	DataDir string
	Port    int
	// MemoryMB sizes the engine's own cache (InnoDB buffer pool / shared_buffers).
	MemoryMB int
	// Extra are further lines for the server's configuration file, from a
	// module (ext.ServerOptioner). MySQL: the [mysqld] section of my.cnf.
	// PostgreSQL ignores them for now.
	Extra []string
	// LegacyAuth loads MySQL 8.4's mysql_native_password plugin (ignored elsewhere).
	LegacyAuth bool
	// Maintenance starts the server for work on its data only (a module
	// replaying changes after a restore): it takes no network connections.
	Maintenance bool
}

// DBServer runs database server instances for one engine.
// StatsUpdater is a DBServer that can refresh the optimizer statistics of
// every database (and, where the engine has them, mark rows as visible to
// all), so a base snapshot carries them. Without it, a snapshot taken right
// after a load carries the statistics of the empty tables (InnoDB computes
// them in the background, PostgreSQL's autovacuum comes later): every
// branch then plans range queries as full scans, and on PostgreSQL the first
// read of each page in every branch rewrites it to set hint bits.
type StatsUpdater interface {
	UpdateStats(ctx context.Context, s Server) error
}

type DBServer interface {
	// Initialize creates an empty data directory with a passwordless local admin.
	Initialize(ctx context.Context, s Server) error
	// PrepareClone removes per-instance identity (server UUID, pid files) from a cloned data directory.
	PrepareClone(ctx context.Context, s Server) error
	Start(ctx context.Context, s Server) error
	Stop(ctx context.Context, s Server, mode StopMode) error
	Running(ctx context.Context, s Server) (bool, error)
	// Exec runs SQL as the local admin. Only SQL db-branch wrote itself goes here.
	Exec(ctx context.Context, s Server, sql string) error
	// ExecAs runs SQL as the branch login, never as the admin: init SQL comes
	// from whoever asked for the branch, who may not be an admin. A client
	// command inside it (psql's \!, mysql's system) must not run on the node.
	ExecAs(ctx context.Context, s Server, user, password, sql string) error
	// CreateUser makes (or keeps, and puts right) the login clients use. The
	// login may read and change the data and the objects in it, and nothing
	// else: no superuser, no server files, no programs. The name is a branch
	// name and the password letters and digits only.
	CreateUser(ctx context.Context, s Server, user, password string) error
	// Load imports a SQL dump (optionally gzip-compressed) as the local admin.
	Load(ctx context.Context, s Server, dumpPath string) error
	// Remove deletes the instance's configuration and logs (not its data).
	Remove(ctx context.Context, s Server) error
	// Admin is how local tools reach the running server as its admin.
	Admin(s Server) AdminAccess
}
