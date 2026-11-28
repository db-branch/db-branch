// Package httpapi serves the /v1 REST API, the SSE event stream and the web console.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/settings"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

// Branches is the base and branch use case the API needs.
type Branches interface {
	PlaceBranch(ctx context.Context, req dbbranch.CreateBranchRequest) (string, error)
	CreateBranch(ctx context.Context, req dbbranch.CreateBranchRequest, report dbbranch.Reporter) (domain.Branch, error)
	ResetBranch(ctx context.Context, name domain.Name, report dbbranch.Reporter) (domain.Branch, error)
	PauseBranch(ctx context.Context, name domain.Name, report dbbranch.Reporter) (domain.Branch, error)
	ResumeBranch(ctx context.Context, name domain.Name, report dbbranch.Reporter) (domain.Branch, error)
	ValidateFork(ctx context.Context, req dbbranch.ForkBranchRequest) error
	ForkBranch(ctx context.Context, req dbbranch.ForkBranchRequest, report dbbranch.Reporter) (domain.Branch, error)
	PlaceRecreate(ctx context.Context, name domain.Name) (string, error)
	RecreateBranch(ctx context.Context, name domain.Name, report dbbranch.Reporter) (domain.Branch, error)
	DeleteBranch(ctx context.Context, name domain.Name, report dbbranch.Reporter) error
	ExtendBranch(ctx context.Context, name domain.Name, ttl time.Duration) (domain.Branch, error)
	GetBranch(ctx context.Context, name domain.Name) (domain.Branch, error)
	ListBranches(ctx context.Context) ([]domain.Branch, error)
	BranchStorage(ctx context.Context, name domain.Name) (domain.DatasetInfo, error)
	// CheckpointsAvailable reports whether this node's storage supports checkpoints.
	CheckpointsAvailable() bool
	ListCheckpoints(ctx context.Context, name domain.Name) ([]domain.Checkpoint, error)
	CheckpointGraph(ctx context.Context, name domain.Name) (domain.Graph, error)
	CreateCheckpoint(ctx context.Context, name domain.Name, checkpoint string) (domain.Checkpoint, error)
	RestoreCheckpoint(ctx context.Context, name domain.Name, checkpoint string, report dbbranch.Reporter) (domain.Branch, error)
	DeleteCheckpoint(ctx context.Context, name domain.Name, checkpoint string) error
	// ValidateUpgrade says at once what an upgrade request would be refused for.
	ValidateUpgrade(ctx context.Context, name domain.Name, to domain.RuntimeRef, checkOnly bool) (domain.Branch, error)
	UpgradeBranch(ctx context.Context, req dbbranch.UpgradeRequest, report dbbranch.Reporter) (domain.UpgradeResult, error)
	// ValidateMerge checks synchronous preconditions for merging a line/checkpoint into live.
	ValidateMerge(ctx context.Context, req dbbranch.MergeBranchRequest) (domain.Branch, error)
	MergeBranch(ctx context.Context, req dbbranch.MergeBranchRequest, report dbbranch.Reporter) (domain.MergeResult, error)
	// ValidateTarget checks, for a branch created on another runtime, that this node can run it.
	ValidateTarget(ctx context.Context, from, to domain.RuntimeRef) error
	CreateBranchUpgraded(ctx context.Context, req dbbranch.CreateBranchRequest, report dbbranch.Reporter) (domain.Branch, *domain.UpgradeResult, error)

	CreateBase(ctx context.Context, req dbbranch.CreateBaseRequest, report dbbranch.Reporter) (domain.Base, error)
	SnapshotBase(ctx context.Context, name domain.Name, report dbbranch.Reporter) (domain.BaseSnapshot, error)
	MaskBase(ctx context.Context, req dbbranch.MaskRequest, report dbbranch.Reporter) (domain.BaseSnapshot, error)
	GetBase(ctx context.Context, name domain.Name) (domain.Base, error)
	ListBases(ctx context.Context) ([]domain.Base, error)
	DeleteBase(ctx context.Context, name domain.Name) error
	StartBase(ctx context.Context, name domain.Name) error
	StopBase(ctx context.Context, name domain.Name) error
	SetReplicaNodes(ctx context.Context, name domain.Name, nodes *[]string) (domain.Base, error)
	ExecBase(ctx context.Context, name domain.Name, sql string) error
	DescribeBase(ctx context.Context, name domain.Name, description string) (domain.Base, error)
	LoadBase(ctx context.Context, name domain.Name, dump string, report dbbranch.Reporter) error
	GetRefresh(ctx context.Context, name domain.Name) (domain.BaseRefresh, error)
	SetRefresh(ctx context.Context, name domain.Name, req dbbranch.RefreshRequest) (domain.BaseRefresh, error)
	ClearRefresh(ctx context.Context, name domain.Name) error
	RefreshBase(ctx context.Context, name domain.Name, report dbbranch.Reporter) error
	DeleteSnapshot(ctx context.Context, name domain.Name, snapshot string) error
	SetSnapshotPublic(ctx context.Context, name domain.Name, snapshot string, public bool) (domain.Base, error)
	SetAnonymous(ctx context.Context, name domain.Name, l domain.AnonymousLimits) (domain.Base, error)
}

