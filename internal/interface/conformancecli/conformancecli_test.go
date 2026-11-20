package conformancecli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/conformancecli"
	"github.com/db-branch/db-branch/internal/usecase/conformance"
)

const manifestJSON = `{
  "schema_version": 1, "engine": "fakeengine", "display_name": "Fake", "module_version": "0.1.0", "api_version": 1,
  "binary": "%ROOT%/fakeengine/db-branch-rt-fakeengine", "binary_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "catalog": "%ROOT%/fakeengine/runtimes.json", "catalog_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "protocols": [ { "name": "sql", "primary": true } ],
  "capabilities": { "query": true }
}`

// module answers like a well-behaved module with the capability Query only.
type module struct {
	spec    domain.EngineSpec
	command string
	insts   *[]domain.InstanceSpec
}

func (m module) Describe(context.Context) (domain.EngineSpec, error) { return m.spec, nil }
func (m module) PrepareStart(_ context.Context, i domain.InstanceSpec) (domain.LaunchPlan, error) {
	*m.insts = append(*m.insts, i)
	return domain.LaunchPlan{Command: m.command, ServiceType: "simple"}, nil
}
func (module) Query(context.Context, domain.InstanceSpec, domain.QueryRequest) (domain.QueryResult, error) {
	return domain.QueryResult{}, nil
}
func (module) PreFreeze(context.Context, domain.InstanceSpec) (domain.FreezeToken, error) {
	return domain.FreezeToken{}, domain.FeatureNotSupported("live_freeze")
}
func (module) CheckUpgradePath(context.Context, string, string) error {
	return domain.FeatureNotSupported("upgrade")
}
func (module) DumpSource(context.Context, domain.SourceDumpJob, string) error {
	return domain.FeatureNotSupported("source_dump")
}
func (module) UpdateStats(context.Context, domain.InstanceSpec) error {
	return domain.FeatureNotSupported("update_stats")
}
func (module) InstanceMetrics(context.Context, domain.InstanceSpec) ([]domain.MetricSample, error) {
	return nil, domain.FeatureNotSupported("instance_metrics")
}

// setup writes the manifest under a fake runtimes root and returns its path and the root.
func setup(t *testing.T) (manifestPath, root string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "fakeengine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath = filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(strings.ReplaceAll(manifestJSON, "%ROOT%", root)), 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestPath, root
}

type startCall struct {
	engine  string
	current bool
}

func run(t *testing.T, args []string, start conformancecli.StartFunc) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = conformancecli.Main(args, &o, &e, start, nil, nil)
	return code, o.String(), e.String()
}

func starter(m module, calls *[]startCall) conformancecli.StartFunc {
	return func(mf domain.RuntimeManifest, asCurrent bool) (conformance.Provider, func(), error) {
		*calls = append(*calls, startCall{mf.Engine, asCurrent})
		m.spec = mf.Spec()
		return m, func() { *calls = append(*calls, startCall{"stopped", false}) }, nil
	}
}

func TestMain_契約を守るモジュールは合格して終了コード0(t *testing.T) {
	path, root := setup(t)
	var calls []startCall
	var insts []domain.InstanceSpec
	m := module{command: root + "/fakeengine/db-branch-rt-fakeengine", insts: &insts}
	code, out, errOut := run(t, []string{"--manifest", path, "--root", root}, starter(m, &calls))
	if code != 0 {
		t.Fatalf("code %d\nout: %s\nerr: %s", code, out, errOut)
	}
	for _, want := range []string{"PASS manifest", "PASS capability query", "PASS launch plan", "SKIP capability checkpoints", "all checks passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(calls) != 2 || calls[0] != (startCall{"fakeengine", false}) || calls[1].engine != "stopped" {
		t.Fatalf("calls = %+v (the module must be started once and stopped)", calls)
	}
	if len(insts) != 1 || insts[0].StateDir == "" || insts[0].DataDir == "" || insts[0].RunDir == "" {
		t.Fatalf("the checks need directories to work in: %+v", insts)
	}
	if insts[0].RuntimeDir != "/var/lib/db-branch/runtimes/fakeengine/0.0" {
		t.Fatalf("the runtime lives where the node installs it: %q", insts[0].RuntimeDir)
	}
	if insts[0].Ports["sql"] <= 0 {
		t.Fatalf("every protocol of the manifest needs a port: %+v", insts[0].Ports)
	}
	if _, err := os.Stat(insts[0].DataDir); err == nil {
		t.Fatal("the working directories must be removed afterwards")
	}
}

