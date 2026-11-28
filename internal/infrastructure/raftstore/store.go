package raftstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/membership"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

// Forwarder sends a command to the leader's node-to-node API.
type Forwarder interface {
	// Forward returns the Raft log index the leader committed c at.
	Forward(ctx context.Context, apiAddr string, c Command) (uint64, error)
}

// ModuleDataOpRequest describes an atomic or linearizable module-data operation.
type ModuleDataOpRequest struct {
	Op     string `json:"op"`
	Module string `json:"module"`
	Key    string `json:"key"`
	Value  []byte `json:"value,omitempty"`
}

// ModuleDataOpResponse is the result of a ModuleDataOpRequest.
type ModuleDataOpResponse struct {
	Value []byte `json:"value,omitempty"`
	OK    bool   `json:"ok"`
}

// ModuleDataForwarder forwards atomic module-data operations to the leader.
type ModuleDataForwarder interface {
	ModuleDataOp(ctx context.Context, apiAddr string, req ModuleDataOpRequest) (ModuleDataOpResponse, error)
}

// Options configures a Raft-backed store.
type Options struct {
	NodeID string
	// APIAddr is this node's node-to-node API address (host:7070).
	APIAddr   string
	Dir       string
	Transport raft.Transport
	Forwarder Forwarder
	// InMemory keeps the log and snapshots in memory (tests).
	InMemory bool
	// Fast shortens timeouts (tests).
	Fast bool
	Log  io.Writer
	// OnApply sees every applied command on this node (see FSM.OnApply).
	OnApply func(Command)
}

// Store implements domain.Store and domain.NodeStore on top of Raft. Reads
// come from the local FSM (possibly slightly stale on followers); writes go
// through the leader.
type Store struct {
	raft *raft.Raft
	fsm  *FSM
	opts Options
	// bolt is closed after Raft shuts down so the file lock is released.
	bolt  io.Closer
	modMu sync.Mutex
}

var (
	_ domain.Store                 = (*Store)(nil)
	_ domain.BranchOwnerMigrator   = (*Store)(nil)
	_ domain.NodeStore             = (*Store)(nil)
	_ domain.ModuleStore           = (*Store)(nil)
	_ domain.AtomicModuleDataStore = (*Store)(nil)
	_ domain.UserStore           = (*Store)(nil)
	_ operation.Store            = (*Store)(nil)
	_ domain.UpdateStore         = (*Store)(nil)
	_ membership.Members         = (*Store)(nil)
)

// Open starts the local Raft node. Call Bootstrap on the first node only.
func Open(o Options) (*Store, error) {
	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(o.NodeID)
	logOut := o.Log
	if logOut == nil {
		logOut = io.Discard
	}
	cfg.Logger = hclog.New(&hclog.LoggerOptions{Name: "raft", Output: logOut, Level: hclog.Warn})
	if o.Fast {
		cfg.HeartbeatTimeout = 50 * time.Millisecond
		cfg.ElectionTimeout = 50 * time.Millisecond
		cfg.LeaderLeaseTimeout = 50 * time.Millisecond
		cfg.CommitTimeout = 5 * time.Millisecond
	}
	var (
		logs   raft.LogStore
		stable raft.StableStore
		snaps  raft.SnapshotStore
		closer io.Closer = io.NopCloser(nil)
	)
	if o.InMemory {
		mem := raft.NewInmemStore()
		logs, stable, snaps = mem, mem, raft.NewInmemSnapshotStore()
	} else {
		if err := os.MkdirAll(o.Dir, 0o700); err != nil {
			return nil, err
		}
		bolt, err := raftboltdb.NewBoltStore(filepath.Join(o.Dir, "raft.db"))
		if err != nil {
			return nil, err
		}
		fileSnaps, err := raft.NewFileSnapshotStore(o.Dir, 2, logOut)
		if err != nil {
			return nil, err
		}
		logs, stable, snaps, closer = bolt, bolt, fileSnaps, bolt
	}
	fsm := NewFSM()
	fsm.OnApply = o.OnApply
	r, err := raft.NewRaft(cfg, fsm, logs, stable, snaps, o.Transport)
	if err != nil {
		_ = closer.Close()
		return nil, err
	}
	return &Store{raft: r, fsm: fsm, opts: o, bolt: closer}, nil
}

