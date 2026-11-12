// Package runtime manages the database engine binaries installed on a node.
package runtime

import (
	"context"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

// Service installs, lists and removes runtimes.
type Service struct {
	Catalog domain.RuntimeCatalog
	Cache   domain.ArtifactCache
	Store   domain.RuntimeStore
	// Arch is this node's architecture in Go notation (amd64, arm64).
	Arch string
	// Distro is the operating system ("ubuntu-26.04"; see osrelease), which
	// picks the builds of runtimes that link the system libraries. Empty when
	// unknown: such runtimes then cannot be installed.
	Distro string
	// InUse reports whether a base or branch on this node runs ref. Optional.
	InUse func(ctx context.Context, ref domain.RuntimeRef) (bool, error)
}

func (s Service) platform() string { return domain.Platform(s.Arch, s.Distro) }

// Install downloads ref's archive from the catalog and unpacks it.
func (s Service) Install(ctx context.Context, ref domain.RuntimeRef) error {
	has, err := s.Store.Has(ctx, ref)
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("%s: %w", ref, domain.ErrRuntimeInstalled)
	}
	artifacts, err := s.Catalog.Lookup(ref, s.platform())
	if err != nil {
		return fmt.Errorf("%s (%s): %w", ref, s.Arch, err)
	}
	archives := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		path, err := s.Cache.Get(ctx, a)
		if err != nil {
			return fmt.Errorf("download %s: %w", ref, err)
		}
		archives = append(archives, path)
	}
	return s.Store.Install(ctx, ref, archives)
}

// Remove deletes an installed runtime.
func (s Service) Remove(ctx context.Context, ref domain.RuntimeRef) error {
	has, err := s.Store.Has(ctx, ref)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("%s: %w", ref, domain.ErrRuntimeNotInstalled)
	}
	if s.InUse != nil {
		used, err := s.InUse(ctx, ref)
		if err != nil {
			return err
		}
		if used {
			return fmt.Errorf("%w: a base or branch on this node runs %s", domain.ErrInUse, ref)
		}
	}
	return s.Store.Remove(ctx, ref)
}

// List returns the installed runtimes.
func (s Service) List(ctx context.Context) ([]domain.RuntimeRef, error) {
	return s.Store.List(ctx)
}

// Available returns the versions of engine the catalog offers for this node.
func (s Service) Available(engine domain.Engine) []domain.RuntimeRef {
	return s.Catalog.Available(engine, s.platform())
}

// Missing returns the runtimes of desired that are not installed here.
func (s Service) Missing(ctx context.Context, desired []domain.RuntimeRef) ([]domain.RuntimeRef, error) {
	var out []domain.RuntimeRef
	for _, ref := range desired {
		has, err := s.Store.Has(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !has {
			out = append(out, ref)
		}
	}
	return out, nil
}

// ArtifactPath returns a verified local copy of ref's archive for platform, for
// serving to other nodes (the leader fetches it from upstream once).
func (s Service) ArtifactPath(ctx context.Context, ref domain.RuntimeRef, platform string, part int) (string, error) {
	artifacts, err := s.Catalog.Lookup(ref, platform)
	if err != nil {
		return "", fmt.Errorf("%s (%s): %w", ref, platform, err)
	}
	if part < 0 || part >= len(artifacts) {
		return "", fmt.Errorf("%s (%s) part %d: %w", ref, platform, part, domain.ErrRuntimeNotFound)
	}
	return s.Cache.Get(ctx, artifacts[part])
}
