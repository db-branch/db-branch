package domain

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// ModuleSpec is a module the cluster should have: the package
// db-branch-<name> at Version on every node (Wanted), or on none (removed).
type ModuleSpec struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Wanted  bool   `json:"wanted"`
	// Kind is "" for a plugin of the agent (/usr/lib/db-branch/modules) or
	// ModuleKindRuntime for a database engine (/usr/lib/db-branch/runtimes),
	// named "rt-<engine>". Source is "" or SourceAPT for a package every node
	// installs, SourceLocal for one the operator puts on each node. Both are
	// left out of the stored form when empty, so a plain module is stored as before.
	Kind   string `json:"kind,omitempty"`
	Source string `json:"source,omitempty"`
}

const (
	// ModuleKindRuntime marks a runtime module (an external database engine).
	ModuleKindRuntime = "runtime"
	// SourceAPT: the package comes from the APT repositories and the cluster installs it everywhere.
	SourceAPT = "apt"
	// SourceLocal: the operator installs the module on each node; the cluster only checks it is there.
	SourceLocal = "local"
	// ProtocolRuntimeModules is the cluster protocol from which runtime modules
	// may be recorded. They travel inside put_module (no new command), but a
	// node that does not know them would wait for a "plugin" that never comes
	// and refuse the API. 19 and 20 are reserved for branch fork and merge.
	ProtocolRuntimeModules = 21
	// ProtocolModuleForget is the cluster protocol from which removed module
	// records may be deleted (delete_module). Older nodes do not know the
	// command, so it waits until every node speaks 22.
	ProtocolModuleForget = 22
	runtimeModulePrefix  = "rt-"
	// maxRuntimeEngine keeps "db-branch-rt-<engine>" a valid login name (32 characters).
	maxRuntimeEngine = 19
)

// IsRuntime reports whether the module is a database engine.
func (m ModuleSpec) IsRuntime() bool { return m.Kind == ModuleKindRuntime }

// Engine is the engine of a runtime module ("" for a plugin).
func (m ModuleSpec) Engine() string {
	if !m.IsRuntime() {
		return ""
	}
	return strings.TrimPrefix(m.Name, runtimeModulePrefix)
}

// RuntimeModuleName is the module name of an engine's runtime module.
func RuntimeModuleName(engine string) string { return runtimeModulePrefix + engine }

// ValidRuntimeModuleEngine reports whether engine can have a runtime module:
// a valid engine name that is not built into the core and fits its service user.
func ValidRuntimeModuleEngine(engine string) error {
	if !enginePattern.MatchString(engine) || len(engine) > maxRuntimeEngine || Engine(engine) == MySQL || Engine(engine) == Postgres {
		return ErrInvalidModule
	}
	return nil
}

// Package is the APT package that ships the module.
func (m ModuleSpec) Package() string { return ModulePackage(m.Name) }

// ModulePackage names the package of module name.
func ModulePackage(name string) string { return "db-branch-" + name }

// ErrInvalidModule is returned for a module name no package could have.
var ErrInvalidModule = errors.New("invalid module name")

var moduleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ValidModuleName reports whether name can be a module (lowercase, digits, hyphens).
func ValidModuleName(name string) error {
	if !moduleName.MatchString(name) || strings.HasPrefix(name, runtimeModulePrefix) { // rt- is for runtime modules

		return ErrInvalidModule
	}
	return nil
}

// ModuleStore records which modules the cluster should have.
type ModuleStore interface {
	ListModules(ctx context.Context) ([]ModuleSpec, error)
	PutModule(ctx context.Context, m ModuleSpec) error
	DeleteModule(ctx context.Context, name string) error
}

// PendingModules returns the wanted modules that loaded (module names that
// this node's agent runs) lacks.
func PendingModules(specs []ModuleSpec, loaded []string) []string {
	have := map[string]bool{}
	for _, l := range loaded {
		have[l] = true
	}
	var out []string
	for _, m := range specs {
		if m.Wanted && !m.IsRuntime() && !have[m.Name] {
			out = append(out, m.Name)
		}
	}
	return out
}

// PendingRuntimeModules returns the engines of wanted runtime modules that
// loaded (the engines this node's agent runs) lacks, whatever their source.
func PendingRuntimeModules(specs []ModuleSpec, loaded []string) []string {
	have := map[string]bool{}
	for _, l := range loaded {
		have[l] = true
	}
	var out []string
	for _, m := range specs {
		if m.Wanted && m.IsRuntime() && !have[m.Engine()] {
			out = append(out, m.Engine())
		}
	}
	return out
}
