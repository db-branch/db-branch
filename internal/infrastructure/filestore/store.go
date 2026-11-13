// Package filestore implements domain.Store in a single JSON file for single-node use.
package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Store keeps all state in one file, rewritten atomically on every change.
// The file holds branch passwords, so it is created 0600.
type Store struct {
	Path string
	mu   sync.Mutex
}

var (
	_ domain.Store            = (*Store)(nil)
	_ domain.RouteUserStore   = (*Store)(nil)
	_ domain.ModuleStore      = (*Store)(nil)
	_ domain.SettingsStore    = (*Store)(nil)
	_ domain.TrialStore       = (*Store)(nil)
	_ domain.ClusterIDStore   = (*Store)(nil)
	_ domain.TokenStore       = (*Store)(nil)
	_ domain.ModuleDataStore  = (*Store)(nil)
	_ domain.BaseRefreshStore = (*Store)(nil)
	_ domain.UpdateStore      = (*Store)(nil)
)

type state struct {
	Bases      map[string]domain.Base       `json:"bases"`
	Branches   map[string]domain.Branch     `json:"branches"`
	RouteUsers map[string]domain.RouteUser  `json:"route_users,omitempty"`
	Modules    map[string]domain.ModuleSpec `json:"modules,omitempty"`
	Settings   map[string]map[string]string `json:"settings,omitempty"`
	Tokens     map[string]domain.APIToken   `json:"tokens,omitempty"`
	ModuleData map[string]map[string][]byte `json:"module_data,omitempty"`
	// BaseRefreshes are the base refresh schedules by base name.
	BaseRefreshes map[string]domain.BaseRefresh `json:"base_refreshes,omitempty"`
	Update        *domain.UpdateCheck           `json:"update,omitempty"`
	Rollout       *domain.Rollout               `json:"rollout,omitempty"`
	// ClusterID is the anonymous cluster identifier (domain.ClusterIDStore).
	ClusterID string `json:"cluster_id,omitempty"`
}

func (s *Store) load() (state, error) {
	st := state{Bases: map[string]domain.Base{}, Branches: map[string]domain.Branch{}, RouteUsers: map[string]domain.RouteUser{}}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("state file %s: %w", s.Path, err)
	}
	return st, nil
}

func (s *Store) save(st state) error {
	b, _ := json.MarshalIndent(st, "", "  ") // plain structs: cannot fail
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s *Store) update(f func(*state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	if err := f(&st); err != nil {
		return err
	}
	return s.save(st)
}

func (s *Store) read() (state, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func notFound(kind string, n domain.Name) error {
	return fmt.Errorf("%s %s: %w", kind, n, domain.ErrNotFound)
}

// GetBase returns the base called name.
func (s *Store) GetBase(_ context.Context, name domain.Name) (domain.Base, error) {
	st, err := s.read()
	if err != nil {
		return domain.Base{}, err
	}
	b, ok := st.Bases[name.String()]
	if !ok {
		return domain.Base{}, notFound("base", name)
	}
	return b, nil
}

// ListBases returns all bases sorted by name.
func (s *Store) ListBases(context.Context) ([]domain.Base, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]domain.Base, 0, len(st.Bases))
	for _, b := range st.Bases {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name.String() < out[j].Name.String() })
	return out, nil
}

// PutBase creates or replaces a base, keeping its replica record.
func (s *Store) PutBase(_ context.Context, b domain.Base) error {
	return s.update(func(st *state) error {
		b.Replicas = st.Bases[b.Name.String()].Replicas
		b.ReplicaNodes = st.Bases[b.Name.String()].ReplicaNodes
		b.Anonymous = st.Bases[b.Name.String()].Anonymous
		st.Bases[b.Name.String()] = b
		return nil
	})
}

// AddReplica records that node holds a copy of the base's snapshot.
func (s *Store) AddReplica(_ context.Context, base domain.Name, snapshot, node string) error {
	return s.update(func(st *state) error {
		b, ok := st.Bases[base.String()]
		if !ok {
			return notFound("base", base)
		}
		b.AddReplica(snapshot, node)
		st.Bases[base.String()] = b
		return nil
	})
}

// RemoveReplica forgets the copies node held of the base.
func (s *Store) RemoveReplica(_ context.Context, base domain.Name, node string) error {
	return s.update(func(st *state) error {
		b, ok := st.Bases[base.String()]
		if !ok {
			return notFound("base", base)
		}
		b.RemoveReplica(node)
		st.Bases[base.String()] = b
		return nil
	})
}

