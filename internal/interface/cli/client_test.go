package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

// fakeAPI answers requests from a table keyed by "METHOD path".
type fakeAPI struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]any
	answers  map[string]any
	errors   map[string]error
	// opStates is returned in order for GET /v1/operations/{id}.
	opStates []map[string]any
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{bodies: map[string]any{}, answers: map[string]any{}, errors: map[string]error{}}
}

func (f *fakeAPI) Do(_ context.Context, method, path string, in, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + path
	f.requests = append(f.requests, key)
	if in != nil {
		f.bodies[key] = in
	}
	if err := f.errors[key]; err != nil {
		return err
	}
	ans, ok := f.answers[key]
	if strings.HasPrefix(path, "/v1/operations/") && len(f.opStates) > 0 {
		ans, ok = f.opStates[0], true
		if len(f.opStates) > 1 {
			f.opStates = f.opStates[1:]
		}
	}
	if !ok {
		return &cli.APIError{Status: 404, Code: "not_found", Message: key + " not stubbed"}
	}
	if out != nil {
		b, _ := json.Marshal(ans)
		return json.Unmarshal(b, out)
	}
	return nil
}

func runAPI(t *testing.T, api *fakeAPI, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := cli.App{
		Version: "test", Stdout: &out, Stderr: &errOut,
		Client: func(string) cli.Client { return api },
	}
	args = append([]string{"--config", filepath.Join(t.TempDir(), "none.toml")}, args...)
	code := app.Run(context.Background(), args)
	return code, out.String(), errOut.String()
}

func accepted(id, kind string) map[string]any {
	return map[string]any{"id": id, "kind": kind, "state": "running"}
}

func TestBranchCreate_作成を依頼して完了まで段階を表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches"] = accepted("op_1", "branch.create")
	api.opStates = []map[string]any{
		{"id": "op_1", "state": "running", "phase": "cloning"},
		{"id": "op_1", "state": "running", "phase": "starting"},
		{"id": "op_1", "state": "succeeded"},
	}
	api.answers["GET /v1/branches/feature-x"] = map[string]any{"name": "feature-x", "status": "ready",
		"connection": map[string]any{"host": "192.0.2.10", "port": 40001, "user": "branch"}}

	code, out, errOut := runAPI(t, api, "branch", "create", "feature-x", "--base", "prod", "--ttl", "72h", "--init-sql", "SELECT 1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	body := api.bodies["POST /v1/branches"].(map[string]any)
	if body["name"] != "feature-x" || body["base"] != "prod" || body["ttl"] != "72h" || body["init_sql"] != "SELECT 1" {
		t.Errorf("body = %v", body)
	}
	for _, want := range []string{"cloning", "starting", "feature-x", "ready", "192.0.2.10:40001"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("output lacks %q:\n%s%s", want, out, errOut)
		}
	}
}

func TestBranchCreate_init_sqlをファイルから読める(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches"] = accepted("op_1", "branch.create")
	api.opStates = []map[string]any{{"id": "op_1", "state": "succeeded"}}
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "ready"}
	file := filepath.Join(t.TempDir(), "init.sql")
	writeFile(t, file, "UPDATE users SET email = NULL;")

	if code, _, errOut := runAPI(t, api, "branch", "create", "x", "--base", "prod", "--init-file", file); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if api.bodies["POST /v1/branches"].(map[string]any)["init_sql"] != "UPDATE users SET email = NULL;" {
		t.Errorf("body = %v", api.bodies)
	}
	if code, _, _ := runAPI(t, api, "branch", "create", "y", "--base", "prod", "--init-file", "/nope"); code == 0 {
		t.Error("missing init file should fail")
	}
}

func TestBranchCreate_所有者と公開範囲を指定できる(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches"] = accepted("op_1", "branch.create")
	api.opStates = []map[string]any{{"id": "op_1", "state": "succeeded"}}
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "ready", "owner": "alice", "id": "alice/x", "access": "open"}

	code, out, errOut := runAPI(t, api, "branch", "create", "x", "--base", "prod", "--owner", "alice", "--access", "open")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	body := api.bodies["POST /v1/branches"].(map[string]any)
	if body["owner"] != "alice" || body["access"] != "open" {
		t.Errorf("body = %v", body)
	}
	for _, want := range []string{"alice/x", "open"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code, _, _ := runAPI(t, api, "branch", "create", "y", "--base", "prod", "--access", "public"); code == 0 {
		t.Error("an unknown access should fail")
	}
}

func Test待たない指定ならオペレーションIDだけ表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/reset"] = accepted("op_9", "branch.reset")

	code, out, _ := runAPI(t, api, "branch", "reset", "x", "--no-wait")
	if code != 0 || strings.TrimSpace(out) != "op_9" {
		t.Errorf("exit %d out %q", code, out)
	}
}

func Testオペレーションが失敗したら理由を表示して失敗する(t *testing.T) {
	api := newFakeAPI()
	api.answers["DELETE /v1/branches/x"] = accepted("op_2", "branch.delete")
	api.opStates = []map[string]any{{"id": "op_2", "state": "failed", "error": "zfs destroy: busy"}}

	code, _, errOut := runAPI(t, api, "branch", "delete", "x")
	if code == 0 || !strings.Contains(errOut, "zfs destroy: busy") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestBranchList_表で表示しJSONも出せる(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{
		map[string]any{"name": "feature-x", "status": "ready", "base": "prod", "runtime": "mysql@8.4.13",
			"expires_at": "2026-11-12T12:00:00Z", "connection": map[string]any{"host": "h", "port": 40001},
			"storage": map[string]any{"used": 1048576}},
	}}

	code, out, _ := runAPI(t, api, "branch", "list")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"NAME", "STATUS", "feature-x", "ready", "prod", "mysql@8.4.13", "40001", "1.0 MiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = runAPI(t, api, "branch", "list", "-o", "json")
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || parsed[0]["name"] != "feature-x" {
		t.Errorf("json = %q (%v)", out, err)
	}
}

