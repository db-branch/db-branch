// Package moduledata gives each module its cluster-wide store (ext.Store)
// within limits, so one module cannot bloat the Raft state.
package moduledata

import (
	"context"
	"errors"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// Limits on what one module keeps.
const (
	MaxKey     = domain.MaxModuleKey
	MaxValue   = domain.MaxModuleValue
	MaxRecords = domain.MaxModuleRecords
	MaxTotal   = domain.MaxModuleTotal
)

// ErrTooLarge: a key, a value or the module's total is over its limit.
var ErrTooLarge = domain.ErrModuleDataTooLarge

// ErrUnavailable: the node is not initialised yet.
var ErrUnavailable = errors.New("module data is not available before `db-branch init`")

// For returns the store of module (unavailable when s is nil).
func For(s domain.ModuleDataStore, module string) ext.Store {
	return store{s: s, module: module}
}

type store struct {
	s      domain.ModuleDataStore
	module string
}

func (st store) List(ctx context.Context) (map[string][]byte, error) {
	if st.s == nil {
		return nil, ErrUnavailable
	}
	return st.s.ListModuleData(ctx, st.module)
}

func (st store) checkBounds(key string, value []byte) error {
	if st.s == nil {
		return ErrUnavailable
	}
	if key == "" || len(key) > MaxKey || len(value) > MaxValue {
		return fmt.Errorf("%w: key 1..%d bytes, value up to %d bytes", ErrTooLarge, MaxKey, MaxValue)
	}
	return nil
}

func (st store) checkWrite(ctx context.Context, key string, value []byte) error {
	if err := st.checkBounds(key, value); err != nil {
		return err
	}
	all, err := st.s.ListModuleData(ctx, st.module)
	if err != nil {
		return err
	}
	total, records := len(key)+len(value), 1
	for k, v := range all {
		if k != key {
			total += len(k) + len(v)
			records++
		}
	}
	if records > MaxRecords || total > MaxTotal {
		return fmt.Errorf("%w: a module keeps up to %d records and %d bytes", ErrTooLarge, MaxRecords, MaxTotal)
	}
	return nil
}

func (st store) Put(ctx context.Context, key string, value []byte) error {
	if err := st.checkWrite(ctx, key, value); err != nil {
		return err
	}
	return st.s.PutModuleData(ctx, st.module, key, value)
}

func (st store) Delete(ctx context.Context, key string) error {
	if st.s == nil {
		return ErrUnavailable
	}
	return st.s.DeleteModuleData(ctx, st.module, key)
}

func (st store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if st.s == nil {
		return nil, false, ErrUnavailable
	}
	if a, ok := st.s.(domain.AtomicModuleDataStore); ok {
		return a.GetModuleData(ctx, st.module, key)
	}
	all, err := st.s.ListModuleData(ctx, st.module)
	if err != nil {
		return nil, false, err
	}
	v, ok := all[key]
	return v, ok, nil
}

func (st store) PutIfAbsent(ctx context.Context, key string, value []byte) (bool, error) {
	if err := st.checkBounds(key, value); err != nil {
		return false, err
	}
	if a, ok := st.s.(domain.AtomicModuleDataStore); ok {
		return a.PutIfAbsentModuleData(ctx, st.module, key, value)
	}
	if err := st.checkWrite(ctx, key, value); err != nil {
		return false, err
	}
	all, err := st.s.ListModuleData(ctx, st.module)
	if err != nil {
		return false, err
	}
	if _, exists := all[key]; exists {
		return false, nil
	}
	return true, st.s.PutModuleData(ctx, st.module, key, value)
}

func (st store) Take(ctx context.Context, key string) ([]byte, bool, error) {
	if st.s == nil {
		return nil, false, ErrUnavailable
	}
	if a, ok := st.s.(domain.AtomicModuleDataStore); ok {
		return a.TakeModuleData(ctx, st.module, key)
	}
	all, err := st.s.ListModuleData(ctx, st.module)
	if err != nil {
		return nil, false, err
	}
	v, ok := all[key]
	if !ok {
		return nil, false, nil
	}
	if err := st.s.DeleteModuleData(ctx, st.module, key); err != nil {
		return nil, false, err
	}
	return v, true, nil
}