// Routing is the proxy routing use case the API needs.
type Routing interface {
	AddUser(ctx context.Context, u domain.RouteUser) error
	RemoveUser(ctx context.Context, name string) error
	ListUsers(ctx context.Context) ([]domain.RouteUser, error)
}

// Runtimes is the runtime use case the API needs.
type Runtimes interface {
	Install(ctx context.Context, ref domain.RuntimeRef) error
	Remove(ctx context.Context, ref domain.RuntimeRef) error
	List(ctx context.Context) ([]domain.RuntimeRef, error)
	Available(engine domain.Engine) []domain.RuntimeRef
}

// Server holds the API's dependencies.
type Server struct {
	// branchAudiences remembers who sees each branch (name -> Audience) from
	// its last event, to address the event that tells it was deleted or made
	// private. branchSeed fills it from the branch list on the first stream.
	branchAudiences sync.Map
	seedMu          sync.Mutex
	publishMu       sync.Mutex
	seeded          bool

	// anonLimiter counts the branches each client address creates without a
	// login (anonymousRate per hour), in memory.
	anonLimiter IPLimiter
	// anonPending are the branches (name -> base) anonymous callers asked
	// for that the list does not show yet; they count against the caps.
	anonMu      sync.Mutex
	anonPending map[string]*anonReservation

	// policyPending tracks branches currently being created/forked after
	// passing BranchPolicy, preventing concurrent MaxBranches limit bypasses.
	policyMu      sync.Mutex
	policyPending map[string]*policyReservation

	// Branches returns the service, or an error before `db-branch init`.
	Branches func() (Branches, error)
	Runtimes Runtimes
	Ops      *operation.Manager
	Bus      *operation.Bus
	Auth     *auth.Service
	// Host is how clients reach this node's database servers (used in DSNs).
	Host string
	// Node is this node's name.
	Node    string
	Version string
	// System describes this node's OS, kernel and ZFS (single-node mode;
	// in a cluster the nodes report it in their heartbeats). Optional.
	System func(context.Context) domain.NodeSystem
	// Console serves the web console's static files. Optional.
	Console http.Handler
	// Cluster is set in cluster mode. Optional.
	Cluster Cluster
	// Routing manages route users; nil when this node has no proxy.
	Routing Routing
	// ProxyPorts are the proxy's client ports per engine (none: no proxy).
	ProxyPorts map[domain.Engine]int
	// Ext holds the module ports; unset ones use the OSS defaults (admin
	// password, allow everyone logged in, audit to Log).
	Ext ext.Module
	// Modules lists the loaded modules as "name version" (/v1/capabilities).
	Modules []string
	// ExtHandlers are the loaded modules' own APIs by module name, and
	// ExtPages their console pages.
	ExtHandlers map[string]ext.Handler
	// ExtPublic are, by module, the API path prefixes that need no login;
	// LoginMethods the modules' ways to sign in.
	ExtPublic    map[string][]string
	LoginMethods []LoginMethod
	ExtPages     []ModulePage
	// Storage reports what this node's storage holds and who uses the space. Optional.
	Storage func(ctx context.Context) (domain.StorageReport, error)
	// Metrics returns this node's recent performance samples, oldest first. Optional.
	Metrics func() []domain.Metrics
	// Tokens manages API tokens; TokenAuth checks them. Optional.
	Tokens    Tokens
	TokenAuth ext.Authenticator
	// Providers are the engines' runtime providers; SQL runs on branches
	// through them (API and MCP). Optional.
	Providers domain.RuntimeProviders
	// TLSCertPEM is served at /tls.pem so clients (MCP) can trust the node.
	// When TLSCert is set, it overrides TLSCertPEM dynamically (for hot reload).
	TLSCertPEM []byte
	TLSCert    func() []byte
	// Settings manages cluster-wide settings. Optional.
	Settings Settings
	// DefaultTTL is applied to branches created without one. Optional.
	DefaultTTL func() time.Duration
	// Anonymous limits branches made without logging in (from public
	// snapshots): their longest TTL and how many may exist. Optional
	// (24h, 10).
	Anonymous func() (time.Duration, int)
	// AnonymousRate is how many branches one client address may create per
	// hour without logging in (0: unlimited). Optional (5). Counted per node,
	// in memory only.
	AnonymousRate func() int
	// ModuleAdmin manages the cluster's modules. Optional.
	ModuleAdmin Modules
	// ModuleProblems returns this node's module configuration errors. Optional.
	ModuleProblems func() map[string]string
	// RuntimeWarnings are shown by GET /v1/capabilities (for example that a
	// runtime module loaded without a signature).
	RuntimeWarnings []string
	// PendingModules names wanted modules this node does not run yet; while
	// any are pending the console and remote API refuse all but inspection.
	PendingModules func() []string
	// Updates checks for newer versions and rolls them out. Optional.
	Updates Updates
	// Preferences keeps each person's console preferences (language). Optional.
	Preferences domain.SettingsStore
	// Backups saves and restores the cluster state. Optional.
	Backups Backups
	// Licensing verifies and applies licence keys. Optional.
	Licensing Licensing
	// Renewal fetches the renewed key of a subscription. Optional.
	Renewal Renewal
	// PaidModules names the loaded modules that need a licence key.
	PaidModules []string
	// Uploads keeps dumps sent to this node. Optional (no uploads).
	Uploads domain.UploadStore
	// PoolFree reports the free bytes of this node's ZFS pool. Optional.
	PoolFree func() uint64
	// PoolStats reports total and free bytes of this node's ZFS pool (single-node mode). Optional.
	PoolStats func(context.Context) (size, free uint64)
	// AuditFailClosed says whether a change that cannot be audited is
	// refused (503 audit_unavailable) instead of going ahead with the failure
	// only logged. Optional (false: fail open).
	AuditFailClosed func() bool
	// Log receives audit events and audit failures. Optional.
	Log      *slog.Logger
	inner    internalMux
	requests requestCounter

	// OnPasswordReset runs after a reset through the socket. Optional.
	OnPasswordReset func()
	// OnRuntimesChanged runs after an engine is installed or removed here. Optional.
	OnRuntimesChanged func()
}

