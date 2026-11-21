// Package modcatalog adds the downloadable builds a runtime module lists
// (RuntimeProvider.Catalog) to the core's catalog. What a module lists is
// untrusted: every URL passes the SSRF check (https, no credentials, a public
// address), every digest must be a SHA-256, and the download goes through a
// client that connects to public addresses only (Cache).
package modcatalog

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Source is a runtime module's catalog.
type Source interface {
	Catalog(ctx context.Context) ([]domain.CatalogEntry, error)
}

// Catalog answers for the engines of Modules and passes the rest to Base.
type Catalog struct {
	Base    domain.RuntimeCatalog
	Modules map[domain.Engine]Source
	// Resolve looks up host names for the URL check.
	Resolve domain.Resolver
	// TTL is how long a module's list is kept before the module is asked again.
	TTL time.Duration
	Now func() time.Time

	mu    sync.Mutex
	cache map[domain.Engine]cached
}

type cached struct {
	at      time.Time
	entries []domain.CatalogEntry
}

var _ domain.RuntimeCatalog = (*Catalog)(nil)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// entries returns the module's list, kept for TTL.
func (c *Catalog) entries(ctx context.Context, e domain.Engine, src Source) ([]domain.CatalogEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit, ok := c.cache[e]; ok && c.Now().Sub(hit.at) < c.TTL {
		return hit.entries, nil
	}
	list, err := src.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("runtime module %s catalog: %w", e, err)
	}
	if c.cache == nil {
		c.cache = map[domain.Engine]cached{}
	}
	c.cache[e] = cached{at: c.Now(), entries: list}
	return list, nil
}

// matches: an entry for the exact platform, or for its architecture alone.
func matches(entryPlatform, platform string) bool {
	if entryPlatform == platform {
		return true
	}
	arch, _ := domain.SplitPlatform(platform)
	return entryPlatform == arch
}

// Lookup returns the archive of ref for platform. An entry for the exact
// platform wins over one for the architecture alone.
func (c *Catalog) Lookup(ref domain.RuntimeRef, platform string) ([]domain.Artifact, error) {
	src, ok := c.Modules[ref.Engine]
	if !ok {
		return c.Base.Lookup(ref, platform)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := c.entries(ctx, ref.Engine, src)
	if err != nil {
		return nil, err
	}
	var found *domain.CatalogEntry
	for i := range list {
		e := &list[i]
		if e.Version != ref.Version || !matches(e.Platform, platform) {
			continue
		}
		if found == nil || (e.Platform == platform && found.Platform != platform) {
			found = e
		}
	}
	if found == nil {
		return nil, domain.ErrRuntimeNotFound
	}
	if !sha256Hex.MatchString(found.SHA256) {
		return nil, fmt.Errorf("runtime module %s: %s: sha256 %q is not a SHA-256 digest", ref.Engine, ref, found.SHA256)
	}
	if err := domain.ValidateCatalogURL(ctx, found.URL, c.Resolve); err != nil {
		return nil, fmt.Errorf("runtime module %s: %s: %w", ref.Engine, ref, err)
	}
	return []domain.Artifact{{URL: found.URL, SHA256: found.SHA256, Size: found.Size, Ref: ref, Arch: platform}}, nil
}

// Available lists the versions of engine for platform, newest first.
func (c *Catalog) Available(engine domain.Engine, platform string) []domain.RuntimeRef {
	src, ok := c.Modules[engine]
	if !ok {
		return c.Base.Available(engine, platform)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := c.entries(ctx, engine, src)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var refs []domain.RuntimeRef
	for _, e := range list {
		if matches(e.Platform, platform) && !seen[e.Version] {
			seen[e.Version] = true
			refs = append(refs, domain.RuntimeRef{Engine: engine, Version: e.Version})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return before(refs[i].Version, refs[j].Version) })
	return refs
}

// before reports whether version a is newer than b (compared by number).
func before(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			return na > nb
		}
	}
	return len(pa) > len(pb)
}

// Cache downloads the archives of the modules' engines through External (a
// client that connects to public addresses only) and everything else through Core.
type Cache struct {
	Core, External domain.ArtifactCache
	// Engines says whether an engine comes from a module.
	Engines func(domain.Engine) bool
}

var _ domain.ArtifactCache = Cache{}

// Get returns the local copy of a.
func (c Cache) Get(ctx context.Context, a domain.Artifact) (string, error) {
	if c.Engines(a.Ref.Engine) {
		return c.External.Get(ctx, a)
	}
	return c.Core.Get(ctx, a)
}
