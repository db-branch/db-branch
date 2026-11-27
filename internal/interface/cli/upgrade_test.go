package cli_test

import (
	"encoding/json"
	"github.com/db-branch/db-branch/internal/interface/cli"
	"strings"
	"testing"
)

func upgradeOp(state string, status string, extra map[string]any) map[string]any {
	res := map[string]any{"branch": "x", "from": "mysql@8.0.46", "to": "mysql@8.4.13", "status": status, "checkpoint": "before-upgrade-20261113-120000",
		"pre_check": []any{
			map[string]any{"level": "info", "code": "upgrade_path", "message": "mysql@8.0.46 to mysql@8.4.13 on node n1"},
			map[string]any{"level": "warning", "code": "native_password_users", "message": "1 account(s) use mysql_native_password"},
		},
		"log": []any{map[string]any{"level": "warning", "code": "log_warning", "message": "MY-010068: CA certificate is self signed"}}}
	for k, v := range extra {
		res[k] = v
	}
	op := map[string]any{"id": "op_9", "kind": "branch.upgrade", "state": state, "result": res}
	if state == "failed" {
		op["error"] = "upgrade to mysql@8.4.13 failed and the branch was rolled back"
	}
	return op
}

func TestBranchUpgrade_事前確認とログの警告と戻し方を表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/upgrade"] = accepted("op_9", "branch.upgrade")
	api.opStates = []map[string]any{{"id": "op_9", "state": "running", "phase": "upgrading"}, upgradeOp("succeeded", "upgraded", nil)}

	code, out, errOut := runAPI(t, api, "branch", "upgrade", "x", "--runtime", "mysql@8.4.13")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	body := api.bodies["POST /v1/branches/x/upgrade"].(map[string]any)
	if body["runtime"] != "mysql@8.4.13" || body["check_only"] != false || body["force"] != false || body["no_rollback"] != false {
		t.Errorf("body = %v", body)
	}
	for _, want := range []string{"Pre-check: mysql@8.0.46 to mysql@8.4.13", "warning  1 account(s) use mysql_native_password", "Server log during the upgrade:", "CA certificate is self signed",
		"Upgraded x from mysql@8.0.46 to mysql@8.4.13", "db-branch branch restore x before-upgrade-20261113-120000"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "upgrading...") {
		t.Errorf("phases = %q", errOut)
	}
}

func TestBranchUpgrade_フラグを依頼に渡す(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/upgrade"] = accepted("op_9", "branch.upgrade")
	api.opStates = []map[string]any{upgradeOp("succeeded", "upgraded", nil)}
	runAPI(t, api, "branch", "upgrade", "alice/x", "--runtime", "mysql@8.4.13", "--no-rollback", "--force")
	body := api.bodies["POST /v1/branches/x/upgrade"].(map[string]any)
	if body["no_rollback"] != true || body["force"] != true {
		t.Errorf("body = %v", body)
	}
}

func TestBranchUpgrade_確認だけなら何も変えていないと言いエラーなら強制を案内する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/upgrade"] = accepted("op_9", "branch.upgrade")
	api.opStates = []map[string]any{upgradeOp("succeeded", "checked", map[string]any{"check_only": true})}
	code, out, _ := runAPI(t, api, "branch", "upgrade", "x", "--runtime", "mysql@8.4.13", "--check-only")
	if code != 0 || !strings.Contains(out, "nothing was changed") || !strings.Contains(out, "db-branch branch upgrade x --runtime mysql@8.4.13") {
		t.Errorf("exit %d out:\n%s", code, out)
	}
	if body := api.bodies["POST /v1/branches/x/upgrade"].(map[string]any); body["check_only"] != true {
		t.Errorf("body = %v", body)
	}

	api.opStates = []map[string]any{upgradeOp("succeeded", "checked", map[string]any{"pre_check": []any{map[string]any{"level": "error", "code": "native_password_removed", "message": "2 accounts"}}})}
	_, out, _ = runAPI(t, api, "branch", "upgrade", "x", "--runtime", "mysql@9.6.0", "--check-only")
	if !strings.Contains(out, "1 error(s) block the upgrade unless you add --force") {
		t.Errorf("out:\n%s", out)
	}
}