type ctxKey struct{}

// Handler returns the HTTP handler. local is true for the UNIX socket, where
// the peer was already authenticated by its credentials.
func (s *Server) Handler(local bool) http.Handler {
	mux := http.NewServeMux()
	s.routes(mux, local)
	var api http.Handler = mux
	if local {
		api = s.localPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Cluster peers forward StartMaskOn here after refresh even though
			// POST /v1/bases/{name}/mask is hidden from 1.0 API/CLI/local callers.
			if target, ok := internalMaskTarget(r); ok {
				r.SetPathValue("name", target)
				s.guard("base.mask", s.named(s.maskBase)).ServeHTTP(w, r)
				return
			}
			mux.ServeHTTP(w, r)
		}))
	} else {
		cop := http.NewCrossOriginProtection()
		cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.auditAuthFailure(r, Anonymous, "auth.csrf", http.StatusForbidden)
			writeError(w, http.StatusForbidden, "csrf_failed", "cross-origin request blocked")
		}))
		sessAPI := s.requireSession(mux)
		protected := cop.Handler(sessAPI)
		api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				sessAPI.ServeHTTP(w, r)
				return
			}
			protected.ServeHTTP(w, r)
		})
	}
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.health(w, r) {
			return
		}
		if r.URL.Path == pathAuthMethods && r.Method == http.MethodGet {
			s.loginMethods(w, r)
			return
		}
		if r.URL.Path == pathTLS {
			if pem := s.tlsCertPEM(); pem != nil {
				w.Header().Set("Content-Type", "application/x-pem-file")
				_, _ = w.Write(pem)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			api.ServeHTTP(w, r)
			return
		}
		if s.Console == nil {
			http.NotFound(w, r)
			return
		}
		s.Console.ServeHTTP(w, r)
	}))
}

