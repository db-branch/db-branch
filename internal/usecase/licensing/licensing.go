// Package licensing checks a licence key in the console and, once it is
// good, saves it and enables the paid modules it covers.
package licensing

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

var (
	// ErrNoRoot: this build has no licence root key to check signatures.
	ErrNoRoot = errors.New("this build cannot verify licence keys (no root key built in)")
	// ErrUnusable: the key is genuine but expired or over its node limit.
	ErrUnusable = errors.New("licence not usable")
)

// Modules is the cluster's module management.
type Modules interface {
	Available(ctx context.Context) (map[string]string, error)
	List(ctx context.Context) ([]domain.ModuleSpec, error)
	Install(ctx context.Context, name, version string) (domain.ModuleSpec, error)
}

// Settings stores cluster-wide settings.
type Settings interface {
	Values(ctx context.Context, scope string) (map[string]string, error)
	Put(ctx context.Context, scope string, values map[string]string) error
}

// KeySetting is where the licence key is kept (core scope).
const KeySetting = "license.key"

// Service verifies and applies licence keys.
type Service struct {
	Root     *ecdsa.PublicKey
	Clock    func() time.Time
	Nodes    func() int
	Modules  Modules
	Settings Settings
	// Trial is nil where the store keeps no trial.
	Trial domain.TrialStore
}

// Report is what a key says, checked.
type Report struct {
	License license.License `json:"license"`
	State   license.State   `json:"state"`
	// Trial: no key is saved; this is the trial (license.Trial).
	Trial bool `json:"trial,omitempty"`
	// All: the key covers every module ("*").
	All bool `json:"all"`
	// Modules it covers that the package repositories offer; Unavailable
	// the ones they do not.
	Modules     []string `json:"modules"`
	Unavailable []string `json:"unavailable"`
	// Offline: the package repositories could not be asked, so Modules and
	// Unavailable are not known (every named module reads as unavailable)
	// and Apply enabled none; apply the key again once they are reachable.
	Offline bool `json:"offline,omitempty"`
	// Enabled are the modules Apply turned on.
	Enabled []string `json:"enabled"`
}

// Verify checks key without saving anything.
func (s *Service) Verify(ctx context.Context, key string) (Report, error) {
	if s.Root == nil {
		return Report{}, ErrNoRoot
	}
	l, err := license.Verify(s.Root, strings.TrimSpace(key))
	if err != nil {
		return Report{}, err
	}
	r := Report{License: l, State: l.State(s.Clock(), s.Nodes()), Modules: []string{}, Unavailable: []string{}, Enabled: []string{}}
	// Offline (an air-gapped node cannot ask the package repositories): the
	// key is still valid, so report its modules as not offered rather than
	// failing the check.
	available, err := s.Modules.Available(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return r, err
		}
		available, r.Offline = nil, true
	}
	for _, m := range l.Modules {
		switch _, ok := available[m]; {
		case m == "*":
			r.All = true
		case ok:
			r.Modules = append(r.Modules, m)
		default:
			r.Unavailable = append(r.Unavailable, m)
		}
	}
	sort.Strings(r.Modules)
	sort.Strings(r.Unavailable)
	return r, nil
}

// Save verifies key and, if usable, saves it for the whole cluster. It
// enables no module (Apply does that for a key an admin pastes) and does
// not ask the package repositories, so a renewal needs neither.
func (s *Service) Save(ctx context.Context, key string) (license.License, error) {
	return s.save(ctx, key, false)
}

// SaveRenewed is Save for a key the licence service sent: one that allows
// fewer nodes than the cluster has is saved too (it then reads over_nodes
// and the usual node-limit warnings apply); only an expired one is refused.
func (s *Service) SaveRenewed(ctx context.Context, key string) (license.License, error) {
	return s.save(ctx, key, true)
}

func (s *Service) save(ctx context.Context, key string, allowOver bool) (license.License, error) {
	if s.Root == nil {
		return license.License{}, ErrNoRoot
	}
	key = strings.TrimSpace(key)
	l, err := license.Verify(s.Root, key)
	if err != nil {
		return l, err
	}
	st := l.State(s.Clock(), s.Nodes())
	if !st.Usable() && !(allowOver && st == license.OverNodes && l.State(s.Clock(), 0).Usable()) {
		return l, fmt.Errorf("%w: it is %s", ErrUnusable, st)
	}
	return l, s.Settings.Put(ctx, "core", map[string]string{KeySetting: key})
}

// Apply verifies key and, if usable, saves it and enables the modules it
// names that are not on yet. A key for every module ("*") enables none:
// the admin picks them.
func (s *Service) Apply(ctx context.Context, key string) (Report, error) {
	key = strings.TrimSpace(key)
	r, err := s.Verify(ctx, key)
	if err != nil {
		return r, err
	}
	if _, err := s.Save(ctx, key); err != nil {
		return r, err
	}
	specs, err := s.Modules.List(ctx)
	if err != nil {
		return r, err
	}
	on := func(name string) bool {
		return slices.ContainsFunc(specs, func(m domain.ModuleSpec) bool { return m.Name == name && m.Wanted })
	}
	var errs []error
	for _, m := range r.Modules {
		if on(m) {
			continue
		}
		if _, err := s.Modules.Install(ctx, m, ""); err != nil {
			errs = append(errs, fmt.Errorf("enable %s: %w", m, err))
			continue
		}
		r.Enabled = append(r.Enabled, m)
	}
	return r, errors.Join(errs...)
}

// Current checks the saved key; false when no key is saved. Without a key
// the report is the trial's, once it began.
func (s *Service) Current(ctx context.Context) (Report, bool, error) {
	v, err := s.Settings.Values(ctx, "core")
	if err != nil {
		return Report{}, false, err
	}
	key := strings.TrimSpace(v[KeySetting])
	if key != "" {
		r, err := s.Verify(ctx, key)
		return r, true, err
	}
	if s.Trial == nil {
		return Report{}, false, nil
	}
	start, err := s.Trial.TrialStarted(ctx)
	if err != nil || start.IsZero() {
		return Report{}, false, err
	}
	l := license.Trial(start)
	return Report{License: l, State: l.State(s.Clock(), s.Nodes()), Trial: true, All: true, Modules: []string{}, Unavailable: []string{}, Enabled: []string{}}, false, nil
}

// StartTrial returns when the cluster's trial began, beginning it now if
// it has not: the agent calls it once a paid module is loaded. The trial
// begins once per cluster, with or without a key.
func (s *Service) StartTrial(ctx context.Context) (time.Time, error) {
	if s.Trial == nil {
		return time.Time{}, nil
	}
	if t, err := s.Trial.TrialStarted(ctx); err != nil || !t.IsZero() {
		return t, err
	}
	return s.Trial.StartTrial(ctx, s.Clock().UTC().Truncate(time.Second))
}
