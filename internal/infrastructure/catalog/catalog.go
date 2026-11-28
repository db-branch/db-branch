// Package catalog is the list of downloadable runtimes, pinned by SHA-256.
//
// Each entry is added by a maintainer after verifying the upstream signature
// (MySQL: the .asc file signed by MySQL Release Engineering; PostgreSQL: the
// PGDG InRelease signature, whose Packages index lists the .deb digests).
// Nodes then only need to check the digest.
//
// Runtimes that link the system libraries (the PostgreSQL .debs) list one
// artifact set per distribution under "distros"; the others (the MySQL
// tarballs) give their artifact directly and run anywhere. A platform is
// "arch" or "arch/distro" (see domain.Platform).
package catalog

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"golang.org/x/mod/semver"
)

//go:embed runtimes.json
var builtin []byte

// Catalog implements domain.RuntimeCatalog.
type Catalog struct {
	artifacts map[domain.RuntimeRef]map[string]entry
}

// entry is what a runtime offers for one architecture: one set of archives
// for every OS, or one per distribution.
type entry struct {
	any     []domain.Artifact
	distros map[string][]domain.Artifact
}

var _ domain.RuntimeCatalog = (*Catalog)(nil)

type file struct {
	Runtimes []struct {
		Engine    string                  `json:"engine"`
		Version   string                  `json:"version"`
		Artifacts map[string]artifactJSON `json:"artifacts"`
	} `json:"runtimes"`
}

type artifactJSON struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Parts lists several archives (e.g. the .debs of a PostgreSQL build)
	// in place of URL/SHA256/Size.
	Parts []artifactJSON `json:"parts"`
	// Distros gives the archives per distribution ("ubuntu-24.04") in place
	// of all of the above.
	Distros map[string]artifactJSON `json:"distros"`
}

func (a artifactJSON) parts() []artifactJSON {
	if len(a.Parts) > 0 {
		return a.Parts
	}
	return []artifactJSON{a}
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Default returns the catalog built into the binary.
func Default() (*Catalog, error) { return Parse(builtin) }

// Parse reads a catalog document.
func Parse(data []byte) (*Catalog, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("runtime catalog: %w", err)
	}
	c := &Catalog{artifacts: map[domain.RuntimeRef]map[string]entry{}}
	for _, rt := range f.Runtimes {
		ref, err := domain.ParseRuntimeRef(rt.Engine + "@" + rt.Version)
		if err != nil {
			return nil, fmt.Errorf("runtime catalog: %w", err)
		}
		if _, dup := c.artifacts[ref]; dup {
			return nil, fmt.Errorf("runtime catalog: %s listed twice", ref)
		}
		c.artifacts[ref] = map[string]entry{}
		for arch, art := range rt.Artifacts {
			e, err := parseEntry(ref, arch, art)
			if err != nil {
				return nil, err
			}
			c.artifacts[ref][arch] = e
		}
	}
	return c, nil
}

func parseEntry(ref domain.RuntimeRef, arch string, art artifactJSON) (entry, error) {
	if len(art.Distros) == 0 {
		parts, err := parseParts(ref, arch, art)
		return entry{any: parts}, err
	}
	if art.URL != "" || art.SHA256 != "" || len(art.Parts) > 0 {
		return entry{}, fmt.Errorf("runtime catalog: %s/%s: give either distros or an artifact, not both", ref, arch)
	}
	e := entry{distros: map[string][]domain.Artifact{}}
	for distro, d := range art.Distros {
		if len(d.Distros) > 0 {
			return entry{}, fmt.Errorf("runtime catalog: %s/%s/%s: distros cannot nest", ref, arch, distro)
		}
		parts, err := parseParts(ref, arch+"/"+distro, d)
		if err != nil {
			return entry{}, err
		}
		e.distros[distro] = parts
	}
	return e, nil
}