func TestMain_違反があれば不合格を示して終了コード1(t *testing.T) {
	path, root := setup(t)
	var calls []startCall
	var insts []domain.InstanceSpec
	m := module{command: "/usr/bin/curl", insts: &insts}
	code, out, _ := run(t, []string{"--manifest", path, "--root", root}, starter(m, &calls))
	if code != 1 || !strings.Contains(out, "FAIL launch plan") || strings.Contains(out, "all checks passed") {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestMain_現在のユーザーで動かす開発用の指定を渡せる(t *testing.T) {
	path, root := setup(t)
	var calls []startCall
	var insts []domain.InstanceSpec
	m := module{command: root + "/fakeengine/x", insts: &insts}
	run(t, []string{"--manifest", path, "--root", root, "--as-current-user"}, starter(m, &calls))
	if !calls[0].current {
		t.Fatal("--as-current-user was lost")
	}
}

func TestMain_引数と入力の誤りは終了コード2(t *testing.T) {
	path, root := setup(t)
	never := func(domain.RuntimeManifest, bool) (conformance.Provider, func(), error) {
		t.Fatal("must not start the module")
		return nil, nil, nil
	}
	if code, _, e := run(t, nil, never); code != 2 || !strings.Contains(e, "--manifest") {
		t.Fatalf("no manifest: %d %q", code, e)
	}
	if code, _, e := run(t, []string{"--bogus"}, never); code != 2 || e == "" {
		t.Fatalf("bad flag: %d %q", code, e)
	}
	if code, _, e := run(t, []string{"--manifest", filepath.Join(root, "missing.json"), "--root", root}, never); code != 2 || e == "" {
		t.Fatalf("missing file: %d %q", code, e)
	}
	bad := filepath.Join(root, "fakeengine", "bad.json")
	_ = os.WriteFile(bad, []byte(`{"engine":"x"}`), 0o644)
	if code, _, e := run(t, []string{"--manifest", bad, "--root", root}, never); code != 2 || !strings.Contains(e, "manifest") {
		t.Fatalf("bad manifest: %d %q", code, e)
	}
	_ = path
}

func TestMain_モジュールを起動できなければ終了コード2(t *testing.T) {
	path, root := setup(t)
	fail := func(domain.RuntimeManifest, bool) (conformance.Provider, func(), error) {
		return nil, nil, errors.New("exec format error")
	}
	code, _, e := run(t, []string{"--manifest", path, "--root", root}, fail)
	if code != 2 || !strings.Contains(e, "exec format error") {
		t.Fatalf("%d %q", code, e)
	}
}

func TestMain_作業ディレクトリを作れなければ終了コード2(t *testing.T) {
	path, root := setup(t)
	t.Setenv("TMPDIR", filepath.Join(root, "no-such-dir"))
	never := func(domain.RuntimeManifest, bool) (conformance.Provider, func(), error) {
		t.Fatal("must not start the module")
		return nil, nil, nil
	}
	if code, _, e := run(t, []string{"--manifest", path, "--root", root}, never); code != 2 || e == "" {
		t.Fatalf("%d %q", code, e)
	}
}

func TestMain_専用ユーザーで動かすときは作業ディレクトリをそのユーザーに渡してから起動する(t *testing.T) {
	path, root := setup(t)
	var order []string
	var insts []domain.InstanceSpec
	m := module{command: root + "/fakeengine/x", insts: &insts}
	start := func(mf domain.RuntimeManifest, asCurrent bool) (conformance.Provider, func(), error) {
		order = append(order, "start")
		m.spec = mf.Spec()
		return m, func() {}, nil
	}
	own := func(mf domain.RuntimeManifest, paths ...string) error {
		order = append(order, "own "+mf.Engine+" "+strings.Join(paths, ","))
		return nil
	}
	var o, e bytes.Buffer
	conformancecli.Main([]string{"--manifest", path, "--root", root}, &o, &e, start, own, nil)
	if len(order) != 2 || !strings.HasPrefix(order[0], "own fakeengine ") || order[1] != "start" {
		t.Fatalf("order = %v", order)
	}
	for _, d := range []string{insts[0].DataDir, insts[0].StateDir, insts[0].RunDir} {
		if !strings.Contains(order[0], d) {
			t.Errorf("%s not handed over: %s", d, order[0])
		}
	}
}

func TestMain_現在のユーザーで動かすときは所有者を変えない(t *testing.T) {
	path, root := setup(t)
	var insts []domain.InstanceSpec
	m := module{command: root + "/fakeengine/x", insts: &insts}
	own := func(domain.RuntimeManifest, ...string) error { t.Fatal("must not hand over directories"); return nil }
	var calls []startCall
	var o, e bytes.Buffer
	conformancecli.Main([]string{"--manifest", path, "--root", root, "--as-current-user"}, &o, &e, starter(m, &calls), own, nil)
}

func TestMain_ディレクトリを渡せなければ起動せず終了コード2(t *testing.T) {
	path, root := setup(t)
	never := func(domain.RuntimeManifest, bool) (conformance.Provider, func(), error) {
		t.Fatal("must not start the module")
		return nil, nil, nil
	}
	own := func(domain.RuntimeManifest, ...string) error { return errors.New("no such user") }
	var o, e bytes.Buffer
	code := conformancecli.Main([]string{"--manifest", path, "--root", root}, &o, &e, never, own, nil)
	if code != 2 || !strings.Contains(e.String(), "no such user") {
		t.Fatalf("%d %q", code, e.String())
	}
}

// dbServer is a DBServer that works (or fails at failOn).
type dbServer struct {
	running bool
	failOn  string
	removed *bool
}

func (s *dbServer) fail(n string) error {
	if s.failOn == n {
		return errors.New(n + " failed")
	}
	return nil
}
func (s *dbServer) Initialize(context.Context, domain.Server) error   { return s.fail("Initialize") }
func (s *dbServer) PrepareClone(context.Context, domain.Server) error { return nil }
func (s *dbServer) Start(context.Context, domain.Server) error {
	s.running = true
	return s.fail("Start")
}
func (s *dbServer) Stop(context.Context, domain.Server, domain.StopMode) error {
	s.running = false
	return nil
}
func (s *dbServer) Running(context.Context, domain.Server) (bool, error) { return s.running, nil }
func (s *dbServer) Exec(context.Context, domain.Server, string) error    { return nil }
func (s *dbServer) ExecAs(context.Context, domain.Server, string, string, string) error {
	return nil
}
func (s *dbServer) CreateUser(context.Context, domain.Server, string, string) error { return nil }
func (s *dbServer) Load(context.Context, domain.Server, string) error               { return nil }
func (s *dbServer) Remove(context.Context, domain.Server) error {
	*s.removed = true
	return nil
}
func (s *dbServer) Admin(domain.Server) domain.AdminAccess { return domain.AdminAccess{} }

func lifecycleFor(srv *dbServer, cleaned *bool) conformancecli.LifecycleFunc {
	return func(domain.RuntimeManifest, conformance.Provider) (domain.DBServer, domain.Server, func(), error) {
		return srv, domain.Server{ID: "conformance"}, func() { *cleaned = true }, nil
	}
}

func TestMain_lifecycle指定でライフサイクルの検査も行う(t *testing.T) {
	path, root := setup(t)
	var calls []startCall
	var insts []domain.InstanceSpec
	m := module{command: root + "/fakeengine/x", insts: &insts}
	var removed, cleaned bool
	var o, e bytes.Buffer
	code := conformancecli.Main([]string{"--manifest", path, "--root", root, "--lifecycle"}, &o, &e, starter(m, &calls), nil, lifecycleFor(&dbServer{removed: &removed}, &cleaned))
	if code != 0 || !strings.Contains(o.String(), "PASS lifecycle start") || !strings.Contains(o.String(), "PASS lifecycle crash recovery") {
		t.Fatalf("code %d\n%s\n%s", code, o.String(), e.String())
	}
	if !removed || !cleaned {
		t.Fatalf("removed=%v cleaned=%v", removed, cleaned)
	}
}

func TestMain_lifecycleの失敗は終了コード1(t *testing.T) {
	path, root := setup(t)
	var calls []startCall
	var insts []domain.InstanceSpec
	m := module{command: root + "/fakeengine/x", insts: &insts}
	var removed, cleaned bool
	var o, e bytes.Buffer
	code := conformancecli.Main([]string{"--manifest", path, "--root", root, "--lifecycle"}, &o, &e, starter(m, &calls), nil, lifecycleFor(&dbServer{failOn: "Start", removed: &removed}, &cleaned))
	if code != 1 || !strings.Contains(o.String(), "FAIL lifecycle start") || !cleaned {
		t.Fatalf("code %d cleaned=%v\n%s", code, cleaned, o.String())
	}
}

func TestMain_lifecycleを作れなければ終了コード2(t *testing.T) {
	path, root := setup(t)
	var calls []startCall
	var insts []domain.InstanceSpec
	m := module{command: root + "/fakeengine/x", insts: &insts}
	var o, e bytes.Buffer
	if code := conformancecli.Main([]string{"--manifest", path, "--root", root, "--lifecycle"}, &o, &e, starter(m, &calls), nil, nil); code != 2 || !strings.Contains(e.String(), "lifecycle") {
		t.Fatalf("no support: %d %q", code, e.String())
	}
	broken := func(domain.RuntimeManifest, conformance.Provider) (domain.DBServer, domain.Server, func(), error) {
		return nil, domain.Server{}, nil, errors.New("systemd is not running")
	}
	e.Reset()
	if code := conformancecli.Main([]string{"--manifest", path, "--root", root, "--lifecycle"}, &o, &e, starter(m, &calls), nil, broken); code != 2 || !strings.Contains(e.String(), "systemd is not running") {
		t.Fatalf("broken: %d %q", code, e.String())
	}
}

func TestMain_作業の準備に失敗したら起動せず終了コード2(t *testing.T) {
	path, root := setup(t)
	never := func(domain.RuntimeManifest, bool) (conformance.Provider, func(), error) {
		t.Fatal("must not start the module")
		return nil, nil, nil
	}
	for name, fail := range map[string]func(error) func(){
		"port":  conformancecli.FailFreePort,
		"mkdir": conformancecli.FailMkdirAll,
	} {
		restore := fail(errors.New(name + " exhausted"))
		code, _, e := run(t, []string{"--manifest", path, "--root", root}, never)
		restore()
		if code != 2 || !strings.Contains(e, name+" exhausted") {
			t.Errorf("%s: %d %q", name, code, e)
		}
	}
}
