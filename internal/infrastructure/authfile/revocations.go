package authfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/auth"
)

// Revocations keeps the logged-out sessions of a single node in a 0600 JSON
// file (session id -> expiry, unix seconds), so a logout survives a restart.
// It implements auth.Revocations.
type Revocations struct {
	path string
	mu   sync.Mutex
	ids  map[string]int64
}

var _ auth.Revocations = (*Revocations)(nil)

// NewRevocations opens (lazily) the list stored at path. A missing or broken
// file reads as empty.
func NewRevocations(path string) *Revocations { return &Revocations{path: path} }

func (r *Revocations) load() {
	if r.ids != nil {
		return
	}
	r.ids = map[string]int64{}
	if b, err := os.ReadFile(r.path); err == nil {
		if json.Unmarshal(b, &r.ids) != nil || r.ids == nil {
			r.ids = map[string]int64{}
		}
	}
}

// Revoked reports whether session id was logged out.
func (r *Revocations) Revoked(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	_, ok := r.ids[id]
	return ok
}

// Revoke records the logout and drops the records of sessions that have expired.
func (r *Revocations) Revoke(id string, expires, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	for k, exp := range r.ids {
		if exp <= now.Unix() {
			delete(r.ids, k)
		}
	}
	r.ids[id] = expires.Unix()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(r.ids)
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
