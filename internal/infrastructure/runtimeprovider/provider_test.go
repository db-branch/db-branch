package runtimeprovider_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
)

var ctx = context.Background()

func testSpec() domain.EngineSpec {
	return domain.EngineSpec{
		Engine: "testdb", DisplayName: "TestDB",
		Protocols: []domain.ProtocolPort{{Name: "sql", Primary: true}},
		Capabilities: domain.RuntimeCaps{
			Query: true, Checkpoints: true, Upgrade: true, SourceDump: true, UpdateStats: true, HistoryRecord: true,
		},
		FormatDSN: func(host string, port int, user, password string) string { return "testdb://" + user + "@" + host },
	}
}

func testInstance() domain.InstanceSpec {
	return domain.InstanceSpec{
		ID: "branch-x", Runtime: domain.RuntimeRef{Engine: "testdb", Version: "1.2.3"}, DataDir: "/data/x",
		Ports: map[string]int{"sql": 4001}, Limits: domain.ResourceLimits{MemoryMB: 256},
	}
}

func TestDescribe_実装されていない機能はfalseにして宣言する(t *testing.T) {
	p := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{})

	got, err := p.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Capabilities
	if c.Query || c.SourceDump || c.Upgrade || c.UpdateStats {
		t.Errorf("a server without those ports must not claim them: %+v", c)
	}
	if !c.Checkpoints || !c.HistoryRecord || got.Engine != "testdb" {
		t.Errorf("structural capabilities must stay: %+v", c)
	}
}

func TestDescribe_配線された機能は宣言どおりtrueにする(t *testing.T) {
	hooks := runtimeprovider.Hooks{
		Query: func(context.Context, domain.Server, domain.QueryRequest) (domain.QueryResult, error) {
			return domain.QueryResult{}, nil
		},
		DumpSource: func(context.Context, domain.SourceDumpJob, string) error { return nil },
	}
	p := runtimeprovider.New(testSpec(), &fakeFull{}, hooks)

	got, _ := p.Describe(ctx)
	if c := got.Capabilities; !c.Query || !c.SourceDump || !c.Upgrade || !c.UpdateStats {
		t.Errorf("capabilities = %+v", c)
	}
}

func TestFormatDSN_主ポートでエンジンの形のURLを返す(t *testing.T) {
	spec := testSpec()
	spec.FormatDSN = func(host string, port int, user, password string) string {
		return "testdb://" + user + ":" + password + "@" + host + ":" + string(rune('0'+port%10))
	}
	p := runtimeprovider.New(spec, &fakeServer{}, runtimeprovider.Hooks{})

	got, err := p.FormatDSN(ctx, testInstance(), "h", "u", "pw")
	if err != nil || got != "testdb://u:pw@h:1" {
		t.Errorf("FormatDSN = %q, %v", got, err)
	}
}

func TestCatalog_組み込みエンジンのカタログはコアが持つので空を返す(t *testing.T) {
	p := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{})
	got, err := p.Catalog(ctx)
	if err != nil || len(got) != 0 {
		t.Errorf("Catalog = %v, %v", got, err)
	}
}

var errBoom = errors.New("boom")

func TestInitializeとPrepareClone_InstanceSpecを既存のServerへ写して委譲する(t *testing.T) {
	f := &fakeServer{}
	p := runtimeprovider.New(testSpec(), f, runtimeprovider.Hooks{})

	if err := p.Initialize(ctx, testInstance()); err != nil {
		t.Fatal(err)
	}
	if err := p.PrepareClone(ctx, testInstance()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Initialize {ID:branch-x Runtime:testdb@1.2.3 DataDir:/data/x Port:4001 MemoryMB:256 Extra:[] LegacyAuth:false Maintenance:false}",
		"PrepareClone branch-x",
	}
	if len(f.calls) != 2 || f.calls[0] != want[0] || f.calls[1] != want[1] {
		t.Errorf("calls = %q", f.calls)
	}
}

