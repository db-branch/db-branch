package httpapi_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/interface/nodeapi"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/membership"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

type leanDecisionMatrix struct {
	TokenScopeCases   []leanTokenScopeRow   `json:"token_scope_cases"`
	HTTPCases         []leanHTTPRow         `json:"http_cases"`
	BranchReadCases   []leanBranchReadRow   `json:"branch_read_cases"`
	LicenseHasCases   []leanLicenseHasRow   `json:"license_has_cases"`
	LicenseStateCases []leanLicenseStateRow `json:"license_state_cases"`
}

type leanTokenScopeRow struct {
	Action          string `json:"action"`
	BranchesAllowed bool   `json:"branches_allowed"`
}

type leanBranchReadRow struct {
	Role      string `json:"role"`
	IsTokenID bool   `json:"is_token_id"`
	Ownership string `json:"ownership"`
	CanRead   bool   `json:"can_read"`
}

type leanHTTPRow struct {
	Channel    string `json:"channel"`
	AuthKind   string `json:"auth_kind"`
	Role       string `json:"role"`
	IsTokenID  bool   `json:"is_token_id"`
	SessState  string `json:"sess_state"`
	Endpoint   string `json:"endpoint"`
	Ownership  string `json:"ownership"`
	Allowed    bool   `json:"allowed"`
	DenyReason string `json:"deny_reason"`
}

type leanLicenseHasRow struct {
	Plan    string   `json:"plan"`
	Modules []string `json:"modules"`
	Feature string   `json:"feature"`
	Has     bool     `json:"has"`
}

type leanLicenseStateRow struct {
	MaxNodes   int    `json:"max_nodes"`
	Nodes      int    `json:"nodes"`
	NowDay     int    `json:"now_day"`
	ExpiresDay int    `json:"expires_day"`
	GraceDays  int    `json:"grace_days"`
	State      string `json:"state"`
	Usable     bool   `json:"usable"`
}

type leanBranchPolicy struct{}

func (leanBranchPolicy) CheckBranch(_ context.Context, _ ext.Principal, _ ext.BranchRequest) error {
	return nil
}

func (leanBranchPolicy) SharesBranches(_ context.Context, _ ext.Principal, owner string) bool {
	return owner == "sso:teammate"
}

type leanExtHandler struct{}

func (leanExtHandler) Serve(_ context.Context, _ ext.Request) (ext.Response, error) {
	return ext.Response{Status: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
}

type leanUploads struct{}

func (leanUploads) Save(io.Reader, string, int64) (domain.Upload, error) {
	return domain.Upload{ID: "u1", Name: "dump.sql", Path: "/tmp/u1"}, nil
}
func (leanUploads) Get(id string) (domain.Upload, error) {
	if id == "u1" {
		return domain.Upload{ID: "u1", Name: "dump.sql", Path: "/tmp/u1"}, nil
	}
	return domain.Upload{}, domain.ErrNotFound
}
func (leanUploads) Remove(string) error   { return nil }
func (leanUploads) Room() (int64, error)  { return 1 << 30, nil }

type leanTokenAuth struct{}

func (leanTokenAuth) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	switch c.Token {
	case "tok-local":
		return httpapi.LocalPrincipal, nil
	case "tok-admin":
		return ext.Principal{ID: "admin", Name: "admin", Roles: []string{"admin"}}, nil
	case "tok-sso-admin":
		return ext.Principal{ID: "sso:admin-user", Name: "SSOAdmin", Roles: []string{"admin"}}, nil
	case "tok-scope-full":
		return ext.Principal{ID: "token:full", Name: "ci-full", Roles: []string{"scope:full"}}, nil
	case "tok-scope-branches-svc":
		return ext.Principal{ID: "token:dev", Name: "ci-dev", Roles: []string{"scope:branches"}}, nil
	case "tok-scope-branches-personal":
		return ext.Principal{ID: "sso:alice", Name: "Alice", Roles: []string{"scope:branches"}}, nil
	case "tok-viewer":
		return ext.Principal{ID: "sso:viewer", Name: "Viewer", Roles: []string{"viewer"}}, nil
	case "tok-anon":
		return httpapi.Anonymous, nil
	case "tok-empty":
		return ext.Principal{ID: "sso:empty", Name: "EmptyUser", Roles: nil}, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

type leanMemTokens struct {
	svc *token.Service
}

func newLeanMemTokens() leanMemTokens {
	return leanMemTokens{svc: &token.Service{Store: &memTokenStore{}, Clock: func() time.Time { return t0 }}}
}

func (m leanMemTokens) Create(ctx context.Context, name, scope string, ttl time.Duration) (string, domain.APIToken, error) {
	return m.svc.Create(ctx, name, scope, ttl)
}
func (m leanMemTokens) ListService(ctx context.Context) ([]domain.APIToken, error) {
	return m.svc.ListService(ctx)
}
func (m leanMemTokens) Revoke(ctx context.Context, id string) error {
	return m.svc.Revoke(ctx, id)
}
func (m leanMemTokens) CreatePersonal(ctx context.Context, p ext.Principal, name string, ttl time.Duration) (string, domain.APIToken, error) {
	return m.svc.CreatePersonal(ctx, p, name, ttl)
}
func (m leanMemTokens) ListOwned(ctx context.Context, owner string) ([]domain.APIToken, error) {
	return m.svc.ListOwned(ctx, owner)
}
func (m leanMemTokens) RevokeOwned(ctx context.Context, owner, id string) error {
	return m.svc.RevokeOwned(ctx, owner, id)
}
func (m leanMemTokens) RevokeByOwner(ctx context.Context, owner string) error {
	return m.svc.RevokeByOwner(ctx, owner)
}

type leanCluster struct {
	leader string
}

func (c *leanCluster) Status(context.Context) (httpapi.ClusterStatus, error) {
	return httpapi.ClusterStatus{Mode: "cluster", Leader: c.leader, Protocol: 999}, nil
}
func (c *leanCluster) NewToken(context.Context, time.Duration) (string, time.Time, error) {
	return "join-tok", t0.Add(24 * time.Hour), nil
}
func (c *leanCluster) Forward(string) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), nil
}
func (c *leanCluster) NodeHost(string) string                          { return "" }
func (c *leanCluster) BranchUsage(string, string) (domain.Usage, bool) { return domain.Usage{}, false }
func (c *leanCluster) NodeCheckpoints(string) bool                     { return false }
func (c *leanCluster) AddRuntime(context.Context, domain.RuntimeRef) error {
	return nil
}
func (c *leanCluster) RemoveRuntime(context.Context, domain.RuntimeRef) error {
	return nil
}
func (c *leanCluster) RemoveNode(context.Context, string, bool) (membership.RemoveReport, error) {
	return membership.RemoveReport{}, nil
}

type leanRevocations struct {
	revoked map[string]bool
}

