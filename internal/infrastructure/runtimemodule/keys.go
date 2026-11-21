package runtimemodule

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// TrustKey is a public key entry stored in the trusted keys directory.
type TrustKey struct {
	KeyID       string
	Fingerprint domain.Fingerprint
	PublicKey   ed25519.PublicKey
	// Err is set by List when a key file is unreadable, untrusted, or malformed.
	Err error
}

// KeyStore manages trusted Ed25519 public keys (<key_id>.pub) under Dir.
type KeyStore struct {
	// Dir holds <key_id>.pub files (default DefaultKeysDir).
	Dir string
	// Owner reports the uid owning a file (default: from the file system); false when unknown.
	Owner func(fs.FileInfo) (uint32, bool)
	// Chown sets file ownership (default: os.Chown).
	Chown func(name string, uid, gid int) error
	// Rand supplies randomness for temporary file names (default: crypto/rand.Reader).
	Rand io.Reader
}

// KeysDir returns Dir or DefaultKeysDir.
func (s KeyStore) KeysDir() string {
	if s.Dir != "" {
		return s.Dir
	}
	return DefaultKeysDir
}

func (s KeyStore) loader() Loader {
	return Loader{KeysDir: s.KeysDir(), Owner: s.Owner}
}

func applyChown(fn func(string, int, int) error, name string) error {
	if fn != nil {
		return fn(name, 0, 0)
	}
	return os.Chown(name, 0, 0)
}

func randomHex(r io.Reader, nBytes int) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	buf := make([]byte, nBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cErr := d.Close(); err == nil {
		err = cErr
	}
	return err
}

// Get reads and validates the trusted key for keyID.
// Returns domain.ErrNotFound when the key is not registered.
func (s KeyStore) Get(keyID string) (TrustKey, error) {
	if !KeyIDValid(keyID) {
		return TrustKey{}, fmt.Errorf("%w: invalid key_id %q", domain.ErrInvalidRef, keyID)
	}
	l := s.loader()
	if err := l.checkTrustDir(s.KeysDir()); err != nil {
		return TrustKey{}, err
	}
	return s.readKey(l, keyID)
}

func (s KeyStore) readKey(l Loader, keyID string) (TrustKey, error) {
	path := filepath.Join(s.KeysDir(), keyID+".pub")
	data, err := l.readTrustedFile(path, maxKeyBytes, ErrBadSignature)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TrustKey{}, fmt.Errorf("%w: trusted key %q", domain.ErrNotFound, keyID)
		}
		return TrustKey{}, err
	}
	pub, err := ParsePublicKey(data)
	if err != nil {
		return TrustKey{}, err
	}
	fp, _ := KeyFingerprint(pub) // ParsePublicKey guarantees 32 bytes
	return TrustKey{KeyID: keyID, Fingerprint: fp, PublicKey: pub}, nil
}

// List enumerates *.pub files in KeysDir in name order. A corrupt or untrusted
// key file is returned with Err set rather than aborting the listing.
func (s KeyStore) List() ([]TrustKey, error) {
	l := s.loader()
	if err := l.checkTrustDir(s.KeysDir()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(s.KeysDir())
	if err != nil {
		return nil, err
	}
	var out []TrustKey
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".pub") {
			continue
		}
		keyID := strings.TrimSuffix(name, ".pub")
		if !KeyIDValid(keyID) {
			out = append(out, TrustKey{KeyID: keyID, Err: fmt.Errorf("%w: invalid key_id %q", domain.ErrInvalidRef, keyID)})
			continue
		}
		k, err := s.readKey(l, keyID)
		if err != nil {
			out = append(out, TrustKey{KeyID: keyID, Err: err})
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

// Add writes a new trusted key atomically without ever overwriting an existing key.
// Writes to an exclusive temporary file (0644, root:root), fsyncs, and publishes
// via hard link so an existing key_id fails with domain.ErrKeyExists.
func (s KeyStore) Add(keyID string, pub ed25519.PublicKey) (TrustKey, error) {
	if !KeyIDValid(keyID) {
		return TrustKey{}, fmt.Errorf("%w: invalid key_id %q", domain.ErrInvalidRef, keyID)
	}
	fp, err := KeyFingerprint(pub)
	if err != nil {
		return TrustKey{}, err
	}
	if err := s.loader().checkTrustDir(s.KeysDir()); err != nil {
		return TrustKey{}, err
	}
	suffix, err := randomHex(s.Rand, 12)
	if err != nil {
		return TrustKey{}, err
	}
	tmpPath := filepath.Join(s.KeysDir(), ".tmp-"+suffix)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return TrustKey{}, err
	}
	defer func() { _ = os.Remove(tmpPath) }()

	payload := []byte(base64.StdEncoding.EncodeToString(pub) + "\n")
	if err := writeAndSyncFile(f, tmpPath, payload, 0o644, s.Chown); err != nil {
		return TrustKey{}, err
	}
	destPath := filepath.Join(s.KeysDir(), keyID+".pub")
	if err := os.Link(tmpPath, destPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return TrustKey{}, fmt.Errorf("%w: %s", domain.ErrKeyExists, keyID)
		}
		return TrustKey{}, err
	}
	if err := syncDir(s.KeysDir()); err != nil {
		return TrustKey{}, err
	}
	return TrustKey{
		KeyID:       keyID,
		Fingerprint: fp,
		PublicKey:   append(ed25519.PublicKey(nil), pub...),
	}, nil
}

// Remove deletes the trusted key file for keyID.
// Returns domain.ErrNotFound when the key does not exist.
func (s KeyStore) Remove(keyID string) error {
	if !KeyIDValid(keyID) {
		return fmt.Errorf("%w: invalid key_id %q", domain.ErrInvalidRef, keyID)
	}
	if err := s.loader().checkTrustDir(s.KeysDir()); err != nil {
		return err
	}
	path := filepath.Join(s.KeysDir(), keyID+".pub")
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: trusted key %q", domain.ErrNotFound, keyID)
		}
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUntrusted, path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDir(s.KeysDir())
}

func writeAndSyncFile(f *os.File, path string, data []byte, mode fs.FileMode, chown func(string, int, int) error) error {
	_, wErr := f.Write(data)
	var cErr, mErr, sErr error
	if wErr == nil {
		cErr = applyChown(chown, path)
	}
	if wErr == nil && cErr == nil {
		mErr = f.Chmod(mode)
	}
	if wErr == nil && cErr == nil && mErr == nil {
		sErr = f.Sync()
	}
	closeErr := f.Close()
	for _, err := range []error{wErr, cErr, mErr, sErr, closeErr} {
		if err != nil {
			return err
		}
	}
	return nil
}
