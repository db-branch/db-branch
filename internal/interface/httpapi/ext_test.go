package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

type moduleAPI struct{ got []ext.Request }

func (m *moduleAPI) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	m.got = append(m.got, r)
	switch r.Path {
	case "/ui/":
		return ext.Response{ContentType: "text/html", Body: []byte("<h1>audit</h1>")}, nil
	case "/boom":
		return ext.Response{}, errors.New("module failed")
	case "/denied":
		return ext.Response{}, ext.ErrForbidden
	}
	return ext.Response{Status: 201, Body: []byte(`{"path":"` + r.Path + `","who":"` + r.Principal.ID + `"}`)}, nil
}

func TestExt_モジュールのAPIへ利用者と一緒に渡し書き込みは監査する(t *testing.T) {
	f := newFixture(t)
	api := &moduleAPI{}
	sink := &recordingSink{}
	f.srv.ExtHandlers = map[string]ext.Handler{"audit": api}
	f.srv.Ext = ext.Module{AuditSink: sink}
	cookie, csrf := login(t, f)
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/ext/audit/events/search?q=x", map[string]any{"a": 1}, withSession(cookie, csrf))
	if resp.StatusCode != 201 || body["path"] != "/events/search" || body["who"] != "admin" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if g := api.got[0]; g.Method != "POST" || g.RawQuery != "q=x" || string(g.Body) != `{"a":1}` {
		t.Errorf("request = %+v", g)
	}
	if got := sink.actions(); len(got) != 2 || got[1] != "ext.audit.write:admin:ext/audit:ok" {
		t.Errorf("audit = %v", got)
	}
	// Pages may be framed by the console; reads are not audited.
	resp, _ = send(t, f.srv.Handler(false), "GET", "/v1/ext/audit/ui/", nil, withSession(cookie, ""))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html" || resp.Header.Get("X-Frame-Options") != "SAMEORIGIN" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'self'") || len(sink.events) != 2 {
		t.Errorf("page: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, body := f.do(t, "GET", "/v1/ext/audit/boom", nil); resp.StatusCode != 500 {
		t.Errorf("module error: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "GET", "/v1/ext/audit/denied", nil); resp.StatusCode != 403 {
		t.Errorf("module denial: %d", resp.StatusCode)
	}
	if resp, body := f.do(t, "GET", "/v1/ext/nothing/x", nil); resp.StatusCode != 404 || errCode(body) != "not_found" {
		t.Errorf("unknown module: %d %v", resp.StatusCode, body)
	}
	big := strings.Repeat("x", 1<<20+1)
	if resp, _ := f.do(t, "PUT", "/v1/ext/audit/x", big); resp.StatusCode != 413 {
		t.Errorf("big body: %d", resp.StatusCode)
	}
}

func TestExt_モジュールの認可と範囲付きトークンが効く(t *testing.T) {
	f := newFixture(t)
	f.srv.ExtHandlers = map[string]ext.Handler{"auth": &moduleAPI{}}
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}}
	bearer := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}
	if resp, _ := send(t, f.srv.Handler(false), "GET", "/v1/ext/auth/users", nil, bearer("t-viewer")); resp.StatusCode != 201 {
		t.Errorf("viewer read: %d", resp.StatusCode)
	}
	if resp, _ := send(t, f.srv.Handler(false), "POST", "/v1/ext/auth/users", nil, bearer("t-viewer")); resp.StatusCode != 403 {
		t.Errorf("viewer write: %d", resp.StatusCode)
	}
}

func TestExt_ノード指定でそのノードのモジュールに頼む(t *testing.T) {
	f, c := remoteFixture(t)
	f.srv.ExtHandlers = map[string]ext.Handler{"audit": &moduleAPI{}}
	resp, _ := f.do(t, "GET", "/v1/ext/audit/events?node=n2", nil)
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 GET /v1/ext/audit/events") {
		t.Errorf("%d %v", resp.StatusCode, c.forwarded)
	}
	if resp, _ := f.do(t, "GET", "/v1/ext/audit/events?node=node1", nil); resp.StatusCode != 201 {
		t.Errorf("this node: %d", resp.StatusCode)
	}
}

