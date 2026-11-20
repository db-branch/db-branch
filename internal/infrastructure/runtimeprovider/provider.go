// Package runtimeprovider adapts a DBServer (the built-in MySQL and
// PostgreSQL servers) to the domain.RuntimeProvider contract, so the core can
// treat built-in and external engines alike.
package runtimeprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

// Hooks are the parts an engine does differently from what DBServer offers.
// A nil Query or DumpSource means the engine does not offer it through this provider.
type Hooks struct {
	// PrepareRuntime fixes up an unpacked runtime (library links); nil: nothing to do.
	PrepareRuntime func(dir string) error
	PrepareStart   func(ctx context.Context, s domain.Server) (domain.LaunchPlan, error)
	Health         func(ctx context.Context, s domain.Server) (domain.HealthState, error)
	Quiesce        func(ctx context.Context, s domain.Server, mode domain.StopMode) error
	VerifyShutdown func(ctx context.Context, s domain.Server) error
	TailLog        func(ctx context.Context, s domain.Server, lines int) (string, error)
	Query          func(ctx context.Context, s domain.Server, q domain.QueryRequest) (domain.QueryResult, error)
	DumpSource     func(ctx context.Context, job domain.SourceDumpJob, outPath string) error
}

// Provider is a domain.RuntimeProvider over a DBServer. The optional
// contracts it does not override answer ErrFeatureNotSupported.
type Provider struct {
	domain.UnsupportedFeatures
	spec     domain.EngineSpec
	server   domain.DBServer
	hooks    Hooks
	registry *domain.RuntimeRegistry
}

var _ domain.RuntimeProvider = (*Provider)(nil)

// New adapts server, an engine described by spec.
func New(spec domain.EngineSpec, server domain.DBServer, hooks Hooks) *Provider {
	return &Provider{spec: spec.Clone(), server: server, hooks: hooks, registry: domain.DefaultRuntimes}
}

// WithRegistry makes upgrade path checks use reg instead of domain.DefaultRuntimes.
func (p *Provider) WithRegistry(reg *domain.RuntimeRegistry) *Provider {
	p.registry = reg
	return p
}

// Describe declares the engine. A capability only stays true when this
// provider can do it: the DBServer lacks the port, or the hook is not set,
// and the feature is declared false instead of failing on first use.
func (p *Provider) Describe(context.Context) (domain.EngineSpec, error) {
	s := p.spec.Clone()
	_, upgrader := p.server.(domain.Upgrader)
	_, stats := p.server.(domain.StatsUpdater)
	s.Capabilities.Upgrade = s.Capabilities.Upgrade && upgrader
	s.Capabilities.UpdateStats = s.Capabilities.UpdateStats && stats
	s.Capabilities.Query = s.Capabilities.Query && p.hooks.Query != nil
	s.Capabilities.SourceDump = s.Capabilities.SourceDump && p.hooks.DumpSource != nil
	return s, nil
}

// Catalog is empty: the core's embedded catalog lists the built-in runtimes.
func (p *Provider) Catalog(context.Context) ([]domain.CatalogEntry, error) { return nil, nil }

// FormatDSN is the client URL for the instance at host.
func (p *Provider) FormatDSN(_ context.Context, s domain.InstanceSpec, host, user, password string) (string, error) {
	srv, err := p.srv(s)
	if err != nil {
		return "", err
	}
	return p.spec.DSN(host, srv.Port, user, password), nil
}

// ErrNoPrimaryPort is returned when an instance has no port for the engine's
// primary protocol: a server would be configured on port 0.
var ErrNoPrimaryPort = errors.New("instance has no port for the primary protocol")

// srv is the instance as the DBServer sees it, refused when it has no port.
func (p *Provider) srv(s domain.InstanceSpec) (domain.Server, error) {
	if len(p.spec.Protocols) > 0 {
		if port := s.PrimaryPort(p.spec); port < 1 || port > 65535 {
			return domain.Server{}, fmt.Errorf("%w: %s (port %d)", ErrNoPrimaryPort, s.ID, port)
		}
	}
	return s.Server(p.spec), nil
}

