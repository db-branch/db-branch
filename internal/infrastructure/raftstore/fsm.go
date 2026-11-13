// Package raftstore replicates cluster state (bases, branches, nodes) with Raft.
package raftstore

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

// State is everything the cluster agrees on.
type State struct {
	Bases    map[string]domain.Base   `json:"bases"`
	Branches map[string]domain.Branch `json:"branches"`
	Nodes    map[string]domain.Node   `json:"nodes"`
	// Operations keeps the running ones and the newest finished ones.
	Operations     map[string]operation.Operation `json:"operations"`
	JoinSecretHash string                         `json:"join_secret_hash"`
	// Admin is the console password, one for the whole cluster.
	Admin *auth.Credentials `json:"admin,omitempty"`
	// Runtimes every node should have installed ("engine@version").
	Runtimes map[string]bool `json:"runtimes,omitempty"`
	// RouteUsers are the proxy users shared by every branch.
	RouteUsers map[string]domain.RouteUser `json:"route_users,omitempty"`
	// Modules every node should (or, once removed, should not) have.
	Modules map[string]domain.ModuleSpec `json:"modules,omitempty"`
	// Settings by scope ("core", module names).
	Settings map[string]map[string]string `json:"settings,omitempty"`
	// Tokens are API tokens by id (hashes only).
	Tokens map[string]domain.APIToken `json:"tokens,omitempty"`
	// ModuleData are modules' records by module and key.
	ModuleData map[string]map[string][]byte `json:"module_data,omitempty"`
	// BaseRefreshes are the base refresh schedules by base name. They are
	// kept apart from the bases so that put_base never touches them.
	BaseRefreshes map[string]domain.BaseRefresh `json:"base_refreshes,omitempty"`
	// Update is what the repositories offered at the last check, and Rollout
	// the current (or last) update of the cluster's packages.
	Update  *domain.UpdateCheck `json:"update,omitempty"`
	Rollout *domain.Rollout     `json:"rollout,omitempty"`
	// ServiceIDs are the db-branch user and group ids every node uses (nil
	// until the leader records its own; see init_service_ids).
	ServiceIDs *domain.ServiceIDs `json:"service_ids,omitempty"`
	// ClusterID is the cluster's anonymous identifier (see
	// domain.ClusterIDStore); empty until the leader draws one.
	ClusterID string `json:"cluster_id,omitempty"`
}

func newState() State {
	return State{Bases: map[string]domain.Base{}, Branches: map[string]domain.Branch{}, Nodes: map[string]domain.Node{}, Operations: map[string]operation.Operation{}}
}

// Command is one change to State, as written to the Raft log.
type Command struct {
	Op        string               `json:"op"`
	Base      *domain.Base         `json:"base,omitempty"`
	Branch    *domain.Branch       `json:"branch,omitempty"`
	Node      *domain.Node         `json:"node,omitempty"`
	Operation *operation.Operation `json:"operation,omitempty"`
	Admin     *auth.Credentials    `json:"admin,omitempty"`
	Replica   *Replica             `json:"replica,omitempty"`
	RouteUser *domain.RouteUser    `json:"route_user,omitempty"`
	Module    *domain.ModuleSpec   `json:"module,omitempty"`
	Settings  map[string]string    `json:"settings,omitempty"`
	Token     *domain.APIToken     `json:"token,omitempty"`
	Name      string               `json:"name,omitempty"`
	Key       string               `json:"key,omitempty"`
	Value     []byte               `json:"value,omitempty"`
	Refresh   *domain.BaseRefresh  `json:"refresh,omitempty"`
	Run       *domain.RefreshRun   `json:"run,omitempty"`
	Update    *domain.UpdateCheck  `json:"update,omitempty"`
	Rollout   *domain.Rollout      `json:"rollout,omitempty"`
	Event     *domain.RolloutEvent `json:"event,omitempty"`
	// IDs is the payload of init_service_ids.
	IDs *domain.ServiceIDs `json:"service_ids,omitempty"`
	// ClusterID is the payload of init_cluster_id.
	ClusterID string `json:"cluster_id,omitempty"`
	// Nodes is the replica list of set_base_replica_nodes; absent (null)
	// means every node, [] means the base's own node only.
	Nodes *[]string `json:"nodes"`

	// Anonymous is the limits of set_base_anonymous; the zero value (absent)
	// means inherit.
	Anonymous domain.AnonymousLimits `json:"anonymous,omitzero"`

	raw []byte
}

