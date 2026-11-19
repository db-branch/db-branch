package domain_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestRuntimeCaps_非対応の機能はErrFeatureNotSupportedで断る(t *testing.T) {
	caps := domain.RuntimeCaps{Query: true, BranchMerge: false}

	if err := caps.Require("query"); err != nil {
		t.Errorf("Require(query) = %v, want nil", err)
	}
	err := caps.Require("branch_merge")
	if !errors.Is(err, domain.ErrFeatureNotSupported) || !strings.Contains(err.Error(), "branch_merge") {
		t.Errorf("Require(branch_merge) = %v, want ErrFeatureNotSupported naming the feature", err)
	}
	if err := caps.Require("teleport"); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("Require(unknown) = %v, want ErrFeatureNotSupported", err)
	}
}

func TestRuntimeCaps_Featuresは全機能を真偽で列挙する(t *testing.T) {
	got := domain.RuntimeCaps{Query: true, LiveFreeze: true}.Features()

	want := []string{"query", "checkpoints", "live_freeze", "upgrade", "source_dump", "branch_merge", "history_record", "update_stats", "instance_metrics"}
	if len(got) != len(want) {
		t.Fatalf("Features() has %d entries, want %d: %v", len(got), len(want), got)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("Features() lacks %q", k)
		}
	}
	if !got["query"] || !got["live_freeze"] || got["branch_merge"] {
		t.Errorf("Features() = %v", got)
	}
}

func TestUnsupportedFeatures_任意契約はすべてErrFeatureNotSupportedを返す(t *testing.T) {
	var u domain.UnsupportedFeatures
	ctx := context.Background()
	s := domain.InstanceSpec{}
	checks := map[string]error{
		"PreFreeze":        func() error { _, err := u.PreFreeze(ctx, s); return err }(),
		"PostFreeze":       u.PostFreeze(ctx, s, domain.FreezeToken{}),
		"UpdateStats":      u.UpdateStats(ctx, s),
		"CheckUpgradePath": u.CheckUpgradePath(ctx, "1.0", "2.0"),
		"CheckUpgrade":     func() error { _, err := u.CheckUpgrade(ctx, s, "2.0", domain.Login{}); return err }(),
		"StartUpgraded":    func() error { _, _, err := u.StartUpgraded(ctx, s, domain.Login{}); return err }(),
		"DumpSource":       u.DumpSource(ctx, domain.SourceDumpJob{}, "/x"),
		"Load":             u.Load(ctx, s, "/x"),
		"InstanceMetrics":  func() error { _, err := u.InstanceMetrics(ctx, s); return err }(),
	}
	for name, err := range checks {
		if !errors.Is(err, domain.ErrFeatureNotSupported) {
			t.Errorf("%s = %v, want ErrFeatureNotSupported", name, err)
		}
	}
}

func TestInstanceSpec_主ポートと既存のServer表現へ写せる(t *testing.T) {
	spec := domain.EngineSpec{Engine: "clickhouse", Protocols: []domain.ProtocolPort{{Name: "native"}, {Name: "http", Primary: true}}}
	i := domain.InstanceSpec{
		ID: "branch-x", Runtime: domain.RuntimeRef{Engine: "clickhouse", Version: "25.3.1"}, DataDir: "/tank/db-branch/branches/x",
		Ports: map[string]int{"http": 8123, "native": 9000}, Limits: domain.ResourceLimits{MemoryMB: 512},
		Extra: []string{"a=1"}, LegacyAuth: true, Maintenance: true,
	}

	if got := i.PrimaryPort(spec); got != 8123 {
		t.Errorf("PrimaryPort = %d, want 8123", got)
	}
	want := domain.Server{ID: "branch-x", Runtime: i.Runtime, DataDir: i.DataDir, Port: 8123, MemoryMB: 512, Extra: []string{"a=1"}, LegacyAuth: true, Maintenance: true}
	if got := i.Server(spec); !reflect.DeepEqual(got, want) {
		t.Errorf("Server = %+v, want %+v", got, want)
	}
	if got := i.PrimaryPort(domain.EngineSpec{}); got != 0 {
		t.Errorf("PrimaryPort without protocols = %d, want 0", got)
	}
}

func TestHealthState_読める名前を返す(t *testing.T) {
	for h, want := range map[domain.HealthState]string{domain.HealthStopped: "stopped", domain.HealthStarting: "starting", domain.HealthReady: "ready"} {
		if h.String() != want {
			t.Errorf("%d.String() = %q, want %q", h, h.String(), want)
		}
	}
}

func TestLaunchPlan_正しい計画を受理する(t *testing.T) {
	plan := domain.LaunchPlan{
		Command: "/var/lib/db-branch/runtimes/clickhouse/25.3.1/bin/clickhouse", Args: []string{"server", "--config-file=/var/lib/db-branch/instances/x/config.xml"},
		Env: map[string]string{"LD_LIBRARY_PATH": "/opt/lib"}, ServiceType: "notify", KillSignal: "SIGINT", TimeoutStopSec: 300,
	}
	if err := plan.Validate(); err != nil {
		t.Errorf("Validate = %v", err)
	}
	if err := (domain.LaunchPlan{Command: "/bin/x"}).Validate(); err != nil {
		t.Errorf("a plan with only a command (unit defaults) = %v", err)
	}
}

func TestLaunchPlan_systemdディレクティブを注入できる計画を拒否する(t *testing.T) {
	ok := domain.LaunchPlan{Command: "/bin/x"}
	cases := map[string]func(p *domain.LaunchPlan){
		"コマンドが空":          func(p *domain.LaunchPlan) { p.Command = "" },
		"コマンドが相対パス":       func(p *domain.LaunchPlan) { p.Command = "bin/x" },
		"コマンドに改行":         func(p *domain.LaunchPlan) { p.Command = "/bin/x\nUser=root" },
		"引数に改行":           func(p *domain.LaunchPlan) { p.Args = []string{"a\nExecStartPre=/bin/evil"} },
		"引数にCR":           func(p *domain.LaunchPlan) { p.Args = []string{"a\rb"} },
		"引数にNUL":          func(p *domain.LaunchPlan) { p.Args = []string{"a\x00b"} },
		"引数に制御文字":         func(p *domain.LaunchPlan) { p.Args = []string{"a\x1bb"} },
		"環境変数の値に改行":       func(p *domain.LaunchPlan) { p.Env = map[string]string{"A": "x\nUser=root"} },
		"環境変数名に=":         func(p *domain.LaunchPlan) { p.Env = map[string]string{"A=B": "x"} },
		"環境変数名が空":         func(p *domain.LaunchPlan) { p.Env = map[string]string{"": "x"} },
		"環境変数名に空白":        func(p *domain.LaunchPlan) { p.Env = map[string]string{"A B": "x"} },
		"許可外のServiceType": func(p *domain.LaunchPlan) { p.ServiceType = "forking\nUser=root" },
		"許可外のKillSignal":  func(p *domain.LaunchPlan) { p.KillSignal = "SIGUSR3" },
		"負のタイムアウト":        func(p *domain.LaunchPlan) { p.TimeoutStopSec = -1 },
	}
	for name, mutate := range cases {
		p := ok
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, domain.ErrInvalidLaunchPlan) {
			t.Errorf("%s: Validate = %v, want ErrInvalidLaunchPlan", name, err)
		}
	}
}