func parseParts(ref domain.RuntimeRef, platform string, art artifactJSON) ([]domain.Artifact, error) {
	var out []domain.Artifact
	for n, a := range art.parts() {
		if !strings.HasPrefix(a.URL, "https://") {
			return nil, fmt.Errorf("runtime catalog: %s/%s: url must use https", ref, platform)
		}
		if !sha256Pattern.MatchString(a.SHA256) {
			return nil, fmt.Errorf("runtime catalog: %s/%s: sha256 must be 64 lowercase hex digits", ref, platform)
		}
		out = append(out, domain.Artifact{URL: a.URL, SHA256: a.SHA256, Size: a.Size, Ref: ref, Arch: platform, Part: n})
	}
	return out, nil
}

// pick returns the archives of e for distro.
func (e entry) pick(ref domain.RuntimeRef, distro string) ([]domain.Artifact, error) {
	if e.distros == nil {
		return e.any, nil
	}
	supported := make([]string, 0, len(e.distros))
	for d := range e.distros {
		supported = append(supported, d)
	}
	sort.Strings(supported)
	if distro == "" {
		return nil, fmt.Errorf("%s: the operating system could not be detected (/etc/os-release); supported: %s: %w", ref, strings.Join(supported, ", "), domain.ErrUnsupportedDistro)
	}
	parts, ok := e.distros[distro]
	if !ok {
		return nil, fmt.Errorf("%s: %s is not supported; supported: %s: %w", ref, distro, strings.Join(supported, ", "), domain.ErrUnsupportedDistro)
	}
	return parts, nil
}

// Lookup returns the archives of ref for platform ("arch" or "arch/distro").
func (c *Catalog) Lookup(ref domain.RuntimeRef, platform string) ([]domain.Artifact, error) {
	arch, distro := domain.SplitPlatform(platform)
	e, ok := c.artifacts[ref][arch]
	if !ok {
		return nil, domain.ErrRuntimeNotFound
	}
	parts, err := e.pick(ref, distro)
	if err != nil {
		return nil, err
	}
	out := append([]domain.Artifact(nil), parts...)
	for i := range out {
		out[i].Arch = platform
	}
	return out, nil
}

// Available lists the versions of engine offered for platform, newest first.
func (c *Catalog) Available(engine domain.Engine, platform string) []domain.RuntimeRef {
	var refs []domain.RuntimeRef
	for ref := range c.artifacts {
		if ref.Engine != engine {
			continue
		}
		if _, err := c.Lookup(ref, platform); err == nil {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return NewerVersion(refs[i].Version, refs[j].Version) })
	return refs
}

func toSemver(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if semver.IsValid(v) {
		return v, true
	}
	// Expand shorthand MAJOR-pre or MAJOR.MINOR-pre (accepted by domain.ParseRuntimeRef)
	// into MAJOR.0.0-pre / MAJOR.MINOR.0-pre for golang.org/x/mod/semver.
	if idx := strings.IndexAny(v, "-+"); idx > 1 {
		core, rest := v[:idx], v[idx:]
		switch strings.Count(core, ".") {
		case 0:
			v = core + ".0.0" + rest
		case 1:
			v = core + ".0" + rest
		}
		if semver.IsValid(v) {
			return v, true
		}
	}
	return "", false
}

// NewerVersion reports whether version a sorts before b (newest first) using SemVer 2.0,
// falling back to dot-separated numeric comparison for non-SemVer strings.
func NewerVersion(a, b string) bool {
	sa, oka := toSemver(a)
	sb, okb := toSemver(b)
	if oka && okb {
		if c := semver.Compare(sa, sb); c != 0 {
			return c > 0
		}
		return a > b
	}
	if oka != okb {
		return oka
	}
	return compareNumeric(a, b)
}

func compareNumeric(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, erra := strconv.Atoi(pa[i])
		nb, errb := strconv.Atoi(pb[i])
		if erra != nil || errb != nil {
			if (erra == nil) != (errb == nil) {
				return erra == nil
			}
			if pa[i] != pb[i] {
				return pa[i] > pb[i]
			}
			continue
		}
		if na != nb {
			return na > nb
		}
	}
	if len(pa) != len(pb) {
		return len(pa) > len(pb)
	}
	return a > b
}
