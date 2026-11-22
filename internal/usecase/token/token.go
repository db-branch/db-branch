// Package token issues and checks API tokens: bearer credentials for API
// clients and MCP (LLM agents). A token is shown once; only its hash is kept.
package token

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// ErrInvalid is returned for a token request that cannot be issued.
var ErrInvalid = errors.New("invalid token request")

// Prefix marks db-branch tokens ("dbb_<id>_<secret>").
const Prefix = "dbb_"

// Service manages tokens.
type Service struct {
	Store domain.TokenStore
	Clock func() time.Time
	// Settle is how long to wait for a token this node does not know yet: it
	// may have just been created on another node. Zero (a single node) means
	// do not wait: an unknown token is simply unknown.
	Settle time.Duration

	settlers atomic.Int32
}

// MaxSettlers caps the requests waiting for an unknown token at once, so a
// flood of bad bearer tokens cannot hold the agent's goroutines and store reads.
const MaxSettlers = 16

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Create issues a token and returns its secret form (shown once) and record.
// ttl 0 never expires.
func (s *Service) Create(ctx context.Context, name, scope string, ttl time.Duration) (string, domain.APIToken, error) {
	return s.issue(ctx, domain.APIToken{Name: name, Scope: scope}, ttl)
}

// CreatePersonal issues a token that acts as p (e.g. for an MCP client):
// branches scope only, never with p's admin role. Tokens and anonymous or
// socket callers cannot have one.
func (s *Service) CreatePersonal(ctx context.Context, p ext.Principal, name string, ttl time.Duration) (string, domain.APIToken, error) {
	if p.ID == "" || p.ID == "anonymous" || p.ID == "local" || strings.HasPrefix(p.ID, "token:") || slices.ContainsFunc(p.Roles, func(r string) bool { return strings.HasPrefix(r, "scope:") }) {
		return "", domain.APIToken{}, fmt.Errorf("%w: sign in to the console to create a personal token", ErrInvalid)
	}
	owner := p.Name
	if owner == "" {
		owner = p.ID
	}
	return s.issue(ctx, domain.APIToken{Name: name, Scope: domain.ScopeBranches, Owner: p.ID, OwnerName: owner}, ttl)
}

func (s *Service) issue(ctx context.Context, t domain.APIToken, ttl time.Duration) (string, domain.APIToken, error) {
	name, scope := strings.TrimSpace(t.Name), t.Scope
	if name == "" || len(name) > 64 {
		return "", domain.APIToken{}, fmt.Errorf("%w: a name of up to 64 characters is required", ErrInvalid)
	}
	if scope != domain.ScopeFull && scope != domain.ScopeBranches {
		return "", domain.APIToken{}, fmt.Errorf("%w: scope must be %q or %q", ErrInvalid, domain.ScopeFull, domain.ScopeBranches)
	}
	if ttl < 0 {
		return "", domain.APIToken{}, fmt.Errorf("%w: negative ttl", ErrInvalid)
	}
	id, secret := random(6), random(24)
	t.ID, t.Name, t.Hash, t.CreatedAt = id, name, hash(secret), s.Clock().UTC().Truncate(time.Second)
	if ttl > 0 {
		exp := t.CreatedAt.Add(ttl)
		t.ExpiresAt = &exp
	}
	if err := s.Store.PutToken(ctx, t); err != nil {
		return "", domain.APIToken{}, err
	}
	return Prefix + id + "_" + secret, t, nil
}

// List returns the tokens (hashes included; callers must not show them).
func (s *Service) List(ctx context.Context) ([]domain.APIToken, error) {
	return s.Store.ListTokens(ctx)
}

// Revoke deletes a token.
func (s *Service) Revoke(ctx context.Context, id string) error { return s.Store.DeleteToken(ctx, id) }

// ListService returns the service tokens (not tied to a person).
func (s *Service) ListService(ctx context.Context) ([]domain.APIToken, error) {
	return s.filter(ctx, func(t domain.APIToken) bool { return t.Owner == "" })
}

