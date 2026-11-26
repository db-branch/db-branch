package domain

import (
	"context"
	"errors"
)

// Module data quotas per module (shared between usecase validation and atomic leader enforcement).
const (
	MaxModuleKey     = 256
	MaxModuleValue   = 64 << 10
	MaxModuleRecords = 10000
	MaxModuleTotal   = 8 << 20
)

// ErrModuleDataTooLarge reports a key, value, or per-module storage quota violation.
var ErrModuleDataTooLarge = errors.New("module data over its limit")

// ErrUnsupported reports a route or operation that a mixed-version peer does not implement yet.
var ErrUnsupported = errors.New("operation unsupported by peer")

// ModuleDataStore keeps modules' cluster-wide records (users, policies),
// by module and key. Values are opaque to db-branch.
type ModuleDataStore interface {
	ListModuleData(ctx context.Context, module string) (map[string][]byte, error)
	PutModuleData(ctx context.Context, module, key string, value []byte) error
	// DeleteModuleData succeeds when the key is already gone.
	DeleteModuleData(ctx context.Context, module, key string) error
}

// AtomicModuleDataStore is an optional extension of ModuleDataStore providing
// leader-linearizable reads and atomic one-time operations.
type AtomicModuleDataStore interface {
	ModuleDataStore
	GetModuleData(ctx context.Context, module, key string) ([]byte, bool, error)
	PutIfAbsentModuleData(ctx context.Context, module, key string, value []byte) (bool, error)
	TakeModuleData(ctx context.Context, module, key string) ([]byte, bool, error)
}