func (r *leanRevocations) Revoke(id string, _, _ time.Time) error {
	r.revoked[id] = true
	return nil
}

func (r *leanRevocations) Revoked(id string) bool { return r.revoked[id] }

type leanHarness struct {
	f             *fixture
	socketHandler http.Handler
	httpHandler   http.Handler
	nodeHandler   http.Handler
	clock         time.Time
	adminCookie     *http.Cookie
	adminCSRF       string
	ssoAdminCookie  *http.Cookie
	ssoAdminCSRF    string
	viewerCookie    *http.Cookie
	viewerCSRF    string
	mustCookie    *http.Cookie
	mustCSRF      string
	revokedCookie *http.Cookie
	revokedCSRF   string
	expiredCookie *http.Cookie
	expiredCSRF   string
	wrongGenCookie *http.Cookie
	wrongGenCSRF   string
	localUserCookie *http.Cookie
	localUserCSRF   string
	opSeq         atomic.Int64
}

func newLeanHarness(t *testing.T) *leanHarness {
	t.Helper()
	f := newFixture(t)
	rev := &leanRevocations{revoked: map[string]bool{}}
	now := t0
	h := &leanHarness{f: f, clock: now}

	// Set up auth service with controllable clock for session states.
	creds := &memCreds{}
	authSvc := &auth.Service{
		Store:       creds,
		Hasher:      sha{},
		Clock:       func() time.Time { return h.clock },
		Revocations: rev,
	}
	if err := authSvc.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	f.srv.Auth = authSvc
	f.srv.TokenAuth = leanTokenAuth{}
	f.srv.Tokens = newLeanMemTokens()
	f.srv.Providers = providersOf(&fakeQuery{})
	f.srv.Uploads = leanUploads{}
	f.srv.TLSCertPEM = []byte("CERT")
	f.srv.Backups = &fakeBackups{files: []domain.BackupFile{{Name: "b1.json.gz", Size: 10, TakenAt: t0}}}
	f.srv.Cluster = &leanCluster{leader: "node1"}
	f.srv.Ext = ext.Module{BranchPolicy: leanBranchPolicy{}}
	f.srv.ExtHandlers = map[string]ext.Handler{"mod": leanExtHandler{}}
	f.srv.ExtPublic = map[string][]string{"mod": {"/sso/"}}
	f.srv.Anonymous = func() (time.Duration, int) { return time.Hour, 10000 }
	f.srv.AnonymousRate = func() int { return 0 }

	// Public snapshot for anonymous branch creation.
	prod := f.branches.bases["prod"]
	prod.Snapshots[0].Public = true
	f.branches.bases["prod"] = prod

	// Seed branches covering every OwnerRel.
	f.branches.branches["b-shared"] = domain.Branch{Name: name("b-shared"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:teammate", OwnerName: "Teammate", Runtime: mysql84}
	f.branches.branches["b-other"] = domain.Branch{Name: name("b-other"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:other", OwnerName: "Other", Runtime: mysql84}
	f.branches.branches["b-noowner"] = domain.Branch{Name: name("b-noowner"), Base: name("prod"), Status: domain.BranchReady, Owner: "", OwnerName: "", Runtime: mysql84}
	f.branches.branches["b-open"] = domain.Branch{Name: name("b-open"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:other", OwnerName: "Other", Access: domain.AccessOpen, Runtime: mysql84}

	// Pre-issue session cookies covering every SessState.
	// 1. wrongGen session (issued at Gen 1 before password change)
	wgSess, err := authSvc.Begin("admin", "admin", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	h.wrongGenCookie = &http.Cookie{Name: "dbb_session", Value: wgSess.Token}
	h.wrongGenCSRF = wgSess.CSRF

	// Bump generation (SetPassword -> Gen 2).
	if err := authSvc.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}

	// 2. expired session (issued at t0, clock later moves past DefaultSessionTTL, Wait: easier to issue short TTL!)
	shortAuth := &auth.Service{
		Store:       creds,
		Hasher:      sha{},
		Clock:       func() time.Time { return h.clock },
		TTL:         func() time.Duration { return time.Minute },
		Revocations: rev,
	}
	expSess, err := shortAuth.Begin("admin", "admin", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	h.expiredCookie = &http.Cookie{Name: "dbb_session", Value: expSess.Token}
	h.expiredCSRF = expSess.CSRF

	// Advance clock by 2 minutes so expSess is expired, while remaining sessions are valid.
	h.clock = t0.Add(2 * time.Minute)

	// 3. valid admin session
	admSess, err := authSvc.Begin("admin", "admin", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	h.adminCookie = &http.Cookie{Name: "dbb_session", Value: admSess.Token}
	h.adminCSRF = admSess.CSRF

	// 3b. valid sso_admin session (non-built-in "admin" user ID with "admin" role)
	ssoAdmSess, err := authSvc.Begin("sso:admin-user", "SSOAdmin", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	h.ssoAdminCookie = &http.Cookie{Name: "dbb_session", Value: ssoAdmSess.Token}
	h.ssoAdminCSRF = ssoAdmSess.CSRF

	// 4. valid viewer session
	vSess, err := authSvc.Begin("sso:viewer", "Viewer", []string{"viewer"})
	if err != nil {
		t.Fatal(err)
	}
	h.viewerCookie = &http.Cookie{Name: "dbb_session", Value: vSess.Token}
	h.viewerCSRF = vSess.CSRF

	// 5. revoked session
	rSess, err := authSvc.Begin("admin", "admin", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := authSvc.Logout(rSess.Token); err != nil {
		t.Fatal(err)
	}
	h.revokedCookie = &http.Cookie{Name: "dbb_session", Value: rSess.Token}
	h.revokedCSRF = rSess.CSRF

	// 6. localUserSession (session whose User == "local")
	locSess, err := authSvc.Begin(httpapi.LocalPrincipal.ID, "local", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	h.localUserCookie = &http.Cookie{Name: "dbb_session", Value: locSess.Token}
	h.localUserCSRF = locSess.CSRF

	// 7. mustChange session (initial password sets MustChange = true; keep same clock/gen on separate creds? No, MustChange lives on Credentials!)
	// Note: MustChange is evaluated via a second server sharing the same routes when sessState == "must_change".
	h.socketHandler = f.srv.Handler(true)
	h.httpHandler = f.srv.Handler(false)
	h.nodeHandler = (&nodeapi.Server{API: h.socketHandler}).Handler()

	// Pre-seed operations for operationGet checks.
	for _, owner := range []string{"anonymous", "local", "sso:admin-user", "admin", "token:full", "token:dev", "sso:alice", "sso:viewer", "sso:empty"} {
		op, err := f.ops.StartAs(owner, "branch.create", dbbranchTarget(owner), func(context.Context, func(string)) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		f.waitOp(t, op.ID)
	}

	return h
}

func (h *leanHarness) mustChangeHandler(t *testing.T) (http.Handler, *http.Cookie, string) {
	t.Helper()
	f2 := newFixture(t)
	a2 := &auth.Service{Store: &memCreds{}, Hasher: sha{}, Clock: func() time.Time { return h.clock }}
	if err := a2.SetInitialPassword("initial-pass-1234"); err != nil {
		t.Fatal(err)
	}
	f2.srv.Auth = a2
	f2.srv.Backups = &fakeBackups{files: []domain.BackupFile{{Name: "b1.json.gz", Size: 10, TakenAt: t0}}}
	f2.srv.Cluster = &leanCluster{leader: "node1"}
	f2.srv.ExtHandlers = map[string]ext.Handler{"mod": leanExtHandler{}}
	f2.srv.ExtPublic = map[string][]string{"mod": {"/sso/"}}
	sess, err := a2.Login("admin", "initial-pass-1234")
	if err != nil {
		t.Fatal(err)
	}
	return f2.srv.Handler(false), &http.Cookie{Name: "dbb_session", Value: sess.Token}, sess.CSRF
}

func bearerForRole(role string, isTokenID bool) string {
	switch role {
	case "local_principal":
		return "tok-local"
	case "admin":
		return "tok-admin"
	case "sso_admin":
		return "tok-sso-admin"
	case "scope_full":
		return "tok-scope-full"
	case "scope_branches":
		if isTokenID {
			return "tok-scope-branches-svc"
		}
		return "tok-scope-branches-personal"
	case "viewer":
		return "tok-viewer"
	case "anonymous":
		return "tok-anon"
	case "empty":
		return "tok-empty"
	default:
		return ""
	}
}

func principalForRole(role string, isTokenID bool) ext.Principal {
	switch role {
	case "local_principal":
		return httpapi.LocalPrincipal
	case "admin":
		return ext.Principal{ID: "admin", Name: "admin", Roles: []string{"admin"}}
	case "sso_admin":
		return ext.Principal{ID: "sso:admin-user", Name: "SSOAdmin", Roles: []string{"admin"}}
	case "scope_full":
		return ext.Principal{ID: "token:full", Name: "ci-full", Roles: []string{"scope:full"}}
	case "scope_branches":
		if isTokenID {
			return ext.Principal{ID: "token:dev", Name: "ci-dev", Roles: []string{"scope:branches"}}
		}
		return ext.Principal{ID: "sso:alice", Name: "Alice", Roles: []string{"scope:branches"}}
	case "viewer":
		return ext.Principal{ID: "sso:viewer", Name: "Viewer", Roles: []string{"viewer"}}
	case "anonymous":
		return httpapi.Anonymous
	case "empty":
		return ext.Principal{ID: "sso:empty", Name: "EmptyUser", Roles: nil}
	default:
		return ext.Principal{}
	}
}

func (h *leanHarness) branchForRel(row leanHTTPRow) string {
	p := principalForRole(row.Role, row.IsTokenID)
	if row.Channel == "socket" {
		p = httpapi.LocalPrincipal
	} else if row.Channel == "public_http" && row.AuthKind == "session_cookie" && row.Role == "admin" {
		p = ext.Principal{ID: "admin", Name: "admin", Roles: []string{"admin"}}
	}
	h.f.branches.mu.Lock()
	defer h.f.branches.mu.Unlock()
	switch row.Ownership {
	case "own_by_id":
		bName := "b-own-id"
		h.f.branches.branches[bName] = domain.Branch{Name: name(bName), Base: name("prod"), Status: domain.BranchReady, Owner: p.ID, OwnerName: p.Name, Runtime: mysql84}
		return bName
	case "own_by_legacy_name":
		bName := "b-own-legacy"
		owner := p.Name
		if owner == "" {
			owner = p.ID
		}
		h.f.branches.branches[bName] = domain.Branch{Name: name(bName), Base: name("prod"), Status: domain.BranchReady, Owner: owner, OwnerName: "", Runtime: mysql84}
		return bName
	case "shared":
		h.f.branches.branches["b-shared"] = domain.Branch{Name: name("b-shared"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:teammate", OwnerName: "Teammate", Runtime: mysql84}
		return "b-shared"
	case "other":
		h.f.branches.branches["b-other"] = domain.Branch{Name: name("b-other"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:other", OwnerName: "Other", Runtime: mysql84}
		return "b-other"
	case "no_owner":
		h.f.branches.branches["b-noowner"] = domain.Branch{Name: name("b-noowner"), Base: name("prod"), Status: domain.BranchReady, Owner: "", OwnerName: "", Runtime: mysql84}
		return "b-noowner"
	case "open_branch":
		h.f.branches.branches["b-open"] = domain.Branch{Name: name("b-open"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:other", OwnerName: "Other", Access: domain.AccessOpen, Runtime: mysql84}
		return "b-open"
	case "unknown_branch":
		delete(h.f.branches.branches, "b-unknown")
		return "b-unknown"
	default:
		h.f.branches.branches["b-other"] = domain.Branch{Name: name("b-other"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:other", OwnerName: "Other", Runtime: mysql84}
		return "b-other"
	}
}

func (h *leanHarness) endpointMethodPathBody(ep, branch string, seq int64) (string, string, any) {
	uniqueName := fmt.Sprintf("cb-%d", seq)
	switch ep {
	case "healthz":
		return http.MethodGet, "/healthz", nil
	case "readyz":
		return http.MethodGet, "/readyz", nil
	case "tls_pem":
		return http.MethodGet, "/tls.pem", nil
	case "auth_methods":
		return http.MethodGet, "/v1/auth/methods", nil
	case "auth_login":
		return http.MethodPost, "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}
	case "auth_session":
		return http.MethodGet, "/v1/auth/session", nil
	case "auth_logout":
		return http.MethodPost, "/v1/auth/logout", nil
	case "auth_password":
		return http.MethodPut, "/v1/auth/password", map[string]any{"current_password": "correct horse battery", "new_password": "short"}
	case "auth_reset":
		return http.MethodPost, "/v1/auth/reset", nil
	case "branch_list":
		return http.MethodGet, "/v1/branches", nil
	case "branch_get":
		return http.MethodGet, "/v1/branches/" + branch, nil
	case "branch_dsn":
		return http.MethodGet, "/v1/branches/" + branch + "/dsn", nil
	case "branch_query":
		return http.MethodPost, "/v1/branches/" + branch + "/query", map[string]any{"sql": "SELECT 1"}
	case "branch_create":
		return http.MethodPost, "/v1/branches", map[string]any{"name": uniqueName, "base": "prod"}
	case "branch_reset":
		return http.MethodPost, "/v1/branches/" + branch + "/reset", nil
	case "branch_delete":
		return http.MethodDelete, "/v1/branches/" + branch, nil
	case "branch_extend":
		return http.MethodPatch, "/v1/branches/" + branch, map[string]any{"ttl": "1h"}
	case "branch_pause":
		return http.MethodPost, "/v1/branches/" + branch + "/pause", nil
	case "branch_resume":
		return http.MethodPost, "/v1/branches/" + branch + "/resume", nil
	case "branch_checkpoint_read":
		return http.MethodGet, "/v1/branches/" + branch + "/checkpoints", nil
	case "branch_checkpoint_create":
		return http.MethodPost, "/v1/branches/" + branch + "/checkpoints", map[string]any{"name": "cp1"}
	case "branch_checkpoint_delete":
		return http.MethodDelete, "/v1/branches/" + branch + "/checkpoints/cp1", nil
	case "branch_restore":
		return http.MethodPost, "/v1/branches/" + branch + "/checkpoints/cp1/restore", nil
	case "branch_upgrade":
		return http.MethodPost, "/v1/branches/" + branch + "/upgrade", map[string]any{"runtime": "mysql@8.4.14"}
	case "branch_fork":
		return http.MethodPost, "/v1/branches/" + branch + "/fork", map[string]any{"name": uniqueName}
	case "branch_merge":
		return http.MethodPost, "/v1/branches/" + branch + "/merge", map[string]any{"checkpoint": "cp1"}
	case "branch_recreate":
		return http.MethodPost, "/v1/branches/" + branch + "/recreate", nil
	case "base_list":
		return http.MethodGet, "/v1/bases", nil
	case "base_get":
		return http.MethodGet, "/v1/bases/prod", nil
	case "base_public":
		return http.MethodGet, "/v1/public/bases", nil
	case "base_create":
		return http.MethodPost, "/v1/bases", map[string]any{}
	case "base_describe":
		return http.MethodPatch, "/v1/bases/prod", map[string]any{"description": "d"}
	case "base_replicas":
		return http.MethodPut, "/v1/bases/prod/replicas", map[string]any{"nodes": []string{}}
	case "base_patch_anonymous":
		return http.MethodPatch, "/v1/bases/prod/anonymous", map[string]any{}
	case "base_delete":
		return http.MethodDelete, "/v1/bases/missing", nil
	case "base_snapshot":
		return http.MethodPost, "/v1/bases/prod/snapshots", nil
	case "base_mask":
		return http.MethodPost, "/v1/bases/prod/mask", map[string]any{}
	case "base_start":
		return http.MethodPost, "/v1/bases/prod/start", nil
	case "base_stop":
		return http.MethodPost, "/v1/bases/prod/stop", nil
	case "base_sql":
		return http.MethodPost, "/v1/bases/prod/sql", map[string]any{"sql": "SELECT 1"}
	case "base_load_path":
		return http.MethodPost, "/v1/bases/prod/load", map[string]any{"dump": "/tmp/dump.sql"}
	case "base_load_upload":
		return http.MethodPost, "/v1/bases/prod/load", map[string]any{"dump": "upload:u1"}
	case "base_refresh_read":
		return http.MethodGet, "/v1/bases/prod/refresh", nil
	case "base_refresh_write":
		return http.MethodPut, "/v1/bases/prod/refresh", map[string]any{}
	case "base_refresh_delete":
		return http.MethodDelete, "/v1/bases/prod/refresh", nil
	case "base_refresh_run":
		return http.MethodPost, "/v1/bases/prod/refresh/run", nil
	case "upload_create":
		return http.MethodPost, "/v1/uploads", nil
	case "upload_room":
		return http.MethodGet, "/v1/uploads/room", nil
	case "base_snapshot_patch":
		return http.MethodPatch, "/v1/bases/prod/snapshots/s1", map[string]any{"public": true}
	case "base_snapshot_delete":
		return http.MethodDelete, "/v1/bases/prod/snapshots/s1", nil
	case "route_user_read":
		return http.MethodGet, "/v1/route-users", nil
	case "route_user_write":
		return http.MethodPut, "/v1/route-users/u1", map[string]any{}
	case "route_user_delete":
		return http.MethodDelete, "/v1/route-users/u1", nil
	case "token_read":
		return http.MethodGet, "/v1/tokens", nil
	case "token_write":
		return http.MethodPost, "/v1/tokens", map[string]any{"name": "t1", "scope": "branches"}
	case "token_delete":
		return http.MethodDelete, "/v1/tokens/t1", nil
	case "account_preference_read":
		return http.MethodGet, "/v1/account/preferences", nil
	case "account_preference_write":
		return http.MethodPut, "/v1/account/preferences", map[string]any{"language": "ja"}
	case "account_token_read":
		return http.MethodGet, "/v1/account/tokens", nil
	case "account_token_create":
		return http.MethodPost, "/v1/account/tokens", map[string]any{"name": "pt1"}
	case "account_token_delete":
		return http.MethodDelete, "/v1/account/tokens/p1", nil
	case "mcp_use":
		return http.MethodPost, "/v1/mcp", map[string]any{}
	case "setting_read":
		return http.MethodGet, "/v1/settings", nil
	case "setting_write":
		return http.MethodPut, "/v1/settings/core", map[string]any{}
	case "license_read":
		return http.MethodGet, "/v1/license", nil
	case "license_verify":
		return http.MethodPost, "/v1/license/verify", map[string]any{}
	case "license_write":
		return http.MethodPut, "/v1/license", map[string]any{}
	case "license_renewal_read":
		return http.MethodGet, "/v1/license/renewal", nil
	case "license_renew":
		return http.MethodPost, "/v1/license/renew", nil
	case "module_read":
		return http.MethodGet, "/v1/modules", nil
	case "module_install":
		return http.MethodPost, "/v1/modules", map[string]any{}
	case "module_remove":
		return http.MethodDelete, "/v1/modules/audit", nil
	case "module_forget":
		return http.MethodPost, "/v1/modules/audit/forget", nil
	case "update_read":
		return http.MethodGet, "/v1/updates", nil
	case "update_check":
		return http.MethodPost, "/v1/updates/check", nil
	case "update_apply":
		return http.MethodPost, "/v1/updates", map[string]any{}
	case "update_cancel":
		return http.MethodDelete, "/v1/updates", nil
	case "runtime_read":
		return http.MethodGet, "/v1/runtimes", nil
	case "runtime_available":
		return http.MethodGet, "/v1/runtimes/available", nil
	case "runtime_install":
		return http.MethodPost, "/v1/runtimes", map[string]any{}
	case "runtime_remove":
		return http.MethodDelete, "/v1/runtimes/mysql/8.4.13", nil
	case "operation_list":
		return http.MethodGet, "/v1/operations", nil
	case "operation_get":
		return http.MethodGet, "/v1/operations/op-missing", nil
	case "event_read":
		return http.MethodGet, "/v1/events", nil
	case "capability_read":
		return http.MethodGet, "/v1/capabilities", nil
	case "cluster_read":
		return http.MethodGet, "/v1/cluster", nil
	case "node_metrics":
		return http.MethodGet, "/v1/nodes/node1/metrics", nil
	case "node_storage":
		return http.MethodGet, "/v1/nodes/node1/storage", nil
	case "metrics_read":
		return http.MethodGet, "/v1/metrics", nil
	case "cluster_remove":
		return http.MethodDelete, "/v1/cluster/nodes/n2", nil
	case "cluster_token":
		return http.MethodPost, "/v1/cluster/token", nil
	case "backup_read":
		return http.MethodGet, "/v1/backups", nil
	case "backup_create":
		return http.MethodPost, "/v1/backups", nil
	case "backup_download":
		return http.MethodGet, "/v1/backups/b1.json.gz", nil
	case "backup_restore":
		return http.MethodPost, "/v1/backups/restore", map[string]any{}
	case "ext_private_read":
		return http.MethodGet, "/v1/ext/mod/data", nil
	case "ext_private_write":
		return http.MethodPost, "/v1/ext/mod/data", map[string]any{}
	case "ext_public_read":
		return http.MethodGet, "/v1/ext/mod/sso/start", nil
	case "ext_public_write":
		return http.MethodPost, "/v1/ext/mod/sso/cb", map[string]any{}
	default:
		return "", "", nil
	}
}

func (h *leanHarness) dispatch(t *testing.T, row leanHTTPRow) (int, string) {
	t.Helper()
	seq := h.opSeq.Add(1)
	branch := h.branchForRel(row)
	method, path, body := h.endpointMethodPathBody(row.Endpoint, branch, seq)
	if method == "" {
		t.Fatalf("unmapped endpoint %q", row.Endpoint)
	}

	// Special case: must_change uses dedicated Auth server with MustChange=true.
	if row.Channel == "public_http" && row.AuthKind == "session_cookie" && row.SessState == "must_change" {
		mh, cookie, csrf := h.mustChangeHandler(t)
		if row.Endpoint == "auth_login" {
			body = map[string]any{"username": "admin", "password": "initial-pass-1234"}
		} else if row.Endpoint == "auth_password" {
			body = map[string]any{"current_password": "initial-pass-1234", "new_password": "short"}
		}
		resp, out := send(t, mh, method, path, body, withSession(cookie, csrf))
		return resp.StatusCode, errCode(out)
	}

	// Keep auth_reset and auth_logout side-effect-free for subsequent matrix rows.
	if row.Endpoint == "auth_reset" || row.Endpoint == "auth_logout" {
		iso := newLeanHarness(t)
		iso.socketHandler = iso.f.srv.Handler(true)
		iso.httpHandler = iso.f.srv.Handler(false)
		iso.nodeHandler = (&nodeapi.Server{API: iso.socketHandler}).Handler()
		return iso.dispatchIsolated(t, row, method, path, body)
	}

	return h.dispatchIsolated(t, row, method, path, body)
}

func (h *leanHarness) dispatchIsolated(t *testing.T, row leanHTTPRow, method, path string, body any) (int, string) {
	t.Helper()

	var handler http.Handler
	var mutate func(*http.Request)

	switch row.Channel {
	case "socket":
		handler = h.socketHandler
	case "node_forward":
		handler = h.nodeHandler
		path = nodeapi.APIPrefix + path
		mutate = func(r *http.Request) {
			r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
			switch row.AuthKind {
			case "missing_forward_headers":
				// Omit X-Db-Branch-Forwarded and X-Db-Branch-Principal to test nodeapi.forwardedAPI.
			case "malformed_forward":
				r.Header.Set("X-Db-Branch-Forwarded", "node2")
				r.Header.Set("X-Db-Branch-Principal", "not-json")
			default:
				r.Header.Set("X-Db-Branch-Forwarded", "node2")
				p := principalForRole(row.Role, row.IsTokenID)
				b, _ := json.Marshal(p)
				r.Header.Set("X-Db-Branch-Principal", string(b))
			}
		}
	case "public_http":
		handler = h.httpHandler
		switch row.AuthKind {
		case "none":
			// No credentials.
		case "bearer_token":
			tok := bearerForRole(row.Role, row.IsTokenID)
			mutate = bearer(tok)
		case "session_cookie":
			switch row.SessState {
			case "valid":
				switch row.Role {
				case "viewer":
					mutate = withSession(h.viewerCookie, h.viewerCSRF)
				case "sso_admin":
					mutate = withSession(h.ssoAdminCookie, h.ssoAdminCSRF)
				default:
					mutate = withSession(h.adminCookie, h.adminCSRF)
				}
			case "csrf_bad":
				mutate = withSession(h.adminCookie, "wrong-csrf")
			case "expired":
				mutate = withSession(h.expiredCookie, h.expiredCSRF)
			case "revoked":
				mutate = withSession(h.revokedCookie, h.revokedCSRF)
			case "wrong_gen":
				mutate = withSession(h.wrongGenCookie, h.wrongGenCSRF)
			case "local_user_session":
				mutate = withSession(h.localUserCookie, h.localUserCSRF)
			}
		}
	default:
		t.Fatalf("unknown channel %q", row.Channel)
	}

	if row.Endpoint == "event_read" {
		prevMutate := mutate
		mutate = func(r *http.Request) {
			if prevMutate != nil {
				prevMutate(r)
			}
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Millisecond)
			t.Cleanup(cancel)
			*r = *r.WithContext(ctx)
		}
	}

	// Avoid mutating f.branches during async branchCreate/branchFork by waiting on ops.
	resp, out := send(t, handler, method, path, body, mutate)
	if id, ok := out["id"].(string); ok && resp.StatusCode == http.StatusAccepted {
		h.f.waitOp(t, id)
	}
	return resp.StatusCode, errCode(out)
}

func TestLeanAuthz_DifferentialMatrix(t *testing.T) {
	path := filepath.Join("..", "..", "..", "formal", "lean", "decision_matrix.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lean decision matrix %s: %v", path, err)
	}
	var matrix leanDecisionMatrix
	if err := json.Unmarshal(raw, &matrix); err != nil {
		t.Fatalf("unmarshal matrix: %v", err)
	}
	if len(matrix.HTTPCases) == 0 || len(matrix.BranchReadCases) == 0 || len(matrix.LicenseHasCases) == 0 || len(matrix.LicenseStateCases) == 0 {
		t.Fatal("decision matrix has empty sections")
	}

	h := newLeanHarness(t)
	for i, row := range matrix.HTTPCases {
		status, code := h.dispatch(t, row)

		// Map Go HTTP response into Lean's (allowed, deny_reason) semantics.
		// Note: operation_get intentionally returns 404 for "op-missing" after Authorize passes,
		// and non-branch handlers may return 400/404/409/501/503 if stubbed minimally,
		// whereas 401 and 403 are authn/authz denials (and 404 on unknown_branch distinguishes Authorize).
		var gotAllowed bool
		var gotReason string
		switch {
		case status == http.StatusUnauthorized:
			gotAllowed = false
			gotReason = "unauthenticated"
		case status == http.StatusForbidden:
			gotAllowed = false
			if code == "" && row.AuthKind == "missing_forward_headers" {
				// nodeapi.forwardedAPI writes plain-text 403 ("forwarded headers required")
				gotReason = "forbidden"
			} else {
				gotReason = code
			}
		case status == http.StatusBadRequest && (row.Endpoint == "account_token_create" || row.Endpoint == "base_load_path"):
			gotAllowed = false
			gotReason = code
		case status == http.StatusNotFound && (row.Ownership == "unknown_branch" || row.Endpoint == "base_mask"):
			gotAllowed = false
			gotReason = "not_found"
		default:
			gotAllowed = true
			gotReason = "ok"
		}

		// Note on auth_logout with expired/revoked/wrongGen session:
		// Wait: requireSession blocks invalid sessions at 401 before logout runs.
		if gotAllowed != row.Allowed || gotReason != row.DenyReason {
			t.Fatalf("case[%d] %+v: go returned status=%d code=%q (allowed=%v reason=%q), lean expected allowed=%v reason=%q",
				i, row, status, code, gotAllowed, gotReason, row.Allowed, row.DenyReason)
		}
	}

	// Branch list visibility (GET /v1/branches) differential checks.
	for i, br := range matrix.BranchReadCases {
		if br.Role == "anonymous" {
			// Anonymous cannot call GET /v1/branches (401 in requireSession); checked on GET /v1/branches/{name} above.
			continue
		}
		hVis := newLeanHarness(t)
		row := leanHTTPRow{Channel: "public_http", AuthKind: "bearer_token", Role: br.Role, IsTokenID: br.IsTokenID, Ownership: br.Ownership}
		targetName := hVis.branchForRel(row)
		hVis.f.branches.mu.Lock()
		for k := range hVis.f.branches.branches {
			if k != targetName {
				delete(hVis.f.branches.branches, k)
			}
		}
		hVis.f.branches.mu.Unlock()
		resp, out := send(t, hVis.httpHandler, http.MethodGet, "/v1/branches", nil, bearer(bearerForRole(br.Role, br.IsTokenID)))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("branch_read[%d] %+v: status=%d %v", i, br, resp.StatusCode, out)
		}
		items, _ := out["items"].([]any)
		gotVisible := len(items) == 1
		if gotVisible != br.CanRead {
			t.Fatalf("branch_read[%d] %+v: go visible=%v lean=%v", i, br, gotVisible, br.CanRead)
		}
	}

	// License.Has differential checks.
	for i, lc := range matrix.LicenseHasCases {
		lic := license.License{Plan: lc.Plan, Modules: lc.Modules}
		if got := lic.Has(lc.Feature); got != lc.Has {
			t.Fatalf("license_has[%d] %+v: go=%v lean=%v", i, lc, got, lc.Has)
		}
	}

	// Token scope whitelist differential checks (including Decision 13 aliases).
	branchesPrincipal := ext.Principal{ID: "token:scope-check", Roles: []string{"scope:" + domain.ScopeBranches}}
	for i, tc := range matrix.TokenScopeCases {
		gotAllowed := (token.Scoped{}).Authorize(t.Context(), branchesPrincipal, tc.Action, ext.Resource{}) == nil
		if gotAllowed != tc.BranchesAllowed {
			t.Fatalf("token_scope[%d] action=%q: go=%v lean=%v", i, tc.Action, gotAllowed, tc.BranchesAllowed)
		}
	}

	// License.State and Usable differential checks.
	baseTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, sc := range matrix.LicenseStateCases {
		lic := license.License{
			Plan:      license.Lite,
			Modules:   []string{"history"},
			MaxNodes:  sc.MaxNodes,
			Expires:   baseTime.AddDate(0, 0, sc.ExpiresDay),
			GraceDays: sc.GraceDays,
		}
		now := baseTime.AddDate(0, 0, sc.NowDay)
		gotState := lic.State(now, sc.Nodes)
		if string(gotState) != sc.State || gotState.Usable() != sc.Usable {
			t.Fatalf("license_state[%d] %+v: go=(%s, %v) lean=(%s, %v)",
				i, sc, gotState, gotState.Usable(), sc.State, sc.Usable)
		}
	}
}

// --- Safety Verification Tests (R-05, W-22, R-02, F-09) ---

func TestLeanAuthz_R05_Verified_NodeAPI_EnforcesSocketOnly(t *testing.T) {
	f := newFixture(t)
	f.srv.Backups = &fakeBackups{}
	f.srv.Cluster = &leanCluster{leader: "node1"}
	nodeHandler := (&nodeapi.Server{API: f.srv.Handler(true)}).Handler()

	// Previously a Bug Candidate on 7bcdba6; fixed in PR #83 (R-05). Verify mTLS nodeForward rejects socket_only with 403 socket_only.
	pJSON, _ := json.Marshal(ext.Principal{ID: "sso:viewer", Name: "Viewer", Roles: []string{"viewer"}})
	for _, ep := range []string{"/v1/auth/reset", "/v1/backups/restore", "/v1/cluster/token"} {
		req := httptest.NewRequest(http.MethodPost, nodeapi.APIPrefix+ep, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
		req.Header.Set("X-Db-Branch-Forwarded", "node2")
		req.Header.Set("X-Db-Branch-Principal", string(pJSON))
		rec := httptest.NewRecorder()
		nodeHandler.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusForbidden || errCode(out) != "socket_only" {
			t.Fatalf("expected 403 socket_only on %s after R-05, got %d %s", ep, rec.Code, rec.Body.String())
		}
	}
}

func TestLeanAuthz_W22_Verified_LegacyAliasesAndBaseMaskHidden(t *testing.T) {
	// 1. base mask is hidden from public HTTP and local socket, but reachable via nodeForward (internalMaskTarget).
	f := newFixture(t)
	f.srv.TokenAuth = leanTokenAuth{}
	for _, h := range []http.Handler{f.srv.Handler(true), f.srv.Handler(false)} {
		resp, _ := send(t, h, http.MethodPost, "/v1/bases/prod/mask", map[string]any{"source": "prod", "masker": "greenmask"}, bearer("tok-admin"))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 on unforwarded base mask, got %d", resp.StatusCode)
		}
	}

	// 2. legacyActionAuthorizer makes branch.checkpoint.delete <-> branch.checkpoint_delete and base.snapshot.delete <-> base.snapshot_delete equivalent.
	legacyOnly := func(allowedAction string) ext.Authorizer {
		return extAuthFunc(func(_ context.Context, _ ext.Principal, action string, _ ext.Resource) error {
			if action == allowedAction {
				return nil
			}
			return ext.ErrForbidden
		})
	}
	f2 := newFixture(t)
	f2.srv.TokenAuth = leanTokenAuth{}
	f2.srv.Ext = ext.Module{Authorizer: legacyOnly("branch.checkpoint_delete")}
	f2.branches.branches["b1"] = domain.Branch{Name: name("b1"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:viewer", OwnerName: "Viewer", Runtime: mysql84}
	resp, _ := send(t, f2.srv.Handler(false), http.MethodDelete, "/v1/branches/b1/checkpoints/cp1", nil, bearer("tok-viewer"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected legacy branch.checkpoint_delete permission to authorize DELETE checkpoint (branch.checkpoint.delete), got %d", resp.StatusCode)
	}
}

type extAuthFunc func(context.Context, ext.Principal, string, ext.Resource) error

func (fn extAuthFunc) Authorize(ctx context.Context, p ext.Principal, a string, r ext.Resource) error {
	return fn(ctx, p, a, r)
}

func TestLeanAuthz_R02_Verified_BranchesTokenWhitelistAndPersonalRoleInheritance(t *testing.T) {
	// Part A: scope:branches token rejects non-whitelisted reads (cluster/license/module/ext/updates/settings/tokens/backups)
	// with 403 forbidden while allowing whitelisted branch/base/metrics/account reads.
	f := newFixture(t)
	f.srv.TokenAuth = leanTokenAuth{}
	f.srv.Tokens = newLeanMemTokens()
	f.srv.ExtHandlers = map[string]ext.Handler{"audit": leanExtHandler{}}
	h := f.srv.Handler(false)
	for _, path := range []string{"/v1/cluster", "/v1/license", "/v1/modules", "/v1/ext/audit/events", "/v1/updates", "/v1/settings", "/v1/tokens", "/v1/backups"} {
		resp, body := send(t, h, http.MethodGet, path, nil, bearer("tok-scope-branches-svc"))
		if resp.StatusCode != http.StatusForbidden || errCode(body) != "forbidden" {
			t.Fatalf("expected 403 forbidden for scope:branches on %s after R-02, got %d %v", path, resp.StatusCode, body)
		}
	}
	for _, path := range []string{"/v1/branches", "/v1/bases", "/v1/metrics", "/v1/account/tokens", "/v1/account/preferences"} {
		resp, body := send(t, h, http.MethodGet, path, nil, bearer("tok-scope-branches-svc"))
		if resp.StatusCode == http.StatusForbidden {
			t.Fatalf("expected whitelisted %s to be allowed for scope:branches, got 403 %v", path, body)
		}
	}

	// Part B: personal token preserves owner's "viewer" role (no privilege escalation past viewerReadsOnly)
	// and strips "admin" role (an admin's personal token cannot perform admin actions).
	f2 := newFixture(t)
	store := &memTokenStore{}
	tokSvc := &token.Service{Store: store, Clock: func() time.Time { return t0 }}
	viewer := ext.Principal{ID: "sso:alice", Name: "Alice", Roles: []string{"viewer"}}
	viewerSecret, _, err := tokSvc.CreatePersonal(t.Context(), viewer, "mcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	adminUser := ext.Principal{ID: "admin", Name: "admin", Roles: []string{"admin"}}
	adminSecret, _, err := tokSvc.CreatePersonal(t.Context(), adminUser, "mcp-admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	f2.srv.TokenAuth = tokSvc
	f2.srv.Ext = ext.Module{Authorizer: viewerReadsOnly{}}
	f2.branches.branches["alices"] = domain.Branch{Name: name("alices"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:alice", OwnerName: "Alice", Runtime: mysql84}
	h2 := f2.srv.Handler(false)

	// Viewer's personal token can read her branch, but is denied DELETE (403) because it retains "viewer".
	if resp, body := send(t, h2, http.MethodGet, "/v1/branches/alices", nil, bearer(viewerSecret)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected viewer personal token GET to succeed, got %d %v", resp.StatusCode, body)
	}
	if resp, body := send(t, h2, http.MethodDelete, "/v1/branches/alices", nil, bearer(viewerSecret)); resp.StatusCode != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("expected viewer personal token DELETE to be forbidden after R-02, got %d %v", resp.StatusCode, body)
	}
	// Admin's personal token strips "admin" role and is scoped to "branches", so admin endpoints and others' branches are denied (403).
	if resp, body := send(t, h2, http.MethodGet, "/v1/cluster", nil, bearer(adminSecret)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected admin personal token GET /v1/cluster to be forbidden, got %d %v", resp.StatusCode, body)
	}
	if resp, body := send(t, h2, http.MethodDelete, "/v1/branches/alices", nil, bearer(adminSecret)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected admin personal token DELETE on other user's branch to be forbidden, got %d %v", resp.StatusCode, body)
	}
}

func TestLeanAuthz_F09_Verified_LegacyUnmigratedBranchRejectsDisplayNameCollision(t *testing.T) {
	f := newFixture(t)
	f.srv.TokenAuth = leanTokenAuth{}
	// Legacy unmigrated branch: OwnerName == "", Owner == "Viewer" (display name, not immutable ID)
	f.branches.branches["legacy-alice"] = domain.Branch{
		Name: name("legacy-alice"), Base: name("prod"), Status: domain.BranchReady,
		Owner: "Viewer", OwnerName: "", Runtime: mysql84,
	}
	// Migrated branch: OwnerName != "", Owner == "sso:real-viewer"
	f.branches.branches["migrated-alice"] = domain.Branch{
		Name: name("migrated-alice"), Base: name("prod"), Status: domain.BranchReady,
		Owner: "sso:real-viewer", OwnerName: "Viewer", Runtime: mysql84,
	}
	h := f.srv.Handler(false)

	// Attacker has ID="sso:viewer" (different ID!), Name="Viewer" (colliding display name).
	// Post-F-09: BOTH migrated and legacy unmigrated branches reject display name collisions with 403.
	for _, bName := range []string{"migrated-alice", "legacy-alice"} {
		resp, body := send(t, h, http.MethodGet, "/v1/branches/"+bName, nil, bearer("tok-viewer"))
		if resp.StatusCode != http.StatusForbidden || errCode(body) != "forbidden" {
			t.Fatalf("%s should reject display name collision after F-09, got %d %v", bName, resp.StatusCode, body)
		}
		respDel, bodyDel := send(t, h, http.MethodDelete, "/v1/branches/"+bName, nil, bearer("tok-viewer"))
		if respDel.StatusCode != http.StatusForbidden || errCode(bodyDel) != "forbidden" {
			t.Fatalf("%s DELETE should reject display name collision after F-09, got %d %v", bName, respDel.StatusCode, bodyDel)
		}
	}
	// Admin can still access legacy unmigrated branches.
	respAdmin, _ := send(t, h, http.MethodGet, "/v1/branches/legacy-alice", nil, bearer("tok-admin"))
	if respAdmin.StatusCode != http.StatusOK {
		t.Fatalf("admin should access legacy unmigrated branch, got %d", respAdmin.StatusCode)
	}
}

type leanSwitchableRevocations struct {
	upgrading atomic.Bool
	shared    *leanRevocations
}

func (r *leanSwitchableRevocations) Revoke(id string, until, now time.Time) error {
	if r.upgrading.Load() {
		return domain.ErrClusterUpgrading
	}
	return r.shared.Revoke(id, until, now)
}
func (r *leanSwitchableRevocations) Revoked(id string) bool { return r.shared.Revoked(id) }

func TestLeanAuthz_F09_Verified_LogoutDuringClusterUpgradeReturns503AndFlushes(t *testing.T) {
	now := t0
	creds := &memCreds{}
	rev := &leanSwitchableRevocations{shared: &leanRevocations{revoked: map[string]bool{}}}
	rev.upgrading.Store(true)

	f := newFixture(t)
	nodeA := &auth.Service{Store: creds, Hasher: sha{}, Clock: func() time.Time { return now }, Revocations: rev}
	nodeB := &auth.Service{Store: creds, Hasher: sha{}, Clock: func() time.Time { return now }, Revocations: rev}
	if err := nodeA.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	f.srv.Auth = nodeA
	h := f.srv.Handler(false)

	respLogin, bodyLogin := send(t, h, http.MethodPost, "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil)
	if respLogin.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d %v", respLogin.StatusCode, bodyLogin)
	}
	cookie := respLogin.Cookies()[0]
	csrf := bodyLogin["csrf_token"].(string)

	// During mixed-cluster upgrade (protocol < 24), HTTP POST /v1/auth/logout returns 503 cluster_upgrading
	// (never silent 204) and preserves the cookie while revoking locally on nodeA.
	respLogout, bodyLogout := send(t, h, http.MethodPost, "/v1/auth/logout", nil, withSession(cookie, csrf))
	if respLogout.StatusCode != http.StatusServiceUnavailable || errCode(bodyLogout) != "cluster_upgrading" {
		t.Fatalf("expected 503 cluster_upgrading on logout during upgrade, got %d %v", respLogout.StatusCode, bodyLogout)
	}
	for _, c := range respLogout.Cookies() {
		if c.Name == "dbb_session" && c.MaxAge < 0 {
			t.Fatalf("expected cookie not to be cleared on 503 cluster_upgrading, got %+v", c)
		}
	}
	if _, err := nodeA.Session(cookie.Value); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("nodeA should reject locally, got %v", err)
	}

	// Once cluster finishes upgrading, FlushRevocations (or retrying logout) replicates to nodeB.
	rev.upgrading.Store(false)
	if err := nodeA.FlushRevocations(); err != nil {
		t.Fatalf("FlushRevocations failed: %v", err)
	}
	if _, err := nodeB.Session(cookie.Value); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expected nodeB to reject session after upgrade + flush, got %v", err)
	}
}

func TestLeanAuthz_R02_Verified_ExternalUserDeletionRevokesPersonalTokens(t *testing.T) {
	f := newFixture(t)
	store := &memTokenStore{}
	tokSvc := &token.Service{Store: store, Clock: func() time.Time { return t0 }}
	alice := ext.Principal{ID: "user:alice@example.com", Name: "Alice", Roles: []string{"developer"}}
	bob := ext.Principal{ID: "user:bob@example.com", Name: "Bob", Roles: []string{"developer"}}
	aliceSecret, _, err := tokSvc.CreatePersonal(t.Context(), alice, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	bobSecret, _, err := tokSvc.CreatePersonal(t.Context(), bob, "desktop", 0)
	if err != nil {
		t.Fatal(err)
	}
	svcSecret, _, err := tokSvc.Create(t.Context(), "ci-bot", domain.ScopeBranches, 0)
	if err != nil {
		t.Fatal(err)
	}

	f.srv.Tokens = leanMemTokens{svc: tokSvc}
	f.srv.TokenAuth = tokSvc
	f.srv.ExtHandlers = map[string]ext.Handler{
		"auth": extHandlerFunc(func(_ context.Context, req ext.Request) (ext.Response, error) {
			if req.Method == http.MethodDelete && req.Path == "/users/alice@example.com" {
				return ext.Response{Status: http.StatusNoContent}, nil
			}
			return ext.Response{Status: http.StatusNotFound}, nil
		}),
	}
	h := f.srv.Handler(false)

	// Before deletion, Alice's personal token works.
	if resp, _ := send(t, h, http.MethodGet, "/v1/branches", nil, bearer(aliceSecret)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected Alice's personal token to succeed before deletion, got %d", resp.StatusCode)
	}

	// Admin deletes Alice via external auth module API (DELETE /v1/ext/auth/users/alice@example.com).
	respDel, bodyDel := f.do(t, http.MethodDelete, "/v1/ext/auth/users/alice@example.com", nil)
	if respDel.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 deleting external user, got %d %v", respDel.StatusCode, bodyDel)
	}

	// Alice's personal token is immediately revoked (ErrUnauthenticated / 401),
	// while Bob's personal token and the service token remain valid.
	if _, err := tokSvc.Authenticate(t.Context(), ext.Credentials{Token: aliceSecret}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Fatalf("expected Alice's token to be revoked after user deletion, got err=%v", err)
	}
	if resp, _ := send(t, h, http.MethodGet, "/v1/branches", nil, bearer(aliceSecret)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for deleted user's personal token, got %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, http.MethodGet, "/v1/branches", nil, bearer(bobSecret)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected Bob's personal token to remain valid, got %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, http.MethodGet, "/v1/branches", nil, bearer(svcSecret)); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected service token to remain valid, got %d", resp.StatusCode)
	}
}

type extHandlerFunc func(context.Context, ext.Request) (ext.Response, error)

func (fn extHandlerFunc) Serve(ctx context.Context, r ext.Request) (ext.Response, error) {
	return fn(ctx, r)
}

type memTokenStore struct {
	tokens []domain.APIToken
}

func (m *memTokenStore) ListTokens(context.Context) ([]domain.APIToken, error) {
	return append([]domain.APIToken(nil), m.tokens...), nil
}
func (m *memTokenStore) PutToken(_ context.Context, t domain.APIToken) error {
	m.tokens = append(m.tokens, t)
	return nil
}
func (m *memTokenStore) DeleteToken(_ context.Context, id string) error {
	for i, t := range m.tokens {
		if t.ID == id {
			m.tokens = append(m.tokens[:i], m.tokens[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

func dbbranchTarget(name string) operation.Target {
	return operation.Target{Resource: "branch", Name: name}
}

var _ = io.Discard