// Protocol is the cluster protocol this binary speaks. Raise it whenever a
// command is added or changes meaning, and list the command in opProtocol.
//
// 17: init_cluster_id records the cluster's anonymous identifier (sent to the
// licence service with a key renewal). The id is drawn when it is first
// needed, so an existing cluster gets it lazily once every node is at 17.
//
// 16: a branch may run another runtime than its base (branch upgrade) and an
// operation carries a result. Both travel inside put_branch and put_operation,
// so there is no new command; nodes at 15 would reset or recreate such a
// branch on the base's runtime, so the feature waits for protocol 16
// everywhere (domain.ProtocolBranchRuntime, checked by the use case).
const Protocol = 17

// opProtocol is the protocol each command needs. A command is written to the
// log only once every member speaks it, so an older node never meets a
// command it would misread (see docs/cluster-plan.md).
var opProtocol = map[string]int{
	"put_base": 1, "delete_base": 1, "put_branch": 1, "delete_branch": 1,
	"put_node": 1, "delete_node": 1, "set_join_secret": 1,
	"insert_base": 2, "insert_branch": 2, "put_operation": 2,
	"set_admin": 3, "init_admin": 3,
	"add_replica": 4,
	"add_runtime": 5, "remove_runtime": 5,
	"put_route_user": 6, "delete_route_user": 6,
	"put_module": 7, "put_settings": 7,
	"put_token": 8, "delete_token": 8,
	"put_module_data": 9, "delete_module_data": 9,
	"start_trial":      10,
	"set_base_refresh": 11, "record_base_refresh_run": 11, "clear_base_refresh": 11,
	"put_update_check": 12, "start_rollout": 12, "record_rollout": 12,
	"set_base_replica_nodes": 13, "remove_replica": 13,
	"set_base_anonymous": 14,
	"init_service_ids":   15,
	"init_cluster_id":    17,
}

// Needs returns the protocol the command requires (0 for unknown commands).
func (c Command) Needs() int { return opProtocol[c.Op] }

// Encode serializes the command for raft.Apply. A command received from
// another node keeps its original bytes, so fields this version does not
// know are not lost on the way to the log.
func (c Command) Encode() ([]byte, error) {
	if c.raw != nil {
		return c.raw, nil
	}
	return json.Marshal(c)
}

// DecodeCommand parses a command and remembers its bytes for Encode.
func DecodeCommand(b []byte) (Command, error) {
	var c Command
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	c.raw = append([]byte(nil), b...)
	return c, nil
}

func PutBase(b domain.Base) Command      { return Command{Op: "put_base", Base: &b} }
func DeleteBase(n domain.Name) Command   { return Command{Op: "delete_base", Name: n.String()} }
func PutBranch(b domain.Branch) Command  { return Command{Op: "put_branch", Branch: &b} }
func DeleteBranch(n domain.Name) Command { return Command{Op: "delete_branch", Name: n.String()} }
func PutNode(n domain.Node) Command      { return Command{Op: "put_node", Node: &n} }
func DeleteNode(name string) Command     { return Command{Op: "delete_node", Name: name} }
func SetJoinSecret(hash string) Command  { return Command{Op: "set_join_secret", Name: hash} }

// InsertBase and InsertBranch fail with ErrAlreadyExists when the name is taken,
// which makes name reservations race-free across nodes.
func InsertBase(b domain.Base) Command     { return Command{Op: "insert_base", Base: &b} }
func InsertBranch(b domain.Branch) Command { return Command{Op: "insert_branch", Branch: &b} }
func SetAdmin(c auth.Credentials) Command  { return Command{Op: "set_admin", Admin: &c} }

// Replica says node holds a copy of a base snapshot.
type Replica struct {
	Base     string `json:"base"`
	Snapshot string `json:"snapshot"`
	Node     string `json:"node"`
}

// AddReplica records a copy of a base snapshot on node.
func AddReplica(base domain.Name, snapshot, node string) Command {
	return Command{Op: "add_replica", Replica: &Replica{Base: base.String(), Snapshot: snapshot, Node: node}}
}

// SetBaseReplicaNodes limits the nodes that keep a copy of the base (nil: all
// of them); RemoveReplica forgets the copies node held of the base.
func SetBaseReplicaNodes(base domain.Name, nodes *[]string) Command {
	return Command{Op: "set_base_replica_nodes", Name: base.String(), Nodes: nodes}
}
func RemoveReplica(base domain.Name, node string) Command {
	return Command{Op: "remove_replica", Name: base.String(), Key: node}
}