func TestCapabilities_モジュールのページを並べる(t *testing.T) {
	f := newFixture(t)
	f.srv.ExtPages = []httpapi.ModulePage{{Module: "auth", Path: "/ui/", Title: "Users"}, {Module: "audit", Path: "/ui/", Title: "Audit log"}}
	_, body := f.do(t, "GET", "/v1/capabilities", nil)
	pages := body["pages"].([]any)
	if len(pages) != 2 || pages[0].(map[string]any)["module"] != "audit" || pages[1].(map[string]any)["title"] != "Users" {
		t.Errorf("pages = %v", pages)
	}
}

// ssoAPI redirects from its public paths and sets its own cookie.
type ssoAPI struct{ got []ext.Request }

func (m *ssoAPI) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	m.got = append(m.got, r)
	return ext.Response{Status: 302, Headers: map[string]string{
		"Location":   "https://idp.example.com/authorize?state=s1",
		"Set-Cookie": "dbbx_auth_state=s1; Path=/v1/ext/auth/; HttpOnly; Secure; SameSite=Lax",
	}}, nil
}

func TestExt_公開パスはログインなしで届きリダイレクトとモジュールのクッキーを返せる(t *testing.T) {
	f := newFixture(t)
	api := &ssoAPI{}
	f.srv.ExtHandlers = map[string]ext.Handler{"auth": api}
	f.srv.ExtPublic = map[string][]string{"auth": {"/sso/"}}
	f.srv.LoginMethods = []httpapi.LoginMethod{{Module: "auth", Label: "Sign in with SSO", Path: "/sso/start"},
		{Module: "auth", Label: "Off", Path: "/x", StatusPath: "/sso/status"}, {Module: "gone", Label: "Gone", Path: "/x", StatusPath: "/s"}}
	h := f.srv.Handler(false)
	req := func(path string, cookies ...*http.Cookie) *http.Response {
		r, _ := http.NewRequest("GET", path, nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		resp, _ := send(t, h, "GET", path, nil, func(x *http.Request) {
			for _, c := range cookies {
				x.AddCookie(c)
			}
		})
		return resp
	}
	resp := req("/v1/ext/auth/sso/start", &http.Cookie{Name: "dbbx_auth_state", Value: "old"}, &http.Cookie{Name: "dbb_session", Value: "x"}, &http.Cookie{Name: "dbbx_other_x", Value: "y"})
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "https://idp.example.com/authorize?state=s1" || !strings.HasPrefix(resp.Header.Get("Set-Cookie"), "dbbx_auth_state=s1") {
		t.Errorf("start: %d %v", resp.StatusCode, resp.Header)
	}
	if g := api.got[0]; g.Principal.ID != "anonymous" || g.Cookies["dbbx_auth_state"] != "old" || len(g.Cookies) != 1 {
		t.Errorf("request %+v", g)
	}
	if resp := req("/v1/ext/auth/users"); resp.StatusCode != 401 {
		t.Errorf("a non-public path without login: %d", resp.StatusCode)
	}
	resp, body := send(t, h, "GET", "/v1/auth/methods", nil, nil)
	m := body["items"].([]any)[0].(map[string]any)
	if resp.StatusCode != 200 || m["label"] != "Sign in with SSO" || m["url"] != "/v1/ext/auth/sso/start" {
		t.Errorf("methods: %d %v", resp.StatusCode, body)
	}
	if v, ok := body["next_cursor"]; !ok || v != nil {
		t.Errorf("auth/methods next_cursor = %v (present=%v), want null", v, ok)
	}
}

// badHeaders tries to set what a module must not.
type badHeaders struct{}

func (badHeaders) Serve(context.Context, ext.Request) (ext.Response, error) {
	return ext.Response{Status: 200, Headers: map[string]string{"Set-Cookie": "dbb_session=stolen; Path=/", "X-Frame-Options": "ALLOW", "Cache-Control": "no-store"}}, nil
}

func TestExt_モジュールは本体のクッキーや安全のヘッダーを変えられない(t *testing.T) {
	f := newFixture(t)
	f.srv.ExtHandlers = map[string]ext.Handler{"auth": badHeaders{}}
	resp, _ := f.do(t, "GET", "/v1/ext/auth/x", nil)
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Frame-Options") == "ALLOW" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", resp.Header)
	}
}

// statusAPI says whether its login method is set up.
type statusAPI struct{ enabled bool }

