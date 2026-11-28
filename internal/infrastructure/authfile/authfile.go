// Package authfile stores the admin credentials in a 0600 JSON file and hashes with argon2id.
package authfile

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/db-branch/db-branch/internal/usecase/auth"
)

// Store implements auth.Store and auth.StampStore.
type Store struct{ Path string }

var (
	_ auth.Store      = Store{}
	_ auth.StampStore = Store{}
)

// Stamp returns file metadata that changes whenever the credentials file is
// replaced or removed, letting auth.Service skip redundant reads/JSON parses.
func (s Store) Stamp() (string, error) {
	info, err := os.Stat(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", auth.ErrNoCredentials
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%s", info.Size(), info.ModTime().UnixNano(), fileStampID(info)), nil
}

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

const (
	argon2Memory  uint32 = 19 * 1024
	argon2Time    uint32 = 2
	argon2Threads uint8  = 1
	argon2KeyLen  uint32 = 32
)

// Argon2 hashes with argon2id using the OWASP-recommended minimum (19 MiB, t=2, p=1)
// encoded in standard PHC string format.
type Argon2 struct{}

// Hash returns a PHC-formatted argon2id hash string as bytes.
func (a Argon2) Hash(password string, salt []byte) []byte {
	key := a.HashLegacy(password, salt)
	phc := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
	return []byte(phc)
}

// HashLegacy returns a 32-byte raw argon2id key for mixed-cluster compatibility.
func (Argon2) HashLegacy(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
}

// Verify checks password against either a PHC-formatted hash or a legacy raw
// 32-byte argon2id key with its separate salt.
func (a Argon2) Verify(password string, salt, hash []byte) bool {
	if len(hash) == 0 {
		return false
	}
	if bytes.HasPrefix(hash, []byte("$argon2id$")) {
		mem, t, p, parsedSalt, parsedKey, ok := parsePHC(string(hash))
		if !ok {
			return false
		}
		got := argon2.IDKey([]byte(password), parsedSalt, t, mem, p, uint32(len(parsedKey)))
		return subtle.ConstantTimeCompare(got, parsedKey) == 1
	}
	return subtle.ConstantTimeCompare(a.HashLegacy(password, salt), hash) == 1
}

// NeedsRehash reports whether hash is not a valid PHC string using current OWASP parameters.
func (Argon2) NeedsRehash(hash []byte) bool {
	mem, t, p, _, key, ok := parsePHC(string(hash))
	return !ok || mem != argon2Memory || t != argon2Time || p != argon2Threads || uint32(len(key)) != argon2KeyLen
}

func parsePHC(phc string) (mem, t uint32, p uint8, salt, key []byte, ok bool) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, false
	}
	verStr, hasVer := strings.CutPrefix(parts[2], "v=")
	ver, err := strconv.Atoi(verStr)
	if !hasVer || err != nil || ver != argon2.Version {
		return 0, 0, 0, nil, nil, false
	}
	paramParts := strings.Split(parts[3], ",")
	if len(paramParts) != 3 {
		return 0, 0, 0, nil, nil, false
	}
	mStr, hasM := strings.CutPrefix(paramParts[0], "m=")
	tStr, hasT := strings.CutPrefix(paramParts[1], "t=")
	pStr, hasP := strings.CutPrefix(paramParts[2], "p=")
	if !hasM || !hasT || !hasP {
		return 0, 0, 0, nil, nil, false
	}
	mv, err1 := strconv.ParseUint(mStr, 10, 32)
	tv, err2 := strconv.ParseUint(tStr, 10, 32)
	pv, err3 := strconv.ParseUint(pStr, 10, 8)
	if err1 != nil || err2 != nil || err3 != nil || mv < 8 || mv > 1<<20 || tv < 1 || tv > 64 || pv < 1 || pv > 64 || mv < 8*pv {
		return 0, 0, 0, nil, nil, false
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 4 {
		return 0, 0, 0, nil, nil, false
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return 0, 0, 0, nil, nil, false
	}
	return uint32(mv), uint32(tv), uint8(pv), salt, key, true
}