// ListOwned returns the personal tokens of owner (a principal ID).
func (s *Service) ListOwned(ctx context.Context, owner string) ([]domain.APIToken, error) {
	return s.filter(ctx, func(t domain.APIToken) bool { return t.Owner == owner })
}

func (s *Service) filter(ctx context.Context, keep func(domain.APIToken) bool) ([]domain.APIToken, error) {
	all, err := s.Store.ListTokens(ctx)
	out := []domain.APIToken{}
	for _, t := range all {
		if keep(t) {
			out = append(out, t)
		}
	}
	return out, err
}

// RevokeOwned deletes owner's token id (ErrNotFound if it is not theirs).
func (s *Service) RevokeOwned(ctx context.Context, owner, id string) error {
	mine, err := s.ListOwned(ctx, owner)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(mine, func(t domain.APIToken) bool { return t.ID == id }) {
		return fmt.Errorf("token %s: %w", id, domain.ErrNotFound)
	}
	return s.Store.DeleteToken(ctx, id)
}

// Authenticate implements ext.Authenticator for bearer tokens.
func (s *Service) Authenticate(ctx context.Context, c ext.Credentials) (ext.Principal, error) {
	rest, ok := strings.CutPrefix(c.Token, Prefix)
	if !ok {
		return ext.Principal{}, ext.ErrUnauthenticated
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok {
		return ext.Principal{}, ext.ErrUnauthenticated
	}
	t, found, err := s.find(ctx, id)
	if err != nil {
		return ext.Principal{}, err
	}
	if found {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(hash(secret))) != 1 {
			return ext.Principal{}, fmt.Errorf("%w: unknown token", ext.ErrUnauthenticated)
		}
		if t.ExpiresAt != nil && !s.Clock().Before(*t.ExpiresAt) {
			return ext.Principal{}, fmt.Errorf("%w: token %s expired", ext.ErrUnauthenticated, t.Name)
		}
		if t.Owner != "" {
			// A personal token acts as its owner, within its scope.
			return ext.Principal{ID: t.Owner, Name: t.OwnerName, Roles: []string{"scope:" + t.Scope}}, nil
		}
		return ext.Principal{ID: "token:" + t.ID, Name: t.Name, Roles: []string{"scope:" + t.Scope}}, nil
	}
	return ext.Principal{}, fmt.Errorf("%w: unknown token", ext.ErrUnauthenticated)
}

// find looks a token up, waiting up to Settle for one not replicated yet.
func (s *Service) find(ctx context.Context, id string) (domain.APIToken, bool, error) {
	deadline := time.Now().Add(s.Settle)
	waiting := false
	defer func() {
		if waiting {
			s.settlers.Add(-1)
		}
	}()
	for {
		list, err := s.Store.ListTokens(ctx)
		if err != nil {
			return domain.APIToken{}, false, err
		}
		for _, t := range list {
			if t.ID == id {
				return t, true, nil
			}
		}
		if !time.Now().Before(deadline) {
			return domain.APIToken{}, false, nil
		}
		if !waiting {
			if s.settlers.Add(1) > MaxSettlers {
				s.settlers.Add(-1)
				return domain.APIToken{}, false, nil
			}
			waiting = true
		}
		select {
		case <-ctx.Done():
			return domain.APIToken{}, false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Scoped is the OSS authorizer for token scopes: a "branches" token may
// work with branches and read the rest, nothing else.
type Scoped struct{}

// secretReads expose secrets (settings hold the licence key and the open
// branch password) or other people's access; branches tokens cannot.
var secretReads = map[string]bool{"setting.read": true, "token.read": true, "base.refresh.read": true, "route-user.read": true, "backup.read": true}

func (Scoped) Authorize(_ context.Context, p ext.Principal, action string, _ ext.Resource) error {
	for _, r := range p.Roles {
		if r != "scope:"+domain.ScopeBranches {
			continue
		}
		if strings.HasPrefix(action, "branch.") || (strings.HasSuffix(action, ".read") && !secretReads[action]) || action == "mcp.use" || action == "base.public" {
			return nil
		}
		return fmt.Errorf("%w: a %s-scoped token cannot %s", ext.ErrForbidden, domain.ScopeBranches, action)
	}
	return nil
}
