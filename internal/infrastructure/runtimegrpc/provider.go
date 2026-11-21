// Package runtimegrpc is the core's side of the gRPC boundary to an external
// runtime module: a domain.RuntimeProvider whose every call goes to the module
// process over the Runtime service (pkg/ext/proto/runtime/v1).
package runtimegrpc

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/db-branch/db-branch/internal/domain"
	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

// Provider talks to one runtime module.
type Provider struct {
	c pb.RuntimeClient
}

var _ domain.RuntimeProvider = (*Provider)(nil)

// New makes a Provider over an established connection to the module.
func New(conn grpc.ClientConnInterface) *Provider { return &Provider{c: pb.NewRuntimeClient(conn)} }

// wrap turns a gRPC failure into the core's errors: UNIMPLEMENTED is "the
// engine does not offer it", FAILED_PRECONDITION is an unclean shutdown for
// VerifyCleanShutdown and a refused version move for CheckUpgradePath (422 for
// the caller), anything else keeps the module's message.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("runtime module %s: %w", op, err)
	}
	switch st.Code() {
	case codes.Unimplemented:
		return fmt.Errorf("%w: %s", domain.ErrFeatureNotSupported, op)
	case codes.FailedPrecondition:
		switch op {
		case "VerifyCleanShutdown":
			return fmt.Errorf("%w: %s", domain.ErrUncleanShutdown, st.Message())
		case "CheckUpgradePath":
			return fmt.Errorf("%w: %s", domain.ErrUpgradeRefused, st.Message())
		}
	}
	return errors.New("runtime module " + op + ": " + st.Message())
}

func (p *Provider) Describe(ctx context.Context) (domain.EngineSpec, error) {
	r, err := p.c.Describe(ctx, &pb.DescribeRequest{})
	if err != nil {
		return domain.EngineSpec{}, wrap("Describe", err)
	}
	spec := domain.EngineSpec{
		Engine: domain.Engine(r.GetEngine()), DisplayName: r.GetDisplayName(), VersionPattern: r.GetVersionPattern(),
		CollationBound: r.GetCollationBound(), RunAsUser: r.GetRunAsUser(),
	}
	if len(r.GetZfsProps()) > 0 {
		spec.ZFSProps = r.GetZfsProps()
	}
	for _, pp := range r.GetProtocols() {
		spec.Protocols = append(spec.Protocols, domain.ProtocolPort{Name: pp.GetName(), Primary: pp.GetPrimary()})
	}
	c := r.GetCapabilities()
	spec.Capabilities = domain.RuntimeCaps{
		Query: c.GetQuery(), Checkpoints: c.GetCheckpoints(), LiveFreeze: c.GetLiveFreeze(), Upgrade: c.GetUpgrade(),
		SourceDump: c.GetSourceDump(), BranchMerge: c.GetBranchMerge(), HistoryRecord: c.GetHistoryRecord(),
		UpdateStats: c.GetUpdateStats(), InstanceMetrics: c.GetInstanceMetrics(),
	}
	return spec, nil
}

func (p *Provider) Catalog(ctx context.Context) ([]domain.CatalogEntry, error) {
	r, err := p.c.Catalog(ctx, &pb.CatalogRequest{})
	if err != nil {
		return nil, wrap("Catalog", err)
	}
	var out []domain.CatalogEntry
	for _, e := range r.GetEntries() {
		out = append(out, domain.CatalogEntry{Version: e.GetVersion(), Platform: e.GetPlatform(), URL: e.GetUrl(), SHA256: e.GetSha256(), Size: e.GetSize()})
	}
	return out, nil
}

func (p *Provider) PrepareRuntime(ctx context.Context, version, stagingDir string) error {
	_, err := p.c.PrepareRuntime(ctx, &pb.PrepareRuntimeRequest{Version: version, StagingDir: stagingDir})
	return wrap("PrepareRuntime", err)
}

func (p *Provider) Initialize(ctx context.Context, s domain.InstanceSpec) error {
	_, err := p.c.Initialize(ctx, &pb.InstanceRequest{Instance: instance(s)})
	return wrap("Initialize", err)
}