func (a statusAPI) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	if a.enabled {
		return ext.Response{Status: 200, Body: []byte(`{"enabled":true}`)}, nil
	}
	return ext.Response{Status: 200, Body: []byte(`{"enabled":false}`)}, nil
}

func TestLoginMethods_設定済みのものだけ出す(t *testing.T) {
	f := newFixture(t)
	f.srv.LoginMethods = []httpapi.LoginMethod{{Module: "sso", Label: "SSO", Path: "/sso/start", StatusPath: "/sso/status"}}
	for _, on := range []bool{false, true} {
		f.srv.ExtHandlers = map[string]ext.Handler{"sso": statusAPI{on}}
		_, body := send(t, f.srv.Handler(false), "GET", "/v1/auth/methods", nil, nil)
		if n := len(body["items"].([]any)); (n == 1) != on {
			t.Errorf("enabled %v: %v", on, body)
		}
	}
}

func TestHTTPAPI_branchesトークンは許可外APIで403を返し許可APIと別名は通る(t *testing.T) {
	f := checkpointFixture(t)
	st := &filestore.Store{Path: filepath.Join(t.TempDir(), "t.json")}
	toks := &token.Service{Store: st, Clock: time.Now}
	f.srv.Tokens, f.srv.TokenAuth, f.srv.Preferences, f.srv.Providers = toks, toks, st, providersOf(&fakeQuery{})
	f.srv.ExtHandlers = map[string]ext.Handler{"auth": &moduleAPI{}}
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Base: name("prod"), Status: domain.BranchReady, Owner: "admin", OwnerName: "admin", Runtime: mysql84, Port: 40001, User: "x", Password: "pw"}
	// Make branch x owned by the token itself so ownership allows branch actions.
	secret, rec, err := toks.Create(ctx, "dev", domain.ScopeBranches, 0)
	if err != nil {
		t.Fatal(err)
	}
	bx := f.branches.branches["x"]
	bx.Owner, bx.OwnerName = "token:"+rec.ID, "dev"
	f.branches.branches["x"] = bx

	h := f.srv.Handler(false)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/ext/auth/users"},
		{"GET", "/v1/license"},
		{"GET", "/v1/license/renewal"},
		{"GET", "/v1/cluster"},
		{"GET", "/v1/nodes/node1/metrics"},
		{"GET", "/v1/nodes/node1/storage"},
		{"GET", "/v1/modules"},
		{"GET", "/v1/updates"},
		{"GET", "/v1/settings"},
		{"GET", "/v1/tokens"},
		{"GET", "/v1/route-users"},
		{"GET", "/v1/backups"},
		{"GET", "/v1/bases/prod/refresh"},
		{"POST", "/v1/account/tokens"},
	} {
		resp, body := send(t, h, tc.method, tc.path, map[string]any{"name": "x"}, as(secret))
		if resp.StatusCode != 403 || errCode(body) != "forbidden" {
			t.Errorf("%s %s: got %d %v, want 403 forbidden", tc.method, tc.path, resp.StatusCode, body)
		}
	}

	for _, tc := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", "/v1/branches", nil, 200},
		{"PATCH", "/v1/branches/x", map[string]any{"ttl": "2h"}, 200},
		{"POST", "/v1/branches/x/pause", nil, 202},
		{"GET", "/v1/branches/x/dsn", nil, 200},
		{"POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 1"}, 200},
		{"GET", "/v1/bases", nil, 200},
		{"GET", "/v1/public/bases", nil, 200},
		{"GET", "/v1/runtimes", nil, 200},
		{"GET", "/v1/operations", nil, 200},
		{"GET", "/v1/capabilities", nil, 200},
		{"GET", "/v1/metrics", nil, 200},
		{"GET", "/v1/account/tokens", nil, 200},
		{"GET", "/v1/account/preferences", nil, 200},
		{"DELETE", "/v1/branches/x/checkpoints/c1", nil, 204},
	} {
		resp, body := send(t, h, tc.method, tc.path, tc.body, as(secret))
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: got %d %v, want %d", tc.method, tc.path, resp.StatusCode, body, tc.want)
		}
	}
	f.branches.mu.Lock()
	bx = f.branches.branches["x"]
	bx.Status = domain.BranchPaused
	f.branches.branches["x"] = bx
	f.branches.mu.Unlock()
	if resp, body := send(t, h, "POST", "/v1/branches/x/resume", nil, as(secret)); resp.StatusCode != 202 {
		t.Errorf("POST /v1/branches/x/resume: got %d %v, want 202", resp.StatusCode, body)
	}
}

