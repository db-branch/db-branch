package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrFeatureNotSupported is returned for an operation the engine does not
// offer (its RuntimeCaps say false). It is distinct from "not implemented
// yet": the engine, by its structure, cannot do it, and nothing is faked.
var ErrFeatureNotSupported = errors.New("feature not supported by this engine")

// RuntimeCaps says which optional features an engine supports.
type RuntimeCaps struct {
	Query           bool
	Checkpoints     bool
	LiveFreeze      bool
	Upgrade         bool
	SourceDump      bool
	BranchMerge     bool
	HistoryRecord   bool
	UpdateStats     bool
	InstanceMetrics bool
}

// FeatureNames are the capability keys, in the order Features lists them.
var FeatureNames = []string{"query", "checkpoints", "live_freeze", "upgrade", "source_dump", "branch_merge", "history_record", "update_stats", "instance_metrics"}

// Features lists every capability by its key (the manifest's and the API's).
func (c RuntimeCaps) Features() map[string]bool {
	return map[string]bool{
		"query": c.Query, "checkpoints": c.Checkpoints, "live_freeze": c.LiveFreeze, "upgrade": c.Upgrade,
		"source_dump": c.SourceDump, "branch_merge": c.BranchMerge, "history_record": c.HistoryRecord,
		"update_stats": c.UpdateStats, "instance_metrics": c.InstanceMetrics,
	}
}

// Require fails with ErrFeatureNotSupported unless the engine has the feature
// (an unknown key counts as not supported).
func (c RuntimeCaps) Require(feature string) error {
	if !c.Features()[feature] {
		return FeatureNotSupported(feature)
	}
	return nil
}

// FeatureNotSupported is the ErrFeatureNotSupported error naming feature.
func FeatureNotSupported(feature string) error {
	return fmt.Errorf("%w: %s", ErrFeatureNotSupported, feature)
}

// HealthState is what a provider reports about a started instance.
type HealthState int

const (
	// HealthStopped: the server is not running.
	HealthStopped HealthState = iota
	// HealthStarting: running, not yet accepting connections.
	HealthStarting
	// HealthReady: accepting connections.
	HealthReady
)

func (h HealthState) String() string {
	switch h {
	case HealthStarting:
		return "starting"
	case HealthReady:
		return "ready"
	}
	return "stopped"
}

// ResourceLimits are what the core allows one instance.
type ResourceLimits struct {
	// MemoryMB sizes the engine's own cache (innodb_buffer_pool_size, shared_buffers, ...).
	MemoryMB       int
	MaxConnections int
	NoFile         int
	// CPUQuota is a percentage of one CPU (0: unlimited).
	CPUQuota int
}

// InstanceSpec is the run specification of one instance, given by the core to
// a provider. The built-in MySQL and PostgreSQL providers use ID, Runtime,
// DataDir, the primary port, Limits.MemoryMB, Extra, LegacyAuth and Maintenance
// (no network: MySQL skip-networking, PostgreSQL an empty listen_addresses).
// Their directories (StateDir, RunDir, RuntimeDir) and bind address come from
// their own configuration, and the other limits (MaxConnections, NoFile,
// CPUQuota) belong to the systemd unit, which only the db-branch-rt@ units of
// external engines (Phase 3) take from the core.
type InstanceSpec struct {
	// ID is unique per node, e.g. "base-prod" or "branch-feature-x".
	ID      string
	Runtime RuntimeRef
	// RuntimeDir is the unpacked runtime.
	RuntimeDir string
	// DataDir is the dataset's mount point: the only place persistent data goes.
	DataDir string
	// StateDir holds the instance's configuration and logs (outside the dataset).
	StateDir string
	// RunDir holds sockets and pid files.
	RunDir string
	// Bind is the listen address (forced local while Maintenance).
	Bind string
	// Ports maps a protocol name (EngineSpec.Protocols) to its TCP port.
	Ports  map[string]int
	Limits ResourceLimits
	// Maintenance: no network, work on the data only.
	Maintenance bool
	// LegacyAuth is an engine specific compatibility flag (MySQL 8.4's mysql_native_password).
	LegacyAuth bool
	// Extra are further configuration lines from a module (ext.ServerOptioner).
	Extra []string
}

// PrimaryPort is the port of the engine's primary protocol (Branch.Port).
func (i InstanceSpec) PrimaryPort(e EngineSpec) int {
	for _, p := range e.Protocols {
		if p.Primary {
			return i.Ports[p.Name]
		}
	}
	return 0
}

// Server is the instance as the DBServer port sees it.
func (i InstanceSpec) Server(e EngineSpec) Server {
	return Server{
		ID: i.ID, Runtime: i.Runtime, DataDir: i.DataDir, Port: i.PrimaryPort(e),
		MemoryMB: i.Limits.MemoryMB, Extra: i.Extra, LegacyAuth: i.LegacyAuth, Maintenance: i.Maintenance,
	}
}

