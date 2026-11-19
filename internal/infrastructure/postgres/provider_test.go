package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/postgres"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
)

func providerFixture(t *testing.T) (fixture, domain.RuntimeProvider, domain.InstanceSpec) {
	t.Helper()
	f := newFixture(t)
	p := postgres.NewProvider(f.srv, runtimeprovider.Hooks{})
	inst := domain.InstanceSpec{
		ID: f.spec.ID, Runtime: f.spec.Runtime, DataDir: f.spec.DataDir,
		Ports: map[string]int{"postgres": f.spec.Port}, Limits: domain.ResourceLimits{MemoryMB: f.spec.MemoryMB},
	}
	return f, p, inst
}

func TestNewProvider_PostgreSQLの宣言を返し配線されていない機能は偽にする(t *testing.T) {
	_, p, _ := providerFixture(t)

	spec, err := p.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Capabilities
	if spec.Engine != domain.Postgres || !spec.CollationBound || !c.Upgrade || !c.UpdateStats || c.HistoryRecord {
		t.Errorf("spec = %+v", spec)
	}
	if c.Query || c.SourceDump {
		t.Errorf("Query and SourceDump are not wired: %+v", c)
	}
}

func TestPrepareStart_envを書き_unitが実行するpostgresのコマンドを返す(t *testing.T) {
	f, p, inst := providerFixture(t)

	plan, err := p.PrepareStart(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Command != filepath.Join(f.rt, "bin", "postgres") {
		t.Errorf("Command = %q", plan.Command)
	}
	args := strings.Join(plan.Args, " ")
	for _, want := range []string{
		"-D " + f.spec.DataDir, "-p 40001", "-c listen_addresses=0.0.0.0",
		"-c unix_socket_directories=/run/db-branch-postgres/base-prod", "-c shared_buffers=128MB", "-c jit=off",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	if plan.ServiceType != "notify" || plan.KillSignal != "SIGINT" || plan.TimeoutStopSec != 900 {
		t.Errorf("unit settings = %+v", plan)
	}
	if plan.Env["LD_LIBRARY_PATH"] != filepath.Join(f.rt, "lib") {
		t.Errorf("Env = %v", plan.Env)
	}
	if !strings.Contains(read(t, filepath.Join(f.state, "base-prod", "env")), "PG_PORT=40001\n") {
		t.Error("PrepareStart must write the env file")
	}
	if f.sys.called("systemctl") {
		t.Errorf("the core, not the provider, runs systemctl: %q", f.sys.calls)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPrepareStart_データの持ち主が違えば何も書かずに断る(t *testing.T) {
	f, _, inst := providerFixture(t)
	f.srv.CheckOwner = func(string) error { return domain.ErrOwnership }

	_, err := postgres.NewProvider(f.srv, runtimeprovider.Hooks{}).PrepareStart(ctx, inst)
	if !errors.Is(err, domain.ErrOwnership) {
		t.Errorf("PrepareStart = %v, want ErrOwnership", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.state, "base-prod", "env")); statErr == nil {
		t.Error("env was written for data the server may not use")
	}
}

func TestPrepareStart_envを書けなければ失敗を返す(t *testing.T) {
	f, _, inst := providerFixture(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.StateRoot = filepath.Join(blocker, "instances")
	if _, err := postgres.NewProvider(f.srv, runtimeprovider.Hooks{}).PrepareStart(ctx, inst); err == nil {
		t.Error("PrepareStart succeeded without being able to write the env file")
	}
}

func TestHealth_停止_起動中_準備完了を見分ける(t *testing.T) {
	f, p, inst := providerFixture(t)

	if h, _ := p.Health(ctx, inst); h != domain.HealthStopped {
		t.Errorf("not running = %v", h)
	}
	f.sys.active = true
	f.sys.notReady = 1
	if h, _ := p.Health(ctx, inst); h != domain.HealthStarting {
		t.Errorf("running, not accepting = %v", h)
	}
	if h, _ := p.Health(ctx, inst); h != domain.HealthReady {
		t.Errorf("accepting = %v", h)
	}
}

func TestQuiesce_PostgreSQLはunitのSIGINTがチェックポイントを書くので何もしない(t *testing.T) {
	f, p, inst := providerFixture(t)
	f.sys.active = true
	for _, m := range []domain.StopMode{domain.StopClean, domain.StopImmediate} {
		if err := p.Quiesce(ctx, inst, m); err != nil {
			t.Errorf("Quiesce(%d) = %v", m, err)
		}
	}
	if len(f.sys.calls) != 0 {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestVerifyCleanShutdown_制御ファイルがshut_downなら成功(t *testing.T) {
	f, p, inst := providerFixture(t)

	if err := p.VerifyCleanShutdown(ctx, inst); err != nil {
		t.Errorf("shut down = %v", err)
	}
	f.sys.state = "in production"
	if err := p.VerifyCleanShutdown(ctx, inst); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("in production = %v, want ErrUncleanShutdown", err)
	}
	if err := p.VerifyCleanShutdown(ctx, inst); err == nil || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("message = %v, want no <nil>", err)
	}
	f.sys.state = "shut down"
	f.sys.failOn = "pg_controldata"
	if err := p.VerifyCleanShutdown(ctx, inst); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("pg_controldata fails = %v, want ErrUncleanShutdown", err)
	}
}

func TestTailLog_unitのジャーナルの末尾を返す(t *testing.T) {
	f, p, inst := providerFixture(t)
	got, err := p.TailLog(ctx, inst, 7)
	if err != nil || got != "FATAL: something" {
		t.Errorf("TailLog = %q, %v", got, err)
	}
	if !f.sys.called("journalctl -u db-branch-postgres@base-prod.service -n 7 --no-pager -o cat") {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestCheckUpgradePath_PostgreSQLは同じメジャーの中だけ許す(t *testing.T) {
	_, p, _ := providerFixture(t)
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"17.2", "17.5", true}, {"9.6.1", "9.6.5", true},
		{"17.2", "18.1", false}, {"9.5.1", "9.6.1", false},
		{"17.5", "17.2", false}, {"17.2", "17.2", false},
	} {
		err := p.CheckUpgradePath(ctx, c.from, c.to)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, domain.ErrUpgradeRefused)) {
			t.Errorf("%s -> %s: %v, want ok=%v", c.from, c.to, err, c.ok)
		}
	}
}

func TestTailLog_行数が正でなければ外部コマンドを呼ばない(t *testing.T) {
	f, p, inst := providerFixture(t)
	for _, n := range []int{0, -5} {
		if got, err := p.TailLog(ctx, inst, n); err != nil || got != "" {
			t.Errorf("TailLog(%d) = %q, %v", n, got, err)
		}
	}
	if len(f.sys.calls) != 0 {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestNewProvider_PostgreSQLが登録されていなければ起動時に気づけるようpanicする(t *testing.T) {
	old := domain.DefaultRuntimes
	domain.DefaultRuntimes = domain.NewRuntimeRegistry()
	defer func() { domain.DefaultRuntimes = old }()
	defer func() {
		if recover() == nil {
			t.Error("NewProvider accepted a registry without postgres")
		}
	}()
	postgres.NewProvider(newFixture(t).srv, runtimeprovider.Hooks{})
}

func TestNewProvider_全フックを配線するとレジストリの宣言と一致する(t *testing.T) {
	f := newFixture(t)
	wired := runtimeprovider.Hooks{
		Query: func(context.Context, domain.Server, domain.QueryRequest) (domain.QueryResult, error) {
			return domain.QueryResult{}, nil
		},
		DumpSource: func(context.Context, domain.SourceDumpJob, string) error { return nil },
	}
	got, _ := postgres.NewProvider(f.srv, wired).Describe(ctx)
	want, _ := domain.DefaultRuntimes.Spec(domain.Postgres)
	if got.Capabilities != want.Capabilities {
		t.Errorf("Describe %+v differs from the registry %+v", got.Capabilities, want.Capabilities)
	}
}

func TestPrepareStart_メンテナンス中はTCPを待ち受けずソケットだけにする(t *testing.T) {
	f, p, inst := providerFixture(t)
	inst.Maintenance = true

	plan, err := p.PrepareStart(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(plan.Args, "listen_addresses=") {
		t.Errorf("args = %q, want an empty listen_addresses", plan.Args)
	}
	if env := read(t, filepath.Join(f.state, "base-prod", "env")); !strings.Contains(env, "PG_BIND=\n") {
		t.Errorf("env = %q, want PG_BIND empty", env)
	}
	inst.Maintenance = false
	plan, _ = p.PrepareStart(ctx, inst)
	if !contains(plan.Args, "listen_addresses=0.0.0.0") {
		t.Errorf("normal start args = %q", plan.Args)
	}
	if env := read(t, filepath.Join(f.state, "base-prod", "env")); !strings.Contains(env, "PG_BIND=0.0.0.0\n") {
		t.Errorf("env = %q", env)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