func TestBranchShowとDSNとExtend(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "ready", "base": "prod"}
	api.answers["GET /v1/branches/x/dsn"] = map[string]any{"dsn": "mysql://branch:pw@h:40001/"}
	api.answers["PATCH /v1/branches/x"] = map[string]any{"name": "x", "expires_at": "2026-11-10T12:00:00Z"}

	if _, out, _ := runAPI(t, api, "branch", "show", "x"); !strings.Contains(out, "prod") {
		t.Errorf("show = %q", out)
	}
	if _, out, _ := runAPI(t, api, "branch", "dsn", "x"); strings.TrimSpace(out) != "mysql://branch:pw@h:40001/" {
		t.Errorf("dsn = %q", out)
	}
	code, out, _ := runAPI(t, api, "branch", "extend", "x", "24h")
	if code != 0 || !strings.Contains(out, "2026-11-10T12:00:00Z") || api.bodies["PATCH /v1/branches/x"].(map[string]any)["ttl"] != "24h" {
		t.Errorf("extend: %d %q %v", code, out, api.bodies)
	}
}

func TestBaseコマンド(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/bases"] = accepted("op_1", "base.create")
	api.answers["POST /v1/bases/prod/snapshots"] = accepted("op_2", "base.snapshot")
	api.opStates = []map[string]any{{"id": "op", "state": "succeeded"}}
	api.answers["GET /v1/bases/prod"] = map[string]any{"name": "prod", "status": "ready", "latest_snapshot": "s2"}
	api.answers["GET /v1/bases"] = map[string]any{"items": []any{map[string]any{"name": "prod", "runtime": "mysql@8.4.13", "status": "ready", "latest_snapshot": "s2"}}}
	api.answers["POST /v1/bases/prod/start"] = nil
	api.answers["POST /v1/bases/prod/stop"] = nil
	api.answers["DELETE /v1/bases/prod"] = nil

	steps := [][]string{
		{"base", "create", "prod", "--runtime", "mysql@8.4.13", "--dump", "/srv/prod.sql.gz"},
		{"base", "snapshot", "prod"},
		{"base", "list"},
		{"base", "show", "prod"},
		{"base", "start", "prod"},
		{"base", "stop", "prod"},
		{"base", "delete", "prod"},
	}
	for _, args := range steps {
		if code, _, errOut := runAPI(t, api, args...); code != 0 {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	body := api.bodies["POST /v1/bases"].(map[string]any)
	if body["runtime"] != "mysql@8.4.13" || body["dump"] != "/srv/prod.sql.gz" {
		t.Errorf("body = %v", body)
	}
}

func TestRuntimeコマンドはAPI経由(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/runtimes"] = map[string]any{"items": []any{map[string]any{"runtime": "mysql@8.4.13"}}}
	api.answers["GET /v1/runtimes/available"] = map[string]any{"items": []any{
		map[string]any{"runtime": "mysql@8.4.13", "engine": "mysql"}, map[string]any{"runtime": "postgres@17.2", "engine": "postgres"}}}
	api.answers["POST /v1/runtimes"] = accepted("op_1", "runtime.install")
	api.opStates = []map[string]any{{"id": "op_1", "state": "succeeded"}}
	api.answers["DELETE /v1/runtimes/mysql/8.4.13"] = nil

	if _, out, _ := runAPI(t, api, "runtime", "list"); out != "mysql@8.4.13\n" {
		t.Errorf("list = %q", out)
	}
	if _, out, _ := runAPI(t, api, "runtime", "available", "postgres"); out != "postgres@17.2\n" {
		t.Errorf("available = %q", out)
	}
	if code, _, _ := runAPI(t, api, "runtime", "install", "mysql@8.4.13"); code != 0 {
		t.Error("install failed")
	}
	if code, _, _ := runAPI(t, api, "runtime", "remove", "mysql@8.4.13"); code != 0 {
		t.Error("remove failed")
	}
	if code, _, errOut := runAPI(t, api, "runtime", "install", "mysql-8"); code == 0 || !strings.Contains(errOut, "engine@version") {
		t.Errorf("bad ref: %d %q", code, errOut)
	}
}

func TestAPIのエラーはコードと説明を表示する(t *testing.T) {
	api := newFakeAPI()
	api.errors["GET /v1/branches/x"] = &cli.APIError{Status: 404, Code: "not_found", Message: "branch x: not found"}

	code, _, errOut := runAPI(t, api, "branch", "show", "x")
	if code == 0 || !strings.Contains(errOut, "branch x: not found") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
	if e := (&cli.APIError{Status: 503, Code: "not_initialized", Message: "run init"}); !strings.Contains(e.Error(), "run init") {
		t.Error(e.Error())
	}
}

func Test名前の誤りはAPIを呼ぶ前に気づく(t *testing.T) {
	api := newFakeAPI()
	for _, args := range [][]string{{"branch", "show", "Bad"}, {"branch", "create", "Bad", "--base", "prod"}, {"base", "show", "Bad"}} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("%v should fail", args)
		}
	}
	if len(api.requests) != 0 {
		t.Errorf("requests = %v", api.requests)
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 512: "512 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := cli.HumanBytes(in); got != want {
			t.Errorf("%d: %q, want %q", in, got, want)
		}
	}
	_ = fmt.Sprint()
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInit_プールを指定して初期化し初期パスワードを表示する(t *testing.T) {
	var gotPool string
	var out, errOut bytes.Buffer
	app := cli.App{
		Stdout: &out, Stderr: &errOut,
		Init: func(_ context.Context, _ config.Config, pool string) (string, error) {
			gotPool = pool
			return "initial-secret", nil
		},
	}
	code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "init", "--pool", "tank"})
	if code != 0 || gotPool != "tank" || !strings.Contains(out.String(), "initial-secret") {
		t.Errorf("exit %d pool %q out %q err %q", code, gotPool, out.String(), errOut.String())
	}
	code = app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "init"})
	if code == 0 {
		t.Error("init without --pool should fail")
	}
	app.Init = func(context.Context, config.Config, string) (string, error) { return "", nil }
	out.Reset()
	if code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "init", "--pool", "tank"}); code != 0 || strings.Contains(out.String(), "password") {
		t.Errorf("re-init: %d %q", code, out.String())
	}
}