// SetBaseReplicaNodes limits the nodes keeping a copy of the base (nil: all).
func (s *Store) SetBaseReplicaNodes(_ context.Context, base domain.Name, nodes *[]string) error {
	return s.update(func(st *state) error {
		b, ok := st.Bases[base.String()]
		if !ok {
			return notFound("base", base)
		}
		b.ReplicaNodes = nodes
		st.Bases[base.String()] = b
		return nil
	})
}

// SetBaseAnonymous sets what callers without credentials may do with the
// base (the zero value: inherit).
func (s *Store) SetBaseAnonymous(_ context.Context, base domain.Name, l domain.AnonymousLimits) error {
	return s.update(func(st *state) error {
		b, ok := st.Bases[base.String()]
		if !ok {
			return notFound("base", base)
		}
		b.Anonymous = l.Clone()
		st.Bases[base.String()] = b
		return nil
	})
}

// DeleteBase removes a base.
func (s *Store) DeleteBase(_ context.Context, name domain.Name) error {
	return s.update(func(st *state) error {
		if _, ok := st.Bases[name.String()]; !ok {
			return notFound("base", name)
		}
		delete(st.Bases, name.String())
		delete(st.BaseRefreshes, name.String())
		return nil
	})
}

// GetBranch returns the branch called name.
func (s *Store) GetBranch(_ context.Context, name domain.Name) (domain.Branch, error) {
	st, err := s.read()
	if err != nil {
		return domain.Branch{}, err
	}
	b, ok := st.Branches[name.String()]
	if !ok {
		return domain.Branch{}, notFound("branch", name)
	}
	return b, nil
}

// ListBranches returns all branches sorted by name.
func (s *Store) ListBranches(context.Context) ([]domain.Branch, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]domain.Branch, 0, len(st.Branches))
	for _, b := range st.Branches {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name.String() < out[j].Name.String() })
	return out, nil
}

// PutBranch creates or replaces a branch. Whether the branch records its
// changes is decided when it is created and never changes: a replacement
// keeps the stored value.
func (s *Store) PutBranch(_ context.Context, b domain.Branch) error {
	return s.update(func(st *state) error {
		if old, ok := st.Branches[b.Name.String()]; ok {
			b.Record = old.Record
		}
		st.Branches[b.Name.String()] = b
		return nil
	})
}

// DeleteBranch removes a branch.
func (s *Store) DeleteBranch(_ context.Context, name domain.Name) error {
	return s.update(func(st *state) error {
		if _, ok := st.Branches[name.String()]; !ok {
			return notFound("branch", name)
		}
		delete(st.Branches, name.String())
		return nil
	})
}

// InsertBase stores a new base, or fails with ErrAlreadyExists.
func (s *Store) InsertBase(_ context.Context, b domain.Base) error {
	return s.update(func(st *state) error {
		if _, ok := st.Bases[b.Name.String()]; ok {
			return fmt.Errorf("base %s: %w", b.Name, domain.ErrAlreadyExists)
		}
		b.Replicas, b.Anonymous = nil, domain.AnonymousLimits{}
		st.Bases[b.Name.String()] = b
		return nil
	})
}

// InsertBranch stores a new branch, or fails with ErrAlreadyExists.
func (s *Store) InsertBranch(_ context.Context, b domain.Branch) error {
	return s.update(func(st *state) error {
		if _, ok := st.Branches[b.Name.String()]; ok {
			return fmt.Errorf("branch %s: %w", b.Name, domain.ErrAlreadyExists)
		}
		st.Branches[b.Name.String()] = b
		return nil
	})
}

