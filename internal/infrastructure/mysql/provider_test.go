package mysql_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/mysql"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
)

func providerFixture(t *testing.T) (fixture, domain.RuntimeProvider, domain.InstanceSpec) {
	t.Helper()
	f := newFixture(t)
	p := mysql.NewProvider(f.srv, runtimeprovider.Hooks{})
	inst := domain.InstanceSpec{
		ID: f.spec.ID, Runtime: f.spec.Runtime, DataDir: f.spec.DataDir,
		Ports: map[string]int{"mysql": f.spec.Port}, Limits: domain.ResourceLimits{MemoryMB: f.spec.MemoryMB},
	}
	return f, p, inst
}

func TestNewProvider_MySQLの宣言を返し配線されていない機能は偽にする(t *testing.T) {
	_, p, _ := providerFixture(t)

	spec, err := p.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Capabilities
	if spec.Engine != domain.MySQL || !c.Upgrade || !c.UpdateStats || !c.HistoryRecord || !c.Checkpoints {
		t.Errorf("spec = %+v", spec)
	}
	if c.Query || c.SourceDump {
		t.Errorf("Query and SourceDump are not wired: %+v", c)
	}
}

func TestNewProvider_追加のフックを配線すると機能を宣言する(t *testing.T) {
	f := newFixture(t)
	extra := runtimeprovider.Hooks{
		PrepareRuntime: func(string) error { return nil },
		Query: func(context.Context, domain.Server, domain.QueryRequest) (domain.QueryResult, error) {
			return domain.QueryResult{}, nil
		},
		DumpSource: func(context.Context, domain.SourceDumpJob, string) error { return nil },
	}
	spec, _ := mysql.NewProvider(f.srv, extra).Describe(ctx)
	if !spec.Capabilities.Query || !spec.Capabilities.SourceDump {
		t.Errorf("capabilities = %+v", spec.Capabilities)
	}
}

