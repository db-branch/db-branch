package cli_test

import (
	"slices"
	"strings"
	"testing"
)

func TestClientOnly_ノード管理コマンドを外しbranchとbaseReadとversionを動かす(t *testing.T) {
	cfgPath := t.TempDir() + "/client.toml"
	osKC := &memSecretBackend{available: true}
	api := newFakeAPI()
	api.answers["GET /v1/branches"] = map[string]any{
		"items": []any{
			map[string]any{"name": "dev", "status": "ready", "base": "prod", "runtime": "mysql@8.4.13", "node": "n1", "connection": map[string]any{"port": 40001}},
		},
	}
	api.answers["POST /v1/branches"] = accepted("op_1", "branch.create")
	api.opStates = []map[string]any{{"id": "op_1", "state": "succeeded"}}
	api.answers["GET /v1/branches/dev"] = map[string]any{
		"name": "dev", "status": "ready", "base": "prod", "runtime": "mysql@8.4.13", "node": "n1",
		"connection": map[string]any{"host": "10.0.0.95", "port": 40001, "user": "branch"},
	}
	api.answers["GET /v1/bases"] = map[string]any{
		"items": []any{
			map[string]any{"name": "prod", "status": "ready", "runtime": "mysql@8.4.13", "node": "n1"},
		},
	}
	api.answers["GET /v1/bases/prod"] = map[string]any{
		"name": "prod", "status": "ready", "runtime": "mysql@8.4.13", "port": 3306, "node": "n1",
	}

	r := &fakeRemoteRunner{api: api}
	env := map[string]string{"DB_BRANCH_URL": "https://10.0.0.95:7080", "DB_BRANCH_TOKEN": "dbb_tok"}

	// Help must NOT list node administration commands or base write subcommands.
	code, out, errOut := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "--help")
	if code != 0 {
		t.Fatalf("--help exit %d: %s", code, errOut)
	}
	for _, forbidden := range []string{"agent", "init", "admin", "cluster", "module", "update", "settings", "token", "backup", "route", "runtime"} {
		if strings.Contains(out, "\n  "+forbidden+" ") {
			t.Errorf("ClientOnly --help unexpectedly lists node command %q:\n%s", forbidden, out)
		}
	}
	for _, removed := range []string{"init", "agent", "admin", "cluster", "module", "update", "settings", "token", "backup", "route", "runtime"} {
		if c, _, _ := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, removed); c == 0 {
			t.Errorf("ClientOnly must not register %q", removed)
		}
	}

	// base --help must list list/show and must NOT list create/delete/snapshot/start/stop.
	code, baseHelp, _ := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "base", "--help")
	if code != 0 {
		t.Fatalf("base --help exit %d", code)
	}
	for _, sub := range []string{"create", "snapshot", "start", "stop", "delete", "refresh", "replicas", "anonymous"} {
		if strings.Contains(baseHelp, "\n  "+sub+" ") {
			t.Errorf("ClientOnly base --help unexpectedly lists %q:\n%s", sub, baseHelp)
		}
	}
	if c, _, _ := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "base", "delete", "prod"); c == 0 {
		t.Error("ClientOnly must not expose base delete")
	}

	// version (text + json)
	if c, vOut, _ := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "version"); c != 0 || vOut != "db-branch 1.0.0\n" {
		t.Errorf("version = (%d, %q)", c, vOut)
	}
	if c, vJSON, _ := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "version", "-o", "json"); c != 0 || !strings.Contains(vJSON, `"version": "1.0.0"`) {
		t.Errorf("version json = (%d, %q)", c, vJSON)
	}

	// base list & show work over HTTPS.
	if c, bOut, eOut := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "base", "list"); c != 0 || !strings.Contains(bOut, "prod") {
		t.Errorf("base list = (%d, %q, %q)", c, bOut, eOut)
	}
	if c, bShow, eOut := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "base", "show", "prod"); c != 0 || !strings.Contains(bShow, "mysql@8.4.13") {
		t.Errorf("base show = (%d, %q, %q)", c, bShow, eOut)
	}

	// branch create / list / show work over HTTPS.
	if c, _, eOut := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "branch", "create", "dev", "--base", "prod"); c != 0 {
		t.Fatalf("branch create = (%d, %s)", c, eOut)
	}
	if c, lOut, eOut := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, "branch", "list", "-o", "json"); c != 0 || !strings.Contains(lOut, `"name": "dev"`) {
		t.Errorf("branch list json = (%d, %q, %q)", c, lOut, eOut)
	}
	// Remaining branch lifecycle subcommands work over HTTPS.
	api.answers["GET /v1/branches/dev/dsn"] = map[string]any{"dsn": "mysql://u:p@10.0.0.95:40001/"}
	api.answers["PATCH /v1/branches/dev"] = map[string]any{"name": "dev", "expires_at": "2026-12-01T00:00:00Z"}
	api.answers["POST /v1/branches/dev/reset"] = accepted("op_r", "branch.reset")
	api.answers["POST /v1/branches/dev/pause"] = accepted("op_p", "branch.pause")
	api.answers["POST /v1/branches/dev/resume"] = accepted("op_u", "branch.resume")
	api.answers["POST /v1/branches/dev/fork"] = accepted("op_f", "branch.fork")
	api.answers["GET /v1/branches/dev2"] = api.answers["GET /v1/branches/dev"]
	api.answers["POST /v1/branches/dev/checkpoints"] = map[string]any{"name": "cp1"}
	api.answers["GET /v1/branches/dev/checkpoints"] = map[string]any{"items": []any{map[string]any{"name": "cp1", "created_at": "2026-11-26T00:00:00Z", "size": 1024, "line": 1}}}
	api.answers["POST /v1/branches/dev/checkpoints/cp1/restore"] = accepted("op_cp", "branch.restore")
	api.answers["POST /v1/branches/dev/merge"] = accepted("op_m", "branch.merge")
	api.answers["DELETE /v1/branches/dev"] = accepted("op_d", "branch.delete")
	for _, sub := range [][]string{
		{"branch", "show", "dev"},
		{"branch", "dsn", "dev"},
		{"branch", "extend", "dev", "24h"},
		{"branch", "reset", "dev"},
		{"branch", "pause", "dev"},
		{"branch", "resume", "dev"},
		{"branch", "fork", "dev", "dev2"},
		{"branch", "checkpoint", "dev", "--name", "cp1"},
		{"branch", "checkpoints", "dev"},
		{"branch", "restore", "dev", "cp1"},
		{"branch", "merge", "dev", "--checkpoint", "cp1", "--dry-run"},
		{"branch", "delete", "dev"},
	} {
		if c, _, eOut := runRemoteCLI(t, cfgPath, osKC, env, "", false, r, sub...); c != 0 {
			t.Errorf("%v failed (%d): %s", sub, c, eOut)
		}
	}
	if !slices.Contains(api.requests, "POST /v1/branches") || !slices.Contains(api.requests, "GET /v1/bases") {
		t.Errorf("unexpected API requests: %v", api.requests)
	}
}