func (p *Provider) PrepareClone(ctx context.Context, s domain.InstanceSpec) error {
	_, err := p.c.PrepareClone(ctx, &pb.InstanceRequest{Instance: instance(s)})
	return wrap("PrepareClone", err)
}

func (p *Provider) PrepareStart(ctx context.Context, s domain.InstanceSpec) (domain.LaunchPlan, error) {
	r, err := p.c.PrepareStart(ctx, &pb.InstanceRequest{Instance: instance(s)})
	if err != nil {
		return domain.LaunchPlan{}, wrap("PrepareStart", err)
	}
	return launchPlan(r), nil
}

func (p *Provider) Health(ctx context.Context, s domain.InstanceSpec) (domain.HealthState, error) {
	r, err := p.c.Health(ctx, &pb.InstanceRequest{Instance: instance(s)})
	if err != nil {
		return domain.HealthStopped, wrap("Health", err)
	}
	switch r.GetState() {
	case pb.HealthState_HEALTH_STATE_STARTING:
		return domain.HealthStarting, nil
	case pb.HealthState_HEALTH_STATE_READY:
		return domain.HealthReady, nil
	}
	return domain.HealthStopped, nil
}

func (p *Provider) Quiesce(ctx context.Context, s domain.InstanceSpec, mode domain.StopMode) error {
	m := pb.StopMode_STOP_MODE_CLEAN
	if mode == domain.StopImmediate {
		m = pb.StopMode_STOP_MODE_IMMEDIATE
	}
	_, err := p.c.Quiesce(ctx, &pb.QuiesceRequest{Instance: instance(s), Mode: m})
	return wrap("Quiesce", err)
}

func (p *Provider) VerifyCleanShutdown(ctx context.Context, s domain.InstanceSpec) error {
	_, err := p.c.VerifyCleanShutdown(ctx, &pb.InstanceRequest{Instance: instance(s)})
	return wrap("VerifyCleanShutdown", err)
}

func (p *Provider) RemoveInstance(ctx context.Context, s domain.InstanceSpec) error {
	_, err := p.c.RemoveInstance(ctx, &pb.InstanceRequest{Instance: instance(s)})
	return wrap("RemoveInstance", err)
}

func (p *Provider) CreateUser(ctx context.Context, s domain.InstanceSpec, user, password string) error {
	_, err := p.c.CreateUser(ctx, &pb.CreateUserRequest{Instance: instance(s), User: user, Password: password})
	return wrap("CreateUser", err)
}

func (p *Provider) ExecAs(ctx context.Context, s domain.InstanceSpec, user, password, sql string) error {
	_, err := p.c.ExecAs(ctx, &pb.ExecAsRequest{Instance: instance(s), User: user, Password: password, Sql: sql})
	return wrap("ExecAs", err)
}

func (p *Provider) AdminAccess(ctx context.Context, s domain.InstanceSpec) (domain.AdminAccess, error) {
	r, err := p.c.AdminAccess(ctx, &pb.InstanceRequest{Instance: instance(s)})
	if err != nil {
		return domain.AdminAccess{}, wrap("AdminAccess", err)
	}
	return adminAccess(r), nil
}

func (p *Provider) FormatDSN(ctx context.Context, s domain.InstanceSpec, host, user, password string) (string, error) {
	r, err := p.c.FormatDSN(ctx, &pb.FormatDSNRequest{Instance: instance(s), Host: host, User: user, Password: password})
	if err != nil {
		return "", wrap("FormatDSN", err)
	}
	return r.GetDsn(), nil
}

