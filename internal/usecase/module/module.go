// Package module keeps every node's modules in line with the cluster: a
// module is installed (its APT package at a recorded version) on all nodes
// or on none, and a node missing a wanted module refuses the API until it
// has it (an authorization module on some nodes only would be a hole).
package module

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
)

// Packages manages APT packages on this node.
type Packages interface {
	// Installed returns the installed versions of packages named prefix*.
	Installed(ctx context.Context, prefix string) (map[string]string, error)
	// Candidate is the version APT would install ("" if none is available).
	Candidate(ctx context.Context, pkg string) (string, error)
	// Search lists available packages named prefix* with their one-line descriptions.
	Search(ctx context.Context, prefix string) (map[string]string, error)
	Install(ctx context.Context, pkg, version string) error
	Remove(ctx context.Context, pkg string) error
}

// ErrUnavailable is returned when no package exists for a module.
var ErrUnavailable = errors.New("no package for this module in the configured APT repositories")

// Service records wanted modules and applies them to this node.
type Service struct {
	Store    domain.ModuleStore
	Packages Packages
	// Rolled returns the versions an update rollout installed, by package.
	// A node that runs one is not put back to the older recorded version
	// while the rollout is under way (or was stopped). Optional.
	Rolled func(ctx context.Context) map[string]string

	mu sync.Mutex
}

const prefix = "db-branch-"

// Install records name as wanted at version (default: what APT offers now);
// every node then installs exactly that version.
func (s *Service) Install(ctx context.Context, name, version string) (domain.ModuleSpec, error) {
	if err := domain.ValidModuleName(name); err != nil {
		return domain.ModuleSpec{}, err
	}
	if version == "" {
		v, err := s.Packages.Candidate(ctx, domain.ModulePackage(name))
		if err != nil {
			return domain.ModuleSpec{}, err
		}
		if v == "" {
			return domain.ModuleSpec{}, fmt.Errorf("%w: %s", ErrUnavailable, domain.ModulePackage(name))
		}
		version = v
	}
	m := domain.ModuleSpec{Name: name, Version: version, Wanted: true}
	return m, s.Store.PutModule(ctx, m)
}

// Remove records that no node should have name.
func (s *Service) Remove(ctx context.Context, name string) error {
	list, err := s.Store.ListModules(ctx)
	if err != nil {
		return err
	}
	for _, m := range list {
		if m.Name == name && m.Wanted {
			m.Wanted = false
			return s.Store.PutModule(ctx, m)
		}
	}
	return fmt.Errorf("module %s: %w", name, domain.ErrNotFound)
}

// Available lists the modules the configured APT repositories offer (by
// name, with a short description): what the console can enable.
func (s *Service) Available(ctx context.Context) (map[string]string, error) {
	pkgs, err := s.Packages.Search(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for pkg, desc := range pkgs {
		if name := pkg[len(prefix):]; domain.ValidModuleName(name) == nil {
			out[name] = desc
		}
	}
	return out, nil
}

// List returns the recorded modules (removed ones included).
func (s *Service) List(ctx context.Context) ([]domain.ModuleSpec, error) {
	return s.Store.ListModules(ctx)
}

// Adopt records module packages installed on this node by hand (before the
// cluster managed modules) as wanted, unless the cluster already decided.
func (s *Service) Adopt(ctx context.Context) error {
	installed, err := s.Packages.Installed(ctx, prefix)
	if err != nil {
		return err
	}
	list, err := s.Store.ListModules(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, m := range list {
		known[m.Name] = true
	}
	var errs []error
	for pkg, version := range installed {
		name := pkg[len(prefix):]
		if known[name] || domain.ValidModuleName(name) != nil {
			continue
		}
		errs = append(errs, s.Store.PutModule(ctx, domain.ModuleSpec{Name: name, Version: version, Wanted: true}))
	}
	return errors.Join(errs...)
}

// Sync installs, upgrades and removes module packages on this node to match
// the record, and reports whether it changed anything. Installing a package
// restarts the agent (the package's postinst), which then loads the module.
func (s *Service) Sync(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.Store.ListModules(ctx)
	if err != nil {
		return false, err
	}
	installed, err := s.Packages.Installed(ctx, prefix)
	if err != nil {
		return false, err
	}
	var rolled map[string]string
	if s.Rolled != nil {
		rolled = s.Rolled(ctx)
	}
	changed := false
	var errs []error
	for _, m := range list {
		have, ok := installed[m.Package()]
		switch {
		case m.Wanted && have != m.Version && have == rolled[m.Package()] && have != "":
			// Installed by an update rollout; the record follows when it ends.
		case m.Wanted && have != m.Version:
			changed = true
			errs = append(errs, s.Packages.Install(ctx, m.Package(), m.Version))
		case !m.Wanted && ok:
			changed = true
			errs = append(errs, s.Packages.Remove(ctx, m.Package()))
		}
	}
	return changed, errors.Join(errs...)
}
