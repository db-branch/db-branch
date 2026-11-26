// Package ext is the stable surface for db-branch modules: replacements for
// how people log in (Authenticator), what they may do (Authorizer) and where
// a record of it goes (AuditSink).
//
// The OSS agent always routes every API call through all three, with its own
// defaults (the single admin password, allow everything that is logged in,
// a structured log line), so a module that implements a port sees every call
// without further changes. Modules run as separate processes; see the plugin
// package for writing and loading them.
package ext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"
)

var (
	// ErrUnauthenticated: the credentials do not identify anyone. An
	// Authenticator returns it for credentials it does not handle.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden: the principal may not perform the action.
	ErrForbidden = errors.New("forbidden")
	// ErrStopAuth stops Authenticator chains even when joined with ErrUnauthenticated,
	// preventing fallback to lower-priority authenticators (e.g. for SSO-only accounts).
	ErrStopAuth = errors.New("stop authentication chain")
	// ErrInvalid indicates an invalid input or request.
	ErrInvalid = errors.New("invalid request")
	// ErrAlreadyExists indicates a resource already exists.
	ErrAlreadyExists = errors.New("already exists")
	// ErrNotFound indicates a resource does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict indicates a state conflict (e.g. removing the last active user).
	ErrConflict = errors.New("conflict")
	// ErrClusterUpgrading indicates the cluster protocol is not ready for the operation.
	ErrClusterUpgrading = errors.New("cluster upgrading")
	// ErrWeakPassword indicates a password fails strength checks.
	ErrWeakPassword = errors.New("weak password")
)

// RateLimitedError indicates too many failed authentication attempts.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("too many failed logins; retry in %s", e.RetryAfter.Round(time.Second))
}

// Principal is who is acting.
type Principal struct {
	// ID is stable and unique, e.g. "admin", "oidc:alice@example.com".
	ID string `json:"id"`
	// Name is for display.
	Name string `json:"name"`
	// Roles are opaque to the agent; Authorizers interpret them.
	Roles []string `json:"roles,omitempty"`
}

// Credentials are what a client presented: a username and password (console
// login) or a bearer token (API clients).
type Credentials struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
	// Client identifies the caller address for per-client rate limiting.
	Client string `json:"client,omitempty"`
}

// Authenticator turns credentials into a principal.
type Authenticator interface {
	Authenticate(ctx context.Context, c Credentials) (Principal, error)
}

// NoPasswordRole marks a Principal that signed in without a password here
// (e.g. through SSO): the console offers it no password change.
const NoPasswordRole = "password:none"

// BaseRolePrefix and TeamRolePrefix carry optional service-token restrictions
// in Principal.Roles across authenticators, RPC modules, and cluster forwarding.
const (
	BaseRolePrefix = "base:"
	TeamRolePrefix = "team:"
)