// agentDown answers every request like a socket nobody listens on.
func agentDown() cli.Client {
	api := newFakeAPI()
	api.errors["POST /v1/auth/reset"] = errors.New("cannot reach the db-branch agent")
	return api
}

func TestAdminResetPassword_エージェントがあればクラスタのパスワードを変える(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/auth/reset"] = map[string]any{"password": "from-agent"}
	_, out, _ := runAPI(t, api, "admin", "reset-password")
	if !strings.Contains(out, "from-agent") {
		t.Errorf("out %q", out)
	}
	api.errors["POST /v1/auth/reset"] = &cli.APIError{Status: 503, Code: "no_leader", Message: "no leader"}
	if code, _, _ := runAPI(t, api, "admin", "reset-password"); code == 0 {
		t.Error("agent error: want failure, not a local fallback")
	}
}

func TestAdminResetPassword_エージェントが動いていなければ手元のファイルを変える(t *testing.T) {
	var out bytes.Buffer
	app := cli.App{
		Stdout: &out, Stderr: &bytes.Buffer{},
		Client:        func(string) cli.Client { return agentDown() },
		ResetPassword: func(config.Config) (string, error) { return "new-secret", nil },
	}
	code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "admin", "reset-password"})
	if code != 0 || !strings.Contains(out.String(), "new-secret") {
		t.Errorf("exit %d out %q", code, out.String())
	}
}

func TestAgent_差し込まれた本体を設定とともに動かす(t *testing.T) {
	var got config.Config
	app := cli.App{
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Agent: func(_ context.Context, c config.Config) error { got = c; return nil },
	}
	if code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "agent"}); code != 0 {
		t.Fatal(code)
	}
	if got.DataDir != config.Default().DataDir {
		t.Errorf("config = %+v", got)
	}
}

func Test設定ファイルが不正ならどのコマンドも失敗する(t *testing.T) {
	api := newFakeAPI()
	var errOut bytes.Buffer
	app := cli.App{Stdout: &bytes.Buffer{}, Stderr: &errOut, Client: func(string) cli.Client { return api },
		Init:          func(context.Context, config.Config, string) (string, error) { return "", nil },
		ResetPassword: func(config.Config) (string, error) { return "", nil }}
	for _, args := range [][]string{{"branch", "list"}, {"init", "--pool", "tank"}, {"admin", "reset-password"}} {
		if code := app.Run(context.Background(), append([]string{"--config", t.TempDir()}, args...)); code == 0 {
			t.Errorf("%v: want failure", args)
		}
	}
}

func TestClusterコマンド(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/cluster/token"] = map[string]any{"token": "dbb1.abc.def"}
	api.answers["GET /v1/cluster"] = map[string]any{"mode": "cluster", "leader": "n1", "nodes": []any{
		map[string]any{"name": "n1", "host": "192.0.2.10", "leader": true, "voter": true, "pool_size": 100 << 30, "pool_free": 60 << 30, "version": "v"},
		map[string]any{"name": "n2", "host": "192.0.2.11", "down": true},
	}}
	if _, out, _ := runAPI(t, api, "cluster", "token"); !strings.Contains(out, "dbb1.abc.def") || !strings.Contains(out, "cluster join") {
		t.Errorf("token: %q", out)
	}
	_, out, _ := runAPI(t, api, "cluster", "status")
	for _, want := range []string{"NODE", "n1", "leader", "n2", "down", "60.0 GiB", "ENGINES", "VOTING", "voter", "non-voter"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if _, out, _ := runAPI(t, api, "cluster", "status", "-o", "json"); !strings.HasPrefix(out, "{") {
		t.Errorf("json: %q", out)
	}
}

func TestClusterInitとJoinは差し込まれた処理を呼ぶ(t *testing.T) {
	var calls []string
	var errOut bytes.Buffer
	app := cli.App{Stdout: &bytes.Buffer{}, Stderr: &errOut,
		ClusterInit: func(context.Context, config.Config) error { calls = append(calls, "init"); return nil },
		ClusterJoin: func(_ context.Context, _ config.Config, addr, token string) error {
			calls = append(calls, "join "+addr+" "+token)
			return nil
		},
	}
	cfgArg := []string{"--config", filepath.Join(t.TempDir(), "x.toml")}
	if code := app.Run(context.Background(), append(cfgArg, "cluster", "init")); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	if code := app.Run(context.Background(), append(cfgArg, "cluster", "join", "192.0.2.10:7070", "--token", "dbb1.x.y")); code != 0 {
		t.Fatalf("join: %s", errOut.String())
	}
	if code := app.Run(context.Background(), append(cfgArg, "cluster", "join", "192.0.2.10:7070")); code == 0 {
		t.Error("join without --token should fail")
	}
	if strings.Join(calls, "|") != "init|join 192.0.2.10:7070 dbb1.x.y" {
		t.Errorf("calls = %q", calls)
	}
	app.ClusterInit = func(context.Context, config.Config) error { return errors.New("boom") }
	if code := app.Run(context.Background(), append(cfgArg, "cluster", "init")); code == 0 {
		t.Error("init failure should fail")
	}
	if code := app.Run(context.Background(), []string{"--config", t.TempDir(), "cluster", "init"}); code == 0 {
		t.Error("bad config should fail")
	}
	if code := app.Run(context.Background(), []string{"--config", t.TempDir(), "cluster", "join", "a", "--token", "t"}); code == 0 {
		t.Error("bad config should fail")
	}
}

func TestClusterの問い合わせ失敗(t *testing.T) {
	api := newFakeAPI()
	for _, args := range [][]string{{"cluster", "token"}, {"cluster", "status"}} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("%v: want failure", args)
		}
	}
}

