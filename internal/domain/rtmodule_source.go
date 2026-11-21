package domain

import (
	"cmp"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Domain errors for runtime module installation and trust management.
var (
	// ErrAlreadyInstalled is returned when installing a runtime module that already exists without --upgrade.
	ErrAlreadyInstalled = errors.New("runtime module already installed")
	// ErrDowngradeRefused is returned when an upgrade target is not newer than the installed version.
	ErrDowngradeRefused = errors.New("runtime module downgrade refused")
	// ErrKeyUntrusted is returned when a signing key is not in the trusted keys directory and not approved.
	ErrKeyUntrusted = errors.New("signing key is not trusted")
	// ErrKeyExists is returned when adding a trusted key whose key_id already exists.
	ErrKeyExists = errors.New("trusted key already exists")
	// ErrKeyMismatch is returned when a public key does not match the expected fingerprint.
	ErrKeyMismatch = errors.New("signing key fingerprint mismatch")
	// ErrUnsafeArchive is returned when a runtime module archive violates safety checks.
	ErrUnsafeArchive = errors.New("unsafe runtime module archive")
	// ErrInvalidRef is returned when a runtime module reference, asset name, fingerprint or version is invalid.
	ErrInvalidRef = errors.New("invalid runtime module reference")
)

const githubRefPrefix = "github.com/"

var githubSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// GitHubRef identifies a GitHub repository and optional release tag
// ("github.com/<owner>/<repo>[@<tag>]"). Empty Tag means the latest release.
type GitHubRef struct {
	Owner string
	Repo  string
	Tag   string
}

// String formats ref in canonical "github.com/<owner>/<repo>[@<tag>]" form.
func (r GitHubRef) String() string {
	base := githubRefPrefix + r.Owner + "/" + r.Repo
	if r.Tag == "" {
		return base
	}
	return base + "@" + r.Tag
}

// githubTagPattern keeps a tag a plain path segment: no '%', '/', '?', '#' or leading symbol.
var githubTagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// ParseGitHubRef strictly parses "github.com/<owner>/<repo>[@<tag>]".
func ParseGitHubRef(raw string) (GitHubRef, error) {
	bad := func(reason string) (GitHubRef, error) {
		return GitHubRef{}, fmt.Errorf("%w %q: %s", ErrInvalidRef, raw, reason)
	}
	if raw == "" || hasControl(raw) || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return bad("must not be empty or contain whitespace or control characters")
	}
	rest, ok := strings.CutPrefix(raw, githubRefPrefix)
	if !ok {
		return bad("must start with github.com/")
	}
	repoPath, tag, hasTag := strings.Cut(rest, "@")
	if hasTag {
		if !githubTagPattern.MatchString(tag) || strings.Contains(tag, "..") {
			return bad("tag must match [A-Za-z0-9][A-Za-z0-9._+-]* without '..'")
		}
	}
	owner, repo, ok := strings.Cut(repoPath, "/")
	if !ok || strings.Contains(repo, "/") {
		return bad("want github.com/<owner>/<repo>[@<tag>]")
	}
	if !validGitHubSegment(owner) || !validGitHubSegment(repo) {
		return bad("owner and repo must match [A-Za-z0-9._-] without '..'")
	}
	return GitHubRef{Owner: owner, Repo: repo, Tag: tag}, nil
}

func validGitHubSegment(s string) bool {
	return githubSegmentPattern.MatchString(s) && s != "." && !strings.Contains(s, "..")
}

const moduleAssetSuffix = ".tar.gz"

var semverIdentPattern = regexp.MustCompile(`^[0-9A-Za-z-]+$`)

// ModuleAsset is the parsed metadata from a runtime module release asset filename
// ("db-branch-rt-<engine>_<version>_linux_<arch>.tar.gz").
type ModuleAsset struct {
	Engine  string
	Version string
	Arch    string
}

// ParseModuleAssetName parses and validates a runtime module release asset filename.
func ParseModuleAssetName(name string) (ModuleAsset, error) {
	bad := func(reason string) (ModuleAsset, error) {
		return ModuleAsset{}, fmt.Errorf("%w %q: %s", ErrInvalidRef, name, reason)
	}
	body, ok := strings.CutPrefix(name, RuntimeUserPrefix)
	if !ok {
		return bad("must start with " + RuntimeUserPrefix)
	}
	body, ok = strings.CutSuffix(body, moduleAssetSuffix)
	if !ok {
		return bad("must end with " + moduleAssetSuffix)
	}
	parts := strings.Split(body, "_")
	if len(parts) != 4 {
		return bad("want db-branch-rt-<engine>_<version>_linux_<arch>.tar.gz")
	}
	engine, version, osName, arch := parts[0], parts[1], parts[2], parts[3]
	if err := ValidRuntimeModuleEngine(engine); err != nil {
		return ModuleAsset{}, fmt.Errorf("%w %q: %w", ErrInvalidRef, name, err)
	}
	if _, err := parseSemVer(version); err != nil {
		return ModuleAsset{}, fmt.Errorf("%w %q: %v", ErrInvalidRef, name, err)
	}
	if osName != "linux" {
		return bad("only linux assets are supported")
	}
	if arch != "amd64" && arch != "arm64" {
		return bad("arch must be amd64 or arm64")
	}
	return ModuleAsset{Engine: engine, Version: version, Arch: arch}, nil
}