// HasState reports whether dir already holds Raft data (the node joined before).
func HasState(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "raft.db"))
	return err == nil
}

// Bootstrap forms a new cluster with the given voters.
func (s *Store) Bootstrap(servers []raft.Server) error {
	return s.raft.BootstrapCluster(raft.Configuration{Servers: servers}).Error()
}

// AddVoter adds a node as a voter (leader only).
func (s *Store) AddVoter(id, addr string) error {
	return s.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), 0, 10*time.Second).Error()
}

// AddNonvoter adds a node that replicates the log but does not vote (leader
// only). The membership loop promotes it when the cluster needs a voter.
// Re-adding the same node at the same address is idempotent (and keeps its
// current suffrage); a conflicting address or ID is rejected.
func (s *Store) AddNonvoter(id, addr string) error {
	if id == "" || addr == "" {
		return fmt.Errorf("node id and raft address are required")
	}
	if !s.IsLeader() {
		return domain.ErrNoLeader
	}
	f := s.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return err
	}
	for _, srv := range f.Configuration().Servers {
		if string(srv.ID) == id {
			if string(srv.Address) == addr {
				return nil
			}
			return fmt.Errorf("node %s is already in the cluster at %s: %w", id, srv.Address, domain.ErrAlreadyExists)
		}
		if string(srv.Address) == addr {
			return fmt.Errorf("raft address %s is already used by node %s: %w", addr, srv.ID, domain.ErrAlreadyExists)
		}
	}
	err := s.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(addr), f.Index(), 10*time.Second).Error()
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
		return fmt.Errorf("%w: %v", domain.ErrNoLeader, err)
	}
	return err
}

// Servers lists the members of the Raft configuration.
func (s *Store) Servers(context.Context) ([]membership.Server, error) {
	f := s.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, err
	}
	var out []membership.Server
	for _, srv := range f.Configuration().Servers {
		out = append(out, membership.Server{ID: string(srv.ID), Voter: srv.Suffrage == raft.Voter})
	}
	return out, nil
}

// Promote makes a non-voter vote (leader only); it returns once committed.
func (s *Store) Promote(_ context.Context, id string) error {
	f := s.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return err
	}
	for _, srv := range f.Configuration().Servers {
		if string(srv.ID) == id {
			return s.raft.AddVoter(srv.ID, srv.Address, 0, 10*time.Second).Error()
		}
	}
	return fmt.Errorf("server %s: %w", id, domain.ErrNotFound)
}

// Demote makes a voter a non-voter (leader only); it returns once committed.
func (s *Store) Demote(_ context.Context, id string) error {
	return s.raft.DemoteVoter(raft.ServerID(id), 0, 10*time.Second).Error()
}

// RemoveServer takes a member out of the Raft configuration (leader only); it
// returns once committed. A node that is not a member is not an error, so a
// removal can be run again.
func (s *Store) RemoveServer(_ context.Context, id string) error {
	if !s.IsLeader() {
		return domain.ErrNoLeader
	}
	f := s.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return err
	}
	if !slices.ContainsFunc(f.Configuration().Servers, func(srv raft.Server) bool { return string(srv.ID) == id }) {
		return nil
	}
	err := s.raft.RemoveServer(raft.ServerID(id), 0, 10*time.Second).Error()
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
		return fmt.Errorf("%w: %v", domain.ErrNoLeader, err)
	}
	return err
}

// IsLeader reports whether this node currently leads.
func (s *Store) IsLeader() bool { return s.raft.State() == raft.Leader }

// Leader returns the current leader's ID and Raft address ("" if unknown).
func (s *Store) Leader() (id, raftAddr string) {
	addr, sid := s.raft.LeaderWithID()
	return string(sid), string(addr)
}

// APIAddress is this node's node-to-node API address.
func (s *Store) APIAddress() string { return s.opts.APIAddr }

// Close stops the local Raft node and releases its files.
func (s *Store) Close() error {
	err := s.raft.Shutdown().Error()
	return errors.Join(err, s.bolt.Close())
}