func TestCreate_ノードを指定して作れる(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches"] = accepted("op_1", "branch.create")
	api.answers["POST /v1/bases"] = accepted("op_2", "base.create")
	api.opStates = []map[string]any{{"id": "op", "state": "succeeded"}}
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "ready", "node": "n2"}
	if code, _, errOut := runAPI(t, api, "branch", "create", "x", "--base", "prod", "--node", "n2"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if code, _, errOut := runAPI(t, api, "base", "create", "p", "--runtime", "mysql@8.4.13", "--node", "n3"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if api.bodies["POST /v1/branches"].(map[string]any)["node"] != "n2" || api.bodies["POST /v1/bases"].(map[string]any)["node"] != "n3" {
		t.Errorf("bodies = %v", api.bodies)
	}
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{map[string]any{"name": "x", "status": "ready", "node": "n2"}}}
	if _, out, _ := runAPI(t, api, "branch", "list"); !strings.Contains(out, "NODE") || !strings.Contains(out, "n2") {
		t.Errorf("list:\n%s", out)
	}
}

func TestClusterStatus_更新途中なら知らせる(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/cluster"] = map[string]any{"mode": "cluster", "leader": "n1", "protocol": 3, "nodes": []any{
		map[string]any{"name": "n1", "protocol": 4}, map[string]any{"name": "n2", "protocol": 3},
	}}
	if _, out, _ := runAPI(t, api, "cluster", "status"); !strings.Contains(out, "Upgrading") {
		t.Errorf("out:\n%s", out)
	}
	api.answers["GET /v1/cluster"] = map[string]any{"mode": "cluster", "leader": "n1", "protocol": 4, "nodes": []any{map[string]any{"name": "n1", "protocol": 4}}}
	if _, out, _ := runAPI(t, api, "cluster", "status"); strings.Contains(out, "Upgrading") {
		t.Errorf("out:\n%s", out)
	}
}

func TestBranchRecreate_作り直して結果を表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/recreate?node=n2"] = accepted("op_1", "branch.recreate")
	api.answers["POST /v1/branches/x/recreate"] = accepted("op_2", "branch.recreate")
	api.opStates = []map[string]any{{"id": "op", "state": "succeeded"}}
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "ready", "connection": map[string]any{"host": "192.0.2.11", "port": 40001, "user": "branch"}}
	if code, out, errOut := runAPI(t, api, "branch", "recreate", "x", "--node", "n2"); code != 0 || !strings.Contains(out, "192.0.2.11:40001") {
		t.Errorf("exit %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := runAPI(t, api, "branch", "recreate", "x"); code != 0 {
		t.Errorf("without node: %s", errOut)
	}
}

func TestRoute_ユーザーの追加と一覧と削除(t *testing.T) {
	api := newFakeAPI()
	api.answers["PUT /v1/route-users/app"] = nil
	api.answers["GET /v1/route-users"] = map[string]any{"items": []any{map[string]any{"name": "app", "default_branch": "main"}}}
	api.answers["DELETE /v1/route-users/app"] = nil
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		app := cli.App{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut, Client: func(string) cli.Client { return api }}
		code := app.Run(context.Background(), append([]string{"--config", filepath.Join(t.TempDir(), "x.toml")}, args...))
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := run("s3cret\n", "route", "user", "add", "app", "--default", "main"); code != 0 {
		t.Fatalf("add: %s", errOut)
	}
	if b := api.bodies["PUT /v1/route-users/app"].(map[string]any); b["password"] != "s3cret" || b["default_branch"] != "main" {
		t.Errorf("body = %v", b)
	}
	if code, _, _ := run("", "route", "user", "add", "app", "--default", "main"); code == 0 {
		t.Error("no password: want failure")
	}
	if _, out, _ := run("", "route", "user", "list"); !strings.Contains(out, "app") || !strings.Contains(out, "main") {
		t.Errorf("list: %q", out)
	}
	if code, _, errOut := run("", "route", "user", "remove", "app"); code != 0 {
		t.Errorf("remove: %s", errOut)
	}
	api.answers["GET /v1/branches/x/dsn?via=proxy"] = map[string]any{"dsn": "mysql://x:pw@192.0.2.10:6033/"}
	if _, out, _ := run("", "branch", "dsn", "x", "--proxy"); !strings.Contains(out, ":6033/") {
		t.Errorf("dsn: %q", out)
	}
	api.errors["GET /v1/route-users"] = errors.New("down")
	if code, _, _ := run("", "route", "user", "list"); code == 0 {
		t.Error("list failure: want failure")
	}
	if code, _, _ := run("", "-o", "json", "route", "user", "list"); code == 0 {
		t.Error("json list failure: want failure")
	}
}

