package conformance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/conformance"
)

// engine is a provider that answers like a well-behaved module whose
// capabilities are Query and UpdateStats only.
type engine struct {
	domain.UnsupportedFeatures
	spec domain.EngineSpec
	plan domain.LaunchPlan
}

func (e engine) Describe(context.Context) (domain.EngineSpec, error)     { return e.spec, nil }
func (e engine) Catalog(context.Context) ([]domain.CatalogEntry, error)  { return nil, nil }
func (e engine) PrepareRuntime(context.Context, string, string) error    { return nil }
func (e engine) Initialize(context.Context, domain.InstanceSpec) error   { return nil }
func (e engine) PrepareClone(context.Context, domain.InstanceSpec) error { return nil }
func (e engine) PrepareStart(context.Context, domain.InstanceSpec) (domain.LaunchPlan, error) {
	return e.plan, nil
}
func (e engine) Health(context.Context, domain.InstanceSpec) (domain.HealthState, error) {
	return domain.HealthStopped, nil
}
func (e engine) Quiesce(context.Context, domain.InstanceSpec, domain.StopMode) error { return nil }
func (e engine) VerifyCleanShutdown(context.Context, domain.InstanceSpec) error      { return nil }
func (e engine) RemoveInstance(context.Context, domain.InstanceSpec) error           { return nil }
func (e engine) CreateUser(context.Context, domain.InstanceSpec, string, string) error {
	return nil
}
func (e engine) ExecAs(context.Context, domain.InstanceSpec, string, string, string) error {
	return nil
}
func (e engine) AdminAccess(context.Context, domain.InstanceSpec) (domain.AdminAccess, error) {
	return domain.AdminAccess{}, nil
}
func (e engine) FormatDSN(context.Context, domain.InstanceSpec, string, string, string) (string, error) {
	return "", nil
}
func (e engine) Query(context.Context, domain.InstanceSpec, domain.QueryRequest) (domain.QueryResult, error) {
	return domain.QueryResult{}, nil
}
func (e engine) TailLog(context.Context, domain.InstanceSpec, int) (string, error) { return "", nil }
func (e engine) UpdateStats(context.Context, domain.InstanceSpec) error            { return nil }

func manifest() domain.RuntimeManifest {
	return domain.RuntimeManifest{
		Engine: "fakeengine", DisplayName: "Fake", Binary: "/usr/lib/db-branch/runtimes/fakeengine/db-branch-rt-fakeengine", Protocols: []domain.ManifestPort{{Name: "sql", Primary: true}},
		Capabilities: map[string]bool{"query": true, "update_stats": true},
	}
}

func good() engine {
	return engine{
		spec: manifest().Spec(),
		plan: domain.LaunchPlan{Command: "/usr/lib/db-branch/runtimes/fakeengine/server", ServiceType: "simple"},
	}
}

func byName(rs []conformance.Result) map[string]conformance.Result {
	m := map[string]conformance.Result{}
	for _, r := range rs {
		m[r.Name] = r
	}
	return m
}

func TestRun_契約を守るモジュールは全項目に合格する(t *testing.T) {
	rs := conformance.Run(context.Background(), manifest(), good(), domain.InstanceSpec{ID: "conformance"})
	if len(rs) == 0 {
		t.Fatal("no checks ran")
	}
	for _, r := range rs {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Name, r.Err)
		}
	}
	if !conformance.Passed(rs) {
		t.Fatal("Passed must be true")
	}
}

func TestRun_Describeがマニフェストと違えば不合格にする(t *testing.T) {
	e := good()
	e.spec.RunAsUser = "root"
	r := byName(conformance.Run(context.Background(), manifest(), e, domain.InstanceSpec{}))["manifest"]
	if !errors.Is(r.Err, domain.ErrInvalidManifest) || !strings.Contains(r.Err.Error(), "run_as_user") {
		t.Fatalf("%+v", r)
	}
}

func TestRun_偽と宣言した機能がErrFeatureNotSupportedを返さなければ不合格にする(t *testing.T) {
	// The manifest says update_stats is false, but the module does it.
	m := manifest()
	m.Capabilities = map[string]bool{"query": true}
	e := good()
	e.spec = m.Spec()
	rs := conformance.Run(context.Background(), m, e, domain.InstanceSpec{})
	r := byName(rs)["capability update_stats"]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "must return ErrFeatureNotSupported") {
		t.Fatalf("%+v", r)
	}
	if conformance.Passed(rs) {
		t.Fatal("Passed must be false")
	}
}

