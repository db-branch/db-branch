package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchFork_既存ブランチから子ブランチをフォークして表示する(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/parent/fork"] = accepted("op_fk", "branch.fork")
	api.opStates = []map[string]any{
		{"id": "op_fk", "state": "running", "phase": "cloning"},
		{"id": "op_fk", "state": "succeeded"},
	}
	api.answers["GET /v1/branches/child"] = map[string]any{
		"name": "child", "status": "ready", "base": "prod", "snapshot": "s1",
		"parent_branch": "parent", "parent_snapshot": "ckpt-cp1",
		"runtime": "mysql@8.4.13", "base_runtime": "mysql@8.4.13", "node": "node1",
		"connection": map[string]any{"host": "192.0.2.10", "port": 40002, "user": "child"},
	}

	sqlFile := filepath.Join(t.TempDir(), "init.sql")
	if err := os.WriteFile(sqlFile, []byte("DELETE FROM tmp;"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runAPI(t, api, "branch", "fork", "alice/parent", "child",
		"--checkpoint", "cp1", "--clean", "--init-file", sqlFile, "--ttl", "24h",
		"--node", "node1", "--owner", "sso:alice", "--access", "open", "--record")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	body := api.bodies["POST /v1/branches/parent/fork"].(map[string]any)
	if body["name"] != "child" || body["checkpoint"] != "cp1" || body["clean"] != true ||
		body["init_sql"] != "DELETE FROM tmp;" || body["ttl"] != "24h" || body["node"] != "node1" ||
		body["owner"] != "sso:alice" || body["access"] != "open" || body["record"] != true {
		t.Fatalf("body = %#v", body)
	}
	if !strings.Contains(out, "Parent:") || !strings.Contains(out, "parent @ ckpt-cp1") {
		t.Errorf("show output lacks parent info:\n%s", out)
	}
}

func TestBranchFork_noWaitと入力バリデーション(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/branches/parent/fork"] = accepted("op_fk2", "branch.fork")
	code, out, errOut := runAPI(t, api, "branch", "fork", "parent", "child", "--no-wait", "--init-sql", "SELECT 1;")
	if code != 0 || strings.TrimSpace(out) != "op_fk2" {
		t.Fatalf("no-wait: exit=%d out=%q err=%q", code, out, errOut)
	}
	// Invalid source name, invalid target name, invalid access, missing init-file.
	for _, args := range [][]string{
		{"branch", "fork", "BAD", "child"},
		{"branch", "fork", "parent", "BAD"},
		{"branch", "fork", "parent", "child", "--access", "public"},
		{"branch", "fork", "parent", "child", "--init-file", "/nonexistent/init.sql"},
	} {
		if code, _, _ := runAPI(t, api, args...); code == 0 {
			t.Errorf("args %v: want non-zero exit", args)
		}
	}
}
