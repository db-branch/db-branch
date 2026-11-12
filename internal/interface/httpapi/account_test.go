package httpapi_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/token"
)

func TestAccountTokens_ログインした本人が自分用のトークンを作り使い消せる(t *testing.T) {
	f := newFixture(t)
	tokens := &token.Service{Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "t.json")}, Clock: time.Now, Settle: time.Millisecond}
	f.srv.Tokens, f.srv.TokenAuth = tokens, tokens
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)
	sess := withSession(cookie, csrf)

	resp, body := send(t, h, "POST", "/v1/account/tokens", map[string]any{"name": "claude-code", "ttl": "720h"}, sess)
	if resp.StatusCode != 201 || !strings.HasPrefix(body["token"].(string), "dbb_") || body["scope"] != "branches" {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	secret, id := body["token"].(string), body["id"].(string)
	_, _, _ = tokens.Create(ctx, "ci", "full", 0)
	if _, body := send(t, h, "GET", "/v1/account/tokens", nil, sess); len(body["items"].([]any)) != 1 {
		t.Errorf("mine: %v", body)
	}
	if _, body := send(t, h, "GET", "/v1/tokens", nil, sess); len(body["items"].([]any)) != 1 || body["items"].([]any)[0].(map[string]any)["name"] != "ci" {
		t.Errorf("service tokens only: %v", body)
	}
	bearer := as(secret)
	// The token acts as its owner: a branch it makes is the owner's.
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "via-mcp", "base": "prod"}, bearer); resp.StatusCode != 202 {
		t.Errorf("branch via token: %d", resp.StatusCode)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("via-mcp"); return ok })
	if b, _ := f.branches.get("via-mcp"); b.Owner != "admin" {
		t.Errorf("owner %q", b.Owner)
	}
	// ... but cannot make more tokens, or act as an admin.
	if resp, _ := send(t, h, "POST", "/v1/account/tokens", map[string]any{"name": "more"}, bearer); resp.StatusCode != 403 {
		t.Errorf("token makes a token: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/settings", nil, bearer); resp.StatusCode != 403 {
		t.Errorf("token reads settings: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "DELETE", "/v1/account/tokens/nope", nil, sess); resp.StatusCode != 404 {
		t.Errorf("someone else's: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "DELETE", "/v1/account/tokens/"+id, nil, sess); resp.StatusCode != 204 {
		t.Errorf("revoke: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, bearer); resp.StatusCode != 401 {
		t.Errorf("revoked token: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/account/tokens", map[string]any{"name": ""}, sess); resp.StatusCode != 400 {
		t.Errorf("no name: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/account/tokens", map[string]any{"name": "x", "ttl": "soon"}, sess); resp.StatusCode != 400 {
		t.Errorf("bad ttl: %d", resp.StatusCode)
	}
	f.srv.Tokens = nil
	if resp, _ := send(t, h, "GET", "/v1/account/tokens", nil, sess); resp.StatusCode != 503 {
		t.Errorf("no tokens: %d", resp.StatusCode)
	}
}

func TestPreferences_言語は本人ごとに保存されログインで返る(t *testing.T) {
	f := newFixture(t)
	f.srv.Preferences = &filestore.Store{Path: filepath.Join(t.TempDir(), "p.json")}
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)
	sess := withSession(cookie, csrf)
	if _, body := send(t, h, "GET", "/v1/account/preferences", nil, sess); body["language"] != "" {
		t.Errorf("default: %v", body)
	}
	if resp, _ := send(t, h, "PUT", "/v1/account/preferences", map[string]any{"language": "ja"}, sess); resp.StatusCode != 200 {
		t.Errorf("set: %d", resp.StatusCode)
	}
	if _, body := send(t, h, "GET", "/v1/auth/session", nil, sess); body["language"] != "ja" {
		t.Errorf("session: %v", body)
	}
	if resp, _ := send(t, h, "PUT", "/v1/account/preferences", map[string]any{"language": "fr"}, sess); resp.StatusCode != 400 {
		t.Errorf("unknown language: %d", resp.StatusCode)
	}
	_, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil)
	if body["language"] != "ja" {
		t.Errorf("login: %v", body)
	}
	f.srv.Preferences = nil
	if resp, _ := send(t, h, "GET", "/v1/account/preferences", nil, sess); resp.StatusCode != 503 {
		t.Errorf("no store: %d", resp.StatusCode)
	}
}
