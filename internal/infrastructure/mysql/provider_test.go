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
	if !f.sys.called("mysql --socket=") || !strings.Contains(strings.Join(f.sys.stdin, "\n"), "SET GLOBAL innodb_fast_shutdown = 0") {
		t.Errorf("calls = %q stdin = %q", f.sys.calls, f.sys.stdin)
	}
}

func TestQuiesce_即時停止と停止済みでは何もしない(t *testing.T) {
	f, p, inst := providerFixture(t)
	f.sys.active = true
	if err := p.Quiesce(ctx, inst, domain.StopImmediate); err != nil || len(f.sys.calls) != 0 {
		t.Errorf("immediate: %v %q", err, f.sys.calls)
	}
	f.sys.calls, f.sys.active = nil, false
	if err := p.Quiesce(ctx, inst, domain.StopClean); err != nil || len(f.sys.calls) != 1 || !f.sys.called("systemctl is-active") {
		t.Errorf("not running: want only the state check, got %v %q", err, f.sys.calls)
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

func TestVerifyCleanShutdown_ログがシンボリックリンクなら辿らない(t *testing.T) {
	f, p, inst := providerFixture(t)
	target := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(target, []byte("ready for connections.\nShutdown complete\n"), 0o600)
	_ = os.MkdirAll(filepath.Dir(f.sys.errorLog), 0o755)
	if err := os.Symlink(target, f.sys.errorLog); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyCleanShutdown(ctx, inst); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("symlinked log = %v, want ErrUncleanShutdown", err)
	}
}

func TestVerifyCleanShutdown_まだ移されていない旧い場所のログも読む(t *testing.T) {
	f, p, inst := providerFixture(t)
	old := filepath.Join(filepath.Dir(filepath.Dir(f.sys.errorLog)), "error.log")
	appendFile(old, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
	if err := p.VerifyCleanShutdown(ctx, inst); err != nil {
		t.Errorf("legacy log = %v", err)
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

func TestCheckUpgradePath_MySQLのLTS規則で判定する(t *testing.T) {
	_, p, _ := providerFixture(t)
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"8.0.40", "8.4.3", true}, {"8.4.3", "8.4.9", true}, {"5.7.44", "8.0.40", true},
		{"5.7.44", "8.4.3", false}, // skips 8.0
		{"8.4.3", "9.7.0", true}, {"8.0.40", "9.7.0", false},
		{"8.4.3", "8.0.40", false}, {"8.4.3", "8.4.3", false}, {"5.5.1", "5.7.44", false},
	} {
		err := p.CheckUpgradePath(ctx, c.from, c.to)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, domain.ErrUpgradeRefused)) {
			t.Errorf("%s -> %s: %v, want ok=%v", c.from, c.to, err, c.ok)
		}
	}
}

func TestNewProvider_MySQLが登録されていなければ起動時に気づけるようpanicする(t *testing.T) {
	old := domain.DefaultRuntimes
	domain.DefaultRuntimes = domain.NewRuntimeRegistry()
	defer func() { domain.DefaultRuntimes = old }()
	defer func() {
		if recover() == nil {
			t.Error("NewProvider accepted a registry without mysql")
		}
	}()
	mysql.NewProvider(newFixture(t).srv, runtimeprovider.Hooks{})
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

func TestVerifyCleanShutdown_停止のあとの起動の途中で落ちていたら未完了(t *testing.T) {
	f, p, inst := providerFixture(t)
	appendFile(f.sys.errorLog, "[Server] /mysqld (mysqld 8.4.13) starting as process 11\n[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
	if err := p.VerifyCleanShutdown(ctx, inst); err != nil {
		t.Fatalf("clean stop = %v", err)
	}
	appendFile(f.sys.errorLog, "[Server] /mysqld (mysqld 8.4.13) starting as process 12\n[ERROR] InnoDB: recovery failed\n")
	if err := p.VerifyCleanShutdown(ctx, inst); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("died while starting after the last clean stop = %v, want ErrUncleanShutdown", err)
	}
}

func TestHealth_systemctlの状態で停止_起動中_準備完了を見分けエラーは返す(t *testing.T) {
	f, p, inst := providerFixture(t)
	f.sys.active = true
	f.sys.outputs = map[string]string{"--property=ActiveState": "activating\n"}
	if h, _ := p.Health(ctx, inst); h != domain.HealthStarting {
		t.Errorf("activating = %v, want starting", h)
	}
	if f.sys.called("mysqladmin") {
		t.Errorf("mysqladmin asked while the unit is activating: %q", f.sys.calls)
	}
	for _, state := range []string{"inactive", "failed", "deactivating"} {
		f.sys.outputs = map[string]string{"--property=ActiveState": state + "\n"}
		if h, _ := p.Health(ctx, inst); h != domain.HealthStopped {
			t.Errorf("%s = %v, want stopped", state, h)
		}
	}
	f.sys.outputs = nil
	f.sys.failOn = "--property=ActiveState"
	if h, err := p.Health(ctx, inst); err == nil || h != domain.HealthStopped {
		t.Errorf("systemctl failing: Health = %v, %v: want the failure reported", h, err)
	}
}