// PrepareRuntime runs the engine's fixup on the unpacked runtime.
func (p *Provider) PrepareRuntime(_ context.Context, _, stagingDir string) error {
	if p.hooks.PrepareRuntime == nil {
		return nil
	}
	return p.hooks.PrepareRuntime(stagingDir)
}

func (p *Provider) Initialize(ctx context.Context, s domain.InstanceSpec) error {
	srv, err := p.srv(s)
	if err != nil {
		return err
	}
	return p.server.Initialize(ctx, srv)
}

func (p *Provider) PrepareClone(ctx context.Context, s domain.InstanceSpec) error {
	srv, err := p.srv(s)
	if err != nil {
		return err
	}
	return p.server.PrepareClone(ctx, srv)
}

// PrepareStart writes the instance's configuration and returns how to launch
// it, after checking the plan can be written into a systemd unit.
func (p *Provider) PrepareStart(ctx context.Context, s domain.InstanceSpec) (domain.LaunchPlan, error) {
	if p.hooks.PrepareStart == nil {
		return domain.LaunchPlan{}, domain.FeatureNotSupported("prepare_start")
	}
	srv, err := p.srv(s)
	if err != nil {
		return domain.LaunchPlan{}, err
	}
	plan, err := p.hooks.PrepareStart(ctx, srv)
	if err != nil {
		return domain.LaunchPlan{}, err
	}
	if err := plan.Validate(); err != nil {
		return domain.LaunchPlan{}, err
	}
	return plan, nil
}

func (p *Provider) Health(ctx context.Context, s domain.InstanceSpec) (domain.HealthState, error) {
	if p.hooks.Health == nil {
		return domain.HealthStopped, domain.FeatureNotSupported("health")
	}
	srv, err := p.srv(s)
	if err != nil {
		return domain.HealthStopped, err
	}
	return p.hooks.Health(ctx, srv)
}

func (p *Provider) Quiesce(ctx context.Context, s domain.InstanceSpec, mode domain.StopMode) error {
	if p.hooks.Quiesce == nil {
		return domain.FeatureNotSupported("quiesce")
	}
	srv, err := p.srv(s)
	if err != nil {
		return err
	}
	return p.hooks.Quiesce(ctx, srv, mode)
}

func (p *Provider) VerifyCleanShutdown(ctx context.Context, s domain.InstanceSpec) error {
	if p.hooks.VerifyShutdown == nil {
		return domain.FeatureNotSupported("verify_clean_shutdown")
	}
	return p.hooks.VerifyShutdown(ctx, s.Server(p.spec))
}

func (p *Provider) RemoveInstance(ctx context.Context, s domain.InstanceSpec) error {
	return p.server.Remove(ctx, s.Server(p.spec))
}

func (p *Provider) CreateUser(ctx context.Context, s domain.InstanceSpec, user, password string) error {
	srv, err := p.srv(s)
	if err != nil {
		return err
	}
	return p.server.CreateUser(ctx, srv, user, password)
}

func (p *Provider) ExecAs(ctx context.Context, s domain.InstanceSpec, user, password, sql string) error {
	srv, err := p.srv(s)
	if err != nil {
		return err
	}
	return p.server.ExecAs(ctx, srv, user, password, sql)
}

func (p *Provider) AdminAccess(_ context.Context, s domain.InstanceSpec) (domain.AdminAccess, error) {
	srv, err := p.srv(s)
	if err != nil {
		return domain.AdminAccess{}, err
	}
	return p.server.Admin(srv), nil
}