// SetBaseAnonymous sets what callers without credentials may do with the
// base (the zero value: inherit the cluster-wide settings).
func SetBaseAnonymous(base domain.Name, l domain.AnonymousLimits) Command {
	return Command{Op: "set_base_anonymous", Name: base.String(), Anonymous: l}
}

// AddRuntime asks every node to install ref; RemoveRuntime withdraws that.
func AddRuntime(ref domain.RuntimeRef) Command { return Command{Op: "add_runtime", Name: ref.String()} }
func RemoveRuntime(ref domain.RuntimeRef) Command {
	return Command{Op: "remove_runtime", Name: ref.String()}
}

func PutSettings(scope string, v map[string]string) Command {
	return Command{Op: "put_settings", Name: scope, Settings: v}
}

// StartTrial records at as the start of the cluster's trial, unless one is
// recorded already.
func StartTrial(at time.Time) Command {
	return Command{Op: "start_trial", Name: at.UTC().Format(time.RFC3339)}
}

// SetBaseRefresh stores a base's refresh schedule (its last run is kept);
// RecordBaseRefreshRun stores the outcome of a run; ClearBaseRefresh removes
// the schedule.
func SetBaseRefresh(r domain.BaseRefresh) Command {
	return Command{Op: "set_base_refresh", Refresh: &r}
}
func RecordBaseRefreshRun(base domain.Name, run domain.RefreshRun) Command {
	return Command{Op: "record_base_refresh_run", Name: base.String(), Run: &run}
}
func ClearBaseRefresh(base domain.Name) Command {
	return Command{Op: "clear_base_refresh", Name: base.String()}
}

// PutUpdateCheck records what the repositories offer. StartRollout begins a
// rollout (it fails while one runs); RecordRollout stores a node's progress or
// ends the rollout.
func PutUpdateCheck(c domain.UpdateCheck) Command { return Command{Op: "put_update_check", Update: &c} }
func StartRollout(r domain.Rollout) Command       { return Command{Op: "start_rollout", Rollout: &r} }
func RecordRollout(ev domain.RolloutEvent) Command {
	return Command{Op: "record_rollout", Event: &ev}
}

func PutToken(t domain.APIToken) Command { return Command{Op: "put_token", Token: &t} }
func DeleteToken(id string) Command      { return Command{Op: "delete_token", Name: id} }

func PutModuleData(module, key string, value []byte) Command {
	return Command{Op: "put_module_data", Name: module, Key: key, Value: value}
}
func DeleteModuleData(module, key string) Command {
	return Command{Op: "delete_module_data", Name: module, Key: key}
}

func PutModule(m domain.ModuleSpec) Command { return Command{Op: "put_module", Module: &m} }

func PutRouteUser(u domain.RouteUser) Command { return Command{Op: "put_route_user", RouteUser: &u} }
func DeleteRouteUser(name string) Command     { return Command{Op: "delete_route_user", Name: name} }

// InitServiceIDs records the db-branch user and group ids of the cluster
// unless some are recorded already.
func InitServiceIDs(ids domain.ServiceIDs) Command { return Command{Op: "init_service_ids", IDs: &ids} }

// InitClusterID records id as the cluster's identifier unless one is recorded
// already.
func InitClusterID(id string) Command { return Command{Op: "init_cluster_id", ClusterID: id} }

// InitAdmin sets the console password only if the cluster has none yet.
func InitAdmin(c auth.Credentials) Command { return Command{Op: "init_admin", Admin: &c} }

func PutOperation(op operation.Operation) Command {
	return Command{Op: "put_operation", Operation: &op}
}

// FSM applies commands to State. It implements raft.FSM.
type FSM struct {
	mu    sync.RWMutex
	state State
	// OnApply is called after every successfully applied command, outside the
	// lock, on every node. Set it before Raft starts.
	OnApply func(Command)
	// index is the last log index applied (successfully or not).
	index atomic.Uint64
}

// Index returns the last log index the FSM has applied.
func (f *FSM) Index() uint64 { return f.index.Load() }

var _ raft.FSM = (*FSM)(nil)

// NewFSM returns an empty FSM.
func NewFSM() *FSM { return &FSM{state: newState()} }