// ListRouteUsers returns the route users sorted by name.
func (s *Store) ListRouteUsers(context.Context) ([]domain.RouteUser, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]domain.RouteUser, 0, len(st.RouteUsers))
	for _, u := range st.RouteUsers {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PutRouteUser adds or replaces a route user.
func (s *Store) PutRouteUser(_ context.Context, u domain.RouteUser) error {
	return s.update(func(st *state) error {
		if st.RouteUsers == nil {
			st.RouteUsers = map[string]domain.RouteUser{}
		}
		st.RouteUsers[u.Name] = u
		return nil
	})
}

// DeleteRouteUser removes a route user.
func (s *Store) DeleteRouteUser(_ context.Context, name string) error {
	return s.update(func(st *state) error {
		if _, ok := st.RouteUsers[name]; !ok {
			return fmt.Errorf("route user %s: %w", name, domain.ErrNotFound)
		}
		delete(st.RouteUsers, name)
		return nil
	})
}

// ListModules returns the recorded modules sorted by name.
func (s *Store) ListModules(context.Context) ([]domain.ModuleSpec, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]domain.ModuleSpec, 0, len(st.Modules))
	for _, m := range st.Modules {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PutModule records a module.
func (s *Store) PutModule(_ context.Context, m domain.ModuleSpec) error {
	return s.update(func(st *state) error {
		if st.Modules == nil {
			st.Modules = map[string]domain.ModuleSpec{}
		}
		st.Modules[m.Name] = m
		return nil
	})
}

// GetSettings returns the settings of scope (empty when none were saved).
func (s *Store) GetSettings(_ context.Context, scope string) (map[string]string, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	v := st.Settings[scope]
	if v == nil {
		v = map[string]string{}
	}
	return v, nil
}

// PutSettings replaces the settings of scope.
func (s *Store) PutSettings(_ context.Context, scope string, values map[string]string) error {
	return s.update(func(st *state) error {
		if st.Settings == nil {
			st.Settings = map[string]map[string]string{}
		}
		st.Settings[scope] = values
		return nil
	})
}

// TrialStarted is when the trial began (zero before it did).
func (s *Store) TrialStarted(context.Context) (time.Time, error) {
	st, err := s.read()
	if err != nil {
		return time.Time{}, err
	}
	return domain.TrialStartedIn(st.Settings[domain.TrialScope]), nil
}

// StartTrial records at unless a start is recorded, and returns the
// recorded one.
func (s *Store) StartTrial(_ context.Context, at time.Time) (time.Time, error) {
	var started time.Time
	err := s.update(func(st *state) error {
		if st.Settings == nil {
			st.Settings = map[string]map[string]string{}
		}
		if started = domain.TrialStartedIn(st.Settings[domain.TrialScope]); started.IsZero() {
			started = at.UTC()
			st.Settings[domain.TrialScope] = map[string]string{domain.TrialKey: started.Format(time.RFC3339)}
		}
		return nil
	})
	return started, err
}

// ClusterID is the recorded cluster id ("" before one is, or when the file
// cannot be read).
func (s *Store) ClusterID(context.Context) string {
	st, err := s.read()
	if err != nil {
		return ""
	}
	return st.ClusterID
}

// InitClusterID records id unless one is recorded, and returns the recorded
// one.
func (s *Store) InitClusterID(_ context.Context, id string) (string, error) {
	if !domain.ValidClusterID(id) {
		return "", fmt.Errorf("cluster id %q is not 32 lowercase hex characters", id)
	}
	var recorded string
	err := s.update(func(st *state) error {
		if st.ClusterID == "" {
			st.ClusterID = id
		}
		recorded = st.ClusterID
		return nil
	})
	return recorded, err
}

// ListTokens returns the API tokens, oldest first.
func (s *Store) ListTokens(context.Context) ([]domain.APIToken, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]domain.APIToken, 0, len(st.Tokens))
	for _, t := range st.Tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// PutToken stores an API token.
func (s *Store) PutToken(_ context.Context, t domain.APIToken) error {
	return s.update(func(st *state) error {
		if st.Tokens == nil {
			st.Tokens = map[string]domain.APIToken{}
		}
		st.Tokens[t.ID] = t
		return nil
	})
}

// DeleteToken removes an API token.
func (s *Store) DeleteToken(_ context.Context, id string) error {
	return s.update(func(st *state) error {
		if _, ok := st.Tokens[id]; !ok {
			return fmt.Errorf("token %s: %w", id, domain.ErrNotFound)
		}
		delete(st.Tokens, id)
		return nil
	})
}

// ListModuleData returns a module's records.
func (s *Store) ListModuleData(_ context.Context, module string) (map[string][]byte, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for k, v := range st.ModuleData[module] {
		out[k] = v
	}
	return out, nil
}

// PutModuleData stores one record of a module.
func (s *Store) PutModuleData(_ context.Context, module, key string, value []byte) error {
	return s.update(func(st *state) error {
		if st.ModuleData == nil {
			st.ModuleData = map[string]map[string][]byte{}
		}
		if st.ModuleData[module] == nil {
			st.ModuleData[module] = map[string][]byte{}
		}
		st.ModuleData[module][key] = value
		return nil
	})
}