func TestModuleとSettingsコマンド(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/modules"] = map[string]any{"name": "proxysql", "version": "1.0", "wanted": true}
	api.answers["DELETE /v1/modules/proxysql"] = nil
	api.answers["GET /v1/modules"] = map[string]any{"items": []any{
		map[string]any{"name": "proxysql", "version": "1.0", "wanted": true, "nodes": []any{
			map[string]any{"node": "n1", "loaded": "1.0", "current": true}, map[string]any{"node": "n2", "loaded": "", "current": false}}},
		map[string]any{"name": "old", "version": "0.1", "wanted": false, "nodes": []any{}},
	}}
	if _, out, errOut := runAPI(t, api, "module", "install", "proxysql", "--version", "1.0"); !strings.Contains(out, "proxysql 1.0") {
		t.Errorf("install: %q %q", out, errOut)
	}
	if api.bodies["POST /v1/modules"].(map[string]any)["version"] != "1.0" {
		t.Errorf("body = %v", api.bodies["POST /v1/modules"])
	}
	_, out, _ := runAPI(t, api, "module", "list")
	if !strings.Contains(out, "enabled 1/2") || !strings.Contains(out, "n2(pending)") || !strings.Contains(out, "removed") {
		t.Errorf("list:\n%s", out)
	}
	if code, _, _ := runAPI(t, api, "module", "remove", "proxysql"); code != 0 {
		t.Error("remove")
	}
	if _, out, _ := runAPI(t, api, "-o", "json", "module", "list"); !strings.Contains(out, `"proxysql"`) {
		t.Errorf("json: %s", out)
	}
	api.answers["GET /v1/settings"] = map[string]any{"items": []any{map[string]any{"scope": "core", "settings": []any{
		map[string]any{"key": "branch.memory_mb", "value": "256", "default": "128"}}}}}
	api.answers["PUT /v1/settings/core"] = map[string]any{}
	if _, out, _ := runAPI(t, api, "settings", "list"); !strings.Contains(out, "branch.memory_mb") || !strings.Contains(out, "256") {
		t.Errorf("settings list:\n%s", out)
	}
	if _, out, _ := runAPI(t, api, "-o", "json", "settings", "list"); !strings.Contains(out, "branch.memory_mb") {
		t.Errorf("settings json: %s", out)
	}
	if code, _, errOut := runAPI(t, api, "settings", "set", "core", "branch.memory_mb=512", "branch.default_ttl="); code != 0 {
		t.Fatalf("set: %s", errOut)
	}
	if v := api.bodies["PUT /v1/settings/core"].(map[string]any)["values"].(map[string]string); v["branch.memory_mb"] != "512" || v["branch.default_ttl"] != "" {
		t.Errorf("values = %v", v)
	}
	if code, _, _ := runAPI(t, api, "settings", "set", "core", "nonsense"); code == 0 {
		t.Error("bad pair: want failure")
	}
	api.errors["GET /v1/modules"] = errors.New("down")
	api.errors["GET /v1/settings"] = errors.New("down")
	api.errors["POST /v1/modules"] = errors.New("down")
	for _, args := range [][]string{{"module", "list"}, {"settings", "list"}, {"module", "install", "x"}} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("%v: want failure", args)
		}
	}
}

func TestTokenコマンド(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/tokens"] = map[string]any{"token": "dbb_x_s", "scope": "branches"}
	api.answers["GET /v1/tokens"] = map[string]any{"items": []any{map[string]any{"id": "x", "name": "claude", "scope": "branches", "created_at": "2026-11-10T00:00:00Z"}}}
	api.answers["DELETE /v1/tokens/x"] = nil
	if code, out, errOut := runAPI(t, api, "token", "create", "claude", "--ttl", "720h"); code != 0 || strings.TrimSpace(out) != "dbb_x_s" || !strings.Contains(errOut, "shown only now") {
		t.Errorf("create: %d %q %q", code, out, errOut)
	}
	if b := api.bodies["POST /v1/tokens"].(map[string]any); b["scope"] != "branches" || b["ttl"] != "720h" {
		t.Errorf("body = %v", b)
	}
	if _, out, _ := runAPI(t, api, "token", "list"); !strings.Contains(out, "claude") {
		t.Errorf("list: %q", out)
	}
	if code, _, _ := runAPI(t, api, "token", "revoke", "x"); code != 0 {
		t.Error("revoke")
	}
	api.errors["POST /v1/tokens"] = errors.New("down")
	api.errors["GET /v1/tokens"] = errors.New("down")
	for _, args := range [][]string{{"token", "create", "y"}, {"token", "list"}} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("%v: want failure", args)
		}
	}
}

func TestBackup_今すぐ取得と一覧と復元(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/backups"] = map[string]any{"name": "db-branch-n1-20261110T030000Z.json.gz", "size": 1234, "taken_at": "2026-11-10T03:00:00Z"}
	if code, out, errOut := runAPI(t, api, "backup", "now"); code != 0 || !strings.Contains(out, "db-branch-n1-20261110T030000Z.json.gz") {
		t.Errorf("now: %d %s %s", code, out, errOut)
	}
	api.answers["GET /v1/backups"] = map[string]any{"node": "n1", "items": []any{map[string]any{"name": "db-branch-n1-20261110T030000Z.json.gz", "size": 1234, "taken_at": "2026-11-10T03:00:00Z"}}}
	if code, out, _ := runAPI(t, api, "backup", "list"); code != 0 || !strings.Contains(out, "1.2 KiB") {
		t.Errorf("list: %d %s", code, out)
	}
	file := filepath.Join(t.TempDir(), "b.json.gz")
	writeFile(t, file, "gzipped")
	api.answers["POST /v1/backups/restore"] = map[string]any{"node": "n1", "taken_at": "2026-11-10T03:00:00Z", "settings": 2, "tokens": 3, "modules": 1, "module_data": 4, "route_users": 0, "runtimes": 2, "admin": true, "skipped_bases": []any{"prod"}, "skipped_branches": []any{}}
	code, out, errOut := runAPI(t, api, "backup", "restore", file)
	if code != 0 || !strings.Contains(out, "3 tokens") || !strings.Contains(errOut, "prod") {
		t.Errorf("restore: %d %s %s", code, out, errOut)
	}
	if raw, ok := api.bodies["POST /v1/backups/restore"].(cli.Raw); !ok || string(raw) != "gzipped" {
		t.Errorf("sent %#v", api.bodies["POST /v1/backups/restore"])
	}
	if code, _, _ := runAPI(t, api, "backup", "restore", "/nope"); code == 0 {
		t.Error("missing file")
	}
}

func TestBackup_JSONとエラー(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/backups"] = map[string]any{"items": []any{map[string]any{"name": "x.json.gz", "size": 1}}}
	if code, out, _ := runAPI(t, api, "-o", "json", "backup", "list"); code != 0 || !strings.Contains(out, "x.json.gz") {
		t.Errorf("json: %d %s", code, out)
	}
	boom := &cli.APIError{Status: 500, Code: "internal", Message: "disk full"}
	for _, k := range []string{"POST /v1/backups", "GET /v1/backups", "POST /v1/backups/restore"} {
		api.errors[k] = boom
	}
	file := filepath.Join(t.TempDir(), "b.json.gz")
	writeFile(t, file, "x")
	for _, args := range [][]string{{"backup", "now"}, {"backup", "list"}, {"backup", "restore", file}} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("%v should fail", args)
		}
	}
	api.errors = map[string]error{}
	api.answers["POST /v1/backups/restore"] = map[string]any{"node": "n1", "skipped_bases": []any{}, "skipped_branches": []any{}}
	if code, _, errOut := runAPI(t, api, "backup", "restore", file); code != 0 || errOut != "" {
		t.Errorf("nothing skipped: %d %q", code, errOut)
	}
}

