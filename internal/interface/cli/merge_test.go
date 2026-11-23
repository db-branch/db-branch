package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/interface/cli"
)

func mergeOp(state, status string, extra map[string]any) map[string]any {
	res := map[string]any{
		"branch":      "x",
		"source_line": 0,
		"target_line": 1,
		"ancestor":    "cp0",
		"checkpoint":  "before-merge-20261123-120000",
		"status":      status,
		"inserted":    3,
		"updated":     1,
		"deleted":     0,
		"tables": []any{
			map[string]any{"database": "shop", "table": "orders", "inserted": 3, "updated": 1, "deleted": 0},
		},
	}
	for k, v := range extra {
		res[k] = v
	}
	op := map[string]any{"id": "op_m1", "kind": "branch.merge", "state": state, "result": res}
	if state == "failed" && op["error"] == nil {
		if code, _ := res["code"].(string); code == "merge_conflict" {
			op["error"] = "merge conflict: 1 conflict(s) detected"
		} else {
			op["error"] = "merge failed"
		}
	}
	return op
}

func TestBranchMerge_ラインやチェックポイントからマージできサマリとロールバック案内を出す(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/merge"] = accepted("op_m1", "branch.merge")
	api.opStates = []map[string]any{{"id": "op_m1", "state": "running", "phase": "diffing"}, mergeOp("succeeded", "merged", nil)}

	code, out, errOut := runAPI(t, api, "branch", "merge", "alice/x", "--line", "0", "--tables", "shop.orders,shop.items", "--allow-ddl", "--ignore-triggers")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	body := api.bodies["POST /v1/branches/x/merge"].(map[string]any)
	if body["source_line"] != 0 || body["allow_ddl"] != true || body["ignore_triggers"] != true || body["dry_run"] != false {
		t.Errorf("body = %#v", body)
	}
	if tables, ok := body["tables"].([]string); !ok || len(tables) != 2 || tables[0] != "shop.orders" || tables[1] != "shop.items" {
		t.Errorf("tables = %#v", body["tables"])
	}
	for _, want := range []string{"shop.orders", "+3 ~1 -0", "Merged line 0 into x", "db-branch branch restore x before-merge-20261123-120000"} {
		if !strings.Contains(out, want) {
			t.Errorf("out lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "diffing...") {
		t.Errorf("errOut = %q", errOut)
	}
}

func TestBranchMerge_ドライランとチェックポイント指定とJSON出力(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/merge"] = accepted("op_m1", "branch.merge")
	api.opStates = []map[string]any{mergeOp("succeeded", "dry_run", map[string]any{"dry_run": true, "checkpoint": "", "source_checkpoint": "cp2"})}

	code, out, _ := runAPI(t, api, "branch", "merge", "x", "--checkpoint", "cp2", "--dry-run")
	if code != 0 || !strings.Contains(out, "Dry run") || !strings.Contains(out, "nothing was changed") {
		t.Errorf("dry-run: exit=%d out=\n%s", code, out)
	}
	body := api.bodies["POST /v1/branches/x/merge"].(map[string]any)
	if body["checkpoint"] != "cp2" || body["dry_run"] != true {
		t.Errorf("body = %#v", body)
	}

	api.opStates = []map[string]any{mergeOp("succeeded", "merged", nil)}
	code, out, _ = runAPI(t, api, "-o", "json", "branch", "merge", "x", "--line", "0")
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 0 || res["status"] != "merged" {
		t.Errorf("json: exit=%d out=%q err=%v", code, out, err)
	}
}

func TestBranchMerge_競合時は詳細を出して終了コード2を返す(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/merge"] = accepted("op_m1", "branch.merge")
	api.opStates = []map[string]any{mergeOp("failed", "conflicted", map[string]any{
		"code":            "merge_conflict",
		"total_conflicts": 1,
		"conflicts": []any{
			map[string]any{"type": "update_update", "database": "shop", "table": "orders", "key": map[string]any{"id": 42}, "message": "both updated amount"},
		},
	})}

	code, out, _ := runAPI(t, api, "branch", "merge", "x", "--line", "0")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	for _, want := range []string{"update_update", "shop.orders", "both updated amount"} {
		if !strings.Contains(out, want) {
			t.Errorf("out lacks %q:\n%s", want, out)
		}
	}
}

func TestBranchMerge_入力検証とnoWaitとその他エラー(t *testing.T) {
	api := newFakeAPI()
	for _, args := range [][]string{
		{"branch", "merge", "x"},
		{"branch", "merge", "x", "--line", "0", "--checkpoint", "cp1"},
		{"branch", "merge", "x", "--line", "-1"},
		{"branch", "merge", "BAD", "--line", "0"},
	} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("args %v: want non-zero exit", args)
		}
	}
	if len(api.requests) != 0 {
		t.Errorf("requests sent on invalid args: %v", api.requests)
	}

	api.answers["POST /v1/branches/x/merge"] = accepted("op_m1", "branch.merge")
	code, out, _ := runAPI(t, api, "branch", "merge", "x", "--line", "0", "--no-wait")
	if code != 0 || strings.TrimSpace(out) != "op_m1" {
		t.Errorf("no-wait: exit=%d out=%q", code, out)
	}

	api.opStates = []map[string]any{mergeOp("failed", "rolled_back", map[string]any{"code": "triggers_not_supported", "error": "triggers present on shop.orders"})}
	code, out, _ = runAPI(t, api, "branch", "merge", "x", "--line", "0")
	if code != 1 || !strings.Contains(out, "rolled back") {
		t.Errorf("non-conflict failure: exit=%d out=%q", code, out)
	}

	api2 := newFakeAPI()
	api2.errors["POST /v1/branches/x/merge"] = &cli.APIError{Status: 400, Code: "invalid_request", Message: "source matches live line"}
	if code, _, errOut := runAPI(t, api2, "branch", "merge", "x", "--line", "0"); code == 0 || !strings.Contains(errOut, "source matches") {
		t.Errorf("api error: exit=%d err=%q", code, errOut)
	}
}