// LaunchPlan is how the core (root) starts an instance a provider prepared:
// the command systemd runs and the unit settings that differ between engines.
type LaunchPlan struct {
	Command string
	Args    []string
	Env     map[string]string
	// ServiceType is systemd's Type= (simple, notify, exec or forking).
	ServiceType string
	// KillSignal is systemd's KillSignal=, e.g. SIGTERM, or SIGINT for PostgreSQL's fast shutdown.
	KillSignal string
	// TimeoutStopSec is how long a stop may take, in seconds.
	TimeoutStopSec int
}

// ErrInvalidLaunchPlan is returned for a LaunchPlan that could not be written
// into a systemd unit safely.
var ErrInvalidLaunchPlan = errors.New("invalid launch plan")

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	serviceTypes = map[string]bool{"": true, "simple": true, "exec": true, "notify": true, "forking": true}
	killSignals  = map[string]bool{"": true, "SIGTERM": true, "SIGINT": true, "SIGQUIT": true, "SIGHUP": true}
)

// Validate refuses a plan a provider could use to inject systemd directives
// (a newline, NUL or other control character in anything written into the
// unit) or to ask for a setting outside the allowed ones. The core calls it
// before it writes the unit's drop-in. It does not quote: the writer of the
// drop-in must escape spaces, quotes, "%" and "$" the way systemd reads
// ExecStart=, so an argument stays one argument.
func (p LaunchPlan) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidLaunchPlan, fmt.Sprintf(format, a...))
	}
	if !strings.HasPrefix(p.Command, "/") || p.Command == "/" || strings.HasSuffix(p.Command, "/") {
		return bad("command %q must be an absolute path", p.Command)
	}
	if hasControl(p.Command) {
		return bad("command contains a control character")
	}
	for _, a := range p.Args {
		if hasControl(a) {
			return bad("argument %q contains a control character", a)
		}
	}
	for k, v := range p.Env {
		if !envName.MatchString(k) {
			return bad("environment variable name %q", k)
		}
		if hasControl(v) {
			return bad("environment variable %s contains a control character", k)
		}
	}
	if !serviceTypes[p.ServiceType] {
		return bad("service type %q", p.ServiceType)
	}
	if !killSignals[p.KillSignal] {
		return bad("kill signal %q", p.KillSignal)
	}
	if p.TimeoutStopSec < 0 {
		return bad("negative stop timeout")
	}
	return nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// FreezeToken is what PreFreeze hands back for PostFreeze (opaque to the core).
type FreezeToken struct{ Data string }

// CatalogEntry is one downloadable build of an engine.
type CatalogEntry struct {
	Version  string
	Platform string
	URL      string
	SHA256   string
	Size     int64
}

// QueryRequest is one statement to run on an instance as a login.
type QueryRequest struct {
	// Host is the address the instance is reached at (the node it runs on);
	// empty: this node.
	Host           string
	User, Password string
	SQL            string
	MaxRows        int
}