func TestBranchUpgrade_失敗したらログの抜粋とロールバックを示して終了コードを返す(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/upgrade"] = accepted("op_9", "branch.upgrade")
	api.opStates = []map[string]any{upgradeOp("failed", "rolled_back", map[string]any{"log_excerpt": "[ERROR] [MY-013170] Upgrade failed\nsecond line"})}
	code, out, errOut := runAPI(t, api, "branch", "upgrade", "x", "--runtime", "mysql@8.4.13")
	if code == 0 || !strings.Contains(errOut, "rolled back") {
		t.Errorf("exit %d err %q", code, errOut)
	}
	for _, want := range []string{"rolled back to mysql@8.0.46", "End of the server's error log:", "  [ERROR] [MY-013170] Upgrade failed", "  second line"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBranchUpgrade_各結果の説明(t *testing.T) {
	for status, want := range map[string]string{
		"failed":  "left as it is",
		"refused": "Nothing was changed on x",
		"aborted": "Nothing was changed on x",
	} {
		api := newFakeAPI()
		api.answers["POST /v1/branches/x/upgrade"] = accepted("op_9", "branch.upgrade")
		api.opStates = []map[string]any{upgradeOp("failed", status, nil)}
		_, out, _ := runAPI(t, api, "branch", "upgrade", "x", "--runtime", "mysql@8.4.13")
		if !strings.Contains(out, want) {
			t.Errorf("%s: output lacks %q:\n%s", status, want, out)
		}
	}
}

func TestBranchUpgrade_JSON出力は結果をそのまま出す(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/upgrade"] = accepted("op_9", "branch.upgrade")
	api.opStates = []map[string]any{upgradeOp("succeeded", "upgraded", nil)}
	code, out, _ := runAPI(t, api, "-o", "json", "branch", "upgrade", "x", "--runtime", "mysql@8.4.13")
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 0 || res["status"] != "upgraded" || len(res["pre_check"].([]any)) != 2 {
		t.Errorf("exit %d out %q (%v)", code, out, err)
	}
}

func TestBranchUpgrade_ランタイム指定が必要で形式も確かめる(t *testing.T) {
	api := newFakeAPI()
	if code, _, errOut := runAPI(t, api, "branch", "upgrade", "x"); code == 0 || !strings.Contains(errOut, "runtime") {
		t.Errorf("exit %d err %q", code, errOut)
	}
	if code, _, errOut := runAPI(t, api, "branch", "upgrade", "x", "--runtime", "latest"); code == 0 || !strings.Contains(errOut, "--runtime") {
		t.Errorf("exit %d err %q", code, errOut)
	}
	if len(api.requests) != 0 {
		t.Errorf("requests = %v", api.requests)
	}
}

func TestBranchUpgrade_待たずにオペレーションIDを返せる(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/upgrade"] = accepted("op_9", "branch.upgrade")
	code, out, _ := runAPI(t, api, "branch", "upgrade", "x", "--runtime", "mysql@8.4.13", "--no-wait")
	if code != 0 || strings.TrimSpace(out) != "op_9" {
		t.Errorf("exit %d out %q", code, out)
	}
}

func TestBranchUpgrade_APIが断ったら理由を表示する(t *testing.T) {
	api := newFakeAPI()
	api.errors["POST /v1/branches/x/upgrade"] = &cli.APIError{Status: 422, Code: "upgrade_not_allowed", Message: "mysql@8.4.13 to mysql@8.0.46 is a downgrade"}
	code, _, errOut := runAPI(t, api, "branch", "upgrade", "x", "--runtime", "mysql@8.0.46")
	if code == 0 || !strings.Contains(errOut, "downgrade") {
		t.Errorf("exit %d err %q", code, errOut)
	}
}

func TestBranchCreate_ランタイムを付けると作成と同時に上げて結果を表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches"] = accepted("op_9", "branch.create")
	api.opStates = []map[string]any{upgradeOp("succeeded", "upgraded", nil)}
	api.answers["GET /v1/branches/x"] = map[string]any{"name": "x", "status": "ready", "runtime": "mysql@8.4.13", "base_runtime": "mysql@8.0.46",
		"connection": map[string]any{"host": "h", "port": 40001, "user": "x"}}
	code, out, errOut := runAPI(t, api, "branch", "create", "x", "--base", "prod", "--runtime", "mysql@8.4.13")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if body := api.bodies["POST /v1/branches"].(map[string]any); body["runtime"] != "mysql@8.4.13" {
		t.Errorf("body = %v", body)
	}
	for _, want := range []string{"Upgraded x from mysql@8.0.46", "mysql@8.4.13 (upgraded; the base runs mysql@8.0.46)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBranchCreate_ランタイム付きの作成が失敗したら結果を見せてエラーにする(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches"] = accepted("op_9", "branch.create")
	api.opStates = []map[string]any{upgradeOp("failed", "rolled_back", nil)}
	code, out, _ := runAPI(t, api, "branch", "create", "x", "--base", "prod", "--runtime", "mysql@8.4.13")
	if code == 0 || !strings.Contains(out, "rolled back to") {
		t.Errorf("exit %d out:\n%s", code, out)
	}
	if code, _, errOut := runAPI(t, api, "branch", "create", "x", "--base", "prod", "--runtime", "x"); code == 0 || !strings.Contains(errOut, "--runtime") {
		t.Errorf("bad runtime: exit %d err %q", code, errOut)
	}
	api2 := newFakeAPI()
	api2.answers["POST /v1/branches"] = accepted("op_9", "branch.create")
	if code, out, _ := runAPI(t, api2, "branch", "create", "x", "--base", "prod", "--runtime", "mysql@8.4.13", "--no-wait"); code != 0 || strings.TrimSpace(out) != "op_9" {
		t.Errorf("no-wait: exit %d out %q", code, out)
	}
}

func TestBranchCheckpoints_チェックポイントのランタイムを表示する(t *testing.T) {
	api := checkpointAPI()
	items := api.answers["GET /v1/branches/x/checkpoints"].(map[string]any)["items"].([]any)
	items[0].(map[string]any)["runtime"] = "mysql@8.0.46"
	_, out, _ := runAPI(t, api, "branch", "checkpoints", "x")
	if !strings.Contains(out, "RUNTIME") || !strings.Contains(out, "mysql@8.0.46") {
		t.Errorf("table:\n%s", out)
	}
}

func TestCLI_BranchCreateUpgraded_JSON出力が単一の有効JSONになりBaseCreateはスラッシュ入り不正名を拒否する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches"] = accepted("op_9", "branch.create")
	api.opStates = []map[string]any{upgradeOp("succeeded", "upgraded", nil)}
	api.answers["GET /v1/branches/x"] = map[string]any{
		"name": "x", "status": "ready", "runtime": "mysql@8.4.13", "base_runtime": "mysql@8.0.46",
		"connection": map[string]any{"host": "h", "port": 40001, "user": "x"},
	}
	code, out, errOut := runAPI(t, api, "-o", "json", "branch", "create", "x", "--base", "prod", "--runtime", "mysql@8.4.13")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("expected single valid JSON, got error %v:\n%s", err, out)
	}
	if got["name"] != "x" || got["runtime"] != "mysql@8.4.13" {
		t.Errorf("unexpected JSON: %v", got)
	}

	// Base commands must reject slash-containing invalid names instead of stripping an owner prefix.
	api2 := newFakeAPI()
	for _, args := range [][]string{
		{"base", "create", "bad/name", "--runtime", "mysql@8.4.13"},
		{"base", "show", "bad/name"},
		{"base", "delete", "bad/name"},
		{"base", "replicas", "bad/name", "all"},
		{"base", "refresh", "show", "bad/name"},
		{"base", "anonymous", "bad/name"},
	} {
		if code, _, _ := runAPI(t, api2, args...); code == 0 {
			t.Errorf("%v: expected non-zero exit for slash name", args)
		}
	}
	if len(api2.requests) != 0 {
		t.Errorf("slash name should be rejected client-side without API calls: %v", api2.requests)
	}
}

func TestBranchShowとList_統計更新中と警告を表示する(t *testing.T) {
	api := newFakeAPI()
	b := map[string]any{
		"name": "pgx", "status": "ready", "runtime": "postgres@19.0", "base_runtime": "postgres@18.6",
		"base": "pgbase", "snapshot": "s1", "node": "n1",
		"connection": map[string]any{"host": "h", "port": 40001, "user": "pgx"},
		"stats_updating": true, "stats_warning": "vacuumdb failed",
	}
	api.answers["GET /v1/branches/pgx"] = b
	api.answers["GET /v1/branches"] = map[string]any{"items": []any{b}}

	code, showOut, errOut := runAPI(t, api, "branch", "show", "pgx")
	if code != 0 || !strings.Contains(showOut, "updating in background") || !strings.Contains(showOut, "vacuumdb failed") {
		t.Fatalf("show code=%d err=%q out:\n%s", code, errOut, showOut)
	}
	code, listOut, errOut := runAPI(t, api, "branch", "list")
	if code != 0 || !strings.Contains(listOut, "ready (updating stats)") {
		t.Fatalf("list code=%d err=%q out:\n%s", code, errOut, listOut)
	}
}