func (p *Provider) Query(ctx context.Context, s domain.InstanceSpec, q domain.QueryRequest) (domain.QueryResult, error) {
	if p.hooks.Query == nil {
		return domain.QueryResult{}, domain.FeatureNotSupported("query")
	}
	srv, err := p.srv(s)
	if err != nil {
		return domain.QueryResult{}, err
	}
	return p.hooks.Query(ctx, srv, q)
}

func (p *Provider) TailLog(ctx context.Context, s domain.InstanceSpec, lines int) (string, error) {
	if p.hooks.TailLog == nil {
		return "", domain.FeatureNotSupported("tail_log")
	}
	return p.hooks.TailLog(ctx, s.Server(p.spec), lines)
}

func (p *Provider) Load(ctx context.Context, s domain.InstanceSpec, dumpPath string) error {
	srv, err := p.srv(s)
	if err != nil {
		return err
	}
	return p.server.Load(ctx, srv, dumpPath)
}

func (p *Provider) DumpSource(ctx context.Context, job domain.SourceDumpJob, outPath string) error {
	if p.hooks.DumpSource == nil {
		return domain.FeatureNotSupported("source_dump")
	}
	return p.hooks.DumpSource(ctx, job, outPath)
}

func (p *Provider) UpdateStats(ctx context.Context, s domain.InstanceSpec) error {
	u, ok := p.server.(domain.StatsUpdater)
	if !ok {
		return domain.FeatureNotSupported("update_stats")
	}
	srv, err := p.srv(s)
	if err != nil {
		return err
	}
	return u.UpdateStats(ctx, srv)
}

// CheckUpgradePath judges a move between two versions of the engine.
func (p *Provider) CheckUpgradePath(_ context.Context, fromVersion, toVersion string) error {
	if _, ok := p.server.(domain.Upgrader); !ok {
		return domain.FeatureNotSupported("upgrade")
	}
	e := string(p.spec.Engine)
	from, err := p.registry.Parse(e + "@" + fromVersion)
	if err != nil {
		return err
	}
	to, err := p.registry.Parse(e + "@" + toVersion)
	if err != nil {
		return err
	}
	return p.registry.CheckUpgradePath(from, to)
}

func (p *Provider) CheckUpgrade(ctx context.Context, s domain.InstanceSpec, toVersion string, own domain.Login) (domain.UpgradePlan, error) {
	u, ok := p.server.(domain.Upgrader)
	if !ok {
		return domain.UpgradePlan{}, domain.FeatureNotSupported("upgrade")
	}
	srv, err := p.srv(s)
	if err != nil {
		return domain.UpgradePlan{}, err
	}
	to, err := p.registry.Parse(string(p.spec.Engine) + "@" + toVersion)
	if err != nil {
		return domain.UpgradePlan{}, err
	}
	return u.CheckUpgrade(ctx, srv, to, own)
}

// StartUpgraded starts the instance on its new runtime and reads its log. The
// launch plan is made and checked first, so a refused plan starts nothing. The
// built-in servers start the unit themselves (the core does not yet run
// systemctl for them), so the returned plan only describes what is started.
func (p *Provider) StartUpgraded(ctx context.Context, s domain.InstanceSpec, own domain.Login) (domain.LaunchPlan, domain.UpgradeLog, error) {
	u, ok := p.server.(domain.Upgrader)
	if !ok {
		return domain.LaunchPlan{}, domain.UpgradeLog{}, domain.FeatureNotSupported("upgrade")
	}
	var plan domain.LaunchPlan
	if p.hooks.PrepareStart != nil {
		var err error
		if plan, err = p.PrepareStart(ctx, s); err != nil {
			return domain.LaunchPlan{}, domain.UpgradeLog{}, err
		}
	}
	srv, err := p.srv(s)
	if err != nil {
		return domain.LaunchPlan{}, domain.UpgradeLog{}, err
	}
	log, err := u.StartUpgraded(ctx, srv, own)
	if err != nil {
		return domain.LaunchPlan{}, log, err
	}
	return plan, log, nil
}