// Paths answered before the routes below, without a login.
const (
	pathHealthz     = "/healthz"
	pathReadyz      = "/readyz"
	pathTLS         = "/tls.pem"
	pathAuthMethods = "/v1/auth/methods"
)

// outerPatterns are the routes Handler answers itself, ahead of the mux. The
// OpenAPI test reads them together with the patterns routes registers.
func (s *Server) tlsCertPEM() []byte {
	if s.TLSCert != nil {
		return s.TLSCert()
	}
	return s.TLSCertPEM
}

var outerPatterns = []string{"GET " + pathHealthz, "GET " + pathReadyz, "GET " + pathTLS, "GET " + pathAuthMethods}

// router is the part of *http.ServeMux that routes needs: registering
// handlers. Tests pass one that only records the patterns.
type router interface {
	Handle(pattern string, h http.Handler)
	HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request))
}

// routes registers every handler under an action name: each call is
// authorized (Authorizer) and, if it changes something, audited (AuditSink).
func (s *Server) routes(mux router, local bool) {
	r := func(pattern, action string, h http.HandlerFunc) {
		if rec, ok := mux.(interface{ recordAction(string, string) }); ok {
			rec.recordAction(pattern, action)
		}
		mux.HandleFunc(pattern, s.guard(action, h))
	}
	r("GET /v1/branches", "branch.read", s.withService(s.listBranches))
	r("POST /v1/branches", "branch.create", s.withService(s.createBranch))
	r("GET /v1/branches/{name}", "branch.read", s.named(s.getBranch))
	r("PATCH /v1/branches/{name}", "branch.extend", s.named(s.patchBranch))
	r("DELETE /v1/branches/{name}", "branch.delete", s.named(s.deleteBranch))
	r("POST /v1/branches/{name}/reset", "branch.reset", s.named(s.resetBranch))
	r("POST /v1/branches/{name}/pause", "branch.pause", s.named(s.pauseBranch))
	r("POST /v1/branches/{name}/resume", "branch.resume", s.named(s.resumeBranch))
	r("POST /v1/branches/{name}/fork", "branch.fork", s.named(s.forkBranch))
	r("POST /v1/branches/{name}/recreate", "branch.recreate", s.named(s.recreateBranch))
	r("GET /v1/branches/{name}/checkpoints", "branch.checkpoint.read", s.named(s.listCheckpoints))
	r("POST /v1/branches/{name}/checkpoints", "branch.checkpoint", s.named(s.createCheckpoint))
	r("POST /v1/branches/{name}/checkpoints/{checkpoint}/restore", "branch.restore", s.named(s.restoreCheckpoint))
	r("DELETE /v1/branches/{name}/checkpoints/{checkpoint}", "branch.checkpoint.delete", s.named(s.deleteCheckpoint))
	r("POST /v1/branches/{name}/upgrade", "branch.upgrade", s.named(s.upgradeBranch))
	r("POST /v1/branches/{name}/merge", "branch.merge", s.named(s.mergeBranch))
	r("GET /v1/branches/{name}/dsn", "branch.connect", s.named(s.branchDSN))
	r("POST /v1/branches/{name}/query", "branch.query", s.named(s.queryBranch))

	r("GET /v1/bases", "base.read", s.withService(s.listBases))
	r("GET /v1/public/bases", "base.public", s.withService(s.publicBases))
	r("POST /v1/bases", "base.create", s.withService(s.createBase))
	r("GET /v1/bases/{name}", "base.read", s.named(s.getBase))
	r("PATCH /v1/bases/{name}", "base.describe", s.named(s.patchBase))
	r("PUT /v1/bases/{name}/replicas", "base.replicas", s.named(s.putReplicas))
	r("PATCH /v1/bases/{name}/anonymous", "base.publish", s.named(s.patchAnonymous))
	r("DELETE /v1/bases/{name}", "base.delete", s.named(s.deleteBase))
	r("POST /v1/bases/{name}/snapshots", "base.snapshot", s.named(s.snapshotBase))
	// base mask is hidden from the 1.0 API surface (DECISIONS.md 8-c); maskBase
	// stays in bases.go and is still reachable by cluster-forwarded StartMaskOn.
	r("POST /v1/bases/{name}/start", "base.start", s.named(s.startBase))
	r("POST /v1/bases/{name}/stop", "base.stop", s.named(s.stopBase))
	r("POST /v1/bases/{name}/sql", "base.sql", s.named(s.baseSQL))
	r("POST /v1/bases/{name}/load", "base.load", s.named(s.loadBase))
	r("GET /v1/bases/{name}/refresh", "base.refresh.read", s.named(s.getRefresh))
	r("PUT /v1/bases/{name}/refresh", "base.refresh", s.named(s.putRefresh))
	r("DELETE /v1/bases/{name}/refresh", "base.refresh", s.named(s.deleteRefresh))
	r("POST /v1/bases/{name}/refresh/run", "base.refresh", s.named(s.runRefresh))
	r("POST /v1/uploads", "base.upload", s.upload)
	r("GET /v1/uploads/room", "base.upload", s.uploadRoom)
	r("PATCH /v1/bases/{name}/snapshots/{snapshot}", "base.publish", s.named(s.patchSnapshot))
	r("DELETE /v1/bases/{name}/snapshots/{snapshot}", "base.snapshot.delete", s.named(s.deleteSnapshot))

	r("GET /v1/route-users", "route-user.read", s.listRouteUsers)
	r("PUT /v1/route-users/{name}", "route-user.write", s.putRouteUser)
	r("DELETE /v1/route-users/{name}", "route-user.delete", s.deleteRouteUser)

	r("GET /v1/tokens", "token.read", s.listTokens)
	r("POST /v1/tokens", "token.write", s.createToken)
	r("DELETE /v1/tokens/{id}", "token.write", s.revokeToken)
	r("GET /v1/account/preferences", "account.preference.read", s.getPreferences)
	r("PUT /v1/account/preferences", "account.preference.write", s.putPreferences)
	r("GET /v1/account/tokens", "account.token.read", s.listMyTokens)
	r("POST /v1/account/tokens", "account.token.write", s.createMyToken)
	r("DELETE /v1/account/tokens/{id}", "account.token.write", s.revokeMyToken)
	// MCP: each tool call goes through the routes above (authorized and audited there).
	r("POST /v1/mcp", "mcp.use", s.mcpHandler().ServeHTTP)

	r("GET /v1/settings", "setting.read", s.listSettings)
	r("PUT /v1/settings/{scope}", "setting.write", s.putSettings)

	r("GET /v1/license", "license.read", s.currentLicense)
	r("POST /v1/license/verify", "license.verify", s.verifyLicense)
	r("PUT /v1/license", "license.write", s.applyLicense)
	r("GET /v1/license/renewal", "license.read", s.getRenewal)
	r("POST /v1/license/renew", "license.write", s.renewLicense)

	r("GET /v1/modules", "module.read", s.listModules)
	r("POST /v1/modules", "module.install", s.installModule)
	r("DELETE /v1/modules/{name}", "module.remove", s.removeModule)
	r("POST /v1/modules/{name}/forget", "module.forget", s.forgetModule)

	r("GET /v1/updates", "update.read", s.withUpdates(s.getUpdates))
	r("POST /v1/updates/check", "update.check", s.withUpdates(s.checkUpdates))
	r("POST /v1/updates", "update.apply", s.withUpdates(s.applyUpdate))
	r("DELETE /v1/updates", "update.apply", s.withUpdates(s.cancelUpdate))

	// Modules' own APIs: authorized and audited per request (ext.go).
	mux.HandleFunc("/v1/ext/{name}/{path...}", s.extAPI)

	r("GET /v1/runtimes", "runtime.read", s.listRuntimes)
	r("GET /v1/runtimes/available", "runtime.read", s.availableRuntimes)
	r("POST /v1/runtimes", "runtime.install", s.installRuntime)
	r("DELETE /v1/runtimes/{engine}/{version}", "runtime.remove", s.removeRuntime)

	r("GET /v1/operations", "operation.read", s.listOperations)
	r("GET /v1/operations/{id}", "operation.read", s.getOperation)
	r("GET /v1/events", "event.read", s.events)
	r("GET /v1/capabilities", "capability.read", s.capabilities)
	r("GET /v1/cluster", "cluster.read", s.cluster)
	r("GET /v1/metrics", "metrics.read", s.withService(s.metrics))
	r("GET /v1/nodes/{name}/metrics", "cluster.read", s.nodeMetrics)
	r("GET /v1/nodes/{name}/storage", "cluster.read", s.nodeStorage)
	r("DELETE /v1/cluster/nodes/{name}", "cluster.remove", s.clusterRemoveNode)
	r("POST /v1/cluster/token", "cluster.token", s.clusterToken(local))

	// Logging in happens before there is a principal; it is audited itself.
	mux.HandleFunc("POST /v1/auth/login", s.login)
	r("GET /v1/auth/session", "auth.session", s.session)
	r("POST /v1/auth/logout", "auth.logout", s.logout)
	r("PUT /v1/auth/password", "auth.password", s.changePassword)
	r("POST /v1/auth/reset", "auth.reset", s.resetPassword(local))

	r("GET /v1/users", "user.read", s.listUsers)
	r("POST /v1/users", "user.write", s.createUser)
	r("GET /v1/users/{id}", "user.read", s.getUser)
	r("PATCH /v1/users/{id}", "user.write", s.updateUser)
	r("DELETE /v1/users/{id}", "user.write", s.deleteUser)
	r("POST /v1/users/{id}/reset-password", "user.write", s.resetUserPassword)

	r("GET /v1/backups", "backup.read", s.listBackups)
	r("POST /v1/backups", "backup.create", s.takeBackup)
	r("POST /v1/backups/restore", "backup.restore", s.restoreBackup(local))
	r("GET /v1/backups/{file}", "backup.download", s.downloadBackup)
}

