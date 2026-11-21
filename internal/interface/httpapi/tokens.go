package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// Tokens is the API token use case the API needs.
type Tokens interface {
	Create(ctx context.Context, name, scope string, ttl time.Duration) (string, domain.APIToken, error)
	// ListService lists the service tokens (not tied to a person).
	ListService(ctx context.Context) ([]domain.APIToken, error)
	Revoke(ctx context.Context, id string) error
	// Personal tokens act as the person who made them.
	CreatePersonal(ctx context.Context, p ext.Principal, name string, ttl time.Duration) (string, domain.APIToken, error)
	ListOwned(ctx context.Context, owner string) ([]domain.APIToken, error)
	RevokeOwned(ctx context.Context, owner, id string) error
}

type tokenJSON struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Scope     string  `json:"scope"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at"`
	// Token is the secret, only in the answer to its creation.
	Token string `json:"token,omitempty"`
}

func tokenView(t domain.APIToken) tokenJSON {
	return tokenJSON{ID: t.ID, Name: t.Name, Scope: t.Scope, CreatedAt: ts(t.CreatedAt), ExpiresAt: optTS(t.ExpiresAt)}
}

func (s *Server) withTokens(w http.ResponseWriter) bool {
	if s.Tokens == nil {
		writeError(w, http.StatusServiceUnavailable, "not_initialized", "tokens are not available on this node")
		return false
	}
	return true
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	if !s.withTokens(w) {
		return
	}
	list, err := s.Tokens.ListService(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]tokenJSON, 0, len(list))
	for _, t := range list {
		out = append(out, tokenView(t))
	}
	writeJSON(w, http.StatusOK, items(out))
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	if !s.withTokens(w) {
		return
	}
	var in struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
		TTL   string `json:"ttl"`
	}
	if !decode(w, r, &in) {
		return
	}
	ttl, err := parseTTL(in.TTL)
	if err != nil {
		fail(w, err)
		return
	}
	if in.Scope == "" {
		in.Scope = domain.ScopeBranches
	}
	secret, t, err := s.Tokens.Create(r.Context(), in.Name, in.Scope, ttl)
	if err != nil {
		fail(w, err)
		return
	}
	v := tokenView(t)
	v.Token = secret
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if !s.withTokens(w) {
		return
	}
	if err := s.Tokens.Revoke(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// queryBranch runs one statement on a ready branch as its own user.
func (s *Server) queryBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in struct {
		SQL     string `json:"sql"`
		MaxRows int    `json:"max_rows"`
	}
	if !decode(w, r, &in) {
		return
	}
	if s.Providers == nil {
		writeError(w, http.StatusServiceUnavailable, "not_initialized", "queries are not available on this node")
		return
	}
	b, err := svc.GetBranch(r.Context(), n)
	if err == nil && b.Status != domain.BranchReady {
		err = domain.ErrConflict
	}
	if err != nil {
		fail(w, err)
		return
	}
	p, spec, err := s.providerWith(r.Context(), b.Runtime.Engine, "query")
	if err != nil {
		fail(w, err)
		return
	}
	res, err := p.Query(r.Context(), b.Instance(spec), domain.QueryRequest{Host: s.hostOf(b.Node), User: b.User, Password: b.Password, SQL: in.SQL, MaxRows: in.MaxRows})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "query_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// providerWith finds the runtime provider of an engine and checks the engine
// declares the feature: a missing provider or a false capability is
// ErrFeatureNotSupported (409), never a quiet fallback.
func (s *Server) providerWith(ctx context.Context, e domain.Engine, feature string) (domain.RuntimeProvider, domain.EngineSpec, error) {
	p, ok := s.Providers.Provider(e)
	if !ok {
		return nil, domain.EngineSpec{}, domain.FeatureNotSupported(feature)
	}
	spec, err := p.Describe(ctx)
	if err != nil {
		return nil, domain.EngineSpec{}, err
	}
	return p, spec, spec.Capabilities.Require(feature)
}

// Personal tokens (account page): each signed-in person makes tokens that
// act as them, e.g. for an MCP client. A token cannot make another.

func (s *Server) listMyTokens(w http.ResponseWriter, r *http.Request) {
	if !s.withTokens(w) {
		return
	}
	list, err := s.Tokens.ListOwned(r.Context(), principalOf(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]tokenJSON, 0, len(list))
	for _, t := range list {
		out = append(out, tokenView(t))
	}
	writeJSON(w, http.StatusOK, items(out))
}

func (s *Server) createMyToken(w http.ResponseWriter, r *http.Request) {
	if !s.withTokens(w) {
		return
	}
	p := principalOf(r)
	if slices.ContainsFunc(p.Roles, func(role string) bool { return strings.HasPrefix(role, "scope:") }) {
		writeError(w, http.StatusForbidden, "forbidden", "sign in to the console to create a personal token")
		return
	}
	var in struct {
		Name string `json:"name"`
		TTL  string `json:"ttl"`
	}
	if !decode(w, r, &in) {
		return
	}
	ttl, err := parseTTL(in.TTL)
	if err != nil {
		fail(w, err)
		return
	}
	secret, t, err := s.Tokens.CreatePersonal(r.Context(), p, in.Name, ttl)
	if err != nil {
		fail(w, err)
		return
	}
	v := tokenView(t)
	v.Token = secret
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) revokeMyToken(w http.ResponseWriter, r *http.Request) {
	if !s.withTokens(w) {
		return
	}
	if err := s.Tokens.RevokeOwned(r.Context(), principalOf(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