func TestBranch_一覧と同じ所有者付きの名前でも指定できる(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "ready"}
	if code, out, errOut := runAPI(t, api, "branch", "show", "alice@example.com/x"); code != 0 || !strings.Contains(out, "x") {
		t.Errorf("%d %s %s", code, out, errOut)
	}
}

func TestInit_モジュールのストレージで初期化する(t *testing.T) {
	var got config.Config
	var gotPool string
	var out, errOut bytes.Buffer
	app := cli.App{Stdout: &out, Stderr: &errOut, Init: func(_ context.Context, cfg config.Config, pool string) (string, error) {
		got, gotPool = cfg, pool
		return "", nil
	}}
	code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "init", "--storage", "fsx"})
	if code != 0 || got.Storage != "fsx" || gotPool != "fsx" || !strings.Contains(out.String(), "fsx module") {
		t.Errorf("exit %d storage %q pool %q out %q err %q", code, got.Storage, gotPool, out.String(), errOut.String())
	}
	if code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "init", "--pool", "tank", "--storage", "fsx"}); code == 0 {
		t.Error("both a pool and a storage module")
	}
	if code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "x.toml"), "init", "--storage", "Bad Name"}); code == 0 {
		t.Error("a bad module name")
	}
}

func TestBase_maskはルールのファイルを送って待つ(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/bases/dev/mask"] = accepted("op_1", "base.mask")
	api.opStates = []map[string]any{{"id": "op_1", "state": "succeeded"}}
	rules := filepath.Join(t.TempDir(), "rules.yaml")
	writeFile(t, rules, "databases: []\n")
	if code, _, errOut := runAPI(t, api, "base", "mask", "dev", "--from", "prod", "--snapshot", "s1", "--masker", "greenmask", "--rules", rules); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	body := api.bodies["POST /v1/bases/dev/mask"].(map[string]any)
	if body["source"] != "prod" || body["snapshot"] != "s1" || body["masker"] != "greenmask" || body["rules"] != "databases: []\n" {
		t.Errorf("body %v", body)
	}
	if code, _, _ := runAPI(t, api, "base", "mask", "dev", "--from", "prod", "--masker", "greenmask", "--rules", "/no/such/file"); code == 0 {
		t.Error("a missing rules file")
	}
	if code, _, _ := runAPI(t, api, "base", "mask", "dev", "--masker", "greenmask"); code == 0 {
		t.Error("no --from")
	}
}

func TestBaseRefreshSet_パスワードは標準入力から読みフラグには出さない(t *testing.T) {
	api := newFakeAPI()
	api.answers["PUT /v1/bases/prod/refresh"] = map[string]any{"base": "prod", "every": "24h0m0s", "next_run": "2026-11-10T12:00:00Z",
		"source": map[string]any{"host": "db", "port": 5432}}
	var out, errOut bytes.Buffer
	app := cli.App{Version: "test", Stdin: strings.NewReader("hunter2\nignored\n"), Stdout: &out, Stderr: &errOut, Client: func(string) cli.Client { return api }}
	code := app.Run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "none.toml"), "base", "refresh", "set", "prod",
		"--host", "db", "--port", "5432", "--user", "dumper", "--databases", "app, billing", "--every", "24h", "--password-stdin"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	body := api.bodies["PUT /v1/bases/prod/refresh"].(map[string]any)
	src := body["source"].(map[string]any)
	if body["every"] != "24h" || src["password"] != "hunter2" || src["host"] != "db" || fmt.Sprint(src["databases"]) != "[app billing]" {
		t.Errorf("body = %v", body)
	}
	if strings.Contains(out.String()+errOut.String(), "hunter2") || !strings.Contains(out.String(), "every 24h0m0s") {
		t.Errorf("output = %q %q", out.String(), errOut.String())
	}
}

func TestBaseRefreshSet_環境変数のパスワードを使い無ければ空で送る(t *testing.T) {
	api := newFakeAPI()
	api.answers["PUT /v1/bases/prod/refresh"] = map[string]any{"base": "prod"}
	args := []string{"base", "refresh", "set", "prod", "--host", "db", "--port", "3306", "--user", "u", "--databases", "app", "--every", "1h"}
	t.Setenv(cli.SourcePasswordEnv, "fromenv")
	if code, _, e := runAPI(t, api, args...); code != 0 {
		t.Fatal(e)
	}
	if pw := api.bodies["PUT /v1/bases/prod/refresh"].(map[string]any)["source"].(map[string]any)["password"]; pw != "fromenv" {
		t.Errorf("password = %v", pw)
	}
	t.Setenv(cli.SourcePasswordEnv, "")
	if code, _, e := runAPI(t, api, args...); code != 0 {
		t.Fatal(e)
	}
	if pw := api.bodies["PUT /v1/bases/prod/refresh"].(map[string]any)["source"].(map[string]any)["password"]; pw != "" {
		t.Errorf("password = %v", pw)
	}
}

func TestBaseRefreshSet_必須の指定や標準入力が無ければ失敗する(t *testing.T) {
	api := newFakeAPI()
	if code, _, e := runAPI(t, api, "base", "refresh", "set", "prod", "--host", "db"); code == 0 || !strings.Contains(e, "required") {
		t.Errorf("missing flags: %d %q", code, e)
	}
	full := []string{"base", "refresh", "set", "prod", "--host", "db", "--port", "1", "--user", "u", "--databases", "a", "--every", "1h", "--password-stdin"}
	if code, _, e := runAPI(t, api, full...); code == 0 || !strings.Contains(e, "no standard input") {
		t.Errorf("no stdin: %d %q", code, e)
	}
	for _, in := range []string{"", "\n"} {
		var errOut bytes.Buffer
		app := cli.App{Stdin: strings.NewReader(in), Stdout: &bytes.Buffer{}, Stderr: &errOut, Client: func(string) cli.Client { return api }}
		if code := app.Run(context.Background(), append([]string{"--config", filepath.Join(t.TempDir(), "none.toml")}, full...)); code == 0 || !strings.Contains(errOut.String(), "empty") {
			t.Errorf("empty stdin %q: %d %q", in, code, errOut.String())
		}
	}
}