const sessionCookie = "dbb_session"

func (s *Server) resolvePrincipal(ctx context.Context, p ext.Principal) (ext.Principal, error) {
	if p.ID == "" || p.ID == LocalPrincipal.ID || p.ID == Anonymous.ID {
		return p, nil
	}
	if s.Auth != nil && s.Auth.Users != nil && (p.ID == domain.AdminUserID || strings.HasPrefix(p.ID, domain.UserIDPrefix)) {
		u, err := s.Auth.Users.GetUser(ctx, p.ID)
		switch {
		case err == nil:
			if u.Disabled {
				return ext.Principal{}, ext.ErrUnauthenticated
			}
			p.Name = u.DisplayName()
		case errors.Is(err, domain.ErrNotFound):
			if p.ID == domain.AdminUserID {
				if s.Auth.Protocol == nil || s.Auth.Protocol() >= domain.ProtocolUsers {
					return ext.Principal{}, ext.ErrUnauthenticated
				}
			} else if s.Ext.PrincipalResolver == nil {
				return ext.Principal{}, ext.ErrUnauthenticated
			}
		default:
			return ext.Principal{}, err
		}
	}
	if s.Ext.PrincipalResolver == nil {
		return p, nil
	}
	return s.Ext.PrincipalResolver.ResolvePrincipal(ctx, p)
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasHeaderFold(r.Header, forwardedHeader, principalHeader) {
			r = r.Clone(r.Context())
			delHeadersFold(r.Header, forwardedHeader, principalHeader)
		}
		if r.URL.Path == "/v1/auth/login" {
			next.ServeHTTP(w, r)
			return
		}
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			// API clients: a module's Authenticator checks the token. No CSRF:
			// browsers never send this header on their own.
			p, err := s.authenticator().Authenticate(r.Context(), ext.Credentials{Token: token})
			if err == nil {
				p, err = s.resolvePrincipal(r.Context(), p)
			}
			if err != nil || p.ID == LocalPrincipal.ID {
				// The ID "local" is the socket's alone (path dumps, no guard
				// for local admin): no remote identity may take it.
				s.auditAuthFailure(r, Anonymous, "auth.token", http.StatusUnauthorized)
				writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid token")
				return
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
			return
		}
		c, err := r.Cookie(sessionCookie)
		if s.publicExt(r) {
			// A module's public path (e.g. single sign-on): whoever comes,
			// signed in or not (it starts flows). A valid session is held to
			// the same CSRF and password-change rules as everywhere else:
			// the cookie is SameSite=Strict, so legitimate flows carry the token.
			p := Anonymous
			if err == nil {
				if sess, serr := s.Auth.Session(c.Value); serr == nil {
					if !s.sessionAllowed(w, r, sess) {
						return
					}
					cand := ext.Principal{ID: sess.User, Name: sess.Name, Roles: sess.Roles}
					if resolved, rerr := s.resolvePrincipal(r.Context(), cand); rerr == nil {
						p = resolved
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
			return
		}
		if err != nil && anonymousAllowed(r) {
			// Branches from public snapshots need no login (ownership.go).
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), Anonymous)))
			return
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "log in first")
			return
		}
		isLogout := r.Method == http.MethodPost && r.URL.Path == "/v1/auth/logout"
		sess, err := s.Auth.Session(c.Value)
		if err != nil && isLogout {
			sess, err = s.Auth.PendingSession(c.Value)
		}
		if err != nil {
			s.auditAuthFailure(r, Anonymous, "auth.session", http.StatusUnauthorized)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "session expired; log in again")
			return
		}
		if !s.sessionAllowed(w, r, sess) {
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, sess)
		p := ext.Principal{ID: sess.User, Name: sess.Name, Roles: sess.Roles}
		if resolved, rerr := s.resolvePrincipal(r.Context(), p); rerr != nil {
			if !isLogout {
				s.auditAuthFailure(r, p, "auth.session", http.StatusUnauthorized)
				writeError(w, http.StatusUnauthorized, "unauthenticated", "session invalid; log in again")
				return
			}
			ctx = withDeactivatedLogout(ctx)
		} else {
			p = resolved
		}
		if sess.Renewed {
			setSessionCookie(w, sess)
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, p)))
	})
}

