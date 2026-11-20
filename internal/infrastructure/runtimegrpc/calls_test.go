package runtimegrpc_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
	pb "github.com/db-branch/db-branch/pkg/ext/proto/runtime/v1"
)

// recorder remembers the last request of each call and answers from canned values.
type recorder struct {
	pb.UnimplementedRuntimeServer
	inst  *pb.InstanceSpec
	req   any
	err   error
	plan  *pb.LaunchPlan
	query *pb.QueryResponse
}

func (r *recorder) note(i *pb.InstanceSpec, req any) error { r.inst, r.req = i, req; return r.err }

func (r *recorder) Catalog(context.Context, *pb.CatalogRequest) (*pb.CatalogResponse, error) {
	return &pb.CatalogResponse{Entries: []*pb.CatalogEntry{{Version: "24.8", Platform: "linux-amd64", Url: "https://x/y", Sha256: "ab", Size: 7}}}, r.err
}
func (r *recorder) PrepareRuntime(_ context.Context, q *pb.PrepareRuntimeRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(nil, q)
}
func (r *recorder) Initialize(_ context.Context, q *pb.InstanceRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) PrepareClone(_ context.Context, q *pb.InstanceRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) PrepareStart(_ context.Context, q *pb.InstanceRequest) (*pb.LaunchPlan, error) {
	return r.plan, r.note(q.Instance, q)
}
func (r *recorder) Health(_ context.Context, q *pb.InstanceRequest) (*pb.HealthResponse, error) {
	st := map[string]pb.HealthState{"starting": pb.HealthState_HEALTH_STATE_STARTING, "ready": pb.HealthState_HEALTH_STATE_READY}[q.Instance.Id]
	return &pb.HealthResponse{State: st}, r.err
}
func (r *recorder) Quiesce(_ context.Context, q *pb.QuiesceRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) VerifyCleanShutdown(context.Context, *pb.InstanceRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.err
}
func (r *recorder) RemoveInstance(_ context.Context, q *pb.InstanceRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) CreateUser(_ context.Context, q *pb.CreateUserRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) ExecAs(_ context.Context, q *pb.ExecAsRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) AdminAccess(_ context.Context, q *pb.InstanceRequest) (*pb.AdminAccessInfo, error) {
	return &pb.AdminAccessInfo{
		Engine: "clickhouse", Host: "127.0.0.1", Port: 8123, Socket: "/run/x", User: "admin", BinDir: "/b", LibDir: "/l",
		Env:       map[string]string{"A": "1"},
		Endpoints: map[string]*pb.Endpoint{"http": {Host: "127.0.0.1", Port: 8123}, "native": {Port: 9000, Socket: "/s"}},
	}, r.err
}
func (r *recorder) FormatDSN(_ context.Context, q *pb.FormatDSNRequest) (*pb.FormatDSNResponse, error) {
	return &pb.FormatDSNResponse{Dsn: "ch://" + q.User + ":" + q.Password + "@" + q.Host}, r.note(q.Instance, q)
}
func (r *recorder) Query(_ context.Context, q *pb.QueryRequest) (*pb.QueryResponse, error) {
	return r.query, r.note(q.Instance, q)
}
func (r *recorder) TailLog(_ context.Context, q *pb.TailLogRequest) (*pb.TailLogResponse, error) {
	return &pb.TailLogResponse{Text: "log"}, r.note(q.Instance, q)
}
func (r *recorder) PreFreeze(context.Context, *pb.InstanceRequest) (*pb.FreezeToken, error) {
	return &pb.FreezeToken{Data: "tok"}, r.err
}
func (r *recorder) PostFreeze(_ context.Context, q *pb.PostFreezeRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) CheckUpgradePath(_ context.Context, q *pb.CheckUpgradePathRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(nil, q)
}
func (r *recorder) CheckUpgrade(_ context.Context, q *pb.CheckUpgradeRequest) (*pb.UpgradePlan, error) {
	return &pb.UpgradePlan{LegacyAuth: true, Findings: []*pb.Finding{{Level: "warning", Code: "c", Message: "m"}}}, r.note(q.Instance, q)
}
func (r *recorder) StartUpgraded(_ context.Context, q *pb.StartUpgradedRequest) (*pb.StartUpgradedResponse, error) {
	return &pb.StartUpgradedResponse{Plan: r.plan, Excerpt: "tail", Findings: []*pb.Finding{{Level: "info", Code: "k", Message: "n"}}}, r.note(q.Instance, q)
}
func (r *recorder) DumpSource(_ context.Context, q *pb.DumpSourceRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(nil, q)
}
func (r *recorder) Load(_ context.Context, q *pb.LoadRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}
func (r *recorder) InstanceMetrics(context.Context, *pb.InstanceRequest) (*pb.MetricsResponse, error) {
	return &pb.MetricsResponse{Samples: []*pb.MetricSample{{Name: "up", Labels: map[string]string{"a": "b"}, Value: 1.5}}}, r.err
}