// Apply commits c, forwarding it to the leader when this node follows. It
// returns once this node has applied c too, so a read right after a write
// sees it on any node.
func (s *Store) Apply(ctx context.Context, c Command) error {
	idx, err := s.ApplyAt(ctx, c)
	if err != nil || s.IsLeader() {
		return err
	}
	return s.waitApplied(ctx, idx)
}

// ApplyAt commits c and returns its log index, without waiting for this
// follower to catch up.
func (s *Store) ApplyAt(ctx context.Context, c Command) (uint64, error) {
	if need, have := c.Needs(), s.ClusterProtocol(); need > have {
		return 0, fmt.Errorf("%w: %s needs cluster protocol %d, the cluster is at %d", domain.ErrClusterUpgrading, c.Op, need, have)
	}
	if s.IsLeader() {
		if c.Op == "put_module_data" || c.Op == "delete_module_data" {
			s.modMu.Lock()
			defer s.modMu.Unlock()
		}
		return s.applyLeader(c)
	}
	id, _ := s.Leader()
	if id == "" || s.opts.Forwarder == nil {
		return 0, domain.ErrNoLeader
	}
	node, ok := s.fsm.State().Nodes[id]
	if !ok || node.Address == "" {
		return 0, fmt.Errorf("%w: leader %s has not registered its API address yet", domain.ErrNoLeader, id)
	}
	return s.opts.Forwarder.Forward(ctx, node.Address, c)
}

func (s *Store) applyLeader(c Command) (uint64, error) {
	data, err := c.Encode()
	if err != nil {
		return 0, err
	}
	f := s.raft.Apply(data, 10*time.Second)
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return 0, fmt.Errorf("%w: %v", domain.ErrNoLeader, err)
		}
		return 0, err
	}
	if err, ok := f.Response().(error); ok {
		return 0, err
	}
	return f.Index(), nil
}

// waitApplied waits (a bounded time) until this node has applied idx. The
// write is committed either way; giving up only risks a stale read.
func (s *Store) waitApplied(ctx context.Context, idx uint64) error {
	deadline := time.Now().Add(5 * time.Second)
	for s.fsm.Index() < idx && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	return nil
}

// ClusterProtocol is the protocol every registered member speaks, capped at
// this binary's own.
func (s *Store) ClusterProtocol() int {
	nodes := make([]domain.Node, 0)
	for _, n := range s.fsm.State().Nodes {
		nodes = append(nodes, n)
	}
	return min(domain.ClusterProtocol(nodes, Protocol), Protocol)
}

// State returns the replicated state as seen by this node.
func (s *Store) State() State { return s.fsm.State() }

func notFound(kind, n string) error { return fmt.Errorf("%s %s: %w", kind, n, domain.ErrNotFound) }

func (s *Store) GetBase(_ context.Context, n domain.Name) (domain.Base, error) {
	b, ok := s.fsm.State().Bases[n.String()]
	if !ok {
		return b, notFound("base", n.String())
	}
	return b, nil
}

func (s *Store) ListBases(context.Context) ([]domain.Base, error) {
	var out []domain.Base
	for _, b := range s.fsm.State().Bases {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name.String() < out[j].Name.String() })
	return out, nil
}

func (s *Store) PutBase(ctx context.Context, b domain.Base) error { return s.Apply(ctx, PutBase(b)) }

func (s *Store) DeleteBase(ctx context.Context, n domain.Name) error {
	if _, err := s.GetBase(ctx, n); err != nil {
		return err
	}
	return s.Apply(ctx, DeleteBase(n))
}

func (s *Store) GetBranch(_ context.Context, n domain.Name) (domain.Branch, error) {
	b, ok := s.fsm.State().Branches[n.String()]
	if !ok {
		return b, notFound("branch", n.String())
	}
	return b, nil
}

func (s *Store) ListBranches(context.Context) ([]domain.Branch, error) {
	var out []domain.Branch
	for _, b := range s.fsm.State().Branches {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name.String() < out[j].Name.String() })
	return out, nil
}

// PutBranch replaces a branch. Whether it records its changes is decided
// when it is created and never changes: the stored value wins.
func (s *Store) PutBranch(ctx context.Context, b domain.Branch) error {
	if old, ok := s.fsm.State().Branches[b.Name.String()]; ok {
		b.Record = old.Record
		if b.OwnerName == "" && old.OwnerName != "" && b.Owner == old.Owner {
			b.OwnerName = old.OwnerName
		}
	}
	return s.Apply(ctx, PutBranch(b))
}

