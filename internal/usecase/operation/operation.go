// Package operation runs long tasks in the background and reports their progress.
package operation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned for an unknown operation ID.
var ErrNotFound = errors.New("operation not found")

// State is where an operation is in its life.
type State string

const (
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
)

// Target names the resource an operation works on.
type Target struct {
	Resource string `json:"resource"`
	Name     string `json:"name"`
}

// Operation is the progress record of one long task.
type Operation struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Target Target `json:"target"`
	// Owner is the immutable ID of the principal that started the task. Only
	// that principal, those who share its branches and admins may read the
	// operation; "" (started by the system) is admin-only. A node that does
	// not know this field drops it, which leaves the operation admin-only.
	Owner string `json:"owner,omitempty"`
	// Node is the node running the task.
	Node       string     `json:"node,omitempty"`
	State      State      `json:"state"`
	Phase      string     `json:"phase,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Seq orders operations by start, newest highest, across nodes.
	Seq uint64 `json:"seq"`
	// Result is what the task reports besides success or failure (JSON; an
	// upgrade's pre-check findings, for one). Set when the task finishes.
	Result json.RawMessage `json:"result,omitempty"`
}

// Store keeps operation records: in memory on a single node, in Raft in a
// cluster so that every node sees every operation.
type Store interface {
	PutOperation(ctx context.Context, op Operation) error
	GetOperation(ctx context.Context, id string) (Operation, error)
	ListOperations(ctx context.Context) ([]Operation, error)
}

// Func is the task body. report updates the operation's phase.
type Func func(ctx context.Context, report func(phase string)) error

// ResultFunc is a task body that also returns a result, kept on the
// operation (as JSON) whether the task succeeded or failed.
type ResultFunc func(ctx context.Context, report func(phase string)) (any, error)

// Manager runs operations and records their progress in Store.
type Manager struct {
	// Store defaults to an in-memory store.
	Store Store
	// Bus receives "operation.updated" events. Optional: in a cluster the
	// events come from the replicated store instead.
	Bus   *Bus
	Clock func() time.Time
	// Node is this node's name, recorded on every operation it runs.
	Node string

	once sync.Once
	mu   sync.Mutex
	seq  uint64
}

func (m *Manager) store() Store {
	m.once.Do(func() {
		if m.Store == nil {
			m.Store = &MemoryStore{}
		}
	})
	return m.Store
}

// now is the API's time resolution: whole seconds, UTC.
func (m *Manager) now() time.Time { return m.Clock().UTC().Truncate(time.Second) }

// nextSeq is the start time in nanoseconds, kept strictly increasing.
func (m *Manager) nextSeq() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := uint64(m.Clock().UnixNano())
	if s <= m.seq {
		s = m.seq + 1
	}
	m.seq = s
	return s
}

func newID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return "op_" + hex.EncodeToString(b)
}

func (m *Manager) put(op Operation) error {
	if err := m.store().PutOperation(context.Background(), op); err != nil {
		return err
	}
	if m.Bus != nil {
		m.Bus.PublishTo(Audience{Owner: op.Owner}, "operation.updated", op)
	}
	return nil
}

// Start records the operation, runs f in the background and returns the
// initial record. f does not run when the record cannot be stored. The task
// is not tied to the caller's context: it keeps running after the HTTP
// request that started it ends.
func (m *Manager) Start(kind string, target Target, f Func) (Operation, error) {
	return m.StartAs("", kind, target, f)
}

// StartAs is Start for a task owned by the principal with the given ID.
func (m *Manager) StartAs(owner, kind string, target Target, f Func) (Operation, error) {
	return m.StartResultAs(owner, kind, target, func(ctx context.Context, report func(string)) (any, error) {
		return nil, f(ctx, report)
	})
}

// StartResult is Start for a task that returns a result.
func (m *Manager) StartResult(kind string, target Target, f ResultFunc) (Operation, error) {
	return m.StartResultAs("", kind, target, f)
}

// StartResultAs is StartResult for a task owned by the principal with the given ID.
func (m *Manager) StartResultAs(owner, kind string, target Target, f ResultFunc) (Operation, error) {
	now := m.now()
	op := Operation{ID: newID(), Kind: kind, Owner: owner, Target: target, Node: m.Node, State: Running, CreatedAt: now, UpdatedAt: now, Seq: m.nextSeq()}
	if err := m.put(op); err != nil {
		return Operation{}, err
	}
	go func() {
		cur := op
		var mu sync.Mutex
		update := func(change func(*Operation)) {
			mu.Lock()
			defer mu.Unlock()
			change(&cur)
			cur.UpdatedAt = m.now()
			_ = m.put(cur) // progress is best effort; the final state is retried below
		}
		result, err := f(context.Background(), func(phase string) { update(func(o *Operation) { o.Phase = phase }) })
		update(func(o *Operation) {
			end := m.now()
			o.State, o.Phase, o.FinishedAt = Succeeded, "", &end
			if err != nil {
				o.State, o.Error = Failed, err.Error()
			}
			if result != nil {
				o.Result, _ = json.Marshal(result)
			}
		})
		m.settle(cur)
	}()
	return op, nil
}

// settle retries storing a finished record for a while, so a short loss of
// the cluster leader does not leave the operation running forever.
func (m *Manager) settle(op Operation) {
	for i := 0; i < 30; i++ {
		got, err := m.store().GetOperation(context.Background(), op.ID)
		if err == nil && got.State == op.State {
			return
		}
		time.Sleep(time.Second)
		_ = m.put(op)
	}
}

// Recover marks this node's running operations failed: the agent restarted,
// so nothing is running them any more.
func (m *Manager) Recover(ctx context.Context) error {
	list, err := m.store().ListOperations(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, op := range list {
		if op.State != Running || op.Node != m.Node {
			continue
		}
		end := m.now()
		op.State, op.Phase, op.Error, op.UpdatedAt, op.FinishedAt = Failed, "", "interrupted by an agent restart", end, &end
		errs = append(errs, m.put(op))
	}
	return errors.Join(errs...)
}

// Get returns one operation.
func (m *Manager) Get(id string) (Operation, error) {
	return m.store().GetOperation(context.Background(), id)
}

// Active returns the running operation on a resource, if any.
func (m *Manager) Active(resource, name string) (Operation, bool) {
	for _, op := range m.List() {
		if op.State == Running && op.Target.Resource == resource && op.Target.Name == name {
			return op, true
		}
	}
	return Operation{}, false
}

// List returns all operations, newest first.
func (m *Manager) List() []Operation {
	list, _ := m.store().ListOperations(context.Background())
	Sort(list)
	return list
}

// Sort orders operations newest first.
func Sort(list []Operation) {
	sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
}

// Keep is how many finished operations a store retains.
const Keep = 200

// Prune returns the IDs of finished operations beyond the newest Keep.
func Prune(list []Operation) []string {
	Sort(list)
	var drop []string
	finished := 0
	for _, op := range list {
		if op.State == Running {
			continue
		}
		if finished++; finished > Keep {
			drop = append(drop, op.ID)
		}
	}
	return drop
}

// MemoryStore keeps operations in memory, retaining the newest Keep finished ones.
type MemoryStore struct {
	mu  sync.Mutex
	ops map[string]Operation
}

func (s *MemoryStore) PutOperation(_ context.Context, op Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ops == nil {
		s.ops = map[string]Operation{}
	}
	s.ops[op.ID] = op
	list := make([]Operation, 0, len(s.ops))
	for _, o := range s.ops {
		list = append(list, o)
	}
	for _, id := range Prune(list) {
		delete(s.ops, id)
	}
	return nil
}

func (s *MemoryStore) GetOperation(_ context.Context, id string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return op, nil
}

func (s *MemoryStore) ListOperations(context.Context) ([]Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Operation, 0, len(s.ops))
	for _, op := range s.ops {
		out = append(out, op)
	}
	return out, nil
}