type semver struct {
	major, minor, patch uint64
	prerelease          []string
}

func parseSemVer(raw string) (semver, error) {
	coreAndPre, build, hasBuild := strings.Cut(raw, "+")
	if hasBuild {
		for _, id := range strings.Split(build, ".") {
			if !semverIdentPattern.MatchString(id) {
				return semver{}, fmt.Errorf("%w: invalid build metadata in %q", ErrInvalidRef, raw)
			}
		}
	}
	core, pre, hasPre := strings.Cut(coreAndPre, "-")
	var preIDs []string
	if hasPre {
		preIDs = strings.Split(pre, ".")
		for _, id := range preIDs {
			if !semverIdentPattern.MatchString(id) || (isAllDigits(id) && len(id) > 1 && id[0] == '0') {
				return semver{}, fmt.Errorf("%w: invalid prerelease in %q", ErrInvalidRef, raw)
			}
		}
	}
	nums := strings.Split(core, ".")
	if len(nums) != 3 {
		return semver{}, fmt.Errorf("%w: invalid semver %q", ErrInvalidRef, raw)
	}
	var parsed [3]uint64
	for i, n := range nums {
		if !isAllDigits(n) || (len(n) > 1 && n[0] == '0') {
			return semver{}, fmt.Errorf("%w: invalid version number in %q", ErrInvalidRef, raw)
		}
		v, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			return semver{}, fmt.Errorf("%w: version number overflow in %q", ErrInvalidRef, raw)
		}
		parsed[i] = v
	}
	return semver{major: parsed[0], minor: parsed[1], patch: parsed[2], prerelease: preIDs}, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

const (
	fingerprintPrefix = "sha256:"
	ed25519PubKeyLen  = 32
)

// Fingerprint is a validated SHA-256 public key fingerprint ("sha256:<64 lowercase hex>").
type Fingerprint struct {
	s string
}

// ParseFingerprint validates s as "sha256:" followed by 64 lowercase hex digits.
func ParseFingerprint(s string) (Fingerprint, error) {
	hexPart, ok := strings.CutPrefix(s, fingerprintPrefix)
	if !ok || !sha256Hex.MatchString(hexPart) {
		return Fingerprint{}, fmt.Errorf("%w: fingerprint must be %s<64 lowercase hex digits>", ErrInvalidRef, fingerprintPrefix)
	}
	return Fingerprint{s: s}, nil
}

// NewFingerprint computes the SHA-256 fingerprint of a 32-byte Ed25519 public key.
func NewFingerprint(pub []byte) (Fingerprint, error) {
	if len(pub) != ed25519PubKeyLen {
		return Fingerprint{}, fmt.Errorf("%w: public key must be %d bytes, got %d", ErrInvalidRef, ed25519PubKeyLen, len(pub))
	}
	sum := sha256.Sum256(pub)
	return Fingerprint{s: fingerprintPrefix + hex.EncodeToString(sum[:])}, nil
}

// String returns the canonical "sha256:<64hex>" representation.
func (f Fingerprint) String() string { return f.s }

// Equal compares two valid fingerprints in constant time. Zero fingerprints never match.
func (f Fingerprint) Equal(other Fingerprint) bool {
	if f.s == "" || other.s == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(f.s), []byte(other.s)) == 1
}

// CompareModuleVersion compares two SemVer 2.0.0 module versions (an optional
// leading "v" is allowed; build metadata is ignored per SemVer 2.0.0 §10).
// Returns -1 if a < b, 0 if a == b, or +1 if a > b.
func CompareModuleVersion(a, b string) (int, error) {
	va, err := parseSemVer(strings.TrimPrefix(a, "v"))
	if err != nil {
		return 0, err
	}
	vb, err := parseSemVer(strings.TrimPrefix(b, "v"))
	if err != nil {
		return 0, err
	}
	return compareSemVer(va, vb), nil
}

// CheckModuleUpgrade allows moving from current to next only when next > current.
func CheckModuleUpgrade(current, next string) error {
	c, err := CompareModuleVersion(next, current)
	if err != nil {
		return err
	}
	if c <= 0 {
		return fmt.Errorf("%w: %s -> %s", ErrDowngradeRefused, current, next)
	}
	return nil
}

func compareSemVer(a, b semver) int {
	if c := cmp.Compare(a.major, b.major); c != 0 {
		return c
	}
	if c := cmp.Compare(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmp.Compare(a.patch, b.patch); c != 0 {
		return c
	}
	switch {
	case len(a.prerelease) == 0 && len(b.prerelease) == 0:
		return 0
	case len(a.prerelease) == 0:
		return 1
	case len(b.prerelease) == 0:
		return -1
	}
	for i := 0; i < len(a.prerelease) && i < len(b.prerelease); i++ {
		if c := comparePrereleaseIdent(a.prerelease[i], b.prerelease[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.prerelease), len(b.prerelease))
}

func comparePrereleaseIdent(a, b string) int {
	aNum, bNum := isAllDigits(a), isAllDigits(b)
	switch {
	case aNum && bNum:
		if c := cmp.Compare(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case aNum:
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}
