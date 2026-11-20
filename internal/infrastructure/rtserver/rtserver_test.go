package rtserver_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/rtserver"
	"github.com/db-branch/db-branch/internal/infrastructure/rtunit"
)

// provider is a module that records what the core asks of it.
type provider struct {
	domain.UnsupportedFeatures
	mu       sync.Mutex
	calls    []string
	insts    []domain.InstanceSpec
	plan     domain.LaunchPlan
	health   []domain.HealthState // answers in turn; the last repeats
	verify   error
	startErr error
	users    [][2]string
	access   domain.AdminAccess
}

func (p *provider) note(name string, i domain.InstanceSpec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, name)
	p.insts = append(p.insts, i)
}
func (p *provider) Describe(context.Context) (domain.EngineSpec, error) {
	return domain.EngineSpec{}, nil
}
func (p *provider) Catalog(context.Context) ([]domain.CatalogEntry, error) { return nil, nil }
func (p *provider) PrepareRuntime(context.Context, string, string) error   { return nil }
func (p *provider) Initialize(_ context.Context, i domain.InstanceSpec) error {
	p.note("Initialize", i)
	return nil
}
func (p *provider) PrepareClone(_ context.Context, i domain.InstanceSpec) error {
	p.note("PrepareClone", i)
	return nil
}
func (p *provider) PrepareStart(_ context.Context, i domain.InstanceSpec) (domain.LaunchPlan, error) {
	p.note("PrepareStart", i)
	return p.plan, p.startErr
}
func (p *provider) Health(_ context.Context, i domain.InstanceSpec) (domain.HealthState, error) {
	p.note("Health", i)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.health) > 1 {
		h := p.health[0]
		p.health = p.health[1:]
		return h, nil
	}
	if len(p.health) == 1 {
		return p.health[0], nil
	}
	return domain.HealthReady, nil
}
func (p *provider) Quiesce(_ context.Context, i domain.InstanceSpec, m domain.StopMode) error {
	p.note("Quiesce", i)
	return nil
}
func (p *provider) VerifyCleanShutdown(_ context.Context, i domain.InstanceSpec) error {
	p.note("VerifyCleanShutdown", i)
	return p.verify
}
func (p *provider) RemoveInstance(_ context.Context, i domain.InstanceSpec) error {
	p.note("RemoveInstance", i)
	return nil
}
func (p *provider) CreateUser(_ context.Context, i domain.InstanceSpec, u, pw string) error {
	p.note("CreateUser", i)
	p.users = append(p.users, [2]string{u, pw})
	return nil
}
func (p *provider) ExecAs(_ context.Context, i domain.InstanceSpec, u, pw, sql string) error {
	p.note("ExecAs", i)
	p.users = append(p.users, [2]string{u, sql})
	return nil
}
func (p *provider) AdminAccess(_ context.Context, i domain.InstanceSpec) (domain.AdminAccess, error) {
	p.note("AdminAccess", i)
	return p.access, nil
}
func (p *provider) FormatDSN(context.Context, domain.InstanceSpec, string, string, string) (string, error) {
	return "", nil
}
func (p *provider) Query(context.Context, domain.InstanceSpec, domain.QueryRequest) (domain.QueryResult, error) {
	return domain.QueryResult{}, nil
}
func (p *provider) TailLog(context.Context, domain.InstanceSpec, int) (string, error) { return "", nil }
func (p *provider) Load(_ context.Context, i domain.InstanceSpec, path string) error {
	p.note("Load", i)
	p.users = append(p.users, [2]string{"load", path})
	return nil
}

// runner records commands and answers systemctl's questions from a script.
type runner struct {
	mu     sync.Mutex
	cmds   []string
	state  string // answer of "systemctl show --property=ActiveState"
	active string // answer of "systemctl is-active"
	fail   map[string]error
	log    string
}

