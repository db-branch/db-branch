// Package runtimemodule loads the external database engines installed under
// /usr/lib/db-branch/runtimes: it checks what is on disk (owner, permissions,
// digests) and the signature before the core trusts a manifest.
package runtimemodule

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/db-branch/db-branch/internal/domain"
)

const (
	defaultKeysDir   = "/etc/db-branch/trusted-keys"
	maxManifestBytes = 1 << 20  // 1 MiB
	maxSigBytes      = 64 << 10 // 64 KiB
	maxKeyBytes      = 4 << 10  // 4 KiB
)

var (
	// ErrUnsigned is returned for a module without a signature, unless unsigned modules are allowed.
	ErrUnsigned = errors.New("runtime module is not signed")
	// ErrBadSignature is returned for a signature that is not valid for the manifest and a trusted key.
	// It is never overridden by allowing unsigned modules: it means tampering, not absence.
	ErrBadSignature = errors.New("runtime module signature is not valid")
	// ErrDigestMismatch is returned when the binary or catalog differs from the manifest's SHA-256.
	ErrDigestMismatch = errors.New("runtime module file does not match the manifest digest")
	// ErrUntrusted is returned for a file the core must not run or read: not owned by root,
	// writable by others, a symbolic link, or not the kind of file it should be.
	ErrUntrusted = errors.New("runtime module file is not trusted")

	enginePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	keyIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// Loader reads installed runtime modules.
type Loader struct {
	// Root holds one directory per engine (default domain.ManifestRoot).
	Root string
	// KeysDir holds the trusted public keys, <key_id>.pub (default /etc/db-branch/trusted-keys).
	KeysDir string
	// AllowUnsigned lets a module without a signature load, with a warning.
	AllowUnsigned bool
	// Owner reports the uid owning a file (default: from the file system); false when unknown.
	Owner func(fs.FileInfo) (uint32, bool)
}

// Loaded is a runtime module that passed every check.
type Loaded struct {
	Manifest domain.RuntimeManifest
	Signed   bool
	// KeyID names the trusted key that verified the signature.
	KeyID string
	// Warning is set when the module loaded without a signature.
	Warning string
}

// Result is the outcome for one engine directory.
type Result struct {
	Engine string
	Loaded Loaded
	Err    error
}

type manifestSignature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// RootDir is Root or its default.
func (l Loader) RootDir() string {
	if l.Root != "" {
		return l.Root
	}
	return domain.ManifestRoot
}

// TrustedKeysDir is KeysDir or its default.
func (l Loader) TrustedKeysDir() string {
	if l.KeysDir != "" {
		return l.KeysDir
	}
	return defaultKeysDir
}

// Load checks and reads the module of one engine.
func (l Loader) Load(engine string) (Loaded, error) {
	if !enginePattern.MatchString(engine) {
		return Loaded{}, fmt.Errorf("invalid engine name %q", engine)
	}
	dir := filepath.Join(l.RootDir(), engine)
	for _, d := range []string{l.RootDir(), l.TrustedKeysDir(), dir} {
		if err := l.checkTrustDir(d); err != nil {
			return Loaded{}, err
		}
	}
	manPath := filepath.Join(dir, "manifest.json")
	manData, err := l.readTrustedFile(manPath, maxManifestBytes, ErrUntrusted)
	if err != nil {
		return Loaded{}, err
	}
	signed, keyID, warning, err := l.verifySignature(engine, manPath+".sig", manData)
	if err != nil {
		return Loaded{}, err
	}
	manifest, err := domain.ParseManifestIn(l.RootDir(), manData)
	if err != nil {
		return Loaded{}, err
	}
	if manifest.Engine != engine {
		return Loaded{}, fmt.Errorf("%w: manifest engine %q does not match directory %q", domain.ErrInvalidManifest, manifest.Engine, engine)
	}
	if err := l.verifyDigest(manifest.Binary, manifest.BinarySHA256, true); err != nil {
		return Loaded{}, err
	}
	if err := l.verifyDigest(manifest.Catalog, manifest.CatalogSHA256, false); err != nil {
		return Loaded{}, err
	}
	return Loaded{
		Manifest: manifest,
		Signed:   signed,
		KeyID:    keyID,
		Warning:  warning,
	}, nil
}

// LoadAll loads every engine directory under Root, in name order; a broken
// module does not stop the others. A missing Root is no engines.
func (l Loader) LoadAll() []Result {
	entries, err := os.ReadDir(l.RootDir())
	if err != nil {
		return nil
	}
	var results []Result
	for _, e := range entries {
		if !e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		loaded, err := l.Load(e.Name())
		results = append(results, Result{Engine: e.Name(), Loaded: loaded, Err: err})
	}
	return results
}

func (l Loader) ownerOf(fi fs.FileInfo) (uint32, bool) {
	if l.Owner != nil {
		return l.Owner(fi)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return st.Uid, true
	}
	return 0, false
}

func (l Loader) checkTrustInfo(path string, fi fs.FileInfo, wantDir, wantExec bool) error {
	mode := fi.Mode()
	if wantDir && !mode.IsDir() || !wantDir && !mode.IsRegular() {
		return fmt.Errorf("%w: %s has unexpected file type", ErrUntrusted, path)
	}
	special := fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	if wantDir {
		special = fs.ModeSetuid | fs.ModeSticky
	}
	if mode&special != 0 {
		return fmt.Errorf("%w: %s has unsafe mode bits (%s)", ErrUntrusted, path, mode)
	}
	if uid, ok := l.ownerOf(fi); !ok || uid != 0 {
		return fmt.Errorf("%w: %s is not owned by root", ErrUntrusted, path)
	}
	perm := mode.Perm()
	if perm&0o022 != 0 {
		return fmt.Errorf("%w: %s is writable by group or others (%04o)", ErrUntrusted, path, perm)
	}
	if wantExec && perm&0o111 == 0 {
		return fmt.Errorf("%w: %s is not executable (%04o)", ErrUntrusted, path, perm)
	}
	return nil
}

// openTrusted opens without following a symbolic link and without waiting
// (a FIFO or a device would block open(2) for ever; it is refused by its type below).
func (l Loader) openTrusted(path string, wantDir, wantExec bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fmt.Errorf("%w: %s is a symbolic link: %w", ErrUntrusted, path, err)
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = l.checkTrustInfo(path, fi, wantDir, wantExec)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (l Loader) checkTrustDir(path string) error {
	f, err := l.openTrusted(path, true, false)
	if err != nil {
		return err
	}
	return f.Close()
}

func (l Loader) readTrustedFile(path string, maxBytes int64, tooLargeErr error) ([]byte, error) {
	f, err := l.openTrusted(path, false, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err == nil && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", tooLargeErr, path, maxBytes)
	}
	return data, err
}

func (l Loader) verifyDigest(path, want string, wantExec bool) error {
	f, err := l.openTrusted(path, false, wantExec)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("%w: %s", ErrDigestMismatch, path)
	}
	return err
}

func decodeSignature(sigData []byte) (manifestSignature, error) {
	dec := json.NewDecoder(bytes.NewReader(sigData))
	dec.DisallowUnknownFields()
	var sf manifestSignature
	if err := dec.Decode(&sf); err != nil {
		return manifestSignature{}, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return manifestSignature{}, fmt.Errorf("%w: trailing data in signature file", ErrBadSignature)
	}
	return sf, nil
}

func (l Loader) verifySignature(engine, sigPath string, manifestData []byte) (bool, string, string, error) {
	sigData, err := l.readTrustedFile(sigPath, maxSigBytes, ErrBadSignature)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if !l.AllowUnsigned {
				return false, "", "", fmt.Errorf("%w: %s", ErrUnsigned, engine)
			}
			return false, "", fmt.Sprintf("runtime module %s is unsigned", engine), nil
		}
		return false, "", "", err
	}
	sf, err := decodeSignature(sigData)
	if err != nil {
		return false, "", "", err
	}
	if !keyIDPattern.MatchString(sf.KeyID) {
		return false, "", "", fmt.Errorf("%w: invalid key_id %q", ErrBadSignature, sf.KeyID)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sf.Signature)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false, "", "", fmt.Errorf("%w: invalid signature", ErrBadSignature)
	}
	keyPath := filepath.Join(l.TrustedKeysDir(), sf.KeyID+".pub")
	keyData, err := l.readTrustedFile(keyPath, maxKeyBytes, ErrBadSignature)
	if err != nil {
		if errors.Is(err, ErrUntrusted) || errors.Is(err, ErrBadSignature) {
			return false, "", "", err
		}
		return false, "", "", fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	pubBytes, err := base64.StdEncoding.DecodeString(strings.TrimRight(string(keyData), "\r\n"))
	if err != nil || len(pubBytes) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(pubBytes), manifestData, sigBytes) {
		return false, "", "", fmt.Errorf("%w: verification failed", ErrBadSignature)
	}
	return true, sf.KeyID, "", nil
}