// MigrateBranchOwner atomically updates a legacy branch's Owner and OwnerName
// once every cluster node speaks ProtocolBranchOwnerID.
func (s *Store) MigrateBranchOwner(ctx context.Context, n domain.Name, owner, ownerName string) error {
	return s.Apply(ctx, MigrateBranchOwner(n, owner, ownerName))
}

func (s *Store) DeleteBranch(ctx context.Context, n domain.Name) error {
	if _, err := s.GetBranch(ctx, n); err != nil {
		return err
	}
	return s.Apply(ctx, DeleteBranch(n))
}

func (s *Store) PutNode(ctx context.Context, n domain.Node) error { return s.Apply(ctx, PutNode(n)) }

func (s *Store) ListNodes(context.Context) ([]domain.Node, error) {
	var out []domain.Node
	for _, n := range s.fsm.State().Nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) DeleteNode(ctx context.Context, name string) error {
	return s.Apply(ctx, DeleteNode(name))
}

// SetJoinSecretHash records the hash of the current join secret.
func (s *Store) SetJoinSecretHash(ctx context.Context, hash string) error {
	return s.Apply(ctx, SetJoinSecret(hash))
}

// SetJoinToken records the hash of a join secret together with its expiry.
func (s *Store) SetJoinToken(ctx context.Context, hash string, expiresAt time.Time) error {
	return s.Apply(ctx, SetJoinToken(hash, expiresAt))
}

// ConsumeJoinSecret validates hash at now and clears it atomically in the log.
func (s *Store) ConsumeJoinSecret(ctx context.Context, hash string, now time.Time) error {
	return s.Apply(ctx, ConsumeJoinSecret(hash, now))
}

// JoinSecretHash returns the recorded join secret hash.
func (s *Store) JoinSecretHash() string { return s.fsm.State().JoinSecretHash }

func (s *Store) AddReplica(ctx context.Context, base domain.Name, snapshot, node string) error {
	return s.Apply(ctx, AddReplica(base, snapshot, node))
}

func (s *Store) RemoveReplica(ctx context.Context, base domain.Name, node string) error {
	return s.Apply(ctx, RemoveReplica(base, node))
}

func (s *Store) SetBaseReplicaNodes(ctx context.Context, base domain.Name, nodes *[]string) error {
	return s.Apply(ctx, SetBaseReplicaNodes(base, nodes))
}

func (s *Store) SetBaseAnonymous(ctx context.Context, base domain.Name, l domain.AnonymousLimits) error {
	return s.Apply(ctx, SetBaseAnonymous(base, l))
}

func (s *Store) InsertBase(ctx context.Context, b domain.Base) error {
	return s.Apply(ctx, InsertBase(b))
}

func (s *Store) InsertBranch(ctx context.Context, b domain.Branch) error {
	return s.Apply(ctx, InsertBranch(b))
}

func (s *Store) PutOperation(ctx context.Context, op operation.Operation) error {
	return s.Apply(ctx, PutOperation(op))
}

func (s *Store) GetOperation(_ context.Context, id string) (operation.Operation, error) {
	op, ok := s.fsm.State().Operations[id]
	if !ok {
		return op, operation.ErrNotFound
	}
	return op, nil
}

func (s *Store) ListOperations(context.Context) ([]operation.Operation, error) {
	ops := s.fsm.State().Operations
	out := make([]operation.Operation, 0, len(ops))
	for _, op := range ops {
		out = append(out, op)
	}
	operation.Sort(out)
	return out, nil
}

// AdminStore keeps the console password in the cluster state, so the same
// password works on every node. It implements auth.Store.
type AdminStore struct{ Store *Store }

var (
	_ auth.Store      = AdminStore{}
	_ auth.StampStore = AdminStore{}
)

func (a AdminStore) Stamp() (string, error) {
	v, ok := a.Store.fsm.AdminStamp()
	if !ok {
		return "", auth.ErrNoCredentials
	}
	return strconv.FormatUint(v, 10), nil
}