func TestユーザーとSQLの契約_ブランチのログインで実行しエラーはそのまま返す(t *testing.T) {
	f := &fakeServer{}
	p := runtimeprovider.New(testSpec(), f, runtimeprovider.Hooks{})

	_ = p.CreateUser(ctx, testInstance(), "branch-x", "pw")
	_ = p.ExecAs(ctx, testInstance(), "branch-x", "pw", "SELECT 1")
	_ = p.RemoveInstance(ctx, testInstance())
	want := []string{"CreateUser branch-x pw port=4001", "ExecAs branch-x pw SELECT 1", "Remove branch-x"}
	for i, w := range want {
		if f.calls[i] != w {
			t.Errorf("call %d = %q, want %q", i, f.calls[i], w)
		}
	}

	f.err = errBoom
	if err := p.CreateUser(ctx, testInstance(), "u", "p"); !errors.Is(err, errBoom) {
		t.Errorf("CreateUser error = %v", err)
	}
	if err := p.ExecAs(ctx, testInstance(), "u", "p", "x"); !errors.Is(err, errBoom) {
		t.Errorf("ExecAs error = %v", err)
	}
}

func TestAdminAccess_DBServerのAdminを返す(t *testing.T) {
	f := &fakeServer{admin: domain.AdminAccess{Engine: "testdb", User: "root", Port: 4001}}
	p := runtimeprovider.New(testSpec(), f, runtimeprovider.Hooks{})
	got, err := p.AdminAccess(ctx, testInstance())
	if err != nil || got != f.admin {
		t.Errorf("AdminAccess = %+v, %v", got, err)
	}
}

func TestLoad_ダンプを管理者として取り込む(t *testing.T) {
	f := &fakeServer{}
	p := runtimeprovider.New(testSpec(), f, runtimeprovider.Hooks{})
	if err := p.Load(ctx, testInstance(), "/w/dump.sql"); err != nil || f.calls[0] != "Load /w/dump.sql" {
		t.Errorf("Load: %v %q", err, f.calls)
	}
}

func TestライフサイクルのフックはServerを渡して呼ばれ未配線ならErrFeatureNotSupported(t *testing.T) {
	var got []string
	hooks := runtimeprovider.Hooks{
		PrepareRuntime: func(dir string) error { got = append(got, "PrepareRuntime "+dir); return nil },
		PrepareStart: func(_ context.Context, s domain.Server) (domain.LaunchPlan, error) {
			got = append(got, "PrepareStart "+s.ID)
			return domain.LaunchPlan{Command: "/bin/db"}, nil
		},
		Health: func(_ context.Context, s domain.Server) (domain.HealthState, error) {
			got = append(got, "Health "+s.ID)
			return domain.HealthReady, nil
		},
		Quiesce: func(_ context.Context, s domain.Server, m domain.StopMode) error {
			got = append(got, "Quiesce "+s.ID)
			return nil
		},
		VerifyShutdown: func(_ context.Context, s domain.Server) error { got = append(got, "Verify "+s.ID); return nil },
		TailLog: func(_ context.Context, s domain.Server, n int) (string, error) {
			got = append(got, "TailLog "+s.ID)
			return "log", nil
		},
	}
	p := runtimeprovider.New(testSpec(), &fakeServer{}, hooks)
	i := testInstance()

	if err := p.PrepareRuntime(ctx, "1.2.3", "/stage"); err != nil {
		t.Fatal(err)
	}
	plan, err := p.PrepareStart(ctx, i)
	if err != nil || plan.Command != "/bin/db" {
		t.Errorf("PrepareStart = %+v, %v", plan, err)
	}
	if h, err := p.Health(ctx, i); err != nil || h != domain.HealthReady {
		t.Errorf("Health = %v, %v", h, err)
	}
	_ = p.Quiesce(ctx, i, domain.StopClean)
	_ = p.VerifyCleanShutdown(ctx, i)
	if out, _ := p.TailLog(ctx, i, 5); out != "log" {
		t.Errorf("TailLog = %q", out)
	}
	want := "PrepareRuntime /stage|PrepareStart branch-x|Health branch-x|Quiesce branch-x|Verify branch-x|TailLog branch-x"
	if strings.Join(got, "|") != want {
		t.Errorf("hook calls = %q", got)
	}

	bare := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{})
	if err := bare.PrepareRuntime(ctx, "1", "/s"); err != nil {
		t.Errorf("PrepareRuntime without a fixup = %v, want nil", err)
	}
	if _, err := bare.PrepareStart(ctx, i); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("PrepareStart unwired = %v", err)
	}
	if _, err := bare.Health(ctx, i); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("Health unwired = %v", err)
	}
	if err := bare.Quiesce(ctx, i, domain.StopClean); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("Quiesce unwired = %v", err)
	}
	if err := bare.VerifyCleanShutdown(ctx, i); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("VerifyCleanShutdown unwired = %v", err)
	}
	if _, err := bare.TailLog(ctx, i, 1); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("TailLog unwired = %v", err)
	}
}

