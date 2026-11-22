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
	// Busy reports that an update rollout is running: Sync then leaves the
	// packages to it, so apt and dpkg are not used by both. Optional.
	Busy func(ctx context.Context) bool
	// Nodes lists the cluster's nodes, for features that wait until every node
	// speaks a protocol (runtime modules). Nil: a single node.
	Nodes func(ctx context.Context) ([]domain.Node, error)

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

// InstallRuntime records the runtime module of engine as wanted at version
// (default: what APT offers now). source is domain.SourceAPT (default: every
// node installs the package) or domain.SourceLocal (the operator puts it on
// each node; a version is then required). It waits until every node knows
// runtime modules (domain.ProtocolRuntimeModules): an older node would refuse
// the API for a module it can never load.
func (s *Service) InstallRuntime(ctx context.Context, engine, version, source string) (domain.ModuleSpec, error) {
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return domain.ModuleSpec{}, err
	}
	switch source {
	case "":
		source = domain.SourceAPT
	case domain.SourceAPT, domain.SourceLocal:
	default:
		return domain.ModuleSpec{}, fmt.Errorf("%w: unknown source %q (apt or local)", domain.ErrInvalidModule, source)
	}
	if s.Nodes != nil {
		nodes, err := s.Nodes(ctx)
		if err != nil {
			return domain.ModuleSpec{}, err
		}
		if domain.ClusterProtocol(nodes, domain.ProtocolRuntimeModules) < domain.ProtocolRuntimeModules {
			return domain.ModuleSpec{}, domain.ErrClusterUpgrading
		}
	}
	m := domain.ModuleSpec{Name: domain.RuntimeModuleName(engine), Wanted: true, Kind: domain.ModuleKindRuntime, Source: source}
	switch {
	case version != "":
		m.Version = version
	case source == domain.SourceLocal:
		return domain.ModuleSpec{}, fmt.Errorf("%w: a local runtime module needs its version", domain.ErrInvalidModule)
	default:
		v, err := s.Packages.Candidate(ctx, m.Package())
		if err != nil {
			return domain.ModuleSpec{}, err
		}
		if v == "" {
			return domain.ModuleSpec{}, fmt.Errorf("%w: %s", ErrUnavailable, m.Package())
		}
		m.Version = v
	}
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

// Forget deletes the cluster's record of a removed module (Wanted == false)
// so it no longer appears in lists or the console. It refuses while the
// module is still wanted (remove it first), when no record exists, when the
// cluster has not yet reached domain.ProtocolModuleForget, or when the package
// (db-branch-<name>) is still installed on this node.
//
// Only this node's installed packages can be inspected; if the package remains
// installed on another node, that node will adopt it again as a manually
// installed module.
func (s *Service) Forget(ctx context.Context, name string) error {
	if s.Nodes != nil {
		nodes, err := s.Nodes(ctx)
		if err != nil {
			return err
		}
		if have := domain.ClusterProtocol(nodes, domain.ProtocolModuleForget); have < domain.ProtocolModuleForget {
			return fmt.Errorf("%w: the cluster is upgrading (protocol %d, need %d)", domain.ErrClusterUpgrading, have, domain.ProtocolModuleForget)
		}
	}
	list, err := s.Store.ListModules(ctx)
	if err != nil {
		return err
	}
	var (
		found bool
		spec  domain.ModuleSpec
	)
	for _, m := range list {
		if m.Name == name {
			found = true
			spec = m
			break
		}
	}
	if !found {
		return fmt.Errorf("module %s: %w", name, domain.ErrNotFound)
	}
	if spec.Wanted {
		return fmt.Errorf("%w: module %s is still enabled; remove it first", domain.ErrConflict, name)
	}
	installed, err := s.Packages.Installed(ctx, prefix)
	if err != nil {
		return err
	}
	if _, ok := installed[spec.Package()]; ok {
		return fmt.Errorf("%w: package %s is still installed on this node; wait for removal to finish before forgetting (note: if the package remains on another node, that node will adopt it again as manually installed)", domain.ErrConflict, spec.Package())
	}
	return s.Store.DeleteModule(ctx, name)
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
	if s.Busy != nil && s.Busy(ctx) {
		return false, nil
	}
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
		if m.Source == domain.SourceLocal {
			continue // the operator installs these; APT has nothing to do with them
		}
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