// QueryResult is what a statement returned.
type QueryResult struct {
	Columns []string `json:"columns,omitempty"`
	// Rows hold values as text (NULL is nil).
	Rows      [][]*string `json:"rows,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	// Affected is set for statements that return no rows.
	Affected *int64 `json:"affected,omitempty"`
	Millis   int64  `json:"millis"`
}

// MetricSample is one instance metric.
type MetricSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// RuntimeProvider is everything the core needs from a database engine: the
// one contract the built-in engines and external runtime modules satisfy. The
// core, not the provider, owns ZFS, ownership (chown) and systemctl.
//
// The optional contracts are used only when EngineSpec.Capabilities says so;
// a provider without them embeds UnsupportedFeatures.
type RuntimeProvider interface {
	Describe(ctx context.Context) (EngineSpec, error)
	// Catalog lists the downloadable builds. Built-in engines return none:
	// the core's own catalog holds theirs.
	Catalog(ctx context.Context) ([]CatalogEntry, error)
	// PrepareRuntime fixes up an unpacked runtime in stagingDir (library links) and checks it runs.
	PrepareRuntime(ctx context.Context, version, stagingDir string) error

	// Initialize creates an empty cluster in DataDir with a local admin.
	Initialize(ctx context.Context, s InstanceSpec) error
	// PrepareClone removes the identity a clone inherited (server UUID, pid files).
	PrepareClone(ctx context.Context, s InstanceSpec) error
	// PrepareStart writes the instance's configuration and returns how to launch it.
	PrepareStart(ctx context.Context, s InstanceSpec) (LaunchPlan, error)
	Health(ctx context.Context, s InstanceSpec) (HealthState, error)
	// Quiesce readies a running server for the core to stop it (StopClean: flush everything).
	Quiesce(ctx context.Context, s InstanceSpec, mode StopMode) error
	// VerifyCleanShutdown fails with ErrUncleanShutdown unless the last stop was clean.
	VerifyCleanShutdown(ctx context.Context, s InstanceSpec) error
	// RemoveInstance deletes the instance's configuration and logs (not its data).
	RemoveInstance(ctx context.Context, s InstanceSpec) error

	// CreateUser makes (or keeps, and puts right) a login that may read and
	// change the data and the objects in it, and nothing else: no superuser,
	// no server files, no programs. It is safe to repeat; the core repeats it
	// on start, reset, restore and recovery.
	CreateUser(ctx context.Context, s InstanceSpec, user, password string) error
	// ExecAs runs init SQL as the login, never as the admin, with the client's
	// own commands (psql's \!, mysql's system) switched off.
	ExecAs(ctx context.Context, s InstanceSpec, user, password, sql string) error
	AdminAccess(ctx context.Context, s InstanceSpec) (AdminAccess, error)
	FormatDSN(ctx context.Context, s InstanceSpec, host, user, password string) (string, error)
	Query(ctx context.Context, s InstanceSpec, q QueryRequest) (QueryResult, error)
	TailLog(ctx context.Context, s InstanceSpec, lines int) (string, error)

	// Optional contracts.
	PreFreeze(ctx context.Context, s InstanceSpec) (FreezeToken, error)
	PostFreeze(ctx context.Context, s InstanceSpec, tok FreezeToken) error
	UpdateStats(ctx context.Context, s InstanceSpec) error
	CheckUpgradePath(ctx context.Context, fromVersion, toVersion string) error
	CheckUpgrade(ctx context.Context, s InstanceSpec, toVersion string, own Login) (UpgradePlan, error)
	StartUpgraded(ctx context.Context, s InstanceSpec, own Login) (LaunchPlan, UpgradeLog, error)
	DumpSource(ctx context.Context, job SourceDumpJob, outPath string) error
	Load(ctx context.Context, s InstanceSpec, dumpPath string) error
	InstanceMetrics(ctx context.Context, s InstanceSpec) ([]MetricSample, error)
}

// UnsupportedFeatures answers every optional contract with ErrFeatureNotSupported.
// Embed it in a provider and override what the engine offers.
type UnsupportedFeatures struct{}

func (UnsupportedFeatures) PreFreeze(context.Context, InstanceSpec) (FreezeToken, error) {
	return FreezeToken{}, FeatureNotSupported("live_freeze")
}
func (UnsupportedFeatures) PostFreeze(context.Context, InstanceSpec, FreezeToken) error {
	return FeatureNotSupported("live_freeze")
}
func (UnsupportedFeatures) UpdateStats(context.Context, InstanceSpec) error {
	return FeatureNotSupported("update_stats")
}
func (UnsupportedFeatures) CheckUpgradePath(context.Context, string, string) error {
	return FeatureNotSupported("upgrade")
}
func (UnsupportedFeatures) CheckUpgrade(context.Context, InstanceSpec, string, Login) (UpgradePlan, error) {
	return UpgradePlan{}, FeatureNotSupported("upgrade")
}
func (UnsupportedFeatures) StartUpgraded(context.Context, InstanceSpec, Login) (LaunchPlan, UpgradeLog, error) {
	return LaunchPlan{}, UpgradeLog{}, FeatureNotSupported("upgrade")
}
func (UnsupportedFeatures) DumpSource(context.Context, SourceDumpJob, string) error {
	return FeatureNotSupported("source_dump")
}
func (UnsupportedFeatures) Load(context.Context, InstanceSpec, string) error {
	return FeatureNotSupported("source_dump")
}
func (UnsupportedFeatures) InstanceMetrics(context.Context, InstanceSpec) ([]MetricSample, error) {
	return nil, FeatureNotSupported("instance_metrics")
}

// RuntimeProviders finds the provider of an engine.
type RuntimeProviders interface {
	Provider(e Engine) (RuntimeProvider, bool)
}

// ProviderSet is a RuntimeProviders over a map.
type ProviderSet map[Engine]RuntimeProvider

// Provider returns the provider registered for e.
func (p ProviderSet) Provider(e Engine) (RuntimeProvider, bool) {
	rp, ok := p[e]
	return rp, ok
}

// Instance is the branch as a provider sees it. Only what a client needs is
// known here: the port; directories and limits are the provider's own.
func (b Branch) Instance(spec EngineSpec) InstanceSpec {
	ports := map[string]int{}
	for _, p := range spec.Protocols {
		if p.Primary {
			ports[p.Name] = b.Port
		}
	}
	return InstanceSpec{ID: "branch-" + b.Name.String(), Runtime: b.Runtime, Ports: ports}
}