// Apply applies one log entry and returns nil or an error.
func (f *FSM) Apply(l *raft.Log) interface{} {
	var c Command
	if err := json.Unmarshal(l.Data, &c); err != nil {
		f.index.Store(l.Index)
		return fmt.Errorf("decode command: %w", err)
	}
	f.mu.Lock()
	err := f.apply(c)
	f.mu.Unlock()
	f.index.Store(l.Index)
	if err != nil {
		return err
	}
	if f.OnApply != nil {
		f.OnApply(c)
	}
	return nil
}

// complete reports whether c carries what its op needs. A command written by
// a node that did not know a field arrives without it; applying it must fail
// cleanly on every node instead of crashing them all.
func (c Command) complete() bool {
	switch c.Op {
	case "insert_base", "put_base":
		return c.Base != nil
	case "insert_branch", "put_branch":
		return c.Branch != nil
	case "put_node":
		return c.Node != nil
	case "put_operation":
		return c.Operation != nil
	case "set_admin", "init_admin":
		return c.Admin != nil
	case "add_replica":
		return c.Replica != nil
	case "put_route_user":
		return c.RouteUser != nil
	case "put_module":
		return c.Module != nil
	case "put_token":
		return c.Token != nil
	case "put_module_data", "delete_module_data":
		return c.Name != "" && c.Key != ""
	case "set_base_refresh":
		return c.Refresh != nil
	case "record_base_refresh_run":
		return c.Run != nil
	case "put_update_check":
		return c.Update != nil
	case "start_rollout":
		return c.Rollout != nil
	case "record_rollout":
		return c.Event != nil
	case "init_service_ids":
		return c.IDs != nil && c.IDs.Valid()
	case "init_cluster_id":
		return domain.ValidClusterID(c.ClusterID)
	}
	return true
}