func (p *Provider) Query(ctx context.Context, s domain.InstanceSpec, q domain.QueryRequest) (domain.QueryResult, error) {
	r, err := p.c.Query(ctx, &pb.QueryRequest{
		Instance: instance(s), Host: q.Host, User: q.User, Password: q.Password, Sql: q.SQL, MaxRows: int32(q.MaxRows),
	})
	if err != nil {
		return domain.QueryResult{}, wrap("Query", err)
	}
	res := domain.QueryResult{Columns: r.GetColumns(), Truncated: r.GetTruncated(), Millis: r.GetMillis()}
	for _, row := range r.GetRows() {
		cells := make([]*string, 0, len(row.GetCells()))
		for _, c := range row.GetCells() {
			if c.GetIsNull() {
				cells = append(cells, nil)
				continue
			}
			v := c.GetValue()
			cells = append(cells, &v)
		}
		res.Rows = append(res.Rows, cells)
	}
	if r.GetHasAffected() {
		n := r.GetAffected()
		res.Affected = &n
	}
	return res, nil
}

func (p *Provider) TailLog(ctx context.Context, s domain.InstanceSpec, lines int) (string, error) {
	r, err := p.c.TailLog(ctx, &pb.TailLogRequest{Instance: instance(s), Lines: int32(lines)})
	if err != nil {
		return "", wrap("TailLog", err)
	}
	return r.GetText(), nil
}

func (p *Provider) PreFreeze(ctx context.Context, s domain.InstanceSpec) (domain.FreezeToken, error) {
	r, err := p.c.PreFreeze(ctx, &pb.InstanceRequest{Instance: instance(s)})
	if err != nil {
		return domain.FreezeToken{}, wrap("live_freeze", err)
	}
	return domain.FreezeToken{Data: r.GetData()}, nil
}

func (p *Provider) PostFreeze(ctx context.Context, s domain.InstanceSpec, tok domain.FreezeToken) error {
	_, err := p.c.PostFreeze(ctx, &pb.PostFreezeRequest{Instance: instance(s), Token: &pb.FreezeToken{Data: tok.Data}})
	return wrap("live_freeze", err)
}

func (p *Provider) UpdateStats(ctx context.Context, s domain.InstanceSpec) error {
	_, err := p.c.UpdateStats(ctx, &pb.InstanceRequest{Instance: instance(s)})
	return wrap("update_stats", err)
}

func (p *Provider) CheckUpgradePath(ctx context.Context, from, to string) error {
	_, err := p.c.CheckUpgradePath(ctx, &pb.CheckUpgradePathRequest{FromVersion: from, ToVersion: to})
	return wrap("CheckUpgradePath", err)
}

func (p *Provider) CheckUpgrade(ctx context.Context, s domain.InstanceSpec, to string, own domain.Login) (domain.UpgradePlan, error) {
	r, err := p.c.CheckUpgrade(ctx, &pb.CheckUpgradeRequest{Instance: instance(s), ToVersion: to, Login: &pb.Login{User: own.User, Password: own.Password}})
	if err != nil {
		return domain.UpgradePlan{}, wrap("upgrade", err)
	}
	return domain.UpgradePlan{Findings: findings(r.GetFindings()), LegacyAuth: r.GetLegacyAuth()}, nil
}

func (p *Provider) StartUpgraded(ctx context.Context, s domain.InstanceSpec, own domain.Login) (domain.LaunchPlan, domain.UpgradeLog, error) {
	r, err := p.c.StartUpgraded(ctx, &pb.StartUpgradedRequest{Instance: instance(s), Login: &pb.Login{User: own.User, Password: own.Password}})
	if err != nil {
		return domain.LaunchPlan{}, domain.UpgradeLog{}, wrap("upgrade", err)
	}
	return launchPlan(r.GetPlan()), domain.UpgradeLog{Findings: findings(r.GetFindings()), Excerpt: r.GetExcerpt()}, nil
}

func (p *Provider) DumpSource(ctx context.Context, job domain.SourceDumpJob, outPath string) error {
	src := job.Source
	_, err := p.c.DumpSource(ctx, &pb.DumpSourceRequest{
		Tools: adminAccessPB(job.Tools), OutPath: outPath,
		Source: &pb.RefreshSource{Host: src.Host, Port: int32(src.Port), User: src.User, Password: src.Password, Databases: src.Databases},
	})
	return wrap("source_dump", err)
}