func TestPrepareStart_設定を書きsystemdで起動するコマンドを返す(t *testing.T) {
	f, p, inst := providerFixture(t)

	plan, err := p.PrepareStart(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	cnf := filepath.Join(f.state, f.spec.ID, "my.cnf")
	if plan.Command != filepath.Join(f.rtDir, "bin", "mysqld") || len(plan.Args) != 1 || plan.Args[0] != "--defaults-file="+cnf {
		t.Errorf("plan = %+v", plan)
	}
	if plan.ServiceType != "simple" || plan.KillSignal != "SIGTERM" || plan.TimeoutStopSec != 900 {
		t.Errorf("unit settings = %+v", plan)
	}
	if !strings.Contains(read(t, cnf), "port=40001\n") {
		t.Error("PrepareStart must write my.cnf")
	}
	if f.sys.called("systemctl") {
		t.Errorf("the core, not the provider, runs systemctl: %q", f.sys.calls)
	}
}

func TestPrepareStart_データの持ち主が違えば何も書かずに断る(t *testing.T) {
	f, _, inst := providerFixture(t)
	f.srv.CheckOwner = func(string) error { return domain.ErrOwnership }

	_, err := mysql.NewProvider(f.srv, runtimeprovider.Hooks{}).PrepareStart(ctx, inst)
	if !errors.Is(err, domain.ErrOwnership) {
		t.Errorf("PrepareStart = %v, want ErrOwnership", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.state, f.spec.ID, "my.cnf")); statErr == nil {
		t.Error("my.cnf was written for data the server may not use")
	}
}

func TestHealth_停止_起動中_準備完了を見分ける(t *testing.T) {
	f, p, inst := providerFixture(t)

	if h, _ := p.Health(ctx, inst); h != domain.HealthStopped {
		t.Errorf("not running = %v", h)
	}
	f.sys.active = true
	f.sys.pingFails = 1
	if h, _ := p.Health(ctx, inst); h != domain.HealthStarting {
		t.Errorf("running, not answering = %v", h)
	}
	if h, _ := p.Health(ctx, inst); h != domain.HealthReady {
		t.Errorf("answering = %v", h)
	}
}

func TestQuiesce_クリーン停止の前にInnoDBを完全にフラッシュさせる(t *testing.T) {
	f, p, inst := providerFixture(t)
	f.sys.active = true

	if err := p.Quiesce(ctx, inst, domain.StopClean); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("mysql --socket=") || !strings.Contains(strings.Join(f.sys.calls, "\n"), "SET GLOBAL innodb_fast_shutdown = 0") {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestQuiesce_即時停止と停止済みでは何もしない(t *testing.T) {
	f, p, inst := providerFixture(t)
	f.sys.active = true
	if err := p.Quiesce(ctx, inst, domain.StopImmediate); err != nil || len(f.sys.calls) > 1 {
		t.Errorf("immediate: %v %q", err, f.sys.calls)
	}
	f.sys.calls, f.sys.active = nil, false
	if err := p.Quiesce(ctx, inst, domain.StopClean); err != nil || f.sys.called("mysql") {
		t.Errorf("not running: %v %q", err, f.sys.calls)
	}
}

func TestVerifyCleanShutdown_最後の起動より後にShutdown_completeがあれば成功(t *testing.T) {
	f, p, inst := providerFixture(t)
	log := f.sys.errorLog

	appendFile(log, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
	if err := p.VerifyCleanShutdown(ctx, inst); err != nil {
		t.Errorf("clean = %v", err)
	}
	appendFile(log, "[Server] /mysqld: ready for connections.\n")
	if err := p.VerifyCleanShutdown(ctx, inst); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("started again after shutdown = %v, want ErrUncleanShutdown", err)
	}
}

func TestVerifyCleanShutdown_ログがない_Shutdown_completeがなければ未完了(t *testing.T) {
	f, p, inst := providerFixture(t)
	if err := p.VerifyCleanShutdown(ctx, inst); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("no log = %v", err)
	}
	appendFile(f.sys.errorLog, "[ERROR] crashed\n")
	if err := p.VerifyCleanShutdown(ctx, inst); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("crash = %v", err)
	}
}

func TestTailLog_エラーログの末尾の行を返す(t *testing.T) {
	f, p, inst := providerFixture(t)
	appendFile(f.sys.errorLog, "a\nb\nc\nd\n")

	got, err := p.TailLog(ctx, inst, 2)
	if err != nil || got != "c\nd" {
		t.Errorf("TailLog = %q, %v", got, err)
	}
	if got, _ := p.TailLog(ctx, inst, 0); got != "" {
		t.Errorf("TailLog(0) = %q", got)
	}
	empty, p2, inst2 := providerFixture(t)
	_ = empty
	if got, err := p2.TailLog(ctx, inst2, 5); err != nil || got != "" {
		t.Errorf("no log = %q, %v", got, err)
	}
}

func TestCheckUpgradePath_既存のMySQL規則と同じ結果を返す(t *testing.T) {
	_, p, _ := providerFixture(t)
	for _, c := range []struct{ from, to string }{
		{"8.0.40", "8.4.3"}, {"5.7.44", "8.4.3"}, {"8.4.3", "8.0.40"}, {"8.4.3", "8.4.3"}, {"8.4.3", "9.7.0"},
	} {
		want := domain.CheckUpgradePath(domain.RuntimeRef{Engine: domain.MySQL, Version: c.from}, domain.RuntimeRef{Engine: domain.MySQL, Version: c.to})
		got := p.CheckUpgradePath(ctx, c.from, c.to)
		if (got == nil) != (want == nil) || (got != nil && got.Error() != want.Error()) {
			t.Errorf("%s -> %s: provider %v, domain %v", c.from, c.to, got, want)
		}
	}
}

func TestPrepareStart_設定を書けなければ失敗を返す(t *testing.T) {
	f, _, inst := providerFixture(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.StateRoot = filepath.Join(blocker, "instances") // a file where a directory must be

	if _, err := mysql.NewProvider(f.srv, runtimeprovider.Hooks{}).PrepareStart(ctx, inst); err == nil {
		t.Error("PrepareStart succeeded without being able to write the configuration")
	}
}

func TestNewProvider_全フックを配線するとレジストリの宣言と一致する(t *testing.T) {
	f := newFixture(t)
	wired := runtimeprovider.Hooks{
		Query: func(context.Context, domain.Server, domain.QueryRequest) (domain.QueryResult, error) {
			return domain.QueryResult{}, nil
		},
		DumpSource: func(context.Context, domain.SourceDumpJob, string) error { return nil },
	}
	got, _ := mysql.NewProvider(f.srv, wired).Describe(ctx)
	want, _ := domain.DefaultRuntimes.Spec(domain.MySQL)
	if got.Capabilities != want.Capabilities {
		t.Errorf("Describe %+v differs from the registry %+v", got.Capabilities, want.Capabilities)
	}
}