type authUserDeleteAPI struct{}

func (authUserDeleteAPI) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	if r.Method == http.MethodDelete && r.Path == "/users/nonexistent@example.com" {
		return ext.Response{Status: http.StatusNotFound, Body: []byte(`{"error":{"code":"not_found","message":"no such user"}}`)}, nil
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.Path, "/users/") {
		return ext.Response{Status: http.StatusNoContent}, nil
	}
	return ext.Response{Status: http.StatusOK, Body: []byte(`{}`)}, nil
}

type emailUserAuth struct{}

func (emailUserAuth) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	if c.Token == "" && c.Username == "alice@example.com" && c.Password == "alice-pw" {
		return ext.Principal{ID: "user:alice@example.com", Name: "alice@example.com", Roles: []string{"developer"}}, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

func TestHTTPAPI_authモジュールのユーザー削除で個人トークンが失効する(t *testing.T) {
	f := newFixture(t)
	toks := &token.Service{Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "t.json")}, Clock: time.Now}
	f.srv.Tokens, f.srv.TokenAuth = toks, toks
	f.srv.Ext = ext.Module{Authenticator: emailUserAuth{}}
	f.srv.ExtHandlers = map[string]ext.Handler{"auth": authUserDeleteAPI{}}
	h := f.srv.Handler(false)

	// Alice signs in and issues a personal token.
	resp, loginBody := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice@example.com", "password": "alice-pw"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("alice login: %d %v", resp.StatusCode, loginBody)
	}
	aliceSess := withSession(resp.Cookies()[0], loginBody["csrf_token"].(string))
	resp, tokBody := send(t, h, "POST", "/v1/account/tokens", map[string]any{"name": "mcp"}, aliceSess)
	if resp.StatusCode != 201 {
		t.Fatalf("create personal token: %d %v", resp.StatusCode, tokBody)
	}
	aliceSecret := tokBody["token"].(string)
	svcSecret, _, err := toks.Create(ctx, "ci", domain.ScopeBranches, 0)
	if err != nil {
		t.Fatal(err)
	}

	if resp, _ := send(t, h, "GET", "/v1/branches", nil, as(aliceSecret)); resp.StatusCode != 200 {
		t.Fatalf("alice token before delete: %d", resp.StatusCode)
	}

	// Admin deletes a nonexistent user (404) -> Alice's token remains valid.
	adminCookie, adminCSRF := login(t, f)
	adminSess := withSession(adminCookie, adminCSRF)
	if resp, _ := send(t, h, "DELETE", "/v1/ext/auth/users/nonexistent@example.com", nil, adminSess); resp.StatusCode != 404 {
		t.Fatalf("delete nonexistent: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, as(aliceSecret)); resp.StatusCode != 200 {
		t.Fatalf("alice token after 404 delete: %d", resp.StatusCode)
	}

	// Admin deletes Alice (mixed-case / URL-encoded path) -> Alice's personal token is revoked (401), service token stays 200.
	if resp, _ := send(t, h, "DELETE", "/v1/ext/auth/users/Alice%40Example.com", nil, adminSess); resp.StatusCode != 204 {
		t.Fatalf("delete alice: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, as(aliceSecret)); resp.StatusCode != 401 {
		t.Errorf("alice token after user deletion: got %d, want 401", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, as(svcSecret)); resp.StatusCode != 200 {
		t.Errorf("service token after user deletion: got %d, want 200", resp.StatusCode)
	}

	// Trailing slash on DELETE /users/<email>/ also revokes personal tokens.
	aliceTrailing, _, err := toks.CreatePersonal(ctx, ext.Principal{ID: "user:alice@example.com", Name: "alice@example.com"}, "trailing", 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := send(t, h, "DELETE", "/v1/ext/auth/users/alice@example.com/", nil, adminSess); resp.StatusCode != 204 {
		t.Fatalf("delete alice trailing slash: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, as(aliceTrailing)); resp.StatusCode != 401 {
		t.Errorf("alice token after trailing-slash delete: got %d, want 401", resp.StatusCode)
	}

	// If RevokeByOwner fails, user deletion still returns 204 and logs a warning.
	var logs bytes.Buffer
	f.srv.Log = slog.New(slog.NewTextHandler(&logs, nil))
	f.srv.Tokens = failingRevokeTokens{Tokens: toks}
	if resp, _ := send(t, h, "DELETE", "/v1/ext/auth/users/bob@example.com", nil, adminSess); resp.StatusCode != 204 {
		t.Errorf("delete with failing revoke: %d", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "revoking personal tokens") {
		t.Errorf("expected warning log, got %q", logs.String())
	}
}

type failingRevokeTokens struct{ httpapi.Tokens }

func (failingRevokeTokens) RevokeByOwner(context.Context, string) error {
	return errors.New("store down")
}

type viewerSessionAuth struct{}

func (viewerSessionAuth) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	if c.Token == "" && c.Username == "alice" && c.Password == "alice-pw" {
		return ext.Principal{ID: "sso:alice", Name: "Alice", Roles: []string{"viewer"}}, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

type viewerSelfServiceAuthz struct{}

func (viewerSelfServiceAuthz) Authorize(ctx context.Context, p ext.Principal, action string, r ext.Resource) error {
	if action == "account.token.write" || action == "account.token.read" {
		return nil
	}
	return viewerReadsOnly{}.Authorize(ctx, p, action, r)
}

func TestHTTPAPI_個人トークンは所有者のロールを保持しviewerの権限昇格を防ぐ(t *testing.T) {
	f := newFixture(t)
	toks := &token.Service{Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "t.json")}, Clock: time.Now}
	f.srv.Tokens, f.srv.TokenAuth = toks, toks
	f.srv.Ext = ext.Module{Authenticator: viewerSessionAuth{}, Authorizer: viewerSelfServiceAuthz{}}
	f.branches.branches["alices"] = domain.Branch{Name: name("alices"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:alice", OwnerName: "Alice", Runtime: mysql84}
	f.branches.branches["bobs"] = domain.Branch{Name: name("bobs"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:bob", OwnerName: "bob@x.io", Runtime: mysql84}
	h := f.srv.Handler(false)

	// Viewer Alice signs in and creates a personal token via API.
	resp, loginBody := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "alice-pw"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("alice login: %d %v", resp.StatusCode, loginBody)
	}
	aliceSess := withSession(resp.Cookies()[0], loginBody["csrf_token"].(string))
	resp, tokBody := send(t, h, "POST", "/v1/account/tokens", map[string]any{"name": "mcp"}, aliceSess)
	if resp.StatusCode != 201 {
		t.Fatalf("create personal token: %d %v", resp.StatusCode, tokBody)
	}
	aliceSecret := tokBody["token"].(string)

	// Under strict viewerReadsOnly (LEAN-01 BugCandidate2 Part B), viewer's personal token can read her branch
	// but is denied DELETE (403) just like her console session.
	f.srv.Ext.Authorizer = viewerReadsOnly{}
	if resp, _ := send(t, h, "GET", "/v1/branches/alices", nil, as(aliceSecret)); resp.StatusCode != 200 {
		t.Errorf("viewer personal token GET own branch: %d, want 200", resp.StatusCode)
	}
	if resp, body := send(t, h, "DELETE", "/v1/branches/alices", nil, as(aliceSecret)); resp.StatusCode != 403 || errCode(body) != "forbidden" {
		t.Errorf("viewer personal token DELETE own branch: %d %v, want 403 forbidden", resp.StatusCode, body)
	}

	// Admin's personal token never inherits the "admin" role (cannot delete Bob's private branch).
	adminCookie, adminCSRF := login(t, f)
	resp, adminTok := send(t, h, "POST", "/v1/account/tokens", map[string]any{"name": "admin-mcp"}, withSession(adminCookie, adminCSRF))
	if resp.StatusCode != 201 {
		t.Fatalf("create admin personal token: %d %v", resp.StatusCode, adminTok)
	}
	if resp, body := send(t, h, "DELETE", "/v1/branches/bobs", nil, as(adminTok["token"].(string))); resp.StatusCode != 403 || errCode(body) != "forbidden" {
		t.Errorf("admin personal token DELETE bob's branch: %d %v, want 403 forbidden", resp.StatusCode, body)
	}
}