func (p *Provider) Load(ctx context.Context, s domain.InstanceSpec, dumpPath string) error {
	_, err := p.c.Load(ctx, &pb.LoadRequest{Instance: instance(s), DumpPath: dumpPath})
	return wrap("source_dump", err)
}

func (p *Provider) InstanceMetrics(ctx context.Context, s domain.InstanceSpec) ([]domain.MetricSample, error) {
	r, err := p.c.InstanceMetrics(ctx, &pb.InstanceRequest{Instance: instance(s)})
	if err != nil {
		return nil, wrap("instance_metrics", err)
	}
	var out []domain.MetricSample
	for _, m := range r.GetSamples() {
		out = append(out, domain.MetricSample{Name: m.GetName(), Labels: m.GetLabels(), Value: m.GetValue()})
	}
	return out, nil
}

func instance(s domain.InstanceSpec) *pb.InstanceSpec {
	ports := make(map[string]int32, len(s.Ports))
	for k, v := range s.Ports {
		ports[k] = int32(v)
	}
	return &pb.InstanceSpec{
		Id: s.ID, Engine: string(s.Runtime.Engine), Version: s.Runtime.Version, RuntimeDir: s.RuntimeDir,
		DataDir: s.DataDir, StateDir: s.StateDir, RunDir: s.RunDir, Bind: s.Bind, Ports: ports,
		Limits: &pb.ResourceLimits{
			MemoryMb: int32(s.Limits.MemoryMB), MaxConnections: int32(s.Limits.MaxConnections),
			NoFile: int32(s.Limits.NoFile), CpuQuota: int32(s.Limits.CPUQuota),
		},
		Maintenance: s.Maintenance, LegacyAuth: s.LegacyAuth, Extra: s.Extra,
	}
}

func launchPlan(r *pb.LaunchPlan) domain.LaunchPlan {
	return domain.LaunchPlan{
		Command: r.GetCommand(), Args: r.GetArgs(), Env: r.GetEnv(), ServiceType: r.GetServiceType(),
		KillSignal: r.GetKillSignal(), TimeoutStopSec: int(r.GetTimeoutStopSec()), KillMode: r.GetKillMode(),
		NotifyAccess: r.GetNotifyAccess(),
	}
}

func findings(in []*pb.Finding) []domain.Finding {
	var out []domain.Finding
	for _, f := range in {
		out = append(out, domain.Finding{Level: domain.FindingLevel(f.GetLevel()), Code: f.GetCode(), Message: f.GetMessage()})
	}
	return out
}

func adminAccess(r *pb.AdminAccessInfo) domain.AdminAccess {
	a := domain.AdminAccess{
		Engine: domain.Engine(r.GetEngine()), Host: r.GetHost(), Socket: r.GetSocket(), Port: int(r.GetPort()),
		User: r.GetUser(), BinDir: r.GetBinDir(), LibDir: r.GetLibDir(),
	}
	if len(r.GetEnv()) > 0 {
		a.Env = r.GetEnv()
	}
	if len(r.GetEndpoints()) > 0 {
		a.Endpoints = map[string]domain.Endpoint{}
		for k, e := range r.GetEndpoints() {
			a.Endpoints[k] = domain.Endpoint{Host: e.GetHost(), Port: int(e.GetPort()), Socket: e.GetSocket()}
		}
	}
	return a
}

func adminAccessPB(a domain.AdminAccess) *pb.AdminAccessInfo {
	r := &pb.AdminAccessInfo{
		Engine: string(a.Engine), Host: a.Host, Socket: a.Socket, Port: int32(a.Port), User: a.User,
		BinDir: a.BinDir, LibDir: a.LibDir, Env: a.Env,
	}
	if len(a.Endpoints) > 0 {
		r.Endpoints = map[string]*pb.Endpoint{}
		for k, e := range a.Endpoints {
			r.Endpoints[k] = &pb.Endpoint{Host: e.Host, Port: int32(e.Port), Socket: e.Socket}
		}
	}
	return r
}