type noFreeze struct{ engine }

func (noFreeze) PreFreeze(context.Context, domain.InstanceSpec) (domain.FreezeToken, error) {
	return domain.FreezeToken{}, domain.FeatureNotSupported("live_freeze")
}

func TestRun_真と宣言した機能がErrFeatureNotSupportedを返せば不合格にする(t *testing.T) {
	m := manifest()
	m.Capabilities = map[string]bool{"query": true, "update_stats": true, "live_freeze": true}
	e := noFreeze{engine{spec: m.Spec(), plan: good().plan}}
	r := byName(conformance.Run(context.Background(), m, e, domain.InstanceSpec{}))["capability live_freeze"]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "declared") {
		t.Fatalf("%+v", r)
	}
}

func TestRun_起動計画が安全でなければ不合格にする(t *testing.T) {
	e := good()
	e.plan = domain.LaunchPlan{Command: "/bin/sh", Args: []string{"-c", "x\nExecStartPre=/bin/evil"}}
	r := byName(conformance.Run(context.Background(), manifest(), e, domain.InstanceSpec{}))["launch plan"]
	if !errors.Is(r.Err, domain.ErrInvalidLaunchPlan) {
		t.Fatalf("%+v", r)
	}
}

func TestRun_起動計画がモジュールのディレクトリの外を指せば不合格にする(t *testing.T) {
	e := good()
	e.plan.Command = "/usr/bin/curl"
	r := byName(conformance.Run(context.Background(), manifest(), e, domain.InstanceSpec{}))["launch plan"]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "outside") {
		t.Fatalf("%+v", r)
	}
}

type broken struct {
	engine
	describe, start error
}

func (b broken) Describe(context.Context) (domain.EngineSpec, error) {
	if b.describe != nil {
		return domain.EngineSpec{}, b.describe
	}
	return b.engine.Describe(context.Background())
}
func (b broken) PrepareStart(ctx context.Context, i domain.InstanceSpec) (domain.LaunchPlan, error) {
	if b.start != nil {
		return domain.LaunchPlan{}, b.start
	}
	return b.engine.PrepareStart(ctx, i)
}

func TestRun_DescribeやPrepareStartが失敗すれば不合格にする(t *testing.T) {
	rs := byName(conformance.Run(context.Background(), manifest(), broken{engine: good(), describe: errors.New("no answer")}, domain.InstanceSpec{}))
	if r := rs["manifest"]; r.Err == nil || !strings.Contains(r.Err.Error(), "no answer") {
		t.Fatalf("%+v", r)
	}
	rs = byName(conformance.Run(context.Background(), manifest(), broken{engine: good(), start: errors.New("no config")}, domain.InstanceSpec{}))
	if r := rs["launch plan"]; r.Err == nil || !strings.Contains(r.Err.Error(), "no config") {
		t.Fatalf("%+v", r)
	}
}

func TestRun_展開済みランタイムの下のコマンドは許す(t *testing.T) {
	e := good()
	e.plan.Command = "/var/lib/db-branch/runtimes/fakeengine/24.8/bin/server"
	inst := domain.InstanceSpec{RuntimeDir: "/var/lib/db-branch/runtimes/fakeengine/24.8"}
	if r := byName(conformance.Run(context.Background(), manifest(), e, inst))["launch plan"]; r.Err != nil {
		t.Fatalf("%+v", r)
	}
	// A sibling directory with the same prefix is not inside the runtime.
	e.plan.Command = "/var/lib/db-branch/runtimes/fakeengine/24.8-evil/server"
	if r := byName(conformance.Run(context.Background(), manifest(), e, inst))["launch plan"]; r.Err == nil {
		t.Fatal("a sibling directory must be refused")
	}
}

func TestRun_コアが行う機能は検査せず理由を示す(t *testing.T) {
	r := byName(conformance.Run(context.Background(), manifest(), good(), domain.InstanceSpec{}))["capability checkpoints"]
	if r.Err != nil || r.Note == "" {
		t.Fatalf("%+v", r)
	}
}