func (f *FSM) apply(c Command) error {
	if !c.complete() {
		return fmt.Errorf("command %q is missing its data (written by a node of another version?)", c.Op)
	}
	s := &f.state
	switch c.Op {
	case "insert_base":
		if _, ok := s.Bases[c.Base.Name.String()]; ok {
			return fmt.Errorf("base %s: %w", c.Base.Name, domain.ErrAlreadyExists)
		}
		b := *c.Base
		b.Replicas, b.ReplicaNodes, b.Anonymous = nil, nil, domain.AnonymousLimits{}
		s.Bases[b.Name.String()] = b
	case "insert_branch":
		if _, ok := s.Branches[c.Branch.Name.String()]; ok {
			return fmt.Errorf("branch %s: %w", c.Branch.Name, domain.ErrAlreadyExists)
		}
		s.Branches[c.Branch.Name.String()] = *c.Branch
	case "put_operation":
		s.Operations[c.Operation.ID] = *c.Operation
		list := make([]operation.Operation, 0, len(s.Operations))
		for _, op := range s.Operations {
			list = append(list, op)
		}
		for _, id := range operation.Prune(list) {
			delete(s.Operations, id)
		}
	case "put_base":
		// Replicas change only through add_replica, so a base update written
		// from an older read cannot drop copies recorded meanwhile.
		b := *c.Base
		b.Replicas = s.Bases[b.Name.String()].Replicas
		b.ReplicaNodes = s.Bases[b.Name.String()].ReplicaNodes
		b.Anonymous = s.Bases[b.Name.String()].Anonymous
		s.Bases[b.Name.String()] = b
	case "put_route_user":
		if s.RouteUsers == nil {
			s.RouteUsers = map[string]domain.RouteUser{}
		}
		s.RouteUsers[c.RouteUser.Name] = *c.RouteUser
	case "delete_route_user":
		delete(s.RouteUsers, c.Name)
	case "put_token":
		if s.Tokens == nil {
			s.Tokens = map[string]domain.APIToken{}
		}
		s.Tokens[c.Token.ID] = *c.Token
	case "delete_token":
		delete(s.Tokens, c.Name)
	case "put_module_data":
		if s.ModuleData == nil {
			s.ModuleData = map[string]map[string][]byte{}
		}
		if s.ModuleData[c.Name] == nil {
			s.ModuleData[c.Name] = map[string][]byte{}
		}
		s.ModuleData[c.Name][c.Key] = append([]byte{}, c.Value...)
	case "delete_module_data":
		delete(s.ModuleData[c.Name], c.Key)
		if len(s.ModuleData[c.Name]) == 0 {
			delete(s.ModuleData, c.Name)
		}
	case "put_settings":
		if s.Settings == nil {
			s.Settings = map[string]map[string]string{}
		}
		v := map[string]string{}
		for k, x := range c.Settings {
			v[k] = x
		}
		s.Settings[c.Name] = v
	case "start_trial":
		if s.Settings == nil {
			s.Settings = map[string]map[string]string{}
		}
		if domain.TrialStartedIn(s.Settings[domain.TrialScope]).IsZero() {
			s.Settings[domain.TrialScope] = map[string]string{domain.TrialKey: c.Name}
		}
	case "init_service_ids":
		if s.ServiceIDs == nil {
			ids := *c.IDs
			s.ServiceIDs = &ids
		}
	case "init_cluster_id":
		if s.ClusterID == "" {
			s.ClusterID = c.ClusterID
		}
	case "put_module":
		if s.Modules == nil {
			s.Modules = map[string]domain.ModuleSpec{}
		}
		s.Modules[c.Module.Name] = *c.Module
	case "add_runtime":
		if s.Runtimes == nil {
			s.Runtimes = map[string]bool{}
		}
		s.Runtimes[c.Name] = true
	case "remove_runtime":
		delete(s.Runtimes, c.Name)
	case "add_replica":
		b, ok := s.Bases[c.Replica.Base]
		if !ok {
			return fmt.Errorf("base %s: %w", c.Replica.Base, domain.ErrNotFound)
		}
		b.Replicas = copyReplicas(b.Replicas)
		b.AddReplica(c.Replica.Snapshot, c.Replica.Node)
		s.Bases[c.Replica.Base] = b
	case "set_base_replica_nodes":
		b, ok := s.Bases[c.Name]
		if !ok {
			return fmt.Errorf("base %s: %w", c.Name, domain.ErrNotFound)
		}
		b.ReplicaNodes = copyNodes(c.Nodes)
		s.Bases[c.Name] = b
	case "set_base_anonymous":
		b, ok := s.Bases[c.Name]
		if !ok {
			return fmt.Errorf("base %s: %w", c.Name, domain.ErrNotFound)
		}
		b.Anonymous = c.Anonymous.Clone()
		s.Bases[c.Name] = b
	case "remove_replica":
		b, ok := s.Bases[c.Name]
		if !ok {
			return fmt.Errorf("base %s: %w", c.Name, domain.ErrNotFound)
		}
		b.Replicas = copyReplicas(b.Replicas)
		b.RemoveReplica(c.Key)
		s.Bases[c.Name] = b
	case "delete_base":
		delete(s.Bases, c.Name)
		delete(s.BaseRefreshes, c.Name)
	case "set_base_refresh":
		if s.BaseRefreshes == nil {
			s.BaseRefreshes = map[string]domain.BaseRefresh{}
		}
		r := *c.Refresh
		r.LastRun = s.BaseRefreshes[r.Base.String()].LastRun
		s.BaseRefreshes[r.Base.String()] = r
	case "record_base_refresh_run":
		r, ok := s.BaseRefreshes[c.Name]
		if !ok {
			return fmt.Errorf("refresh of base %s: %w", c.Name, domain.ErrNotFound)
		}
		run := *c.Run
		r.LastRun = &run
		s.BaseRefreshes[c.Name] = r
	case "clear_base_refresh":
		delete(s.BaseRefreshes, c.Name)
	case "put_update_check":
		check := cloneCheck(*c.Update)
		s.Update = &check
	case "start_rollout":
		if s.Rollout != nil && s.Rollout.Status == domain.RolloutRunning {
			return fmt.Errorf("%w (%s)", domain.ErrRolloutRunning, s.Rollout.ID)
		}
		r := c.Rollout.Clone()
		s.Rollout = &r
	case "record_rollout":
		if s.Rollout == nil {
			return fmt.Errorf("rollout: %w", domain.ErrNotFound)
		}
		r := s.Rollout.Clone()
		err := r.Record(*c.Event)
		if err != nil {
			return err
		}
		s.Rollout = &r
	case "put_branch":
		s.Branches[c.Branch.Name.String()] = *c.Branch
	case "delete_branch":
		delete(s.Branches, c.Name)
	case "put_node":
		s.Nodes[c.Node.Name] = *c.Node
	case "delete_node":
		delete(s.Nodes, c.Name)
	case "set_join_secret":
		s.JoinSecretHash = c.Name
	case "set_admin":
		admin := *c.Admin
		s.Admin = &admin
	case "init_admin":
		if s.Admin == nil {
			admin := *c.Admin
			s.Admin = &admin
		}
	default:
		return fmt.Errorf("unknown command %q", c.Op)
	}
	return nil
}