var spec = domain.InstanceSpec{
	ID: "branch-x", Runtime: domain.RuntimeRef{Engine: "clickhouse", Version: "24.8"}, RuntimeDir: "/rt", DataDir: "/d",
	StateDir: "/st", RunDir: "/run", Bind: "127.0.0.1", Ports: map[string]int{"http": 8123, "native": 9000},
	Limits:      domain.ResourceLimits{MemoryMB: 512, MaxConnections: 50, NoFile: 1024, CPUQuota: 200},
	Maintenance: true, LegacyAuth: true, Extra: []string{"a=1"},
}

func TestInstanceSpec_全フィールドがモジュールに渡る(t *testing.T) {
	r := &recorder{}
	p := connect(t, r)
	if err := p.Initialize(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	g := r.inst
	if g.Id != "branch-x" || g.Engine != "clickhouse" || g.Version != "24.8" || g.RuntimeDir != "/rt" || g.DataDir != "/d" ||
		g.StateDir != "/st" || g.RunDir != "/run" || g.Bind != "127.0.0.1" || g.Ports["http"] != 8123 || g.Ports["native"] != 9000 ||
		g.Limits.MemoryMb != 512 || g.Limits.MaxConnections != 50 || g.Limits.NoFile != 1024 || g.Limits.CpuQuota != 200 ||
		!g.Maintenance || !g.LegacyAuth || !reflect.DeepEqual(g.Extra, []string{"a=1"}) {
		t.Fatalf("instance = %+v", g)
	}
}

func TestPrepareStart_起動計画を受け取れる(t *testing.T) {
	r := &recorder{plan: &pb.LaunchPlan{Command: "/usr/lib/db-branch/runtimes/ch/server", Args: []string{"--x"}, Env: map[string]string{"K": "v"},
		ServiceType: "notify", KillSignal: "SIGINT", TimeoutStopSec: 90, KillMode: "mixed", NotifyAccess: "main"}}
	got, err := connect(t, r).PrepareStart(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.LaunchPlan{Command: "/usr/lib/db-branch/runtimes/ch/server", Args: []string{"--x"}, Env: map[string]string{"K": "v"},
		ServiceType: "notify", KillSignal: "SIGINT", TimeoutStopSec: 90, KillMode: "mixed", NotifyAccess: "main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %+v", got)
	}
}

func TestHealth_三つの状態を写せる(t *testing.T) {
	r := &recorder{}
	p := connect(t, r)
	for id, want := range map[string]domain.HealthState{"x": domain.HealthStopped, "starting": domain.HealthStarting, "ready": domain.HealthReady} {
		s := spec
		s.ID = id
		got, err := p.Health(context.Background(), s)
		if err != nil || got != want {
			t.Fatalf("%s: %v %v", id, got, err)
		}
	}
	r.err = status.Error(codes.Unavailable, "down")
	if _, err := p.Health(context.Background(), spec); err == nil {
		t.Fatal("a failing module must be an error")
	}
}

func TestQuiesce_停止モードを渡せる(t *testing.T) {
	r := &recorder{}
	p := connect(t, r)
	_ = p.Quiesce(context.Background(), spec, domain.StopImmediate)
	if r.req.(*pb.QuiesceRequest).Mode != pb.StopMode_STOP_MODE_IMMEDIATE {
		t.Fatal("immediate lost")
	}
	_ = p.Quiesce(context.Background(), spec, domain.StopClean)
	if r.req.(*pb.QuiesceRequest).Mode != pb.StopMode_STOP_MODE_CLEAN {
		t.Fatal("clean lost")
	}
}

func TestVerifyCleanShutdown_異常終了はErrUncleanShutdownになる(t *testing.T) {
	r := &recorder{err: status.Error(codes.FailedPrecondition, "redo log not empty")}
	err := connect(t, r).VerifyCleanShutdown(context.Background(), spec)
	if !errors.Is(err, domain.ErrUncleanShutdown) || !strings.Contains(err.Error(), "redo log") {
		t.Fatalf("err = %v", err)
	}
	// FAILED_PRECONDITION elsewhere is only a failure.
	if err := connect(t, r).Initialize(context.Background(), spec); err == nil || errors.Is(err, domain.ErrUncleanShutdown) {
		t.Fatalf("err = %v", err)
	}
}

func TestユーザーとSQL_資格情報が渡る(t *testing.T) {
	r := &recorder{}
	p := connect(t, r)
	ctx := context.Background()
	if err := p.CreateUser(ctx, spec, "u", "pw"); err != nil {
		t.Fatal(err)
	}
	if q := r.req.(*pb.CreateUserRequest); q.User != "u" || q.Password != "pw" {
		t.Fatalf("%+v", q)
	}
	if err := p.ExecAs(ctx, spec, "u", "pw", "select 1"); err != nil {
		t.Fatal(err)
	}
	if q := r.req.(*pb.ExecAsRequest); q.User != "u" || q.Password != "pw" || q.Sql != "select 1" {
		t.Fatalf("%+v", q)
	}
	dsn, err := p.FormatDSN(ctx, spec, "h", "u", "pw")
	if err != nil || dsn != "ch://u:pw@h" {
		t.Fatalf("%q %v", dsn, err)
	}
	if txt, err := p.TailLog(ctx, spec, 20); err != nil || txt != "log" || r.req.(*pb.TailLogRequest).Lines != 20 {
		t.Fatalf("%q %v", txt, err)
	}
}

func TestQuery_NULLと件数と切り詰めを写せる(t *testing.T) {
	r := &recorder{query: &pb.QueryResponse{
		Columns:   []string{"a", "b"},
		Rows:      []*pb.Row{{Cells: []*pb.Cell{{Value: "1"}, {IsNull: true}}}, {Cells: []*pb.Cell{{Value: ""}, {Value: "x"}}}},
		Truncated: true, HasAffected: true, Affected: 0, Millis: 12,
	}}
	got, err := connect(t, r).Query(context.Background(), spec, domain.QueryRequest{Host: "h", User: "u", Password: "pw", SQL: "q", MaxRows: 5})
	if err != nil {
		t.Fatal(err)
	}
	if q := r.req.(*pb.QueryRequest); q.Host != "h" || q.MaxRows != 5 || q.Sql != "q" || q.User != "u" || q.Password != "pw" {
		t.Fatalf("%+v", q)
	}
	if len(got.Rows) != 2 || got.Rows[0][1] != nil || *got.Rows[0][0] != "1" || got.Rows[1][0] == nil || *got.Rows[1][0] != "" {
		t.Fatalf("rows = %v", got.Rows)
	}
	if !got.Truncated || got.Millis != 12 || got.Affected == nil || *got.Affected != 0 || !reflect.DeepEqual(got.Columns, []string{"a", "b"}) {
		t.Fatalf("result = %+v", got)
	}
	r.query = &pb.QueryResponse{}
	got, _ = connect(t, r).Query(context.Background(), spec, domain.QueryRequest{})
	if got.Affected != nil {
		t.Fatal("no count must stay nil")
	}
	r.err = status.Error(codes.Internal, "syntax")
	if _, err := connect(t, r).Query(context.Background(), spec, domain.QueryRequest{}); err == nil {
		t.Fatal("error lost")
	}
}

func TestAdminAccess_複数エンドポイントと環境変数を写せる(t *testing.T) {
	got, err := connect(t, &recorder{}).AdminAccess(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.AdminAccess{Engine: "clickhouse", Host: "127.0.0.1", Port: 8123, Socket: "/run/x", User: "admin", BinDir: "/b", LibDir: "/l",
		Env:       map[string]string{"A": "1"},
		Endpoints: map[string]domain.Endpoint{"http": {Host: "127.0.0.1", Port: 8123}, "native": {Port: 9000, Socket: "/s"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("access = %+v", got)
	}
}

func TestCatalogとPrepareRuntime_カタログと展開先を扱える(t *testing.T) {
	r := &recorder{}
	p := connect(t, r)
	cat, err := p.Catalog(context.Background())
	if err != nil || !reflect.DeepEqual(cat, []domain.CatalogEntry{{Version: "24.8", Platform: "linux-amd64", URL: "https://x/y", SHA256: "ab", Size: 7}}) {
		t.Fatalf("%v %v", cat, err)
	}
	if err := p.PrepareRuntime(context.Background(), "24.8", "/stage"); err != nil {
		t.Fatal(err)
	}
	if q := r.req.(*pb.PrepareRuntimeRequest); q.Version != "24.8" || q.StagingDir != "/stage" {
		t.Fatalf("%+v", q)
	}
	r.err = status.Error(codes.Internal, "x")
	if _, err := p.Catalog(context.Background()); err == nil {
		t.Fatal("error lost")
	}
}

func TestライフサイクルとFreeze_モジュールの結果を返す(t *testing.T) {
	r := &recorder{}
	p := connect(t, r)
	ctx := context.Background()
	for name, f := range map[string]func() error{
		"PrepareClone":   func() error { return p.PrepareClone(ctx, spec) },
		"RemoveInstance": func() error { return p.RemoveInstance(ctx, spec) },
		"UpdateStats":    func() error { return p.UpdateStats(ctx, spec) },
		"Load":           func() error { return p.Load(ctx, spec, "/dump.sql") },
	} {
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	tok, err := p.PreFreeze(ctx, spec)
	if err != nil || tok.Data != "tok" {
		t.Fatalf("%v %v", tok, err)
	}
	if err := p.PostFreeze(ctx, spec, tok); err != nil || r.req.(*pb.PostFreezeRequest).Token.Data != "tok" {
		t.Fatal("token lost")
	}
	r.err = status.Error(codes.Unimplemented, "")
	if _, err := p.PreFreeze(ctx, spec); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Fatalf("%v", err)
	}
	if _, err := p.InstanceMetrics(ctx, spec); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Fatalf("%v", err)
	}
}

func TestUpgrade_経路確認と計画と起動を扱える(t *testing.T) {
	r := &recorder{plan: &pb.LaunchPlan{Command: "/c"}}
	p := connect(t, r)
	ctx := context.Background()
	if err := p.CheckUpgradePath(ctx, "1.0", "2.0"); err != nil || r.req.(*pb.CheckUpgradePathRequest).ToVersion != "2.0" {
		t.Fatal("path")
	}
	plan, err := p.CheckUpgrade(ctx, spec, "2.0", domain.Login{User: "u", Password: "p"})
	if err != nil || !plan.LegacyAuth || len(plan.Findings) != 1 || plan.Findings[0].Level != domain.LevelWarning || plan.Findings[0].Code != "c" {
		t.Fatalf("%+v %v", plan, err)
	}
	if l := r.req.(*pb.CheckUpgradeRequest).Login; l.User != "u" || l.Password != "p" {
		t.Fatal("login lost")
	}
	lp, ul, err := p.StartUpgraded(ctx, spec, domain.Login{User: "u"})
	if err != nil || lp.Command != "/c" || ul.Excerpt != "tail" || len(ul.Findings) != 1 {
		t.Fatalf("%+v %+v %v", lp, ul, err)
	}
	r.err = status.Error(codes.Unimplemented, "")
	for name, err := range map[string]error{
		"path":  p.CheckUpgradePath(ctx, "1.0", "2.0"),
		"check": func() error { _, e := p.CheckUpgrade(ctx, spec, "2.0", domain.Login{}); return e }(),
		"start": func() error { _, _, e := p.StartUpgraded(ctx, spec, domain.Login{}); return e }(),
	} {
		if !errors.Is(err, domain.ErrFeatureNotSupported) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestDumpSource_接続元とツールのパスを渡せる(t *testing.T) {
	r := &recorder{}
	job := domain.SourceDumpJob{
		Tools:  domain.AdminAccess{Engine: "clickhouse", BinDir: "/b", LibDir: "/l", Env: map[string]string{"A": "1"}, Endpoints: map[string]domain.Endpoint{"http": {Port: 1}}},
		Source: domain.RefreshSource{Host: "src", Port: 9, User: "u", Password: "pw", Databases: []string{"d1", "d2"}},
	}
	if err := connect(t, r).DumpSource(context.Background(), job, "/out.sql"); err != nil {
		t.Fatal(err)
	}
	q := r.req.(*pb.DumpSourceRequest)
	if q.OutPath != "/out.sql" || q.Source.Host != "src" || q.Source.Port != 9 || q.Source.Password != "pw" || len(q.Source.Databases) != 2 ||
		q.Tools.BinDir != "/b" || q.Tools.Endpoints["http"].Port != 1 || q.Tools.Env["A"] != "1" {
		t.Fatalf("%+v", q)
	}
}

func TestInstanceMetrics_サンプルを写せる(t *testing.T) {
	got, err := connect(t, &recorder{}).InstanceMetrics(context.Background(), spec)
	if err != nil || !reflect.DeepEqual(got, []domain.MetricSample{{Name: "up", Labels: map[string]string{"a": "b"}, Value: 1.5}}) {
		t.Fatalf("%v %v", got, err)
	}
}

func Testエラー_gRPC以外の失敗も操作名つきで返す(t *testing.T) {
	p := connect(t, fake{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.RemoveInstance(ctx, spec)
	if err == nil || !strings.Contains(err.Error(), "RemoveInstance") {
		t.Fatalf("err = %v", err)
	}
}

func (r *recorder) UpdateStats(_ context.Context, q *pb.InstanceRequest) (*pb.Empty, error) {
	return &pb.Empty{}, r.note(q.Instance, q)
}

func TestWrap_gRPCでない失敗も操作名つきで返す(t *testing.T) {
	err := runtimegrpc.Wrap("Describe", errors.New("boom"))
	if err == nil || !strings.Contains(err.Error(), "Describe") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if runtimegrpc.Wrap("x", nil) != nil {
		t.Fatal("nil stays nil")
	}
}

func Test失敗する呼び出し_メッセージが操作名つきで返る(t *testing.T) {
	p := connect(t, &recorder{err: status.Error(codes.Internal, "broken"), plan: &pb.LaunchPlan{}})
	ctx := context.Background()
	if _, err := p.PrepareStart(ctx, spec); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("PrepareStart: %v", err)
	}
	if _, err := p.AdminAccess(ctx, spec); err == nil {
		t.Fatal("AdminAccess")
	}
	if _, err := p.FormatDSN(ctx, spec, "h", "u", "p"); err == nil {
		t.Fatal("FormatDSN")
	}
	if _, err := p.TailLog(ctx, spec, 1); err == nil {
		t.Fatal("TailLog")
	}
}
