package cli_test

import (
	"encoding/json"
	"github.com/db-branch/db-branch/internal/interface/cli"
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

func TestBranchRestore_段階を表示して戻し今の状態は保存されないと先に伝える(t *testing.T) {
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
	if !strings.Contains(errOut, "stopping") || !strings.Contains(out, "Restored x to c1") || strings.Contains(out, "before-restore") {
		t.Errorf("out %q err %q", out, errOut)
	}
	// The note comes before the work, on standard error only, and says how to keep the state.
	note := "The current state of x is replaced and is not saved. To keep it, save a checkpoint first: db-branch branch checkpoint x"
	if !strings.Contains(errOut, note) || strings.Index(errOut, note) > strings.Index(errOut, "stopping") || strings.Contains(out, "not saved") {
		t.Errorf("the note is missing, late or on standard output: out %q err %q", out, errOut)
	}
	code, out, errOut = runAPI(t, api, "branch", "restore", "x", "c1", "--no-wait")
	if code != 0 || strings.TrimSpace(out) != "op_5" || !strings.Contains(errOut, note) {
		t.Errorf("no-wait: %d %q %q", code, out, errOut)
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

func TestCheckpoint_ストレージが対応しないときは一行で伝える(t *testing.T) {
	unavailable := &cli.APIError{Status: 409, Code: "checkpoints_unavailable", Message: "conflict: checkpoints are not available on this storage (long server text)"}
	for _, c := range []struct {
		method, path string
		args         []string
	}{
		{"POST", "/v1/branches/x/checkpoints", []string{"branch", "checkpoint", "x"}},
		{"GET", "/v1/branches/x/checkpoints", []string{"branch", "checkpoints", "x"}},
		{"DELETE", "/v1/branches/x/checkpoints/c1", []string{"branch", "checkpoint", "delete", "x", "c1"}},
		{"POST", "/v1/branches/x/checkpoints/c1/restore", []string{"branch", "restore", "x", "c1"}},
	} {
		api := newFakeAPI()
		api.errors[c.method+" "+c.path] = unavailable
		code, _, errOut := runAPI(t, api, c.args...)
		// The note a restore prints before it starts is not part of the error.
		var kept []string
		for _, l := range strings.Split(strings.TrimSpace(errOut), "\n") {
			if !strings.HasPrefix(l, "The current state of") {
				kept = append(kept, l)
			}
		}
		errOut = strings.Join(kept, "\n")
		if code == 0 || !strings.Contains(errOut, "Checkpoints are not available on this storage") || strings.Count(strings.TrimSpace(errOut), "\n") != 0 {
			t.Errorf("%v: exit %d, stderr %q", c.args, code, errOut)
		}
	}
}

func TestBranchCheckpoint_空き容量の警告は標準エラーに出す(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/x/checkpoints"] = map[string]any{"name": "c1", "warning": "This branch uses more than half of the pool's free space."}
	code, out, errOut := runAPI(t, api, "branch", "checkpoint", "x", "--name", "c1")
	if code != 0 || !strings.Contains(out, "Saved checkpoint c1") || strings.Contains(out, "half") {
		t.Errorf("exit %d out %q", code, out)
	}
	if !strings.Contains(errOut, "Warning: This branch uses more than half") {
		t.Errorf("stderr = %q", errOut)
	}
}
