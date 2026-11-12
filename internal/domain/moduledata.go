package domain

import "context"

// ModuleDataStore keeps modules' cluster-wide records (users, policies),
// by module and key. Values are opaque to db-branch.
type ModuleDataStore interface {
	ListModuleData(ctx context.Context, module string) (map[string][]byte, error)
	PutModuleData(ctx context.Context, module, key string, value []byte) error
	// DeleteModuleData succeeds when the key is already gone.
	DeleteModuleData(ctx context.Context, module, key string) error
}
