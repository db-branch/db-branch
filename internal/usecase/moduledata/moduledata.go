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
	MaxKey     = 256
	MaxValue   = 64 << 10
	MaxRecords = 10000
	MaxTotal   = 8 << 20
)

// ErrTooLarge: a key, a value or the module's total is over its limit.
var ErrTooLarge = errors.New("module data over its limit")

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

func (st store) Put(ctx context.Context, key string, value []byte) error {
	if st.s == nil {
		return ErrUnavailable
	}
	if key == "" || len(key) > MaxKey || len(value) > MaxValue {
		return fmt.Errorf("%w: key 1..%d bytes, value up to %d bytes", ErrTooLarge, MaxKey, MaxValue)
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
	return st.s.PutModuleData(ctx, st.module, key, value)
}

func (st store) Delete(ctx context.Context, key string) error {
	if st.s == nil {
		return ErrUnavailable
	}
	return st.s.DeleteModuleData(ctx, st.module, key)
}
