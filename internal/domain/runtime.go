package domain

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// Engine is a database engine db-branch can run.
type Engine string

const (
	MySQL    Engine = "mysql"
	Postgres Engine = "postgres"
)

// ErrInvalidRuntime is returned for a malformed runtime reference such as "mysql@latest".
var ErrInvalidRuntime = errors.New("invalid runtime")

// RuntimeRef names one version of an engine, written "engine@version" (e.g. "mysql@8.4.13").
type RuntimeRef struct {
	Engine  Engine
	Version string
}

// major.minor[.patch] with an optional suffix such as "-custom".
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?(-[a-z0-9]+(\.[a-z0-9]+)*)?$`)

// ParseRuntimeRef parses "engine@version" for an engine of DefaultRuntimes.
func ParseRuntimeRef(s string) (RuntimeRef, error) { return DefaultRuntimes.Parse(s) }

func (r RuntimeRef) String() string { return string(r.Engine) + "@" + r.Version }

// Artifact is a downloadable runtime archive pinned by its SHA-256 digest.
type Artifact struct {
	URL    string
	SHA256 string
	Size   int64
	// Ref and Arch say what the archive is, so it can be asked for by name
	// (from the cluster leader) as well as by URL.
	Ref  RuntimeRef
	Arch string
	// Part numbers the archives of a runtime made of several (from 0).
	Part int
}

var (
	// ErrRuntimeNotFound is returned when the catalog has no artifact for a runtime.
	ErrRuntimeNotFound = errors.New("runtime not found")
	// ErrRuntimeInstalled is returned when installing a runtime that is already present.
	ErrRuntimeInstalled = errors.New("runtime already installed")
	// ErrRuntimeNotInstalled is returned when removing a runtime that is not present.
	ErrRuntimeNotInstalled = errors.New("runtime not installed")
)

// RuntimeCatalog lists the runtimes that can be downloaded.
type RuntimeCatalog interface {
	// Lookup returns the archives that make up ref for arch, in install order.
	Lookup(ref RuntimeRef, arch string) ([]Artifact, error)
	Available(engine Engine, arch string) []RuntimeRef
}

// ArtifactCache returns a local, digest-verified copy of an artifact, downloading it if needed.
type ArtifactCache interface {
	Get(ctx context.Context, a Artifact) (path string, err error)
}

// RuntimeStore holds the runtimes installed on this node.
type RuntimeStore interface {
	List(ctx context.Context) ([]RuntimeRef, error)
	Has(ctx context.Context, ref RuntimeRef) (bool, error)
	// Install unpacks the archives (tar.xz or .deb, told apart by content) into one runtime.
	Install(ctx context.Context, ref RuntimeRef, archives []string) error
	Remove(ctx context.Context, ref RuntimeRef) error
}

// MarshalText stores a RuntimeRef as "engine@version" (empty when unset).
func (r RuntimeRef) MarshalText() ([]byte, error) {
	if r == (RuntimeRef{}) {
		return []byte{}, nil
	}
	return []byte(r.String()), nil
}

// UnmarshalText validates the reference while decoding. An empty string is the unset RuntimeRef.
func (r *RuntimeRef) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*r = RuntimeRef{}
		return nil
	}
	parsed, err := ParseRuntimeRef(string(b))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// ErrUnsupportedDistro is returned when a runtime has distribution specific
// builds and none matches this node's operating system.
var ErrUnsupportedDistro = errors.New("operating system not supported")

// A platform names what a runtime archive is built for: the CPU architecture
// (amd64, arm64) and, for builds that link the system libraries, the
// distribution ("ubuntu-24.04"), written "amd64/ubuntu-24.04". Runtimes that
// run anywhere (the MySQL tarballs) are looked up by the architecture alone.

// Platform joins arch and distro ("" distro: the architecture alone).
func Platform(arch, distro string) string {
	if distro == "" {
		return arch
	}
	return arch + "/" + distro
}

// SplitPlatform is the inverse of Platform.
func SplitPlatform(platform string) (arch, distro string) {
	arch, distro, _ = strings.Cut(platform, "/")
	return arch, distro
}

// PeerPlatform completes the platform a cluster member asks for. Members
// older than the distribution aware catalog send the architecture alone;
// they only ran on Ubuntu 24.04.
func PeerPlatform(platform string) string {
	if _, distro := SplitPlatform(platform); distro != "" {
		return platform
	}
	return Platform(platform, "ubuntu-24.04")
}
