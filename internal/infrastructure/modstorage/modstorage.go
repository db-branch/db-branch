// Package modstorage uses a module's storage (e.g. Amazon FSx for OpenZFS)
// as the node's storage for bases and branches.
package modstorage

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// ErrNotLoaded: the node is set up on a module's storage that is not
// loaded (not installed yet, or failed to start).
var ErrNotLoaded = errors.New("storage module is not loaded")

// Storage is domain.Storage and domain.SharedStorage over a module's
// ext.Storage, attached once the module is loaded.
type Storage struct {
	// Module names the module, for errors.
	Module string
	mu     sync.RWMutex
	s      ext.Storage
}

// Attach starts using the module's storage.
func (s *Storage) Attach(st ext.Storage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s = st
}

func (s *Storage) get() (ext.Storage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.s == nil {
		return nil, fmt.Errorf("%w: install and enable the %s module (db-branch module install %s)", ErrNotLoaded, s.Module, s.Module)
	}
	return s.s, nil
}

func (s *Storage) Create(ctx context.Context, ds string, props map[string]string) error {
	st, err := s.get()
	if err != nil {
		return err
	}
	return st.Create(ctx, ds, props)
}

func (s *Storage) Snapshot(ctx context.Context, snap domain.Snapshot) error {
	st, err := s.get()
	if err != nil {
		return err
	}
	return st.Snapshot(ctx, snap.Dataset, snap.Name)
}

func (s *Storage) Clone(ctx context.Context, origin domain.Snapshot, ds string) error {
	st, err := s.get()
	if err != nil {
		return err
	}
	return st.Clone(ctx, origin.Dataset, origin.Name, ds)
}

func (s *Storage) Rollback(ctx context.Context, snap domain.Snapshot) error {
	st, err := s.get()
	if err != nil {
		return err
	}
	return st.Rollback(ctx, snap.Dataset, snap.Name)
}

func (s *Storage) Destroy(ctx context.Context, ds string) error {
	st, err := s.get()
	if err != nil {
		return err
	}
	return st.Destroy(ctx, ds)
}

func (s *Storage) DestroySnapshot(ctx context.Context, snap domain.Snapshot) error {
	st, err := s.get()
	if err != nil {
		return err
	}
	return st.DestroySnapshot(ctx, snap.Dataset, snap.Name)
}

func (s *Storage) Exists(ctx context.Context, ds string) (bool, error) {
	st, err := s.get()
	if err != nil {
		return false, err
	}
	return st.Exists(ctx, ds)
}

func (s *Storage) Info(ctx context.Context, ds string) (domain.DatasetInfo, error) {
	st, err := s.get()
	if err != nil {
		return domain.DatasetInfo{}, err
	}
	in, err := st.Info(ctx, ds)
	return domain.DatasetInfo{Mountpoint: in.Mountpoint, Used: in.Used, Referenced: in.Referenced}, err
}

func (s *Storage) Mounted(ctx context.Context, parent string) ([]string, error) {
	st, err := s.get()
	if err != nil {
		return nil, err
	}
	return st.Mounted(ctx, parent)
}

func (s *Storage) Release(ctx context.Context, ds string) error {
	st, err := s.get()
	if err != nil {
		return err
	}
	return st.Release(ctx, ds)
}