func (a AdminStore) Load() (auth.Credentials, error) {
	c := a.Store.fsm.State().Admin
	if c == nil {
		return auth.Credentials{}, auth.ErrNoCredentials
	}
	return *c, nil
}

func (a AdminStore) Save(c auth.Credentials) error {
	return a.Store.Apply(context.Background(), SetAdmin(c))
}

// Init offers c as the cluster's password; it is kept only if the cluster
// has none. The decision is made in the log, so a node that has not caught
// up yet cannot overwrite a newer password. It returns the password in effect.
func (a AdminStore) Init(c auth.Credentials) (auth.Credentials, error) {
	if err := a.Store.Apply(context.Background(), InitAdmin(c)); err != nil {
		return auth.Credentials{}, err
	}
	return a.Load()
}

// AddRuntime asks every node to install ref.
func (s *Store) AddRuntime(ctx context.Context, ref domain.RuntimeRef) error {
	return s.Apply(ctx, AddRuntime(ref))
}

// RemoveRuntime stops asking nodes to install ref.
func (s *Store) RemoveRuntime(ctx context.Context, ref domain.RuntimeRef) error {
	return s.Apply(ctx, RemoveRuntime(ref))
}

// Runtimes returns the runtimes every node should have, sorted.
func (s *Store) Runtimes() []domain.RuntimeRef {
	var out []domain.RuntimeRef
	for name := range s.fsm.State().Runtimes {
		if ref, err := domain.ParseRuntimeRef(name); err == nil {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (s *Store) ListRouteUsers(context.Context) ([]domain.RouteUser, error) {
	var out []domain.RouteUser
	for _, u := range s.fsm.State().RouteUsers {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) PutRouteUser(ctx context.Context, u domain.RouteUser) error {
	return s.Apply(ctx, PutRouteUser(u))
}

func (s *Store) DeleteRouteUser(ctx context.Context, name string) error {
	if _, ok := s.fsm.State().RouteUsers[name]; !ok {
		return fmt.Errorf("route user %s: %w", name, domain.ErrNotFound)
	}
	return s.Apply(ctx, DeleteRouteUser(name))
}

func (s *Store) ListModules(context.Context) ([]domain.ModuleSpec, error) {
	var out []domain.ModuleSpec
	for _, m := range s.fsm.State().Modules {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) PutModule(ctx context.Context, m domain.ModuleSpec) error {
	return s.Apply(ctx, PutModule(m))
}

func (s *Store) DeleteModule(ctx context.Context, name string) error {
	return s.Apply(ctx, DeleteModule(name))
}

func (s *Store) GetSettings(_ context.Context, scope string) (map[string]string, error) {
	v := s.fsm.State().Settings[scope]
	if v == nil {
		v = map[string]string{}
	}
	return v, nil
}

func (s *Store) PutSettings(ctx context.Context, scope string, values map[string]string) error {
	return s.Apply(ctx, PutSettings(scope, values))
}

// ServiceIDs returns the db-branch user and group ids the cluster agreed on
// (the zero value before they are recorded).
func (s *Store) ServiceIDs() domain.ServiceIDs {
	if ids := s.fsm.State().ServiceIDs; ids != nil {
		return *ids
	}
	return domain.ServiceIDs{}
}

// InitServiceIDs records ids unless the cluster has some already.
func (s *Store) InitServiceIDs(ctx context.Context, ids domain.ServiceIDs) error {
	return s.Apply(ctx, InitServiceIDs(ids))
}

var _ domain.ClusterIDStore = (*Store)(nil)

// ClusterID is the cluster's anonymous identifier ("" before one is recorded).
func (s *Store) ClusterID(context.Context) string { return s.fsm.State().ClusterID }

// InitClusterID records id unless the cluster has one, and returns the
// recorded one: the state machine keeps the first, so two leaders that race
// end up with the same id.
func (s *Store) InitClusterID(ctx context.Context, id string) (string, error) {
	if err := s.Apply(ctx, InitClusterID(id)); err != nil {
		return "", err
	}
	return s.ClusterID(ctx), nil
}

// TrialStarted is when the cluster's trial began (zero before it did).
func (s *Store) TrialStarted(context.Context) (time.Time, error) {
	return domain.TrialStartedIn(s.fsm.State().Settings[domain.TrialScope]), nil
}

// StartTrial records at unless a start is recorded: the state machine keeps
// the first, so a node that has not caught up cannot begin the trial again.
func (s *Store) StartTrial(ctx context.Context, at time.Time) (time.Time, error) {
	if err := s.Apply(ctx, StartTrial(at)); err != nil {
		return time.Time{}, err
	}
	return s.TrialStarted(ctx)
}

func (s *Store) ListTokens(context.Context) ([]domain.APIToken, error) {
	var out []domain.APIToken
	for _, t := range s.fsm.State().Tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) PutToken(ctx context.Context, t domain.APIToken) error {
	return s.Apply(ctx, PutToken(t))
}

func (s *Store) DeleteToken(ctx context.Context, id string) error {
	if _, ok := s.fsm.State().Tokens[id]; !ok {
		return fmt.Errorf("token %s: %w", id, domain.ErrNotFound)
	}
	return s.Apply(ctx, DeleteToken(id))
}

// ListUsers returns all users sorted by ID.
func (s *Store) ListUsers(context.Context) ([]domain.User, error) {
	var out []domain.User
	for _, u := range s.fsm.State().Users {
		out = append(out, u.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetUser returns the user with ID id.
func (s *Store) GetUser(_ context.Context, id string) (domain.User, error) {
	u, ok := s.fsm.State().Users[id]
	if !ok {
		return domain.User{}, fmt.Errorf("user %s: %w", id, domain.ErrNotFound)
	}
	return u.Clone(), nil
}

// InsertUser creates a new user atomically in Raft.
func (s *Store) InsertUser(ctx context.Context, u domain.User) error {
	return s.Apply(ctx, InsertUser(u))
}

// PutUser replaces or creates a user atomically in Raft.
func (s *Store) PutUser(ctx context.Context, u domain.User) error {
	return s.Apply(ctx, PutUser(u))
}

// DeleteUser removes a user atomically in Raft.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.Apply(ctx, DeleteUser(id))
}

func (s *Store) ListModuleData(_ context.Context, module string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for k, v := range s.fsm.State().ModuleData[module] {
		out[k] = v
	}
	return out, nil
}

func (s *Store) PutModuleData(ctx context.Context, module, key string, value []byte) error {
	return s.Apply(ctx, PutModuleData(module, key, value))
}

func (s *Store) DeleteModuleData(ctx context.Context, module, key string) error {
	return s.Apply(ctx, DeleteModuleData(module, key))
}

func barrierTimeout(ctx context.Context) time.Duration {
	timeout := 5 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < timeout {
			if rem <= 0 {
				return time.Millisecond
			}
			return rem
		}
	}
	return timeout
}

// ModuleDataOp executes a linearizable get, put_if_absent, or take on the Raft leader.
func (s *Store) ModuleDataOp(ctx context.Context, req ModuleDataOpRequest) (ModuleDataOpResponse, error) {
	if err := ctx.Err(); err != nil {
		return ModuleDataOpResponse{}, err
	}
	if s.IsLeader() {
		return s.leaderModuleDataOp(ctx, req)
	}
	id, _ := s.Leader()
	if id == "" || s.opts.Forwarder == nil {
		return ModuleDataOpResponse{}, domain.ErrNoLeader
	}
	addr, ok := s.fsm.NodeAddress(id)
	if !ok {
		return ModuleDataOpResponse{}, fmt.Errorf("%w: leader %s has not registered its API address yet", domain.ErrNoLeader, id)
	}
	mf, ok := s.opts.Forwarder.(ModuleDataForwarder)
	if !ok {
		return ModuleDataOpResponse{}, domain.ErrNoLeader
	}
	resp, err := mf.ModuleDataOp(ctx, addr, req)
	if errors.Is(err, domain.ErrUnsupported) {
		return s.legacyModuleDataOp(ctx, req)
	}
	return resp, err
}

func (s *Store) leaderModuleDataOp(ctx context.Context, req ModuleDataOpRequest) (ModuleDataOpResponse, error) {
	s.modMu.Lock()
	defer s.modMu.Unlock()
	if err := ctx.Err(); err != nil {
		return ModuleDataOpResponse{}, err
	}
	if err := s.raft.Barrier(barrierTimeout(ctx)).Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return ModuleDataOpResponse{}, fmt.Errorf("%w: %v", domain.ErrNoLeader, err)
		}
		return ModuleDataOpResponse{}, err
	}
	val, exists := s.fsm.ModuleValue(req.Module, req.Key)
	switch req.Op {
	case "get":
		if !exists {
			return ModuleDataOpResponse{}, nil
		}
		return ModuleDataOpResponse{Value: val, OK: true}, nil
	case "put_if_absent":
		if req.Key == "" || len(req.Key) > domain.MaxModuleKey || len(req.Value) > domain.MaxModuleValue {
			return ModuleDataOpResponse{}, fmt.Errorf("%w: key 1..%d bytes, value up to %d bytes", domain.ErrModuleDataTooLarge, domain.MaxModuleKey, domain.MaxModuleValue)
		}
		if exists {
			return ModuleDataOpResponse{OK: false}, nil
		}
		recs, total := s.fsm.ModuleUsage(req.Module)
		if recs+1 > domain.MaxModuleRecords || total+len(req.Key)+len(req.Value) > domain.MaxModuleTotal {
			return ModuleDataOpResponse{}, fmt.Errorf("%w: a module keeps up to %d records and %d bytes", domain.ErrModuleDataTooLarge, domain.MaxModuleRecords, domain.MaxModuleTotal)
		}
		if _, err := s.applyLeader(PutModuleData(req.Module, req.Key, req.Value)); err != nil {
			return ModuleDataOpResponse{}, err
		}
		return ModuleDataOpResponse{OK: true}, nil
	case "take":
		if !exists {
			return ModuleDataOpResponse{}, nil
		}
		if _, err := s.applyLeader(DeleteModuleData(req.Module, req.Key)); err != nil {
			return ModuleDataOpResponse{}, err
		}
		return ModuleDataOpResponse{Value: val, OK: true}, nil
	default:
		return ModuleDataOpResponse{}, fmt.Errorf("unknown module data op %q", req.Op)
	}
}

func (s *Store) legacyModuleDataOp(ctx context.Context, req ModuleDataOpRequest) (ModuleDataOpResponse, error) {
	all, err := s.ListModuleData(ctx, req.Module)
	if err != nil {
		return ModuleDataOpResponse{}, err
	}
	val, exists := all[req.Key]
	switch req.Op {
	case "get":
		if !exists {
			return ModuleDataOpResponse{}, nil
		}
		return ModuleDataOpResponse{Value: append([]byte(nil), val...), OK: true}, nil
	case "put_if_absent":
		if req.Key == "" || len(req.Key) > domain.MaxModuleKey || len(req.Value) > domain.MaxModuleValue {
			return ModuleDataOpResponse{}, fmt.Errorf("%w: key 1..%d bytes, value up to %d bytes", domain.ErrModuleDataTooLarge, domain.MaxModuleKey, domain.MaxModuleValue)
		}
		if exists {
			return ModuleDataOpResponse{OK: false}, nil
		}
		if err := s.PutModuleData(ctx, req.Module, req.Key, req.Value); err != nil {
			return ModuleDataOpResponse{}, err
		}
		return ModuleDataOpResponse{OK: true}, nil
	case "take":
		if !exists {
			return ModuleDataOpResponse{}, nil
		}
		if err := s.DeleteModuleData(ctx, req.Module, req.Key); err != nil {
			return ModuleDataOpResponse{}, err
		}
		return ModuleDataOpResponse{Value: append([]byte(nil), val...), OK: true}, nil
	default:
		return ModuleDataOpResponse{}, fmt.Errorf("unknown module data op %q", req.Op)
	}
}

func (s *Store) GetModuleData(ctx context.Context, module, key string) ([]byte, bool, error) {
	resp, err := s.ModuleDataOp(ctx, ModuleDataOpRequest{Op: "get", Module: module, Key: key})
	return resp.Value, resp.OK, err
}

func (s *Store) PutIfAbsentModuleData(ctx context.Context, module, key string, value []byte) (bool, error) {
	resp, err := s.ModuleDataOp(ctx, ModuleDataOpRequest{Op: "put_if_absent", Module: module, Key: key, Value: value})
	return resp.OK, err
}

func (s *Store) TakeModuleData(ctx context.Context, module, key string) ([]byte, bool, error) {
	resp, err := s.ModuleDataOp(ctx, ModuleDataOpRequest{Op: "take", Module: module, Key: key})
	return resp.Value, resp.OK, err
}

func (s *Store) GetBaseRefresh(_ context.Context, base domain.Name) (domain.BaseRefresh, error) {
	r, ok := s.fsm.State().BaseRefreshes[base.String()]
	if !ok {
		return r, fmt.Errorf("refresh of base %s: %w", base, domain.ErrNotFound)
	}
	return r, nil
}

func (s *Store) ListBaseRefreshes(context.Context) ([]domain.BaseRefresh, error) {
	var out []domain.BaseRefresh
	for _, r := range s.fsm.State().BaseRefreshes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Base.String() < out[j].Base.String() })
	return out, nil
}

func (s *Store) SetBaseRefresh(ctx context.Context, r domain.BaseRefresh) error {
	return s.Apply(ctx, SetBaseRefresh(r))
}

func (s *Store) RecordBaseRefreshRun(ctx context.Context, base domain.Name, run domain.RefreshRun) error {
	if _, ok := s.fsm.State().BaseRefreshes[base.String()]; !ok {
		return fmt.Errorf("refresh of base %s: %w", base, domain.ErrNotFound)
	}
	return s.Apply(ctx, RecordBaseRefreshRun(base, run))
}

func (s *Store) ClearBaseRefresh(ctx context.Context, base domain.Name) error {
	if _, ok := s.fsm.State().BaseRefreshes[base.String()]; !ok {
		return fmt.Errorf("refresh of base %s: %w", base, domain.ErrNotFound)
	}
	return s.Apply(ctx, ClearBaseRefresh(base))
}

// GetUpdateCheck returns the last check of the repositories (zero before any).
func (s *Store) GetUpdateCheck(context.Context) (domain.UpdateCheck, error) {
	if c := s.fsm.State().Update; c != nil {
		return *c, nil
	}
	return domain.UpdateCheck{}, nil
}

func (s *Store) PutUpdateCheck(ctx context.Context, c domain.UpdateCheck) error {
	return s.Apply(ctx, PutUpdateCheck(c))
}

// GetRollout returns the current or last rollout.
func (s *Store) GetRollout(context.Context) (domain.Rollout, error) {
	if r := s.fsm.State().Rollout; r != nil {
		return *r, nil
	}
	return domain.Rollout{}, fmt.Errorf("rollout: %w", domain.ErrNotFound)
}

// StartRollout is decided in the log, so two admins cannot start two rollouts.
func (s *Store) StartRollout(ctx context.Context, r domain.Rollout) error {
	return s.Apply(ctx, StartRollout(r))
}

func (s *Store) RecordRollout(ctx context.Context, ev domain.RolloutEvent) error {
	return s.Apply(ctx, RecordRollout(ev))
}

// Voters is the number of voting members of the cluster.
func (s *Store) Voters() int {
	f := s.raft.GetConfiguration()
	if f.Error() != nil {
		return 0
	}
	n := 0
	for _, srv := range f.Configuration().Servers {
		if srv.Suffrage == raft.Voter {
			n++
		}
	}
	return n
}

// TransferLeadership hands leadership to another voter (leader only).
func (s *Store) TransferLeadership() error { return s.raft.LeadershipTransfer().Error() }

// SessionRevocations keeps the logged-out console sessions in the cluster
// state, so a logout on one node holds on every node and survives restarts.
// It implements auth.Revocations.
type SessionRevocations struct{ Store *Store }

var _ auth.Revocations = SessionRevocations{}

// Revoke records the logout through the log. It fails with
// domain.ErrClusterUpgrading until every node speaks protocol 24.
func (r SessionRevocations) Revoke(id string, expires, now time.Time) error {
	return r.Store.Apply(context.Background(), RevokeSession(id, expires, now))
}

// Revoked reads this node's copy of the state.
func (r SessionRevocations) Revoked(id string) bool { return r.Store.fsm.SessionRevoked(id) }
