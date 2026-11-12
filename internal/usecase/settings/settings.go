// Package settings keeps cluster-wide settings: db-branch's own ("core")
// and each module's, as the module defines them. Every node reads the same
// values from the replicated store.
package settings

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// CoreScope holds db-branch's own settings.
const CoreScope = "core"

// ErrInvalid is returned for an unknown key or a value of the wrong type.
var ErrInvalid = errors.New("invalid setting")

// Scope is a group of settings shown together.
type Scope struct {
	Name     string        `json:"scope"`
	Title    string        `json:"title"`
	Settings []ext.Setting `json:"settings"`
}

// Service validates and stores settings.
type Service struct {
	Store domain.SettingsStore
	// Core defines db-branch's own settings.
	Core []ext.Setting
	// Modules returns the settings each loaded module defines, by name.
	Modules func() map[string][]ext.Setting
	// Checks add a test of a core setting's value, by key, beyond its type.
	Checks map[string]func(string) error
}

// Scopes lists core first, then the modules that have settings, by name.
func (s *Service) Scopes() []Scope {
	out := []Scope{{Name: CoreScope, Title: "db-branch", Settings: s.Core}}
	if s.Modules == nil {
		return out
	}
	mods := s.Modules()
	names := make([]string, 0, len(mods))
	for n, set := range mods {
		if len(set) > 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, Scope{Name: n, Title: n, Settings: mods[n]})
	}
	return out
}

func (s *Service) scope(name string) (Scope, error) {
	for _, sc := range s.Scopes() {
		if sc.Name == name {
			return sc, nil
		}
	}
	return Scope{}, fmt.Errorf("settings %s: %w", name, domain.ErrNotFound)
}

// Values returns every setting of scope, defaults filled in.
func (s *Service) Values(ctx context.Context, scope string) (map[string]string, error) {
	sc, err := s.scope(scope)
	if err != nil {
		return nil, err
	}
	stored, err := s.Store.GetSettings(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, set := range sc.Settings {
		out[set.Key] = set.Default
		if v, ok := stored[set.Key]; ok {
			out[set.Key] = v
		}
	}
	return out, nil
}

// Put checks and stores values for scope. Keys left out keep their values;
// an empty value returns a setting to its default.
func (s *Service) Put(ctx context.Context, scope string, values map[string]string) error {
	sc, err := s.scope(scope)
	if err != nil {
		return err
	}
	defs := map[string]ext.Setting{}
	for _, set := range sc.Settings {
		defs[set.Key] = set
	}
	stored, err := s.Store.GetSettings(ctx, scope)
	if err != nil {
		return err
	}
	next := map[string]string{}
	for k, v := range stored {
		next[k] = v
	}
	for k, v := range values {
		def, ok := defs[k]
		if !ok {
			return fmt.Errorf("%w: %s has no setting %q", ErrInvalid, scope, k)
		}
		if v == "" {
			delete(next, k)
			continue
		}
		if err := Check(def, v); err != nil {
			return err
		}
		if check := s.Checks[k]; check != nil && scope == CoreScope {
			if err := check(v); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
		}
		next[k] = v
	}
	return s.Store.PutSettings(ctx, scope, next)
}

// Check reports whether v suits the setting's type.
func Check(def ext.Setting, v string) error {
	var err error
	switch def.Type {
	case "int":
		_, err = strconv.Atoi(v)
	case "bool":
		_, err = strconv.ParseBool(v)
	case "duration":
		var d time.Duration
		if d, err = time.ParseDuration(v); err == nil && d < 0 {
			err = errors.New("negative")
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %s must be a %s: %q", ErrInvalid, def.Key, def.Type, v)
	}
	return nil
}