// sessionAllowed applies the CSRF check and the initial-password rule to a
// request made with a session; it answers 403 and returns false otherwise.
func (s *Server) sessionAllowed(w http.ResponseWriter, r *http.Request, sess auth.Session) bool {
	if sess.User == LocalPrincipal.ID {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "session expired; log in again")
		return false
	}
	if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.CSRF)) != 1 {
		s.auditAuthFailure(r, ext.Principal{ID: sess.User, Name: sess.Name, Roles: sess.Roles}, "auth.csrf", http.StatusForbidden)
		writeError(w, http.StatusForbidden, "csrf_failed", "missing or wrong X-CSRF-Token")
		return false
	}
	if sess.MustChange && !allowedBeforePasswordChange(r) {
		writeError(w, http.StatusForbidden, "password_change_required", "set a new password first")
		return false
	}
	return true
}

// allowedBeforePasswordChange: what a session with an initial password may do.
func allowedBeforePasswordChange(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/auth/session", "POST /v1/auth/logout", "PUT /v1/auth/password":
		return true
	}
	return false
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// fail maps a use case error onto the error envelope.
func fail(w http.ResponseWriter, err error) {
	var rl *auth.RateLimitedError
	var erl *ext.RateLimitedError
	var tm *tooManyCreations
	switch {
	case errors.As(err, &tm):
		w.Header().Set("Retry-After", strconv.Itoa(int(tm.retryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.As(err, &erl):
		w.Header().Set("Retry-After", strconv.Itoa(int(erl.RetryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, domain.ErrClusterUpgrading), errors.Is(err, ext.ErrClusterUpgrading):
		writeError(w, http.StatusServiceUnavailable, "cluster_upgrading", err.Error())
	case errors.Is(err, domain.ErrNoLeader):
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusServiceUnavailable, "no_leader", err.Error())
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, operation.ErrNotFound), errors.Is(err, ext.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrAlreadyExists), errors.Is(err, ext.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, domain.ErrInUse):
		writeError(w, http.StatusConflict, "in_use", err.Error())
	case errors.Is(err, domain.ErrUncleanShutdown):
		writeError(w, http.StatusConflict, "unclean_shutdown", err.Error())
	case errors.Is(err, domain.ErrFeatureNotSupported), errors.Is(err, domain.ErrUnsupportedEngine):
		writeError(w, http.StatusConflict, "feature_not_supported", err.Error())
	case errors.Is(err, dbbranch.ErrOtherNode):
		writeError(w, http.StatusConflict, "other_node", err.Error())
	case errors.Is(err, domain.ErrNoCapacity):
		writeError(w, http.StatusServiceUnavailable, "no_capacity", err.Error())
	case errors.Is(err, domain.ErrNoSpace):
		writeError(w, http.StatusInsufficientStorage, "no_space", err.Error())
	case errors.Is(err, domain.ErrQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", err.Error())
	case errors.Is(err, domain.ErrRolloutRunning):
		writeError(w, http.StatusConflict, "rollout_running", err.Error())
	case errors.Is(err, domain.ErrCheckpointsUnavailable):
		writeError(w, http.StatusConflict, "checkpoints_unavailable", err.Error())
	case errors.Is(err, domain.ErrUpgradeRefused):
		writeError(w, http.StatusUnprocessableEntity, "upgrade_not_allowed", err.Error())
	case errors.Is(err, domain.ErrConflict), errors.Is(err, ext.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, domain.ErrRuntimeNotInstalled):
		writeError(w, http.StatusNotFound, "runtime_not_installed", err.Error())
	case errors.Is(err, domain.ErrRuntimeNotFound):
		writeError(w, http.StatusUnprocessableEntity, "runtime_unavailable", err.Error())
	case errors.Is(err, domain.ErrRuntimeInstalled):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, domain.ErrInvalidName), errors.Is(err, domain.ErrInvalidRuntime), errors.Is(err, domain.ErrInvalidTTL), errors.Is(err, domain.ErrInvalidRouteUser),
		errors.Is(err, domain.ErrInvalidModule), errors.Is(err, domain.ErrInvalidRefresh), errors.Is(err, domain.ErrInvalidReplicas), errors.Is(err, domain.ErrInvalidAnonymous), errors.Is(err, domain.ErrInvalidUpdate), errors.Is(err, domain.ErrInvalidMergeSource), errors.Is(err, settings.ErrInvalid), errors.Is(err, token.ErrInvalid), errors.Is(err, ext.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, ext.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, ext.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
	case errors.Is(err, auth.ErrNoSession):
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, ext.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "weak_password", err.Error())
	default:
		internalError(w, err)
	}
}

// internalError answers 500 without the error's text, which can hold file
// paths, ZFS output or SQL; the text goes to the log instead.
func internalError(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error; see the db-branch agent log on the node")
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// withService resolves the branch service (503 before `db-branch init`).
func (s *Server) withService(h func(http.ResponseWriter, *http.Request, Branches)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, err := s.Branches()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "not_initialized", err.Error())
			return
		}
		h(w, r, svc)
	}
}

// named is withService plus a validated {name} path value.
func (s *Server) named(h func(http.ResponseWriter, *http.Request, Branches, domain.Name)) http.HandlerFunc {
	return s.withService(func(w http.ResponseWriter, r *http.Request, svc Branches) {
		n, err := domain.ParseName(r.PathValue("name"))
		if err != nil {
			fail(w, err)
			return
		}
		h(w, r, svc, n)
	})
}

// accepted starts op in the background and answers 202 with the operation.
func (s *Server) accepted(w http.ResponseWriter, r *http.Request, kind string, target operation.Target, f operation.Func) {
	op, err := s.Ops.StartAs(principalOf(r).ID, kind, target, f)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, op)
}

func items[T any](list []T) map[string]any {
	if list == nil {
		list = []T{}
	}
	return map[string]any{"items": list, "next_cursor": nil}
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	modules := append([]string{}, s.Modules...)
	probe := s.recordProbe(r.Context())
	out := map[string]any{"version": s.Version, "node": s.Node, "modules": modules, "pages": s.modulePages(),
		"record": recordAvailable(probe), "engines": s.engineViews(r.Context(), probe)}
	if len(s.RuntimeWarnings) > 0 {
		out["warnings"] = s.RuntimeWarnings
	}
	writeJSON(w, http.StatusOK, out)
}

func internalMaskTarget(r *http.Request) (string, bool) {
	if !forwardedFromNode(r) || r.Method != http.MethodPost {
		return "", false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/bases/")
	if !ok {
		return "", false
	}
	target, ok := strings.CutSuffix(rest, "/mask")
	if !ok || target == "" || strings.Contains(target, "/") {
		return "", false
	}
	return target, true
}
