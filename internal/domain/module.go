package domain

import (
	"context"
	"errors"
	"regexp"
)

// ModuleSpec is a module the cluster should have: the package
// db-branch-<name> at Version on every node (Wanted), or on none (removed).
type ModuleSpec struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Wanted  bool   `json:"wanted"`
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
	if !moduleName.MatchString(name) {
		return ErrInvalidModule
	}
	return nil
}

// ModuleStore records which modules the cluster should have.
type ModuleStore interface {
	ListModules(ctx context.Context) ([]ModuleSpec, error)
	PutModule(ctx context.Context, m ModuleSpec) error
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
		if m.Wanted && !have[m.Name] {
			out = append(out, m.Name)
		}
	}
	return out
}