func TestBaseRefreshShow_ソースと最後の実行と次回を表示しパスワードは出さない(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/bases/prod/refresh"] = map[string]any{"base": "prod", "engine": "postgres", "every": "24h0m0s", "next_run": "2026-11-10T12:00:00Z",
		"source":   map[string]any{"host": "db", "port": 5432, "user": "dumper", "password_set": true, "databases": []string{"app", "billing"}},
		"last_run": map[string]any{"finished_at": "2026-11-09T12:00:00Z", "ok": false, "error": "pg_dump: refused"}}
	code, out, e := runAPI(t, api, "base", "refresh", "show", "prod")
	if code != 0 {
		t.Fatal(e)
	}
	for _, want := range []string{"postgres://dumper@db:5432", "Password:", "set", "app, billing", "failed: pg_dump: refused", "2026-11-10T12:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	api.answers["GET /v1/bases/prod/refresh"] = map[string]any{"base": "prod", "source": map[string]any{}}
	if _, out, _ = runAPI(t, api, "base", "refresh", "show", "prod"); !strings.Contains(out, "not set") || !regexp.MustCompile(`Last run:\s+-\n`).MatchString(out) {
		t.Errorf("no run yet:\n%s", out)
	}
	if code, out, _ = runAPI(t, api, "--output", "json", "base", "refresh", "show", "prod"); code != 0 || !strings.Contains(out, `"base": "prod"`) {
		t.Errorf("json: %d %s", code, out)
	}
}

func TestBaseRefreshRunAndOff_今すぐ実行とオフ(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/bases/prod/refresh/run"] = accepted("op_9", "base.refresh")
	api.opStates = []map[string]any{{"id": "op_9", "kind": "base.refresh", "state": "succeeded"}}
	if code, _, e := runAPI(t, api, "base", "refresh", "run", "prod"); code != 0 {
		t.Fatal(e)
	}
	api.answers["DELETE /v1/bases/prod/refresh"] = nil
	if code, _, e := runAPI(t, api, "base", "refresh", "off", "prod"); code != 0 {
		t.Fatal(e)
	}
}

func TestUpdateコマンド_確認と状態と開始と取り消し(t *testing.T) {
	api := newFakeAPI()
	view := map[string]any{
		"checked_at": "2026-11-11T10:00:00Z", "checked_by": "n1", "latest_minor": "1.2.0",
		"nodes": []any{
			map[string]any{"name": "n1", "version": "1.0.0", "leader": true, "update": map[string]any{"state": "updating"}},
			map[string]any{"name": "n2", "version": "1.0.0", "down": true},
			map[string]any{"name": "n3", "version": "1.0.0"},
		},
		"packages": []any{
			map[string]any{"name": "db-branch", "installed": "1.0.0", "available": []any{
				map[string]any{"version": "2.0.0", "kind": "major"}, map[string]any{"version": "1.2.0", "kind": "minor"}}},
			map[string]any{"name": "db-branch-auth", "installed": "1.0.0", "mixed": true, "available": []any{}},
		},
		"rollout": map[string]any{"id": "r1", "target": "1.2.0", "status": "failed", "error": "node n1: boom", "notes": []any{"auth stays"}},
	}
	api.answers["POST /v1/updates/check"] = view
	api.answers["GET /v1/updates"] = view
	api.answers["POST /v1/updates"] = map[string]any{"id": "r1"}
	api.answers["DELETE /v1/updates"] = nil
	for _, args := range [][]string{{"update", "check"}, {"update", "status"}} {
		code, out, errOut := runAPI(t, api, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		for _, want := range []string{"n1 (leader)", "updating", "down", "1.2.0 (minor)", "major: by hand", "db-branch-auth", "1.0.0 (differs between nodes)", "Rollout r1 to 1.2.0: failed", "error: node n1: boom", "note: auth stays", "Checked"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v lacks %q:\n%s", args, want, out)
			}
		}
	}
	if code, out, _ := runAPI(t, api, "update", "status", "-o", "json"); code != 0 || !strings.Contains(out, `"latest_minor": "1.2.0"`) {
		t.Errorf("json: %d %s", code, out)
	}
	if code, out, _ := runAPI(t, api, "update", "apply", "1.2.0"); code != 0 || !strings.Contains(out, "1.2.0 started") || api.bodies["POST /v1/updates"].(map[string]any)["target"] != "1.2.0" {
		t.Errorf("apply: %d %s %v", code, out, api.bodies)
	}
	if code, _, _ := runAPI(t, api, "update", "cancel"); code != 0 {
		t.Errorf("cancel: %d", code)
	}
	empty := newFakeAPI()
	empty.answers["GET /v1/updates"] = map[string]any{"nodes": []any{}, "packages": []any{}}
	if code, out, _ := runAPI(t, empty, "update", "status"); code != 0 || !strings.Contains(out, "Not checked yet") {
		t.Errorf("unchecked: %d %s", code, out)
	}
	for _, args := range [][]string{{"update", "check"}, {"update", "status"}, {"update", "apply", "1.2.0"}, {"update", "cancel"}} {
		if code, _, _ := runAPI(t, newFakeAPI(), args...); code == 0 {
			t.Errorf("%v with an API error: want failure", args)
		}
	}
}

