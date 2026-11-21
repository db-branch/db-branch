// Package rtmodule implements local runtime module installation, listing,
// removal, and Ed25519 trust key management use cases.
package rtmodule

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"

	"github.com/db-branch/db-branch/internal/domain"
)

const (
	// DefaultMaxDownloadBytes is the maximum size of a downloaded release archive (100 MiB).
	DefaultMaxDownloadBytes int64 = 100 << 20
)

var (
	// ErrBadSignature is returned when a module signature is missing, malformed, or fails verification.
	ErrBadSignature = errors.New("runtime module signature is not valid")
	// ErrSourceMismatch is returned when an upgrade targets a different GitHub repository than the installed module's provenance.
	ErrSourceMismatch = errors.New("runtime module source repository mismatch")
)

// ReleaseAsset is the metadata of one downloadable asset on a GitHub Release.
type ReleaseAsset struct {
	Name        string
	DownloadURL string
	Size        int64
}

// Release is a fetched GitHub Release.
type Release struct {
	Tag        string
	Prerelease bool
	Draft      bool
	Assets     []ReleaseAsset
}

// ReleaseSource fetches GitHub Release metadata and downloads release assets.
type ReleaseSource interface {
	GetRelease(ctx context.Context, ref domain.GitHubRef) (Release, error)
	DownloadAsset(ctx context.Context, asset ReleaseAsset, maxBytes int64, dst io.Writer) (int64, error)
}

// ModuleSource records the provenance of an installed runtime module.
type ModuleSource struct {
	Repo  string
	Tag   string
	KeyID string
}

// ExtractedModule holds the validated contents of an unpacked module archive in a staging directory.
type ExtractedModule struct {
	StageDir        string
	ManifestRaw     []byte
	SignatureRaw    []byte
	CandidateKeyID  string
	CandidateKeyRaw []byte
	WriteSource     func(src ModuleSource) error
	Cleanup         func()
}

// ArchiveExtractor safely unpacks a module .tar.gz archive into a staging directory.
type ArchiveExtractor interface {
	ExtractToStaging(ctx context.Context, archivePath string, engine string) (*ExtractedModule, error)
}

// TrustKey is a public key entry stored in the trusted keys store.
type TrustKey struct {
	KeyID       string
	Fingerprint domain.Fingerprint
	PublicKey   ed25519.PublicKey
	Err         error
}

// KeyStore manages trusted Ed25519 public keys.
type KeyStore interface {
	Get(keyID string) (TrustKey, error)
	List() ([]TrustKey, error)
	Add(keyID string, pub ed25519.PublicKey) (TrustKey, error)
	Remove(keyID string) error
}

// VerifiedModule is a runtime module checked against the loader verification rules.
type VerifiedModule struct {
	Engine   string
	Manifest domain.RuntimeManifest
	KeyID    string
	Signed   bool
	Warning  string
	Err      error
}

// Verifier parses signatures/keys and verifies a staged module using strict loader rules.
type Verifier interface {
	ParseSignature(sigRaw []byte) (keyID string, sig []byte, err error)
	ParsePublicKey(keyRaw []byte) (pub ed25519.PublicKey, fingerprint domain.Fingerprint, err error)
	VerifyWithKey(pub ed25519.PublicKey, manifestRaw, sig []byte) error
	VerifyStaging(stageDir, engine string) (VerifiedModule, error)
}

// ModuleStore manages locking, reading, listing, committing, and removing installed runtime modules.
type ModuleStore interface {
	Lock(ctx context.Context) (unlock func(), err error)
	Get(engine string) (VerifiedModule, error)
	List() []VerifiedModule
	Commit(stageDir, engine string, upgrade bool) error
	Remove(engine string) error
	ReadSource(engine string) (ModuleSource, error)
}

// SystemUsers ensures the isolated OS system user for a runtime module exists.
type SystemUsers interface {
	EnsureSystemUser(ctx context.Context, username string) (created bool, uid, gid uint32, err error)
}

// UsageChecker reports bases and branches currently using an engine.
type UsageChecker interface {
	InUse(ctx context.Context, engine domain.Engine) (bases []string, branches []string, err error)
}

// TrustPrompter obtains explicit operator approval for an unknown signing key.
type TrustPrompter interface {
	ConfirmTrust(ctx context.Context, keyID string, fingerprint domain.Fingerprint, ref domain.GitHubRef, tag string) (bool, error)
}

// AgentRestarter restarts the local db-branch agent after module changes.
type AgentRestarter interface {
	Restart(ctx context.Context) error
}

// TempFile is the minimal handle needed for a temporary archive download file (*os.File satisfies it).
type TempFile interface {
	io.Writer
	Name() string
	Close() error
}