// DeleteModuleData removes one record of a module.
func (s *Store) DeleteModuleData(_ context.Context, module, key string) error {
	return s.update(func(st *state) error {
		delete(st.ModuleData[module], key)
		if len(st.ModuleData[module]) == 0 {
			delete(st.ModuleData, module)
		}
		return nil
	})
}

// GetBaseRefresh returns the base's refresh schedule.
func (s *Store) GetBaseRefresh(_ context.Context, base domain.Name) (domain.BaseRefresh, error) {
	st, err := s.read()
	if err != nil {
		return domain.BaseRefresh{}, err
	}
	r, ok := st.BaseRefreshes[base.String()]
	if !ok {
		return r, fmt.Errorf("refresh of base %s: %w", base, domain.ErrNotFound)
	}
	return r, nil
}

// ListBaseRefreshes returns the schedules sorted by base name.
func (s *Store) ListBaseRefreshes(context.Context) ([]domain.BaseRefresh, error) {
	st, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]domain.BaseRefresh, 0, len(st.BaseRefreshes))
	for _, r := range st.BaseRefreshes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Base.String() < out[j].Base.String() })
	return out, nil
}

// SetBaseRefresh stores a schedule, keeping the last run of the stored one.
func (s *Store) SetBaseRefresh(_ context.Context, r domain.BaseRefresh) error {
	return s.update(func(st *state) error {
		if st.BaseRefreshes == nil {
			st.BaseRefreshes = map[string]domain.BaseRefresh{}
		}
		r.LastRun = st.BaseRefreshes[r.Base.String()].LastRun
		st.BaseRefreshes[r.Base.String()] = r
		return nil
	})
}

// RecordBaseRefreshRun stores the outcome of a run.
func (s *Store) RecordBaseRefreshRun(_ context.Context, base domain.Name, run domain.RefreshRun) error {
	return s.update(func(st *state) error {
		r, ok := st.BaseRefreshes[base.String()]
		if !ok {
			return fmt.Errorf("refresh of base %s: %w", base, domain.ErrNotFound)
		}
		r.LastRun = &run
		st.BaseRefreshes[base.String()] = r
		return nil
	})
}

// ClearBaseRefresh removes a schedule.
func (s *Store) ClearBaseRefresh(_ context.Context, base domain.Name) error {
	return s.update(func(st *state) error {
		if _, ok := st.BaseRefreshes[base.String()]; !ok {
			return fmt.Errorf("refresh of base %s: %w", base, domain.ErrNotFound)
		}
		delete(st.BaseRefreshes, base.String())
		return nil
	})
}

// GetUpdateCheck returns the last check of the repositories (zero before any).
func (s *Store) GetUpdateCheck(context.Context) (domain.UpdateCheck, error) {
	st, err := s.read()
	if err != nil || st.Update == nil {
		return domain.UpdateCheck{}, err
	}
	return *st.Update, nil
}

func (s *Store) PutUpdateCheck(_ context.Context, c domain.UpdateCheck) error {
	return s.update(func(st *state) error { st.Update = &c; return nil })
}

// GetRollout returns the current or last rollout.
func (s *Store) GetRollout(context.Context) (domain.Rollout, error) {
	st, err := s.read()
	if err != nil {
		return domain.Rollout{}, err
	}
	if st.Rollout == nil {
		return domain.Rollout{}, fmt.Errorf("rollout: %w", domain.ErrNotFound)
	}
	return *st.Rollout, nil
}

// StartRollout fails with ErrConflict while a rollout runs.
func (s *Store) StartRollout(_ context.Context, r domain.Rollout) error {
	return s.update(func(st *state) error {
		if st.Rollout != nil && st.Rollout.Status == domain.RolloutRunning {
			return fmt.Errorf("%w (%s)", domain.ErrRolloutRunning, st.Rollout.ID)
		}
		st.Rollout = &r
		return nil
	})
}

// RecordRollout stores a node's progress or ends the rollout.
func (s *Store) RecordRollout(_ context.Context, ev domain.RolloutEvent) error {
	return s.update(func(st *state) error {
		if st.Rollout == nil {
			return fmt.Errorf("rollout: %w", domain.ErrNotFound)
		}
		r := st.Rollout.Clone()
		if err := r.Record(ev); err != nil {
			return err
		}
		st.Rollout = &r
		return nil
	})
}