// State returns a deep copy of the current state.
func (f *FSM) State() State {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := newState()
	for k, v := range f.state.Bases {
		v.Snapshots = append([]domain.BaseSnapshot(nil), v.Snapshots...)
		v.Replicas = copyReplicas(v.Replicas)
		v.ReplicaNodes = copyNodes(v.ReplicaNodes)
		v.Anonymous = v.Anonymous.Clone()
		out.Bases[k] = v
	}
	for k, v := range f.state.Branches {
		out.Branches[k] = v
	}
	for k, v := range f.state.Nodes {
		out.Nodes[k] = v
	}
	for k, v := range f.state.Operations {
		out.Operations[k] = v
	}
	out.JoinSecretHash = f.state.JoinSecretHash
	if f.state.ServiceIDs != nil {
		ids := *f.state.ServiceIDs
		out.ServiceIDs = &ids
	}
	out.ClusterID = f.state.ClusterID
	for k, v := range f.state.Tokens {
		if out.Tokens == nil {
			out.Tokens = map[string]domain.APIToken{}
		}
		out.Tokens[k] = v
	}
	for m, v := range f.state.ModuleData {
		if out.ModuleData == nil {
			out.ModuleData = map[string]map[string][]byte{}
		}
		c := map[string][]byte{}
		for k, x := range v {
			c[k] = append([]byte{}, x...)
		}
		out.ModuleData[m] = c
	}
	for scope, v := range f.state.Settings {
		if out.Settings == nil {
			out.Settings = map[string]map[string]string{}
		}
		c := map[string]string{}
		for k, x := range v {
			c[k] = x
		}
		out.Settings[scope] = c
	}
	for k, v := range f.state.Modules {
		if out.Modules == nil {
			out.Modules = map[string]domain.ModuleSpec{}
		}
		out.Modules[k] = v
	}
	for k, v := range f.state.RouteUsers {
		if out.RouteUsers == nil {
			out.RouteUsers = map[string]domain.RouteUser{}
		}
		out.RouteUsers[k] = v
	}
	for k, v := range f.state.BaseRefreshes {
		if out.BaseRefreshes == nil {
			out.BaseRefreshes = map[string]domain.BaseRefresh{}
		}
		v.Source.Databases = append([]string(nil), v.Source.Databases...)
		if v.LastRun != nil {
			run := *v.LastRun
			v.LastRun = &run
		}
		out.BaseRefreshes[k] = v
	}
	for k, v := range f.state.Runtimes {
		if out.Runtimes == nil {
			out.Runtimes = map[string]bool{}
		}
		out.Runtimes[k] = v
	}
	if f.state.Update != nil {
		check := cloneCheck(*f.state.Update)
		out.Update = &check
	}
	if f.state.Rollout != nil {
		r := f.state.Rollout.Clone()
		out.Rollout = &r
	}
	if f.state.Admin != nil {
		admin := *f.state.Admin
		out.Admin = &admin
	}
	return out
}

type snapshot struct{ data []byte }

// Snapshot captures the state for log compaction.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	b, err := json.Marshal(f.State())
	return &snapshot{data: b}, err
}

func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *snapshot) Release() {}

// Restore replaces the state from a snapshot.
func (f *FSM) Restore(r io.ReadCloser) error {
	defer r.Close()
	st := newState()
	if err := json.NewDecoder(r).Decode(&st); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	if st.Operations == nil { // snapshots from before operations were replicated
		st.Operations = map[string]operation.Operation{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = st
	return nil
}

func copyNodes(n *[]string) *[]string {
	if n == nil {
		return nil
	}
	c := append([]string{}, *n...)
	return &c
}

func copyReplicas(r map[string][]string) map[string][]string {
	if r == nil {
		return nil
	}
	out := make(map[string][]string, len(r))
	for k, v := range r {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func cloneCheck(c domain.UpdateCheck) domain.UpdateCheck {
	pkgs := make(map[string][]domain.PackageVersion, len(c.Packages))
	for k, v := range c.Packages {
		pkgs[k] = append([]domain.PackageVersion(nil), v...)
	}
	c.Packages = pkgs
	return c
}
