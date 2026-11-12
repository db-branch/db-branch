// Package authfile stores the admin credentials in a 0600 JSON file and hashes with argon2id.
package authfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"

	"github.com/db-branch/db-branch/internal/usecase/auth"
)

// Store implements auth.Store.
type Store struct{ Path string }

var _ auth.Store = Store{}

// Load reads the credentials, or returns auth.ErrNoCredentials.
func (s Store) Load() (auth.Credentials, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return auth.Credentials{}, auth.ErrNoCredentials
	}
	if err != nil {
		return auth.Credentials{}, err
	}
	var c auth.Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return auth.Credentials{}, fmt.Errorf("%s: %w", s.Path, err)
	}
	return c, nil
}

// Save writes the credentials atomically.
func (s Store) Save(c auth.Credentials) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Argon2 hashes with argon2id using the OWASP-recommended minimum (19 MiB, t=2, p=1).
type Argon2 struct{}

// Hash returns a 32-byte argon2id key.
func (Argon2) Hash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
}