func TestBaseReplicas_全ノードと起点だけと指定ノードを送り結果を表示する(t *testing.T) {
	api := newFakeAPI()
	for _, tc := range []struct {
		arg  string
		want any
		out  string
		resp any
	}{
		{"all", []string(nil), "all nodes", nil},
		{"origin", []string{}, "origin only", []string{}},
		{"n2, n3", []string{"n2", "n3"}, "origin + n2, n3", []string{"n2", "n3"}},
	} {
		api.answers["PUT /v1/bases/prod/replicas"] = map[string]any{"name": "prod", "replica_nodes": tc.resp}
		code, out, errOut := runAPI(t, api, "base", "replicas", "prod", tc.arg)
		if code != 0 || !strings.Contains(out, tc.out) {
			t.Errorf("%s: exit %d out %q err %q", tc.arg, code, out, errOut)
		}
		got, _ := json.Marshal(api.bodies["PUT /v1/bases/prod/replicas"])
		want, _ := json.Marshal(map[string]any{"replica_nodes": tc.want})
		if string(got) != string(want) {
			t.Errorf("%s: body %s, want %s", tc.arg, got, want)
		}
	}
	if code, _, errOut := runAPI(t, api, "base", "replicas", "prod", "a,,b"); code == 0 || !strings.Contains(errOut, "invalid replicas") {
		t.Errorf("bad list: %d %q", code, errOut)
	}
}

func TestBaseCreateとShow_レプリカ先を指定でき保持ノードを表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/bases"] = accepted("op_1", "base.create")
	api.opStates = []map[string]any{{"id": "op_1", "state": "succeeded"}}
	if code, _, errOut := runAPI(t, api, "base", "create", "big", "--runtime", "mysql@8.4.13", "--replicas", "origin"); code != 0 {
		t.Fatal(errOut)
	}
	if body := api.bodies["POST /v1/bases"].(map[string]any); fmt.Sprint(body["replica_nodes"]) != "[]" {
		t.Errorf("body = %v", body)
	}
	if code, _, _ := runAPI(t, api, "base", "create", "big", "--runtime", "mysql@8.4.13", "--replicas", "a,,b"); code == 0 {
		t.Error("bad list: want failure")
	}
	api.answers["GET /v1/bases/big"] = map[string]any{"name": "big", "status": "ready", "replica_nodes": []string{"n2"},
		"holders": []any{map[string]any{"node": "n1", "state": "held", "origin": true}, map[string]any{"node": "n3", "state": "draining"}}}
	_, out, _ := runAPI(t, api, "base", "show", "big")
	for _, want := range []string{"origin + n2", "n1  origin", "n3  draining"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
}

func TestBaseAnonymous_上限と期限を部分的に送り継承はnullで送り結果を表示する(t *testing.T) {
	three, four := 3, "4h0m0s"
	for _, tc := range []struct {
		args []string
		want map[string]any
		resp map[string]any
		out  []string
	}{
		{[]string{"--max", "3", "--ttl", "4h"}, map[string]any{"max_branches": 3.0, "max_ttl": "4h"},
			map[string]any{"max_branches": &three, "max_ttl": &four}, []string{"base prod", "up to 3 branches", "4h0m0s"}},
		{[]string{"--max", "inherit"}, map[string]any{"max_branches": nil},
			map[string]any{"max_branches": nil, "max_ttl": &four}, []string{"inherits the cluster-wide cap", "4h0m0s"}},
		{[]string{"--ttl", "inherit"}, map[string]any{"max_ttl": nil},
			map[string]any{"max_branches": 0, "max_ttl": nil}, []string{"none (disabled)", "cluster-wide TTL"}},
		{[]string{"--max", "0"}, map[string]any{"max_branches": 0.0},
			map[string]any{"max_branches": 0, "max_ttl": nil}, []string{"none (disabled)"}},
	} {
		api := newFakeAPI()
		api.answers["PATCH /v1/bases/prod/anonymous"] = map[string]any{"name": "prod", "anonymous": tc.resp}
		code, out, errOut := runAPI(t, api, append([]string{"base", "anonymous", "prod"}, tc.args...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d %s", tc.args, code, errOut)
		}
		got, _ := json.Marshal(api.bodies["PATCH /v1/bases/prod/anonymous"])
		want, _ := json.Marshal(tc.want)
		if string(got) != string(want) {
			t.Errorf("%v: body %s, want %s", tc.args, got, want)
		}
		for _, s := range tc.out {
			if !strings.Contains(out, s) {
				t.Errorf("%v: output lacks %q: %s", tc.args, s, out)
			}
		}
	}
}

func TestBaseAnonymous_引数なしは現在の設定を表示し不正な値は送らない(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/bases/prod"] = map[string]any{"name": "prod", "anonymous": map[string]any{"max_branches": nil, "max_ttl": nil}}
	code, out, errOut := runAPI(t, api, "base", "anonymous", "prod")
	if code != 0 || !strings.Contains(out, "inherits the cluster-wide cap") || api.bodies["PATCH /v1/bases/prod/anonymous"] != nil {
		t.Errorf("show: %d %q %q", code, out, errOut)
	}
	for _, args := range [][]string{{"--max", "many"}, {"--max", "-1"}, {"--ttl", "soon"}, {"--ttl", "0s"}, {"--ttl", "-1h"}} {
		if code, _, errOut := runAPI(t, api, append([]string{"base", "anonymous", "prod"}, args...)...); code == 0 || !strings.Contains(errOut, args[0]) {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
	if api.bodies["PATCH /v1/bases/prod/anonymous"] != nil {
		t.Error("an invalid value must not reach the API")
	}
	if code, _, _ := runAPI(t, newFakeAPI(), "base", "anonymous", "prod", "--max", "1"); code == 0 {
		t.Error("API error: want failure")
	}
	if code, _, _ := runAPI(t, newFakeAPI(), "base", "anonymous", "prod"); code == 0 {
		t.Error("API error on show: want failure")
	}
}

func TestBaseShow_匿名の上限を表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["GET /v1/bases/big"] = map[string]any{"name": "big", "status": "ready", "anonymous": map[string]any{"max_branches": 2, "max_ttl": "1h0m0s"}}
	_, out, _ := runAPI(t, api, "base", "show", "big")
	if !strings.Contains(out, "Without sign-in:") || !strings.Contains(out, "up to 2 branches") {
		t.Errorf("show lacks the limits:\n%s", out)
	}
}
