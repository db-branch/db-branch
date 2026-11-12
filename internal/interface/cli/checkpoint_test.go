package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func checkpointAPI() *fakeAPI {
	api := newFakeAPI()
	api.answers["GET /v1/branches/x/checkpoints"] = map[string]any{"items": []any{
		map[string]any{"name": "c1", "created_at": "2026-11-12T10:00:00Z", "size": 8192, "line": 0, "auto": false, "current": true},
		map[string]any{"name": "before-restore-20261112-101500", "created_at": "2026-11-12T10:15:00Z", "size": 1048576, "line": 1, "auto": true, "current": false},
	}}
	return api
}

func TestBranchCheckpoint_名前を付けて保存する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/checkpoints"] = map[string]any{"name": "before-migration", "created_at": "2026-11-12T10:00:00Z"}

	code, out, errOut := runAPI(t, api, "branch", "checkpoint", "x", "--name", "before-migration")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if body := api.bodies["POST /v1/branches/x/checkpoints"].(map[string]any); body["name"] != "before-migration" {
		t.Errorf("body = %v", body)
	}
	if !strings.Contains(out, "before-migration") {
		t.Errorf("output = %q", out)
	}
}

func TestBranchCheckpoint_名前を省くと空で依頼する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/checkpoints"] = map[string]any{"name": "20261112-100000"}
	code, out, _ := runAPI(t, api, "branch", "checkpoint", "alice/x")
	if code != 0 || !strings.Contains(out, "20261112-100000") {
		t.Errorf("exit %d out %q", code, out)
	}
	if body := api.bodies["POST /v1/branches/x/checkpoints"].(map[string]any); body["name"] != "" {
		t.Errorf("body = %v", body)
	}
}

func TestBranchCheckpoints_一覧を表とJSONで出す(t *testing.T) {
	api := checkpointAPI()
	code, out, errOut := runAPI(t, api, "branch", "checkpoints", "x")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"NAME", "CREATED", "SIZE", "LINE", "c1", "8.0 KiB", "current", "before-restore-20261112-101500", "1.0 MiB", "auto"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = runAPI(t, api, "branch", "checkpoints", "x", "-o", "json")
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || len(parsed) != 2 || parsed[0]["name"] != "c1" {
		t.Errorf("json = %q (%v)", out, err)
	}
	empty := newFakeAPI()
	empty.answers["GET /v1/branches/x/checkpoints"] = map[string]any{"items": []any{}}
	if _, out, _ := runAPI(t, empty, "branch", "checkpoints", "x"); !strings.Contains(out, "No checkpoints") {
		t.Errorf("empty = %q", out)
	}
}

func TestBranchRestore_段階を表示して戻し何も失われないと伝える(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/checkpoints/c1/restore"] = accepted("op_5", "branch.restore")
	api.opStates = []map[string]any{
		{"id": "op_5", "state": "running", "phase": "stopping"},
		{"id": "op_5", "state": "succeeded"},
	}
	code, out, errOut := runAPI(t, api, "branch", "restore", "x", "c1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "stopping") || !strings.Contains(out, "Restored x to c1") || !strings.Contains(out, "before-restore") {
		t.Errorf("out %q err %q", out, errOut)
	}
	code, out, _ = runAPI(t, api, "branch", "restore", "x", "c1", "--no-wait")
	if code != 0 || strings.TrimSpace(out) != "op_5" {
		t.Errorf("no-wait: %d %q", code, out)
	}
}

func TestBranchCheckpointDelete_チェックポイントを消す(t *testing.T) {
	api := newFakeAPI()
	api.answers["DELETE /v1/branches/x/checkpoints/c1"] = map[string]any{}
	code, out, errOut := runAPI(t, api, "branch", "checkpoint", "delete", "x", "c1")
	if code != 0 || !strings.Contains(out, "Deleted checkpoint c1") {
		t.Errorf("exit %d out %q err %q", code, out, errOut)
	}
	if code, _, _ := runAPI(t, api, "branch", "checkpoint", "delete", "x", "Bad Name"); code == 0 {
		t.Error("an invalid checkpoint name should fail")
	}
}

func TestBranchCheckpoint_不正な名前とAPIの失敗は失敗にする(t *testing.T) {
	api := newFakeAPI()
	if code, _, _ := runAPI(t, api, "branch", "checkpoint", "x", "--name", "Bad Name"); code == 0 {
		t.Error("an invalid name should fail")
	}
	if code, _, _ := runAPI(t, api, "branch", "restore", "x", "Bad Name"); code == 0 {
		t.Error("an invalid checkpoint should fail")
	}
	if code, _, _ := runAPI(t, api, "branch", "checkpoint", "x"); code == 0 {
		t.Error("an API failure should fail")
	}
	if code, _, _ := runAPI(t, api, "branch", "checkpoints", "x"); code == 0 {
		t.Error("an API failure should fail")
	}
}