func (r *runner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	r.cmds = append(r.cmds, line)
	for prefix, err := range r.fail {
		if strings.HasPrefix(line, prefix) {
			return nil, err
		}
	}
	switch {
	case strings.HasPrefix(line, "systemctl show --property=ActiveState"):
		return []byte(r.state + "\n"), nil
	case strings.HasPrefix(line, "systemctl is-active"):
		return []byte(r.active + "\n"), nil
	case strings.HasPrefix(line, "journalctl"):
		return []byte(r.log), nil
	}
	return nil, nil
}
func (r *runner) has(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
func (r *runner) index(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.cmds {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

type env struct {
	srv     *rtserver.Server
	prov    *provider
	run     *runner
	unitDir string
	chowned []string
}

const runtimeRoot = "/var/lib/db-branch/runtimes"

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{prov: &provider{plan: domain.LaunchPlan{Command: "/usr/lib/db-branch/runtimes/fakeengine/db-branch-rt-fakeengine", Args: []string{"--serve"}, ServiceType: "simple"}}, run: &runner{state: "active", active: "active"}, unitDir: t.TempDir()}
	e.srv = &rtserver.Server{
		Engine:    domain.EngineSpec{Engine: "fakeengine", Protocols: []domain.ProtocolPort{{Name: "sql", Primary: true}, {Name: "admin"}}},
		Provider:  e.prov,
		Run:       e.run,
		Units:     rtunit.Writer{Dir: e.unitDir},
		User:      "db-branch-rt-fakeengine",
		Chown:     func(user, path string) error { e.chowned = append(e.chowned, user+":"+path); return nil },
		StateRoot: rtunit.StateRoot, RuntimeRoot: runtimeRoot,
		Bind: "127.0.0.1", StartTimeout: time.Second, PollInterval: time.Millisecond,
	}
	return e
}

var server = domain.Server{
	ID: "branch-x", Runtime: domain.RuntimeRef{Engine: "fakeengine", Version: "1.2"},
	DataDir: "/tank/branches/x", Port: 15432, MemoryMB: 256, Extra: []string{"a=1"},
}

func TestStart_ドロップインを書いてsystemdで起動しReadyまで待つ(t *testing.T) {
	e := newEnv(t)
	e.prov.health = []domain.HealthState{domain.HealthStopped, domain.HealthStarting, domain.HealthReady}
	if err := e.srv.Start(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	dropin, err := os.ReadFile(filepath.Join(e.unitDir, "db-branch-rt@branch-x.service.d", "override.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"User=db-branch-rt-fakeengine", "/usr/lib/db-branch/runtimes/fakeengine/db-branch-rt-fakeengine", "\"--serve\""} {
		if !strings.Contains(string(dropin), want) {
			t.Errorf("drop-in lacks %q:\n%s", want, dropin)
		}
	}
	reload, start := e.run.index("systemctl daemon-reload"), e.run.index("systemctl start db-branch-rt@branch-x.service")
	if reload < 0 || start < 0 || reload > start {
		t.Fatalf("daemon-reload must precede start: %v", e.run.cmds)
	}
	var healths int
	for _, c := range e.prov.calls {
		if c == "Health" {
			healths++
		}
	}
	if healths != 3 {
		t.Fatalf("Health asked %d times, want 3 (until Ready)", healths)
	}
}

func TestStart_モジュール専用ユーザーに領域を渡してからPrepareStartを呼ぶ(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.Start(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"db-branch-rt-fakeengine:/tank/branches/x",
		"db-branch-rt-fakeengine:" + rtunit.StateRoot + "/branch-x",
		"db-branch-rt-fakeengine:" + rtunit.RunRoot + "/fakeengine/branch-x",
	}
	if strings.Join(e.chowned, " ") != strings.Join(want, " ") {
		t.Fatalf("chowned = %v", e.chowned)
	}
	i := e.prov.insts[0]
	if e.prov.calls[0] != "PrepareStart" || i.ID != "branch-x" || i.Runtime.Version != "1.2" || i.DataDir != "/tank/branches/x" ||
		i.StateDir != rtunit.StateRoot+"/branch-x" || i.RunDir != rtunit.RunRoot+"/fakeengine/branch-x" ||
		i.RuntimeDir != runtimeRoot+"/fakeengine/1.2" || i.Bind != "127.0.0.1" || i.Ports["sql"] != 15432 || len(i.Ports) != 1 ||
		i.Limits.MemoryMB != 256 || len(i.Extra) != 1 {
		t.Fatalf("instance = %+v", i)
	}
}

func TestStart_メンテナンス中はネットワークなしの指定を渡す(t *testing.T) {
	e := newEnv(t)
	s := server
	s.Maintenance = true
	_ = e.srv.Start(context.Background(), s)
	if !e.prov.insts[0].Maintenance {
		t.Fatal("Maintenance lost")
	}
}

func TestStart_起動計画が危険なら何も起動しない(t *testing.T) {
	e := newEnv(t)
	e.prov.plan = domain.LaunchPlan{Command: "/usr/lib/db-branch/runtimes/fakeengine/x", Args: []string{"a\nExecStartPre=/bin/evil"}}
	err := e.srv.Start(context.Background(), server)
	if err == nil {
		t.Fatal("an unsafe plan must be refused")
	}
	if e.run.has("systemctl start") {
		t.Fatal("systemctl start must not run")
	}
	if _, err := os.Stat(filepath.Join(e.unitDir, "db-branch-rt@branch-x.service.d")); err == nil {
		t.Fatal("no drop-in may be written")
	}
}

func TestStart_PrepareStartの失敗はそのまま返す(t *testing.T) {
	e := newEnv(t)
	e.prov.startErr = errors.New("no config")
	if err := e.srv.Start(context.Background(), server); err == nil || !strings.Contains(err.Error(), "no config") {
		t.Fatalf("err = %v", err)
	}
}

func TestStart_起動前にユニットが終了したら起動失敗として理由を返す(t *testing.T) {
	e := newEnv(t)
	e.prov.health = []domain.HealthState{domain.HealthStopped}
	e.run.state, e.run.log = "failed", "boom: cannot open data"
	err := e.srv.Start(context.Background(), server)
	if err == nil || !strings.Contains(err.Error(), "stopped while starting") || !strings.Contains(err.Error(), "cannot open data") {
		t.Fatalf("err = %v", err)
	}
}

func TestStart_時間内にReadyにならなければ失敗する(t *testing.T) {
	e := newEnv(t)
	e.srv.StartTimeout = 20 * time.Millisecond
	e.prov.health = []domain.HealthState{domain.HealthStarting}
	err := e.srv.Start(context.Background(), server)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err = %v", err)
	}
}

func TestStart_systemctlの失敗は理由つきで返す(t *testing.T) {
	e := newEnv(t)
	e.run.fail = map[string]error{"systemctl start": errors.New("exit status 5")}
	e.run.log = "unit not found"
	err := e.srv.Start(context.Background(), server)
	if err == nil || !strings.Contains(err.Error(), "unit not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestStop_通常停止はQuiesceして止めて正常終了を確かめる(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.Stop(context.Background(), server, domain.StopClean); err != nil {
		t.Fatal(err)
	}
	want := []string{"Quiesce", "VerifyCleanShutdown"}
	var got []string
	for _, c := range e.prov.calls {
		got = append(got, c)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") || !e.run.has("systemctl stop db-branch-rt@branch-x.service") {
		t.Fatalf("calls %v, commands %v", got, e.run.cmds)
	}
	if e.run.index("systemctl stop") < 0 {
		t.Fatal("no stop")
	}
}

func TestStop_正常終了でなければErrUncleanShutdownを返す(t *testing.T) {
	e := newEnv(t)
	e.prov.verify = domain.ErrUncleanShutdown
	err := e.srv.Stop(context.Background(), server, domain.StopClean)
	if !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Fatalf("err = %v", err)
	}
}

func TestStop_動いていなければ何もしない(t *testing.T) {
	e := newEnv(t)
	e.run.active = "inactive"
	if err := e.srv.Stop(context.Background(), server, domain.StopClean); err != nil {
		t.Fatal(err)
	}
	if len(e.prov.calls) != 0 || e.run.has("systemctl stop") {
		t.Fatalf("calls %v %v", e.prov.calls, e.run.cmds)
	}
}

func TestStop_即時停止はKILLしてユニットを片付ける(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.Stop(context.Background(), server, domain.StopImmediate); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"systemctl kill --signal=SIGKILL db-branch-rt@branch-x.service", "systemctl stop db-branch-rt@branch-x.service", "systemctl reset-failed db-branch-rt@branch-x.service"} {
		if !e.run.has(c) {
			t.Errorf("missing %q in %v", c, e.run.cmds)
		}
	}
	if len(e.prov.calls) != 0 {
		t.Fatalf("an immediate stop must not call the module: %v", e.prov.calls)
	}
}

func TestStop_停止コマンドの失敗は返す(t *testing.T) {
	e := newEnv(t)
	e.run.fail = map[string]error{"systemctl stop": errors.New("busy")}
	if err := e.srv.Stop(context.Background(), server, domain.StopClean); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunning_ユニットがactiveかどうかを返す(t *testing.T) {
	e := newEnv(t)
	if ok, _ := e.srv.Running(context.Background(), server); !ok {
		t.Fatal("active must be running")
	}
	e.run.active = "failed"
	if ok, _ := e.srv.Running(context.Background(), server); ok {
		t.Fatal("failed must not be running")
	}
}

func TestInitialize_領域を渡してモジュールに初期化させる(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.Initialize(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if len(e.chowned) != 3 || e.prov.calls[0] != "Initialize" || e.prov.insts[0].DataDir != "/tank/branches/x" {
		t.Fatalf("%v %v", e.chowned, e.prov.calls)
	}
}

func TestPrepareClone_領域を渡してモジュールに委ねる(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.PrepareClone(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if len(e.chowned) != 3 || e.prov.calls[0] != "PrepareClone" {
		t.Fatalf("%v %v", e.chowned, e.prov.calls)
	}
}

func TestユーザーとSQL_モジュールに委ねる(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.srv.CreateUser(ctx, server, "u", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.ExecAs(ctx, server, "u", "pw", "select 1"); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Load(ctx, server, "/dump.sql"); err != nil {
		t.Fatal(err)
	}
	if len(e.prov.users) != 3 || e.prov.users[0] != [2]string{"u", "pw"} || e.prov.users[1] != [2]string{"u", "select 1"} || e.prov.users[2] != [2]string{"load", "/dump.sql"} {
		t.Fatalf("%v", e.prov.users)
	}
}

func TestExec_管理者としてのSQLは外部エンジンでは使えない(t *testing.T) {
	e := newEnv(t)
	if err := e.srv.Exec(context.Background(), server, "drop table x"); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestRemove_ユニットとドロップインと設定を片付ける(t *testing.T) {
	e := newEnv(t)
	e.run.active = "active"
	if _, err := e.srv.Units.Write(unitFor(e)); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Remove(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.unitDir, "db-branch-rt@branch-x.service.d")); err == nil {
		t.Fatal("drop-in must be removed")
	}
	if !e.run.has("systemctl stop db-branch-rt@branch-x.service") || !e.run.has("systemctl reset-failed") || e.prov.calls[len(e.prov.calls)-1] != "RemoveInstance" {
		t.Fatalf("%v %v", e.run.cmds, e.prov.calls)
	}
}

func unitFor(e *env) rtunit.Unit {
	return rtunit.Unit{
		ID: "branch-x", Engine: "fakeengine", User: "db-branch-rt-fakeengine", Plan: e.prov.plan, DataDir: "/tank/branches/x",
		StateDir: rtunit.StateRoot + "/branch-x", RunDir: rtunit.RunRoot + "/fakeengine/branch-x",
	}
}

func TestAdmin_モジュールの接続情報を返す(t *testing.T) {
	e := newEnv(t)
	e.prov.access = domain.AdminAccess{Engine: "fakeengine", Port: 15432, User: "admin"}
	if got := e.srv.Admin(server); got.Port != 15432 || got.User != "admin" {
		t.Fatalf("%+v", got)
	}
	e2 := newEnv(t)
	e2.srv.Provider = failingAccess{e2.prov}
	if got := e2.srv.Admin(server); got.Engine != "" {
		t.Fatalf("an unreachable module must give an empty access: %+v", got)
	}
}

type failingAccess struct{ *provider }

func (failingAccess) AdminAccess(context.Context, domain.InstanceSpec) (domain.AdminAccess, error) {
	return domain.AdminAccess{Engine: "x"}, errors.New("down")
}

func TestChownに失敗したら何も始めない(t *testing.T) {
	e := newEnv(t)
	e.srv.Chown = func(string, string) error { return errors.New("operation not permitted") }
	ctx := context.Background()
	for name, err := range map[string]error{
		"Initialize":   e.srv.Initialize(ctx, server),
		"PrepareClone": e.srv.PrepareClone(ctx, server),
		"Start":        e.srv.Start(ctx, server),
	} {
		if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(e.prov.calls) != 0 {
		t.Fatalf("the module must not be called: %v", e.prov.calls)
	}
}

func TestChownが未設定でも動く(t *testing.T) {
	e := newEnv(t)
	e.srv.Chown = nil
	if err := e.srv.Initialize(context.Background(), server); err != nil {
		t.Fatal(err)
	}
}

func TestStart_daemon_reloadの失敗は返す(t *testing.T) {
	e := newEnv(t)
	e.run.fail = map[string]error{"systemctl daemon-reload": errors.New("bus down")}
	if err := e.srv.Start(context.Background(), server); err == nil || !strings.Contains(err.Error(), "bus down") {
		t.Fatalf("err = %v", err)
	}
}

type quiesceFails struct{ *provider }

func (quiesceFails) Quiesce(context.Context, domain.InstanceSpec, domain.StopMode) error {
	return errors.New("flush failed")
}

func TestStop_Quiesceに失敗したらユニットを止めずに返す(t *testing.T) {
	e := newEnv(t)
	e.srv.Provider = quiesceFails{e.prov}
	if err := e.srv.Stop(context.Background(), server, domain.StopClean); err == nil || !strings.Contains(err.Error(), "flush failed") {
		t.Fatalf("err = %v", err)
	}
	if e.run.has("systemctl stop") {
		t.Fatal("the unit must keep running when it could not be readied")
	}
}

func TestRemove_不正なIDは拒否する(t *testing.T) {
	e := newEnv(t)
	s := server
	s.ID = "../evil"
	if err := e.srv.Remove(context.Background(), s); err == nil {
		t.Fatal("an invalid id must be refused")
	}
}

func TestStart_状態とRunのディレクトリを作ってから専用ユーザーに渡す(t *testing.T) {
	e := newEnv(t)
	var order []string
	e.srv.Mkdir = func(path string) error { order = append(order, "mkdir "+path); return nil }
	e.srv.Chown = func(user, path string) error { order = append(order, "chown "+path); return nil }
	if err := e.srv.Start(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	state, run := rtunit.StateRoot+"/branch-x", rtunit.RunRoot+"/fakeengine/branch-x"
	want := []string{"chown /tank/branches/x", "mkdir " + state, "chown " + state, "mkdir " + run, "chown " + run}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v", order)
	}
}

func TestStart_ディレクトリを作れなければ何も始めない(t *testing.T) {
	e := newEnv(t)
	e.srv.Mkdir = func(string) error { return errors.New("read-only file system") }
	if err := e.srv.Start(context.Background(), server); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v", err)
	}
	if len(e.prov.calls) != 0 {
		t.Fatalf("the module must not be called: %v", e.prov.calls)
	}
}

func TestInstance_モジュールに伝えるインスタンスを公開する(t *testing.T) {
	e := newEnv(t)
	i := e.srv.Instance(server)
	if i.ID != "branch-x" || i.Ports["sql"] != 15432 || i.StateDir != rtunit.StateRoot+"/branch-x" {
		t.Fatalf("%+v", i)
	}
}
