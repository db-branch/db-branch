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
	"log/slog"
	"time"
)

var (
	// ErrUnauthenticated: the credentials do not identify anyone. An
	// Authenticator returns it for credentials it does not handle.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden: the principal may not perform the action.
	ErrForbidden = errors.New("forbidden")
)

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
}

// Authenticator turns credentials into a principal.
type Authenticator interface {
	Authenticate(ctx context.Context, c Credentials) (Principal, error)
}

// NoPasswordRole marks a Principal that signed in without a password here
// (e.g. through SSO): the console offers it no password change.
const NoPasswordRole = "password:none"

// PasswordChanger is implemented by Authenticators whose users have a
// password they may change themselves (from the console's account page).
type PasswordChanger interface {
	ChangePassword(ctx context.Context, p Principal, current, next string) error
}

// BranchInfo is an existing branch, as a BranchPolicy sees it.
type BranchInfo struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Base  string `json:"base"`
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
	ID        int
	FlagIn    int
	FlagOut   int
	Pattern   string
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

// Host is what the agent gives a module at start.
type Host struct {
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
	Name          string
	Version       string
	Authenticator Authenticator
	Authorizer    Authorizer
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
		if !errors.Is(err, ErrUnauthenticated) {
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
	var authz AllOf
	var sinks Sinks
	var policies Policies
	var followers Followers
	for _, m := range mods {
		if m.RefreshFollower != nil {
			followers = append(followers, m.RefreshFollower)
		}
		if m.BranchPolicy != nil {
			policies = append(policies, m.BranchPolicy)
		}
		if m.Authenticator != nil {
			auths = append(auths, m.Authenticator)
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
	return out
}