func TestPrepareStart_systemdに書けない計画は拒否する(t *testing.T) {
	hooks := runtimeprovider.Hooks{PrepareStart: func(context.Context, domain.Server) (domain.LaunchPlan, error) {
		return domain.LaunchPlan{Command: "/bin/db", Args: []string{"a\nUser=root"}}, nil
	}}
	p := runtimeprovider.New(testSpec(), &fakeServer{}, hooks)
	if _, err := p.PrepareStart(ctx, testInstance()); !errors.Is(err, domain.ErrInvalidLaunchPlan) {
		t.Errorf("PrepareStart = %v, want ErrInvalidLaunchPlan", err)
	}
}

func TestQueryとDumpSource_配線されたフックに委譲し未配線ならErrFeatureNotSupported(t *testing.T) {
	hooks := runtimeprovider.Hooks{
		Query: func(_ context.Context, s domain.Server, q domain.QueryRequest) (domain.QueryResult, error) {
			return domain.QueryResult{Columns: []string{q.SQL}}, nil
		},
		DumpSource: func(_ context.Context, job domain.SourceDumpJob, out string) error { return errBoom },
	}
	p := runtimeprovider.New(testSpec(), &fakeServer{}, hooks)
	if r, err := p.Query(ctx, testInstance(), domain.QueryRequest{SQL: "select 1"}); err != nil || r.Columns[0] != "select 1" {
		t.Errorf("Query = %+v, %v", r, err)
	}
	if err := p.DumpSource(ctx, domain.SourceDumpJob{}, "/o"); !errors.Is(err, errBoom) {
		t.Errorf("DumpSource = %v", err)
	}
	bare := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{})
	if _, err := bare.Query(ctx, testInstance(), domain.QueryRequest{}); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("Query unwired = %v", err)
	}
	if err := bare.DumpSource(ctx, domain.SourceDumpJob{}, "/o"); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("DumpSource unwired = %v", err)
	}
}

func TestUpdateStatsとアップグレード_DBServerの任意ポートに委譲する(t *testing.T) {
	f := &fakeFull{plan: domain.UpgradePlan{LegacyAuth: true}, log: domain.UpgradeLog{Excerpt: "ok"}}
	hooks := runtimeprovider.Hooks{PrepareStart: func(context.Context, domain.Server) (domain.LaunchPlan, error) {
		return domain.LaunchPlan{Command: "/bin/db", ServiceType: "notify"}, nil
	}}
	p := runtimeprovider.New(testSpec(), f, hooks)
	i := testInstance()

	if err := p.UpdateStats(ctx, i); err != nil {
		t.Fatal(err)
	}
	plan, err := p.CheckUpgrade(ctx, i, "1.3.0", domain.Login{User: "u"})
	if err != nil || !plan.LegacyAuth {
		t.Errorf("CheckUpgrade = %+v, %v", plan, err)
	}
	lp, ulog, err := p.StartUpgraded(ctx, i, domain.Login{User: "u"})
	if err != nil || ulog.Excerpt != "ok" || lp.ServiceType != "notify" {
		t.Errorf("StartUpgraded = %+v %+v %v", lp, ulog, err)
	}
	want := []string{"UpdateStats branch-x", "CheckUpgrade branch-x testdb@1.3.0 u", "StartUpgraded branch-x u"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q", f.calls)
	}
}

func TestアップグレードとUpdateStats_DBServerが持たないときはErrFeatureNotSupported(t *testing.T) {
	p := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{})
	i := testInstance()
	if err := p.UpdateStats(ctx, i); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("UpdateStats = %v", err)
	}
	if _, err := p.CheckUpgrade(ctx, i, "1.3.0", domain.Login{}); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("CheckUpgrade = %v", err)
	}
	if _, _, err := p.StartUpgraded(ctx, i, domain.Login{}); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("StartUpgraded = %v", err)
	}
}