// TokenBases returns the sorted, deduplicated base names a restricted token is
// limited to (nil means no base restriction).
func TokenBases(p Principal) []string {
	var out []string
	for _, r := range p.Roles {
		b, ok := strings.CutPrefix(r, BaseRolePrefix)
		if !ok || b == "" || slices.Contains(out, b) {
			continue
		}
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// TokenTeam returns the team name a restricted token is linked to ("" if none).
func TokenTeam(p Principal) string {
	for _, r := range p.Roles {
		if t, ok := strings.CutPrefix(r, TeamRolePrefix); ok && t != "" {
			return t
		}
	}
	return ""
}

// PasswordChanger is implemented by Authenticators whose users have a
// password they may change themselves (from the console's account page).
type PasswordChanger interface {
	ChangePassword(ctx context.Context, p Principal, current, next string) error
}

// PrincipalResolver re-checks a session or token principal against a module's
// current user directory so role changes and disabled or removed users take
// effect immediately. Resolvers return ErrUnauthenticated when the principal
// belongs to the module and is no longer active, or return p unchanged for
// principals owned by other authenticators.
type PrincipalResolver interface {
	ResolvePrincipal(ctx context.Context, p Principal) (Principal, error)
}

// Resolvers chains PrincipalResolvers in order: each resolver updates or
// rejects principals it owns and passes others through.
type Resolvers []PrincipalResolver

// ResolvePrincipal runs every resolver in order, stopping on the first error.
func (rs Resolvers) ResolvePrincipal(ctx context.Context, p Principal) (Principal, error) {
	for _, r := range rs {
		next, err := r.ResolvePrincipal(ctx, p)
		if err != nil {
			return Principal{}, err
		}
		p = next
	}
	return p, nil
}

// BranchInfo is an existing branch, as a BranchPolicy sees it.
type BranchInfo struct {
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Base      string `json:"base"`
	OwnerTeam string `json:"owner_team,omitempty"`
}

// BranchRequest is a branch someone (not an admin) is about to create.
type BranchRequest struct {
	Name     string        `json:"name"`
	Owner    string        `json:"owner"`
	Base     string        `json:"base"`
	Snapshot string        `json:"snapshot,omitempty"`
	TTL      time.Duration `json:"ttl"` // 0: no expiry
	Access   string        `json:"access"`
	// Existing are all the branches now, with their owners.
	Existing []BranchInfo `json:"existing"`
}

// BranchPolicy lets a module limit branches (e.g. per team: how many,
// how long, from which bases) and let people manage branches of others
// (e.g. their teammates).
type BranchPolicy interface {
	// CheckBranch refuses a request with ErrForbidden or a conflict.
	CheckBranch(ctx context.Context, p Principal, r BranchRequest) error
	// SharesBranches reports whether p may manage owner's branches.
	SharesBranches(ctx context.Context, p Principal, owner string) bool
}

// BranchExtendPolicy may be implemented by a BranchPolicy to check TTL
// extensions (PATCH /v1/branches/{name}). Without it, extension only relies
// on branch ownership and the branch's own TTL rules.
type BranchExtendPolicy interface {
	CheckExtend(ctx context.Context, p Principal, r BranchRequest) error
}

// BranchTTLCap may be implemented by a BranchPolicy to report the maximum
// branch TTL allowed for p on base when a caller omits ttl, so the agent can
// clamp its default TTL instead of refusing with ErrForbidden.
type BranchTTLCap interface {
	MaxBranchTTL(ctx context.Context, p Principal, base string) (time.Duration, bool)
}

// BranchSharer may be implemented by a BranchHost so modules can ask whether a
// caller shares branch access with a branch's owner through BranchPolicy.
type BranchSharer interface {
	SharesBranches(ctx context.Context, p Principal, owner string) bool
}

// Policies combines branch policies: every one must allow a branch, and
// any one may share.
type Policies []BranchPolicy

func (ps Policies) CheckBranch(ctx context.Context, p Principal, r BranchRequest) error {
	for _, x := range ps {
		if err := x.CheckBranch(ctx, p, r); err != nil {
			return err
		}
	}
	return nil
}

// CheckExtend runs CheckExtend on every policy that implements BranchExtendPolicy.
func (ps Policies) CheckExtend(ctx context.Context, p Principal, r BranchRequest) error {
	for _, x := range ps {
		if ep, ok := x.(BranchExtendPolicy); ok {
			if err := ep.CheckExtend(ctx, p, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// MaxBranchTTL returns the tightest maximum TTL enforced across policies that
// implement BranchTTLCap.
func (ps Policies) MaxBranchTTL(ctx context.Context, p Principal, base string) (time.Duration, bool) {
	var minTTL time.Duration
	capped := false
	for _, x := range ps {
		tc, ok := x.(BranchTTLCap)
		if !ok {
			continue
		}
		if d, ok := tc.MaxBranchTTL(ctx, p, base); ok && d > 0 {
			if !capped || d < minTTL {
				minTTL = d
				capped = true
			}
		}
	}
	return minTTL, capped
}

func (ps Policies) SharesBranches(ctx context.Context, p Principal, owner string) bool {
	for _, x := range ps {
		if x.SharesBranches(ctx, p, owner) {
			return true
		}
	}
	return false
}

// Resource is what an action is about.
type Resource struct {
	// Kind is "branch", "base", "runtime", "route-user", "cluster", "auth".
	Kind string `json:"kind"`
	// Name is empty for collections.
	Name string `json:"name,omitempty"`
}

// Authorizer decides whether p may perform action (e.g. "branch.create",
// "branch.read") on r. It returns nil or an error wrapping ErrForbidden.
type Authorizer interface {
	Authorize(ctx context.Context, p Principal, action string, r Resource) error
}

// Outcome of an audited action.
const (
	OutcomeOK     = "ok"
	OutcomeDenied = "denied"
	OutcomeFailed = "failed"
	// OutcomeAttempt is recorded before a change runs when the cluster fails
	// closed (setting audit.fail_closed); the change's own event follows.
	OutcomeAttempt = "attempt"
)

// Event is one audited action.
type Event struct {
	Time      time.Time `json:"time"`
	Node      string    `json:"node"`
	Principal Principal `json:"principal"`
	Action    string    `json:"action"`
	Resource  Resource  `json:"resource"`
	Outcome   string    `json:"outcome"`
	// Status is the HTTP status the call ended with.
	Status int `json:"status"`
	// Remote is the client address ("" for the local socket).
	Remote string `json:"remote,omitempty"`
}

// AuditSink receives every changing action and every denial. Record must not
// block for long; failures are logged, not returned to the client.
type AuditSink interface {
	Record(ctx context.Context, e Event) error
}

// RouteServer is a branch's database server, in the hostgroup of the branch.
type RouteServer struct {
	Hostgroup int
	Host      string
	Port      int
}

// RouteUser is a proxy login and the hostgroup it reaches without a comment.
type RouteUser struct {
	Name      string
	Password  string
	Hostgroup int
}

// RouteRule is a query rule. Rules with FlagIn 0 are checked first; Pattern
// is a regular expression on the query text, comments included.
type RouteRule struct {
	ID      int
	FlagIn  int
	FlagOut int
	Pattern string
	// Username limits the rule to one login; "" matches every login.
	Username  string
	Hostgroup int // destination; 0 keeps the user's
	Error     string
	Apply     bool
}

// RouteTables is what a proxy is told for one protocol.
type RouteTables struct {
	Servers []RouteServer
	Users   []RouteUser
	Rules   []RouteRule
}

// RouteConfig is the full routing of the cluster as seen from this node:
// MySQL branches and PostgreSQL branches.
type RouteConfig struct {
	MySQL    RouteTables
	Postgres RouteTables
}

// Router configures a connection proxy on this node (the db-branch-proxysql
// module drives ProxySQL). The agent calls Route with the whole config
// whenever it changes; Route replaces what the proxy had.
type Router interface {
	Route(ctx context.Context, c RouteConfig) error
}

// Setting describes one setting a module (or db-branch itself) offers in the
// console. Values travel as strings.
type Setting struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
	// Type is "string", "text" (long), "secret" (a key or password, shown
	// masked), "int", "bool" or "duration" (e.g. "72h").
	Type    string `json:"type"`
	Default string `json:"default"`
	// Translations are the Label and Help in other console languages,
	// by language code (e.g. "ja"). Missing: the English text.
	Translations map[string]SettingText `json:"translations,omitempty"`
}

// SettingText is a setting's Label and Help in one language.
type SettingText struct {
	Label string `json:"label,omitempty"`
	Help  string `json:"help,omitempty"`
}

// Configurable receives the module's settings: once at start and whenever
// they change, the same values on every node. Missing keys mean the default.
// Besides its own keys a module receives LicenseKey (the cluster's licence,
// see package license), NodesKey (how many nodes the cluster has) and
// TrialKey (when the cluster's trial began; see license.Trial). An
// error (e.g. no valid licence) is shown in the console for that node.
type Configurable interface {
	Configure(ctx context.Context, values map[string]string) error
}

// Keys the agent adds to every module's settings.
const (
	LicenseKey = "_license"
	NodesKey   = "_nodes"
	TrialKey   = "_trial" // RFC 3339; empty before a paid module was loaded
)

// Request is a call to a module's own API: /v1/ext/<module>/<Path>. The
// agent has already authenticated and authorized it (action
// "<module>.read" for GET, "<module>.write" otherwise) and audits writes.
type Request struct {
	Method string `json:"method"`
	// Path is below the module's prefix, starting with "/".
	Path      string    `json:"path"`
	RawQuery  string    `json:"raw_query,omitempty"`
	Body      []byte    `json:"body,omitempty"`
	Principal Principal `json:"principal"`
	// Cookies are the module's own cookies (named dbbx_<module>_...).
	Cookies map[string]string `json:"cookies,omitempty"`
}

// Response answers a Request.
type Response struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
	// Headers the agent passes on: Location, Cache-Control, and Set-Cookie
	// for the module's own cookies (dbbx_<module>_...). Others are dropped.
	Headers map[string]string `json:"headers,omitempty"`
}

// LoginMethod is a way to sign in a module offers on the console's login
// screen, e.g. single sign-on: a button that opens Path (a public path).
type LoginMethod struct {
	Label string `json:"label"`
	Path  string `json:"path"`
	// StatusPath (public) answers {"enabled":true} when the method is set
	// up; the button is shown only then. Empty: always shown.
	StatusPath string `json:"status_path,omitempty"`
}

// Handler serves a module's own API and console pages.
type Handler interface {
	Serve(ctx context.Context, r Request) (Response, error)
}

// Page is a console page the module serves at /v1/ext/<module><Path> (an
// HTML document; its scripts and styles must be files, not inline). The
// console lists it in the navigation and shows it in a frame.
type Page struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

// Store is a module's cluster-wide data: small records such as users and
// policies, replicated to every node (not for logs). Keys are up to 256
// bytes, values up to 64 KiB.
type Store interface {
	List(ctx context.Context) (map[string][]byte, error)
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
}

// AtomicStore is an optional extension of Store providing cluster-linearizable
// reads and atomic one-time operations.
type AtomicStore interface {
	Store
	Get(ctx context.Context, key string) ([]byte, bool, error)
	PutIfAbsent(ctx context.Context, key string, value []byte) (bool, error)
	Take(ctx context.Context, key string) ([]byte, bool, error)
}

// ServerBranch is a branch whose database server is being configured.
type ServerBranch struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	// Engine is "mysql" or "postgres"; Version the runtime's.
	Engine  string `json:"engine"`
	Version string `json:"version"`
	// Record: the branch records its changes. Decided once, when the branch
	// is created (it asked to, or a module said so in answer to Creating),
	// and fixed for as long as the branch exists: a module records exactly
	// the branches with Record, whatever its settings say later.
	Record bool `json:"record"`
	// Creating: the branch is being created and has not asked to record
	// (Record is false). The agent asks once: a module that wants every
	// new branch recorded (a record_all setting) answers with
	// ServerOptions.Record, and the agent keeps the answer as the branch's
	// Record. It is false for every later question.
	Creating bool `json:"creating,omitempty"`
	// Anonymous: made without a login (open to anyone). Never record these.
	Anonymous bool `json:"anonymous"`
	// Maintenance: the server is started only to work on its data (see
	// RestoreFinisher); it takes no network connections.
	Maintenance bool `json:"maintenance"`
}

// ServerOptions are what a module adds to a branch's server.
type ServerOptions struct {
	// Config are further lines for the server's configuration file. MySQL:
	// lines of the [mysqld] section of my.cnf (e.g. "log_bin=binlog").
	// PostgreSQL: not applied yet. Lines that clash with the agent's own
	// settings are the module's responsibility.
	Config []string `json:"config,omitempty"`
	// Record: only as the answer to ServerBranch.Creating: record this new
	// branch although it did not ask to. The agent stores it as the branch's
	// Record, which never changes afterwards. Ignored for any other question.
	Record bool `json:"record,omitempty"`
}

// ServerOptioner adds options to branch servers. The agent asks each time
// it writes a branch's server configuration: when the branch is created or
// recreated, started again (agent or node restart) and restored. An error at
// creation fails the creation (and is shown to the person creating it, an
// error wrapping ErrForbidden as a refusal); at any later start the server
// starts without the options and the error is logged, so a lapsed licence
// never keeps a branch down.
type ServerOptioner interface {
	ServerOptions(ctx context.Context, b ServerBranch) (ServerOptions, error)
}

// ServerOptioners combines modules' options: all lines, in module order; the
// first error wins.
type ServerOptioners []ServerOptioner

func (list ServerOptioners) ServerOptions(ctx context.Context, b ServerBranch) (ServerOptions, error) {
	var out ServerOptions
	for _, o := range list {
		x, err := o.ServerOptions(ctx, b)
		if err != nil {
			return ServerOptions{}, err
		}
		out.Config = append(out.Config, x.Config...)
		out.Record = out.Record || x.Record
	}
	return out, nil
}

// RestoreFinisher works on a branch's data right after a restore (of a
// checkpoint, or of its creation state) that a module started
// with a note (BranchHost.RestoreCheckpoint): the data is
// in place, the server runs in maintenance mode (ServerBranch.Maintenance;
// it takes no network connections) and nobody has connected yet. The module
// reaches the server through BranchHost.DB. It runs again from the start
// when the agent was interrupted, so it must be repeatable. An error does
// not undo the restore (the branch holds the checkpoint's data): the agent
// logs it, and the module is expected to report it to the person itself.
// It never runs for restores that carry no note.
type RestoreFinisher interface {
	AfterRestore(ctx context.Context, b ServerBranch, note string) error
}

// RestoreFinishers runs every module's work; errors are joined.
type RestoreFinishers []RestoreFinisher

func (fs RestoreFinishers) AfterRestore(ctx context.Context, b ServerBranch, note string) error {
	var errs []error
	for _, f := range fs {
		errs = append(errs, f.AfterRestore(ctx, b, note))
	}
	return errors.Join(errs...)
}

// BranchState is a branch on this node, as a module sees it.
type BranchState struct {
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Engine string `json:"engine"`
	// Version is the runtime's, e.g. "8.4.13".
	Version string `json:"version"`
	// Status is "ready", "restoring" and so on.
	Status    string    `json:"status"`
	Record    bool      `json:"record"`
	Anonymous bool      `json:"anonymous"`
	CreatedAt time.Time `json:"created_at"`
	// Base and Snapshot are what the branch was made from (empty from an
	// agent that does not tell).
	Base     string `json:"base,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
}

// CheckpointInfo is one checkpoint (see docs/checkpoints.md).
type CheckpointInfo struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Line is the line of history it was taken on.
	Line    int  `json:"line"`
	Auto    bool `json:"auto"`
	Current bool `json:"current"`
}

// NoLine is LineInfo.Parent of a line that was not made by a restore.
const NoLine = -1

// LineInfo is a line of a branch's history. A restore starts a new line:
// Parent is the line of the checkpoint it was restored from (NoLine for the
// first line), From that checkpoint's name ("reset" for the creation state).
type LineInfo struct {
	Line   int    `json:"line"`
	Live   bool   `json:"live"`
	Parent int    `json:"parent"`
	From   string `json:"from"`
}

// CheckpointGraph is a branch's checkpoints and the structure of its lines.
type CheckpointGraph struct {
	Checkpoints []CheckpointInfo `json:"checkpoints"`
	Lines       []LineInfo       `json:"lines"`
	// Live is the line the branch runs on; Head the checkpoint its data
	// descends from.
	Live int    `json:"live"`
	Head string `json:"head"`
}

// BranchHost is what a module may do with the branches of the node it runs
// on: look at them, take and restore checkpoints, reach the database server
// and read the data of any line. Branches of other nodes answer an error;
// the module runs on every node and the agent sends a request for a branch
// to its node (see Handler). Host.Branches is nil when the agent is older
// than the module.
type BranchHost interface {
	// Branch returns one branch of this node.
	Branch(ctx context.Context, name string) (BranchState, error)
	// Branches lists the branches of this node.
	Branches(ctx context.Context) ([]BranchState, error)
	// Graph returns the checkpoints and lines of a branch.
	Graph(ctx context.Context, name string) (CheckpointGraph, error)
	// CreateCheckpoint takes a checkpoint ("" names it from the time).
	CreateCheckpoint(ctx context.Context, name, checkpoint string) (CheckpointInfo, error)
	// RestoreCheckpoint restores the branch to a checkpoint and waits until
	// it is done (the branch is ready again). The checkpoint "@reset" is the
	// branch's creation state. As for any restore, what the branch holds now
	// is kept as a checkpoint. A non-empty note is handed to the
	// RestoreFinishers while the server is in maintenance mode.
	RestoreCheckpoint(ctx context.Context, name, checkpoint, note string) error
	// DB is how to reach the branch's server as its local admin (a socket
	// on this node; BinDir holds the client tools, which need LibDir).
	DB(ctx context.Context, name string) (DBAccess, error)
	// OpenLine mounts, read-only, the data of a line of the branch (the live
	// one, or one a restore set aside) at a directory on this node; when
	// checkpoint is not empty, the data as that checkpoint saved it ("@reset":
	// the branch's creation state). Close it with CloseLine. The live line's directory is the server's own data
	// directory: read, never write.
	OpenLine(ctx context.Context, name string, line int, checkpoint string) (dir string, err error)
	// CloseLine releases what OpenLine returned.
	CloseLine(ctx context.Context, dir string) error
	// StartOperation records an operation of this module on a branch, shown
	// like the agent's own (console, GET /v1/operations/{id}). The module
	// reports progress and ends it; one the module never ends fails after
	// an hour. Returns the operation's id.
	StartOperation(ctx context.Context, kind, branch string) (id string, err error)
	// ReportOperation sets the phase shown for the operation.
	ReportOperation(ctx context.Context, id, phase string) error
	// EndOperation finishes the operation; a non-empty failure marks it failed.
	EndOperation(ctx context.Context, id, failure string) error
}

// ModuleAPI lets a module call its own API (its Handler) through the agent,
// the way a client would, and so reach the same module on another node: a
// request below /branches/<name>/ is answered by the node holding the
// branch, and "node=<name>" in RawQuery names a node. The call is
// authorized as the module's ext.<module>.read (GET) or .write action for
// r.Principal, which the module passes on from the request it is serving,
// and a write is audited like any other. Host.API is nil when the agent is
// older than the module.
type ModuleAPI interface {
	Call(ctx context.Context, r Request) (Response, error)
}

// User is a core console user account exposed to modules via Host.Users.
type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email,omitempty"`
	Name        string    `json:"name"`
	Disabled    bool      `json:"disabled,omitempty"`
	MustChange  bool      `json:"must_change,omitempty"`
	HasPassword bool      `json:"has_password,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UserCreate describes a new core user account.
type UserCreate struct {
	Email         string `json:"email"`
	Name          string `json:"name,omitempty"`
	Password      string `json:"password,omitempty"`
	MustChange    bool   `json:"must_change,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	EmptyPassword bool   `json:"empty_password,omitempty"`
}

// UserUpdate describes partial updates to a core user account.
type UserUpdate struct {
	Name          *string `json:"name,omitempty"`
	Password      *string `json:"password,omitempty"`
	Disabled      *bool   `json:"disabled,omitempty"`
	MustChange    *bool   `json:"must_change,omitempty"`
	ClearPassword bool    `json:"clear_password,omitempty"`
}

// UserStore lets modules (such as auth) read and manage core user accounts
// without duplicating user credentials or lifecycle state.
type UserStore interface {
	ListUsers(ctx context.Context) ([]User, error)
	GetUser(ctx context.Context, id string) (User, error)
	CreateUser(ctx context.Context, in UserCreate) (User, error)
	UpdateUser(ctx context.Context, id string, in UserUpdate) (User, error)
	DeleteUser(ctx context.Context, id string) error
	VerifyPassword(ctx context.Context, client, username, password string) (User, error)
}

// Host is what the agent gives a module at start.
type Host struct {
	// API is the module's way to call its own API on any node (see
	// ModuleAPI). Nil when the agent is older than the module.
	API ModuleAPI
	// Branches lets the module work with this node's branches. Nil when the
	// agent is older than the module.
	Branches BranchHost
	// Users lets modules access core user accounts. Nil when the agent is older.
	Users UserStore
	// Node is this node's name.
	Node string
	// DataDir is the module's own directory on this node
	// (/var/lib/db-branch/modules/<name>), for node-local files.
	DataDir string
	// Store is the module's cluster-wide data.
	Store Store
}

// Starter is told about its Host once, after loading and before any call.
type Starter interface {
	Start(ctx context.Context, h Host) error
}

// Module bundles the ports a module provides; unset ports keep the defaults.
type Module struct {
	Name              string
	Version           string
	Authenticator     Authenticator
	PrincipalResolver PrincipalResolver
	Authorizer        Authorizer
	AuditSink     AuditSink
	Router        Router
	// Ports are the Router's client ports by engine ("mysql", "postgres").
	Ports map[string]int
	// Settings are shown in the console and sent to Configurable.
	Settings     []Setting
	Configurable Configurable
	Handler      Handler
	// PublicPaths are path prefixes of the module's API that answer without
	// a login (e.g. "/sso/" for single sign-on). The module checks them.
	PublicPaths []string
	// LoginMethods are shown on the login screen.
	LoginMethods []LoginMethod
	// BranchPolicy limits and shares branches. Optional.
	BranchPolicy BranchPolicy
	// RequiresLicense: the module works only under a licence key that
	// covers it; the console says so beside it.
	RequiresLicense bool
	// Pages are the module's console pages, served by Handler.
	Pages   []Page
	Starter Starter
	// Storage replaces the node's ZFS pool for bases and branches (see
	// Storage). SharedStorage: every node sees the same datasets.
	Storage       Storage
	SharedStorage bool
	// Masker writes masked copies of databases (db-branch base mask
	// --masker <module>); MaskPolicy governs those runs.
	Masker     Masker
	MaskPolicy MaskPolicy
	// RefreshFollower says which masked bases to rebuild after a base is
	// refreshed on its schedule. Optional.
	RefreshFollower RefreshFollower
	// ServerOptioner adds options to branch servers; RestoreFinisher works
	// on a branch right after a restore a module started. Optional.
	ServerOptioner  ServerOptioner
	RestoreFinisher RestoreFinisher
}

// DBAccess is how a tool on the node reaches a database server as its
// local admin: over the local socket, without a password.
type DBAccess struct {
	Engine string `json:"engine"` // "mysql" or "postgres"
	// Host and Port reach it over TCP (tools without socket support need
	// a login of their own: create one over the socket and drop it after).
	Host string `json:"host"`
	// Socket is the socket directory (PostgreSQL) or file (MySQL).
	Socket string `json:"socket"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	// BinDir holds the runtime's client tools; they need LibDir on
	// LD_LIBRARY_PATH.
	BinDir string `json:"bin_dir"`
	LibDir string `json:"lib_dir"`
	// Endpoints are the instance's addresses by protocol name (an engine
	// with several ports, e.g. ClickHouse's "http" and "native"); Host, Port
	// and Socket above are those of its primary protocol. Empty for the
	// engines that have one. Env is what a client tool of the engine needs
	// set in its environment.
	Endpoints map[string]Endpoint `json:"endpoints,omitempty"`
	Env       map[string]string   `json:"env,omitempty"`
}

// Endpoint is where one protocol of an instance listens.
type Endpoint struct {
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Socket string `json:"socket,omitempty"`
}

// MaskJob: read Source and write a masked copy into Target (an empty
// database server of the same engine and version) by Rules, in the
// masker's own format.
type MaskJob struct {
	Source, Target DBAccess
	Rules          string
}

// Masker is a masking tool (Greenmask, PostgreSQL Anonymizer, ...).
type Masker interface {
	Mask(ctx context.Context, job MaskJob) (log string, err error)
}

// MaskPlan describes a mask run to a MaskPolicy.
type MaskPlan struct {
	Source, SourceSnapshot, Target, Masker, Rules string
	// By is who started the run.
	By                 string
	SourceDB, TargetDB DBAccess
}

// MaskPolicy governs mask runs: PrepareMask may refuse one or return the
// rules to use instead (e.g. the approved ones); VerifyMask checks the
// masked target before it is snapshotted (an error discards it);
// MaskRecorded learns the snapshot it became.
type MaskPolicy interface {
	PrepareMask(ctx context.Context, p MaskPlan) (rules string, err error)
	VerifyMask(ctx context.Context, p MaskPlan) error
	MaskRecorded(ctx context.Context, p MaskPlan, snapshot string) error
}

// MaskRun names a mask run to start: the rules come from the MaskPolicy's
// PrepareMask as for any run.
type MaskRun struct {
	Target, Masker string
}

// RefreshFollower is asked after a base has been refreshed from its source
// (snapshot is the one just taken) which mask runs to start with it as the
// source. An error means "none": the refresh itself is never failed by it.
type RefreshFollower interface {
	AfterRefresh(ctx context.Context, base, snapshot string) ([]MaskRun, error)
}

// Followers combines refresh followers: the runs of all are concatenated;
// one that fails is logged and skipped.
type Followers []RefreshFollower

func (fs Followers) AfterRefresh(ctx context.Context, base, snapshot string) ([]MaskRun, error) {
	var runs []MaskRun
	for _, f := range fs {
		r, err := f.AfterRefresh(ctx, base, snapshot)
		if err != nil {
			slog.Warn("a module's after-refresh runs were skipped", "base", base, "err", err)
			continue
		}
		runs = append(runs, r...)
	}
	return runs, nil
}

// Storage is copy-on-write storage a module provides in place of the node's
// ZFS pool, chosen when the node is set up (db-branch init --storage
// <module>). Datasets are slash-separated paths like zfs ones
// ("<pool>/db-branch/branches/x"); a snapshot is a dataset and a name.
type Storage interface {
	// Create makes dataset, and missing parents, with ZFS properties
	// (recordsize, compression, ...) the storage may honour.
	Create(ctx context.Context, dataset string, props map[string]string) error
	Snapshot(ctx context.Context, dataset, snapshot string) error
	// Clone makes target from dataset@snapshot, creating missing parents.
	Clone(ctx context.Context, dataset, snapshot, target string) error
	// Rollback returns dataset to its latest snapshot, which is snapshot.
	Rollback(ctx context.Context, dataset, snapshot string) error
	// Destroy removes dataset and its snapshots; it fails while clones of
	// them exist.
	Destroy(ctx context.Context, dataset string) error
	// DestroySnapshot fails while clones of it exist.
	DestroySnapshot(ctx context.Context, dataset, snapshot string) error
	Exists(ctx context.Context, dataset string) (bool, error)
	// Info mounts dataset on this node if needed and reports where.
	Info(ctx context.Context, dataset string) (DatasetInfo, error)
	// Mounted lists the datasets under parent this node has mounted, by
	// their last path component (shared storage).
	Mounted(ctx context.Context, parent string) ([]string, error)
	// Release unmounts dataset from this node and keeps it (shared storage).
	Release(ctx context.Context, dataset string) error
}

// DatasetInfo is where a dataset is mounted and its space in bytes.
type DatasetInfo struct {
	Mountpoint string
	// Used is the space only this dataset holds; Referenced all it sees.
	Used, Referenced uint64
}

// AllowAuthenticated is the OSS Authorizer: anyone logged in may do anything.
type AllowAuthenticated struct{}

func (AllowAuthenticated) Authorize(_ context.Context, p Principal, _ string, _ Resource) error {
	if p.ID == "" {
		return ErrForbidden
	}
	return nil
}

// Sinks fans an event out to several sinks, returning their errors joined.
type Sinks []AuditSink

func (s Sinks) Record(ctx context.Context, e Event) error {
	var errs []error
	for _, sink := range s {
		errs = append(errs, sink.Record(ctx, e))
	}
	return errors.Join(errs...)
}

// Chain tries authenticators in order; the first that does not return
// ErrUnauthenticated decides.
type Chain []Authenticator

func (c Chain) Authenticate(ctx context.Context, cr Credentials) (Principal, error) {
	for _, a := range c {
		p, err := a.Authenticate(ctx, cr)
		if !errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrStopAuth) {
			return p, err
		}
	}
	return Principal{}, ErrUnauthenticated
}

func (c Chain) changers() []PasswordChanger {
	var out []PasswordChanger
	for _, a := range c {
		if pc, ok := a.(PasswordChanger); ok {
			out = append(out, pc)
		}
	}
	return out
}

// changingChain is a Chain some of whose authenticators let their users
// change passwords: each is asked in turn, as with signing in.
type changingChain struct {
	Chain
	changers []PasswordChanger
}

func (c changingChain) ChangePassword(ctx context.Context, p Principal, current, next string) error {
	for _, pc := range c.changers {
		if err := pc.ChangePassword(ctx, p, current, next); !errors.Is(err, ErrUnauthenticated) {
			return err
		}
	}
	return ErrUnauthenticated
}

// AllOf allows an action only if every authorizer does.
type AllOf []Authorizer

func (a AllOf) Authorize(ctx context.Context, p Principal, action string, r Resource) error {
	for _, z := range a {
		if err := z.Authorize(ctx, p, action, r); err != nil {
			return err
		}
	}
	return nil
}

// Combine merges modules: authenticators are tried in order, every
// authorizer must allow, every sink receives every event. Ports no module
// provides stay nil (the agent's defaults).
func Combine(mods []Module) Module {
	var auths Chain
	var resolvers Resolvers
	var authz AllOf
	var sinks Sinks
	var policies Policies
	var followers Followers
	var optioners ServerOptioners
	var finishers RestoreFinishers
	for _, m := range mods {
		if m.ServerOptioner != nil {
			optioners = append(optioners, m.ServerOptioner)
		}
		if m.RestoreFinisher != nil {
			finishers = append(finishers, m.RestoreFinisher)
		}
		if m.RefreshFollower != nil {
			followers = append(followers, m.RefreshFollower)
		}
		if m.BranchPolicy != nil {
			policies = append(policies, m.BranchPolicy)
		}
		if m.Authenticator != nil {
			auths = append(auths, m.Authenticator)
		}
		if m.PrincipalResolver != nil {
			resolvers = append(resolvers, m.PrincipalResolver)
		}
		if m.Authorizer != nil {
			authz = append(authz, m.Authorizer)
		}
		if m.AuditSink != nil {
			sinks = append(sinks, m.AuditSink)
		}
	}
	var out Module
	for _, m := range mods {
		if m.MaskPolicy != nil {
			out.MaskPolicy = m.MaskPolicy // one governs mask runs: the first wins
			break
		}
	}
	for _, m := range mods {
		if m.Router != nil {
			out.Router, out.Ports = m.Router, m.Ports // one proxy per node: the first wins
			break
		}
	}
	if len(auths) > 0 {
		out.Authenticator = auths
		if pw := auths.changers(); len(pw) > 0 {
			out.Authenticator = changingChain{auths, pw}
		}
	}
	if len(resolvers) > 0 {
		out.PrincipalResolver = resolvers
	}
	if len(authz) > 0 {
		out.Authorizer = authz
	}
	if len(policies) > 0 {
		out.BranchPolicy = policies
	}
	if len(sinks) > 0 {
		out.AuditSink = sinks
	}
	if len(followers) > 0 {
		out.RefreshFollower = followers
	}
	if len(optioners) > 0 {
		out.ServerOptioner = optioners
	}
	if len(finishers) > 0 {
		out.RestoreFinisher = finishers
	}
	return out
}
