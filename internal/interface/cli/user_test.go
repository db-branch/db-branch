package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/interface/cli"
)

func runUserWithStdin(t *testing.T, api *fakeAPI, stdin string, args ...string) (int, string, string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(cfg, []byte("socket = \"/tmp/unused.sock\"\n"), 0o600)
	var out, errOut bytes.Buffer
	app := cli.App{
		Stdin:  strings.NewReader(stdin),
		Stdout: &out,
		Stderr: &errOut,
		Client: func(string) cli.Client { return api },
	}
	all := append([]string{"--config", cfg}, args...)
	code := app.Run(context.Background(), all)
	return code, out.String(), errOut.String()
}

func TestUser_CLI_Commands_TableAndJSON(t *testing.T) {
	api := newFakeAPI()
	api.answers["POST /v1/users"] = map[string]any{
		"user": map[string]any{
			"id":                   "user:alice@example.com",
			"email":                "alice@example.com",
			"name":                 "Alice",
			"kind":                 "user",
			"disabled":             false,
			"must_change_password": true,
			"created_at":           "2026-11-26T00:00:00Z",
			"updated_at":           "2026-11-26T00:00:00Z",
		},
		"initial_password": "generated-password-1234",
	}
	api.answers["GET /v1/users"] = map[string]any{
		"items": []any{
			map[string]any{"id": "admin", "email": "", "name": "admin", "kind": "admin", "disabled": false, "must_change_password": false, "created_at": "2026-11-26T00:00:00Z", "updated_at": "2026-11-26T00:00:00Z"},
			map[string]any{"id": "user:alice@example.com", "email": "alice@example.com", "name": "Alice", "kind": "user", "disabled": true, "must_change_password": true, "created_at": "2026-11-26T00:00:00Z", "updated_at": "2026-11-26T00:00:00Z"},
		},
		"next_cursor": nil,
	}
	api.answers["PATCH /v1/users/user:alice@example.com"] = map[string]any{
		"id": "user:alice@example.com", "email": "alice@example.com", "name": "Alice", "kind": "user", "disabled": true, "must_change_password": false,
	}
	api.answers["PATCH /v1/users/admin"] = map[string]any{
		"id": "admin", "email": "", "name": "admin", "kind": "admin", "disabled": false, "must_change_password": false,
	}
	api.answers["DELETE /v1/users/user:alice@example.com"] = map[string]any{}
	api.answers["POST /v1/users/user:alice@example.com/reset-password"] = map[string]any{
		"user":     map[string]any{"id": "user:alice@example.com", "email": "alice@example.com", "name": "Alice", "kind": "user", "disabled": false, "must_change_password": true},
		"password": "reset-password-1234567890",
	}

	// user add without --password-stdin displays one-time initial_password.
	code, out, errOut := runUserWithStdin(t, api, "", "user", "add", "Alice@Example.com", "--name", "Alice")
	if code != 0 || !strings.Contains(out, "generated-password-1234") {
		t.Fatalf("user add: code=%d out=%q err=%q", code, out, errOut)
	}
	if req := api.bodies["POST /v1/users"].(map[string]any); req["email"] != "Alice@Example.com" || req["name"] != "Alice" {
		t.Fatalf("unexpected POST /v1/users body: %+v", req)
	}

	// user add with --password-stdin and --must-change=false + -o json.
	code, out, errOut = runUserWithStdin(t, api, "stdin secret password\n", "-o", "json", "user", "add", "bob@example.com", "--password-stdin", "--must-change=false")
	if code != 0 || !strings.Contains(out, `"user:alice@example.com"`) {
		t.Fatalf("user add stdin json: code=%d out=%q err=%q", code, out, errOut)
	}
	if req := api.bodies["POST /v1/users"].(map[string]any); req["password"] != "stdin secret password" || req["must_change_password"] != false {
		t.Fatalf("unexpected stdin body: %+v", req)
	}

	// Empty stdin with --password-stdin fails.
	if code, _, _ := runUserWithStdin(t, api, "\n", "user", "add", "bob@example.com", "--password-stdin"); code == 0 {
		t.Fatal("expected error for empty stdin password")
	}

	// user list table + json.
	code, out, errOut = runUserWithStdin(t, api, "", "user", "list")
	if code != 0 || !strings.Contains(out, "ID") || !strings.Contains(out, "alice@example.com") || !strings.Contains(out, "disabled") {
		t.Fatalf("user list table: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = runUserWithStdin(t, api, "", "-o", "json", "user", "list")
	if code != 0 || !strings.Contains(out, `"admin"`) {
		t.Fatalf("user list json: code=%d out=%q err=%q", code, out, errOut)
	}

	// user disable / enable / reset-password / remove with <id-or-email> resolution.
	code, out, errOut = runUserWithStdin(t, api, "", "user", "disable", "alice@example.com")
	if code != 0 || !strings.Contains(out, "user:alice@example.com") {
		t.Fatalf("user disable: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = runUserWithStdin(t, api, "", "user", "enable", "admin")
	if code != 0 || !strings.Contains(out, "admin") {
		t.Fatalf("user enable: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = runUserWithStdin(t, api, "", "user", "reset-password", "user:alice@example.com")
	if code != 0 || !strings.Contains(out, "reset-password-1234567890") {
		t.Fatalf("user reset-password: code=%d out=%q err=%q", code, out, errOut)
	}
	code, _, errOut = runUserWithStdin(t, api, "", "-o", "json", "user", "reset-password", "alice@example.com", "--must-change=false")
	if code != 0 {
		t.Fatalf("user reset-password json: code=%d err=%q", code, errOut)
	}
	code, _, errOut = runUserWithStdin(t, api, "", "user", "remove", "alice@example.com")
	if code != 0 {
		t.Fatalf("user remove: code=%d err=%q", code, errOut)
	}

	// Invalid user identifier rejected before RPC.
	if code, _, _ := runUserWithStdin(t, api, "", "user", "disable", "not-an-email"); code == 0 {
		t.Fatal("expected error for invalid user identifier")
	}
	if code, _, _ := runUserWithStdin(t, api, "", "user", "remove", "not-an-email"); code == 0 {
		t.Fatal("expected error for invalid user identifier on remove")
	}
	if code, _, _ := runUserWithStdin(t, api, "", "user", "reset-password", "not-an-email"); code == 0 {
		t.Fatal("expected error for invalid user identifier on reset-password")
	}
	if code, out, _ := runUserWithStdin(t, api, "", "-o", "json", "user", "disable", "alice@example.com"); code != 0 || !strings.Contains(out, `"user:alice@example.com"`) {
		t.Fatalf("user disable json: code=%d out=%q", code, out)
	}

	// Remote client error & API error paths.
	var outBuf, errBuf bytes.Buffer
	appBadRemote := cli.App{
		Stdout: &outBuf,
		Stderr: &errBuf,
		RemoteClient: func(cli.RemoteOptions) (cli.Client, error) {
			return nil, os.ErrInvalid
		},
	}
	for _, args := range [][]string{
		{"--server", "https://127.0.0.1:7080", "user", "add", "a@example.com"},
		{"--server", "https://127.0.0.1:7080", "user", "list"},
		{"--server", "https://127.0.0.1:7080", "user", "disable", "a@example.com"},
		{"--server", "https://127.0.0.1:7080", "user", "remove", "a@example.com"},
		{"--server", "https://127.0.0.1:7080", "user", "reset-password", "a@example.com"},
	} {
		if c := appBadRemote.Run(context.Background(), args); c == 0 {
			t.Fatalf("expected client error for %v", args)
		}
	}
	if c := (&cli.App{Stdin: nil, Stdout: &outBuf, Stderr: &errBuf}).Run(context.Background(), []string{"user", "add", "a@example.com", "--password-stdin"}); c == 0 {
		t.Fatal("expected error when stdin is nil")
	}
	errAPI := newFakeAPI()
	for _, args := range [][]string{
		{"user", "add", "a@example.com"},
		{"user", "list"},
		{"user", "disable", "a@example.com"},
		{"user", "remove", "a@example.com"},
		{"user", "reset-password", "a@example.com"},
	} {
		if c, _, _ := runUserWithStdin(t, errAPI, "", args...); c == 0 {
			t.Fatalf("expected API error for %v", args)
		}
	}
}