func TestCheckUpgradePath_バージョン文字列をエンジンの規則で判定する(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	_ = reg.Register(domain.EngineSpec{Engine: "testdb", CheckUpgradePath: func(from, to domain.RuntimeRef) error {
		if to.Version == "9.0" {
			return domain.ErrUpgradeRefused
		}
		return nil
	}})
	p := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{}).WithRegistry(reg)

	if err := p.CheckUpgradePath(ctx, "1.0", "1.1"); err != nil {
		t.Errorf("1.0 -> 1.1 = %v", err)
	}
	if err := p.CheckUpgradePath(ctx, "1.0", "9.0"); !errors.Is(err, domain.ErrUpgradeRefused) {
		t.Errorf("1.0 -> 9.0 = %v", err)
	}
	if err := p.CheckUpgradePath(ctx, "bad version", "1.1"); !errors.Is(err, domain.ErrInvalidRuntime) {
		t.Errorf("unreadable version = %v", err)
	}
}

func TestProviderはRuntimeProviderの契約を満たす(t *testing.T) {
	var _ domain.RuntimeProvider = runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{})
	p := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{})
	if _, err := p.PreFreeze(ctx, testInstance()); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("PreFreeze = %v", err)
	}
}

func TestStartUpgraded_起動の失敗と計画の失敗をそのまま返す(t *testing.T) {
	f := &fakeFull{}
	f.err = errBoom
	p := runtimeprovider.New(testSpec(), f, runtimeprovider.Hooks{})
	if _, _, err := p.StartUpgraded(ctx, testInstance(), domain.Login{}); !errors.Is(err, errBoom) {
		t.Errorf("server failure = %v", err)
	}

}

func TestPrepareStart_フックの失敗をそのまま返す(t *testing.T) {
	hooks := runtimeprovider.Hooks{PrepareStart: func(context.Context, domain.Server) (domain.LaunchPlan, error) {
		return domain.LaunchPlan{}, errBoom
	}}
	p := runtimeprovider.New(testSpec(), &fakeServer{}, hooks)
	if _, err := p.PrepareStart(ctx, testInstance()); !errors.Is(err, errBoom) {
		t.Errorf("PrepareStart = %v", err)
	}
}

func TestCheckUpgradePath_移行先の版が読めなければ断る(t *testing.T) {
	p := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{}).WithRegistry(domain.NewRuntimeRegistry())
	// "testdb" is unknown to this registry: neither side parses
	if err := p.CheckUpgradePath(ctx, "1.0", "1.1"); !errors.Is(err, domain.ErrInvalidRuntime) {
		t.Errorf("unknown engine = %v", err)
	}
	reg := domain.NewRuntimeRegistry()
	_ = reg.Register(domain.EngineSpec{Engine: "testdb", VersionPattern: `^1\.[0-9]$`})
	q := runtimeprovider.New(testSpec(), &fakeServer{}, runtimeprovider.Hooks{}).WithRegistry(reg)
	if err := q.CheckUpgradePath(ctx, "1.0", "2.0"); !errors.Is(err, domain.ErrInvalidRuntime) {
		t.Errorf("target outside the pattern = %v", err)
	}
}

func TestStartUpgraded_計画を先に作り検証してからサーバーを起動する(t *testing.T) {
	bad := runtimeprovider.Hooks{PrepareStart: func(context.Context, domain.Server) (domain.LaunchPlan, error) {
		return domain.LaunchPlan{Command: "/bin/db", Args: []string{"a\nUser=root"}}, nil
	}}
	f := &fakeFull{}
	p := runtimeprovider.New(testSpec(), f, bad)
	if _, _, err := p.StartUpgraded(ctx, testInstance(), domain.Login{}); !errors.Is(err, domain.ErrInvalidLaunchPlan) {
		t.Errorf("StartUpgraded = %v, want ErrInvalidLaunchPlan", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("the server was started although the plan was refused: %q", f.calls)
	}

	failing := runtimeprovider.Hooks{PrepareStart: func(context.Context, domain.Server) (domain.LaunchPlan, error) {
		return domain.LaunchPlan{}, errBoom
	}}
	g := &fakeFull{}
	if _, _, err := runtimeprovider.New(testSpec(), g, failing).StartUpgraded(ctx, testInstance(), domain.Login{}); !errors.Is(err, errBoom) || len(g.calls) != 0 {
		t.Errorf("plan failure = %v, calls %q", err, g.calls)
	}
}
