package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/pkg/ext"
)

type recordingSink struct {
	mu     sync.Mutex
	events []ext.Event
	err    error
}

func (s *recordingSink) Record(_ context.Context, e ext.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return s.err
}

func (s *recordingSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = nil
}

func (s *recordingSink) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		out = append(out, e.Action+":"+e.Principal.ID+":"+e.Resource.Kind+"/"+e.Resource.Name+":"+e.Outcome)
	}
	return out
}

// viewers may only read; tokens "t-viewer" and "t-admin" identify people.
type moduleAuth struct{}

func (moduleAuth) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	switch {
	case c.Token == "t-viewer" || (c.Token == "" && c.Username == "viewer"):
		return ext.Principal{ID: "token:viewer", Roles: []string{"viewer"}}, nil
	case c.Token == "t-admin":
		return ext.Principal{ID: "token:admin", Roles: []string{"admin"}}, nil
	case c.Token != "":
		return ext.Principal{}, errors.New("revoked")
	case c.Username == "alice" && c.Password == "alice-pw":
		return ext.Principal{ID: "sso:alice", Name: "Alice", Roles: []string{"viewer"}}, nil
	case c.Username == "bob" && c.Password == "ticket":
		return ext.Principal{ID: "sso:bob", Name: "bob@x.io", Roles: []string{"viewer", ext.NoPasswordRole}}, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

type viewerReadsOnly struct{}

func (viewerReadsOnly) Authorize(_ context.Context, p ext.Principal, action string, _ ext.Resource) error {
	for _, r := range p.Roles {
		if r == "viewer" && !strings.HasSuffix(action, ".read") && !strings.HasPrefix(action, "auth.") && action != "capability.read" {
			return ext.ErrForbidden
		}
	}
	return nil
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func TestAccess_既定では変更を監査ログに出す(t *testing.T) {
	f := newFixture(t)
	var logged bytes.Buffer
	f.srv.Log = slog.New(slog.NewTextHandler(&logged, nil))
	if resp, _ := f.do(t, "POST", "/v1/branches", map[string]any{"name": "feature-x", "base": "prod"}); resp.StatusCode != 202 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	f.do(t, "GET", "/v1/branches", nil)
	out := logged.String()
	if !strings.Contains(out, "action=branch.create") || !strings.Contains(out, "principal=local") || !strings.Contains(out, "resource=branch/feature-x") || strings.Contains(out, "branch.read") {
		t.Errorf("log:\n%s", out)
	}
}

func TestAccess_モジュールの認可で断り断ったことも記録する(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}, AuditSink: sink}
	h := f.srv.Handler(false)

	if resp, _ := send(t, h, "GET", "/v1/branches", nil, bearer("t-viewer")); resp.StatusCode != 200 {
		t.Errorf("viewer read: %d", resp.StatusCode)
	}
	resp, body := send(t, h, "DELETE", "/v1/bases/prod", nil, bearer("t-viewer"))
	if resp.StatusCode != 403 || errCode(body) != "forbidden" {
		t.Errorf("viewer delete: %d %v", resp.StatusCode, body)
	}
	if resp, _ := send(t, h, "POST", "/v1/bases/prod/stop", nil, bearer("t-admin")); resp.StatusCode != 204 {
		t.Errorf("admin stop: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, bearer("nope")); resp.StatusCode != 401 {
		t.Errorf("revoked token: %d", resp.StatusCode)
	}
	got := strings.Join(sink.actions(), " ")
	if got != "base.delete:token:viewer:base/prod:denied base.stop:token:admin:base/prod:ok auth.token:anonymous:auth/:denied" {
		t.Errorf("audit = %s", got)
	}
}

func TestAccess_モジュールの利用者でもコンソールにログインできadminのパスワードも使える(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}, AuditSink: sink}
	h := f.srv.Handler(false)
	resp, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "alice-pw"}, nil)
	if resp.StatusCode != 200 || body["user"].(map[string]any)["id"] != "sso:alice" || body["user"].(map[string]any)["name"] != "Alice" {
		t.Fatalf("alice: %d %v", resp.StatusCode, body)
	}
	cookie := resp.Cookies()[0]
	csrf := body["csrf_token"].(string)
	if resp, _ := send(t, h, "POST", "/v1/branches/x/reset", nil, withSession(cookie, csrf)); resp.StatusCode != 403 {
		t.Errorf("alice (viewer) reset: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil); resp.StatusCode != 200 {
		t.Errorf("admin password still works: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "wrong"}, nil); resp.StatusCode != 401 {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	got := strings.Join(sink.actions(), " ")
	for _, want := range []string{"auth.login:sso:alice:auth/:ok", "branch.reset:sso:alice:branch/x:denied", "auth.login:admin:auth/:ok", "auth.login:admin:auth/:failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit lacks %s: %s", want, got)
		}
	}
}

func TestAccess_転送された要求は元の利用者として扱い二重に記録しない(t *testing.T) {
	f, c := remoteFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{AuditSink: sink}
	cookie, csrf := login(t, f)
	resp, _ := send(t, f.srv.Handler(false), "POST", "/v1/branches/there/reset", nil, withSession(cookie, csrf))
	if resp.StatusCode != 202 || len(c.forwarded) != 1 {
		t.Fatalf("%d %v", resp.StatusCode, c.forwarded)
	}
	if got := sink.actions(); len(got) != 2 || got[1] != "branch.reset:admin:branch/there:ok" {
		t.Errorf("origin audit = %v", got)
	}
	// On the receiving node: the socket handler with the forwarding headers.
	sink.events = nil
	f.branches.branches["here"] = domain.Branch{Name: name("here"), Status: domain.BranchReady, Owner: "sso:bob", OwnerName: "bob"}
	resp, _ = send(t, f.srv.Handler(true), "POST", "/v1/branches/here/reset", nil, func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n9")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"sso:bob","roles":["viewer"]}`)
	})
	if resp.StatusCode != 202 || len(sink.events) != 0 {
		t.Errorf("receiving node: %d audit %v", resp.StatusCode, sink.actions())
	}
	f.srv.Ext.Authorizer = viewerReadsOnly{}
	resp, _ = send(t, f.srv.Handler(true), "POST", "/v1/branches/here/reset", nil, func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n9")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"sso:bob","roles":["viewer"]}`)
	})
	if resp.StatusCode != 403 {
		t.Errorf("the forwarded principal is authorized too: %d", resp.StatusCode)
	}
}

func TestAccess_監査の失敗は要求を止めずログに残す(t *testing.T) {
	f := newFixture(t)
	var logged bytes.Buffer
	f.srv.Log = slog.New(slog.NewTextHandler(&logged, nil))
	f.srv.Ext = ext.Module{AuditSink: &recordingSink{err: errors.New("siem down")}}
	if resp, _ := f.do(t, "POST", "/v1/bases/prod/stop", nil); resp.StatusCode != 204 {
		t.Errorf("status %d", resp.StatusCode)
	}
	if !strings.Contains(logged.String(), "audit sink failed") {
		t.Errorf("log: %s", logged.String())
	}
}

func TestCapabilities_読み込んだモジュールを示す(t *testing.T) {
	f := newFixture(t)
	f.srv.Modules = []string{"audit-file 1.0.0"}
	_, body := f.do(t, "GET", "/v1/capabilities", nil)
	if m := body["modules"].([]any); len(m) != 1 || m[0] != "audit-file 1.0.0" {
		t.Errorf("modules = %v", body["modules"])
	}
}

// changingAuth is moduleAuth whose users can change their own password.
type changingAuth struct {
	moduleAuth
	changed *[]string
}

func (c changingAuth) ChangePassword(_ context.Context, p ext.Principal, current, next string) error {
	if current != "alice-pw" {
		return ext.ErrUnauthenticated
	}
	*c.changed = append(*c.changed, p.ID+":"+next)
	return nil
}

func TestAccount_モジュールの利用者は自分のパスワードをモジュールで変える(t *testing.T) {
	f := newFixture(t)
	var changed []string
	f.srv.Ext = ext.Module{Authenticator: changingAuth{changed: &changed}, Authorizer: viewerReadsOnly{}}
	h := f.srv.Handler(false)
	resp, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "alice-pw"}, nil)
	if resp.StatusCode != 200 || body["can_change_password"] != true {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	cookie, csrf := resp.Cookies()[0], body["csrf_token"].(string)
	if resp, _ := send(t, h, "PUT", "/v1/auth/password", map[string]any{"current_password": "nope", "new_password": "a long new passphrase"}, withSession(cookie, csrf)); resp.StatusCode != 401 {
		t.Errorf("wrong current: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "PUT", "/v1/auth/password", map[string]any{"current_password": "alice-pw", "new_password": "a long new passphrase"}, withSession(cookie, csrf)); resp.StatusCode != 204 {
		t.Errorf("change: %d", resp.StatusCode)
	}
	if len(changed) != 1 || changed[0] != "sso:alice:a long new passphrase" {
		t.Errorf("changed %v", changed)
	}
	// The admin password is not touched by a module user.
	if resp, _ := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil); resp.StatusCode != 200 {
		t.Errorf("admin password changed: %d", resp.StatusCode)
	}
}

func TestAccount_SSOでサインインした利用者にはパスワード変更を出さない(t *testing.T) {
	f := newFixture(t)
	var changed []string
	f.srv.Ext = ext.Module{Authenticator: changingAuth{changed: &changed}, Authorizer: viewerReadsOnly{}}
	h := f.srv.Handler(false)
	resp, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "bob", "password": "ticket"}, nil)
	if body["can_change_password"] != false {
		t.Errorf("session %v", body)
	}
	cookie, csrf := resp.Cookies()[0], body["csrf_token"].(string)
	if resp, body := send(t, h, "PUT", "/v1/auth/password", map[string]any{"current_password": "x", "new_password": "a long new passphrase"}, withSession(cookie, csrf)); resp.StatusCode != 403 || errCode(body) != "managed_elsewhere" {
		t.Errorf("change: %d %v", resp.StatusCode, body)
	}
	if len(changed) != 0 {
		t.Errorf("changed %v", changed)
	}
}

func TestAccount_パスワードを持たない利用者には変更させない(t *testing.T) {
	f := newFixture(t)
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}}
	h := f.srv.Handler(false)
	resp, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "alice-pw"}, nil)
	if body["can_change_password"] != false {
		t.Errorf("session %v", body)
	}
	cookie, csrf := resp.Cookies()[0], body["csrf_token"].(string)
	if resp, body := send(t, h, "PUT", "/v1/auth/password", map[string]any{"current_password": "alice-pw", "new_password": "a long new passphrase"}, withSession(cookie, csrf)); resp.StatusCode != 403 || errCode(body) != "managed_elsewhere" {
		t.Errorf("change: %d %v", resp.StatusCode, body)
	}
	_, body = send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil)
	if body["can_change_password"] != true {
		t.Errorf("admin session %v", body)
	}
}

func TestAudit_接続情報の取得は読み取りでも記録する(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{AuditSink: sink}
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Base: name("prod"), Status: domain.BranchReady, Runtime: mysql84, Port: 40001, User: "x", Password: "pw"}
	_, _ = f.do(t, "GET", "/v1/branches/x/dsn", nil)
	_, _ = f.do(t, "GET", "/v1/branches", nil)
	got := strings.Join(sink.actions(), " ")
	if !strings.Contains(got, "branch.connect:local:branch/x:ok") || strings.Contains(got, "branch.read") {
		t.Errorf("audit = %s", got)
	}
}

func TestSession_できる操作を画面に伝える(t *testing.T) {
	f := newFixture(t)
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}}
	f.srv.Modules = []string{"auth 1.2.0"}
	h := f.srv.Handler(false)
	_, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "alice-pw"}, nil)
	can, _ := body["can"].(map[string]any)
	if can["base.read"] != true || can["base.create"] != false || can["setting.write"] != false || can["ext.auth.read"] != true || can["ext.auth.write"] != false {
		t.Errorf("viewer can %v", body["can"])
	}
	if can["branch.restore"] != false || can["branch.checkpoint"] != false || can["branch.checkpoint_delete"] != false || can["branch.upgrade"] != false {
		t.Errorf("a viewer cannot change a branch's checkpoints: %v", can)
	}
	_, body = send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil)
	can, _ = body["can"].(map[string]any)
	for _, a := range []string{"base.create", "runtime.install", "setting.write", "module.install", "branch.checkpoint", "branch.restore", "branch.checkpoint_delete", "branch.upgrade"} {
		if can[a] != true {
			t.Errorf("admin cannot %s: %v", a, can)
		}
	}
}

func TestLocalPrincipal_ブランチ一覧に出る名前は空白を含まない(t *testing.T) {
	// The name is the owner of branches made over the socket and is shown as
	// "owner/name" in lists, so it must read as one word.
	if n := httpapi.LocalPrincipal.Name; n != "local" {
		t.Errorf("owner name = %q, want \"local\"", n)
	}
}

func TestHTTPAPI_公開リスナーからのXDbBranchForwarded偽装ヘッダで監査ログとmodules_pendingを迂回できない(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}, AuditSink: sink}
	h := f.srv.Handler(false)

	spoof := func(token string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Db-Branch-Forwarded", "n9")
			r.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
		}
	}

	// 1. modules_pending cannot be bypassed with a spoofed header.
	f.srv.PendingModules = func() []string { return []string{"auth"} }
	resp, body := send(t, h, "GET", "/v1/branches", nil, spoof("t-admin"))
	if resp.StatusCode != 503 || errCode(body) != "modules_pending" {
		t.Fatalf("pending bypass: %d %v, want 503 modules_pending", resp.StatusCode, body)
	}

	// 2. Audit logging (ok and denied) cannot be suppressed and principal cannot be spoofed.
	f.srv.PendingModules = nil
	if resp, _ := send(t, h, "POST", "/v1/bases/prod/stop", nil, spoof("t-admin")); resp.StatusCode != 204 {
		t.Fatalf("admin stop: %d, want 204", resp.StatusCode)
	}
	if resp, body := send(t, h, "DELETE", "/v1/bases/prod", nil, spoof("t-viewer")); resp.StatusCode != 403 || errCode(body) != "forbidden" {
		t.Fatalf("viewer delete: %d %v, want 403 forbidden", resp.StatusCode, body)
	}
	got := strings.Join(sink.actions(), " ")
	want := "base.stop:token:admin:base/prod:ok base.delete:token:viewer:base/prod:denied"
	if got != want {
		t.Errorf("audit = %q, want %q", got, want)
	}

	// 3. Session-authenticated requests also ignore spoofed headers (including principal-only).
	sink.reset()
	cookie, csrf := login(t, f)
	f.srv.PendingModules = func() []string { return []string{"auth"} }
	resp, body = send(t, h, "GET", "/v1/branches", nil, func(r *http.Request) {
		withSession(cookie, csrf)(r)
		r.Header.Set("X-Db-Branch-Forwarded", "n9")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
	})
	if resp.StatusCode != 503 || errCode(body) != "modules_pending" {
		t.Fatalf("session pending bypass: %d %v, want 503 modules_pending", resp.StatusCode, body)
	}
	f.srv.PendingModules = nil
	if resp, _ := send(t, h, "POST", "/v1/bases/prod/stop", nil, func(r *http.Request) {
		withSession(cookie, csrf)(r)
		r.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
	}); resp.StatusCode != 204 {
		t.Fatalf("session stop with principal header: %d, want 204", resp.StatusCode)
	}
	if got := strings.Join(sink.actions(), " "); !strings.Contains(got, "base.stop:admin:base/prod:ok") {
		t.Errorf("session audit = %q, want base.stop:admin:base/prod:ok", got)
	}
}

func TestHTTPAPI_正規の転送経路では重複監査抑止とmodules_pending通過と転送ループ防止が維持される(t *testing.T) {
	f, c := remoteFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{AuditSink: sink}
	f.srv.PendingModules = func() []string { return []string{"auth"} }
	f.branches.branches["here"] = domain.Branch{Name: name("here"), Status: domain.BranchReady, Owner: "sso:bob", OwnerName: "bob"}

	trustedFwd := func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n9")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"sso:alice","name":"Alice","roles":["admin"]}`)
	}

	// 1. Trusted mTLS forwarded request passes modules_pending and does not duplicate audit.
	h := f.srv.Handler(true)
	if resp, body := send(t, h, "POST", "/v1/branches/here/reset", nil, trustedFwd); resp.StatusCode != 202 {
		t.Fatalf("trusted forwarded reset while modules pending: %d %v, want 202", resp.StatusCode, body)
	}
	if got := sink.actions(); len(got) != 0 {
		t.Errorf("duplicate audit on receiving node: %v", got)
	}

	// 2. Trusted mTLS forwarded request still enforces Authorizer for the forwarded principal.
	f.srv.Ext.Authorizer = viewerReadsOnly{}
	viewerFwd := func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n9")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"sso:bob","name":"Bob","roles":["viewer"]}`)
	}
	if resp, _ := send(t, h, "POST", "/v1/branches/here/reset", nil, viewerFwd); resp.StatusCode != 403 {
		t.Errorf("trusted forwarded viewer must be denied: %d, want 403", resp.StatusCode)
	}
	if got := sink.actions(); len(got) != 0 {
		t.Errorf("duplicate audit on forwarded denial: %v", got)
	}
	f.srv.Ext.Authorizer = nil

	// 3. Trusted mTLS forwarded request targeting a branch seen on another node stops with 409 (no loop).
	resp, body := send(t, h, "POST", "/v1/branches/there/reset", nil, trustedFwd)
	if resp.StatusCode != 409 || errCode(body) != "other_node" || len(c.forwarded) != 0 {
		t.Errorf("node forward loop guard: %d %v forwarded=%v", resp.StatusCode, body, c.forwarded)
	}
	if got := sink.actions(); len(got) != 0 {
		t.Errorf("duplicate audit on 409 loop guard: %v", got)
	}

	// 4. Trusted mTLS forwarded license renewal on a non-leader does not re-forward to the leader.
	lf := newFixture(t)
	lf.srv.Licensing = &fakeLicensing{}
	lf.srv.Renewal = &fakeRenewal{leading: false, err: licensing.ErrNotLeader}
	lc := &leaderCluster{replyCode: 200, reply: `{}`}
	lc.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "n2"}
	lf.srv.Cluster = lc
	lh := lf.srv.Handler(true)
	if resp, _ := send(t, lh, "POST", "/v1/license/renew", nil, trustedFwd); resp.StatusCode != 503 || len(lc.seen) != 0 {
		t.Errorf("license renew loop guard: %d seen=%v", resp.StatusCode, lc.seen)
	}
	if resp, body := send(t, lh, "GET", "/v1/license/renewal", nil, trustedFwd); resp.StatusCode != 200 || body["last_result"] != "unknown" || len(lc.seen) != 0 {
		t.Errorf("license renewal GET loop guard: %d %v seen=%v", resp.StatusCode, body, lc.seen)
	}
	if resp, body := send(t, lh, "GET", "/v1/license", nil, trustedFwd); resp.StatusCode != 200 || body["renewal"].(map[string]any)["last_result"] != "unknown" || len(lc.seen) != 0 {
		t.Errorf("license status loop guard: %d %v seen=%v", resp.StatusCode, body, lc.seen)
	}
}

func TestHTTPAPI_匿名レート制限とExtAPIとMCPでも偽装Forwardedヘッダを無視する(t *testing.T) {
	spoofHeaders := func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n9")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
	}

	// 1. Anonymous rate limit cannot be bypassed with spoofed forwarding headers.
	af := anonFixture(t)
	af.srv.AnonymousRate = func() int { return 1 }
	ah := af.srv.Handler(false)
	if resp, body := anonCreate(t, ah, "a1", "prod", spoofHeaders); resp.StatusCode != 202 {
		t.Fatalf("first anonymous create: %d %v", resp.StatusCode, body)
	}
	if resp, body := anonCreate(t, ah, "a2", "prod", spoofHeaders); resp.StatusCode != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("second anonymous create bypassed rate limit: %d %v, want 429 rate_limited", resp.StatusCode, body)
	}

	// 2. ExtPublic and private extAPI ignore spoofed headers and audit properly.
	ef := newFixture(t)
	sink := &recordingSink{}
	extCap := &captureExtHandler{}
	ef.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}, AuditSink: sink}
	ef.srv.ExtHandlers = map[string]ext.Handler{"sso": extCap}
	ef.srv.ExtPublic = map[string][]string{"sso": {"/public"}}
	eh := ef.srv.Handler(false)

	if resp, _ := send(t, eh, "GET", "/v1/ext/sso/public/start", nil, spoofHeaders); resp.StatusCode != 200 || len(extCap.principals()) != 1 || extCap.principals()[0].ID != httpapi.Anonymous.ID {
		t.Fatalf("ExtPublic spoof: status=%d seen=%+v, want anonymous", resp.StatusCode, extCap.principals())
	}
	extCap.reset()
	if resp, _ := send(t, eh, "POST", "/v1/ext/sso/private/write", nil, func(r *http.Request) {
		bearer("t-viewer")(r)
		spoofHeaders(r)
	}); resp.StatusCode != 403 {
		t.Fatalf("private ext write as viewer with spoof: %d, want 403", resp.StatusCode)
	}
	if got := strings.Join(sink.actions(), " "); !strings.Contains(got, "ext.sso.write:token:viewer:ext/sso:denied") {
		t.Errorf("ext audit = %q", got)
	}

	// 3. MCP cannot escalate principal or suppress audit via spoofed headers.
	mf := newFixture(t)
	mSink := &recordingSink{}
	mf.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}, AuditSink: mSink}
	mf.branches.branches["x"] = domain.Branch{Name: name("x"), Base: name("prod"), Status: domain.BranchReady}
	mh := mf.srv.Handler(false)
	mcpBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "delete_branch",
			"arguments": map[string]any{"name": "x"},
		},
	}
	if resp, _ := send(t, mh, "POST", "/v1/mcp", mcpBody, func(r *http.Request) {
		bearer("t-viewer")(r)
		spoofHeaders(r)
		r.Header.Set("Accept", "application/json, text/event-stream")
	}); resp.StatusCode != 403 {
		t.Fatalf("mcp viewer status = %d, want 403", resp.StatusCode)
	}
	if resp, _ := send(t, mh, "POST", "/v1/mcp", mcpBody, func(r *http.Request) {
		bearer("t-admin")(r)
		spoofHeaders(r)
		r.Header.Set("Accept", "application/json, text/event-stream")
	}); resp.StatusCode != 200 {
		t.Fatalf("mcp admin status = %d, want 200", resp.StatusCode)
	}
	if got := strings.Join(mSink.actions(), " "); !strings.Contains(got, "branch.delete:token:admin:branch/x:ok") {
		t.Errorf("mcp audit = %q, want branch.delete:token:admin:branch/x:ok", got)
	}
}

func TestLocalPrincipal_転送ヘッダありでPrincipal欠落や不正ならLocalPrincipalへ昇格させない(t *testing.T) {
	f := newFixture(t)
	extSeen := &captureExtHandler{}
	f.srv.ExtHandlers = map[string]ext.Handler{"sso": extSeen}
	f.srv.ExtPublic = map[string][]string{"sso": {"/public"}}
	h := f.srv.Handler(true)

	badCases := []struct {
		name string
		set  func(http.Header)
	}{
		{"missing_principal", func(hdr http.Header) {
			hdr.Set("X-Db-Branch-Forwarded", "n9")
		}},
		{"malformed_principal", func(hdr http.Header) {
			hdr.Set("X-Db-Branch-Forwarded", "n9")
			hdr.Set("X-Db-Branch-Principal", "{bad-json")
		}},
		{"empty_id", func(hdr http.Header) {
			hdr.Set("X-Db-Branch-Forwarded", "n9")
			hdr.Set("X-Db-Branch-Principal", `{"id":"","name":"x","roles":["admin"]}`)
		}},
		{"principal_only_without_forwarded", func(hdr http.Header) {
			hdr.Set("X-Db-Branch-Principal", `{"id":"sso:alice","roles":["admin"]}`)
		}},
		{"empty_forwarded", func(hdr http.Header) {
			hdr["X-Db-Branch-Forwarded"] = []string{""}
			hdr.Set("X-Db-Branch-Principal", `{"id":"sso:alice","roles":["admin"]}`)
		}},
		{"multi_forwarded_leading_empty", func(hdr http.Header) {
			hdr["X-Db-Branch-Forwarded"] = []string{"", "n9"}
			hdr.Set("X-Db-Branch-Principal", `{"id":"sso:alice","roles":["admin"]}`)
		}},
		{"multi_principal", func(hdr http.Header) {
			hdr.Set("X-Db-Branch-Forwarded", "n9")
			hdr["X-Db-Branch-Principal"] = []string{`{"id":"sso:alice","roles":["admin"]}`, `{"id":"sso:bob","roles":["viewer"]}`}
		}},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := func(r *http.Request) { tc.set(r.Header) }
			// Guarded route must deny with 403 forbidden (fail-closed, never LocalPrincipal).
			if resp, body := send(t, h, "GET", "/v1/branches", nil, hdr); resp.StatusCode != 403 || errCode(body) != "forbidden" {
				t.Errorf("guarded route: %d %v, want 403 forbidden", resp.StatusCode, body)
			}
			// Even on ExtPublic, the principal passed to the module must be empty, never LocalPrincipal.
			extSeen.reset()
			if resp, _ := send(t, h, "GET", "/v1/ext/sso/public/info", nil, hdr); resp.StatusCode != 200 || len(extSeen.principals()) != 1 || extSeen.principals()[0].ID != "" {
				t.Errorf("ExtPublic principal: status=%d seen=%+v, want empty ID", resp.StatusCode, extSeen.principals())
			}
		})
	}
}

type legacyOnlyAuthorizer struct {
	mu    sync.Mutex
	allow map[string]bool
	seen  []string
}

func (a *legacyOnlyAuthorizer) Authorize(_ context.Context, _ ext.Principal, action string, _ ext.Resource) error {
	a.mu.Lock()
	a.seen = append(a.seen, action)
	a.mu.Unlock()
	if a.allow[action] {
		return nil
	}
	return ext.ErrForbidden
}

func TestAccess_ドット区切りとpause_resume分離の新アクション名と旧名別名の互換(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	az := &legacyOnlyAuthorizer{allow: map[string]bool{
		"branch.extend":            true,
		"branch.update":            true,
		"branch.checkpoint_delete": true,
		"base.snapshot_delete":     true,
		"branch.read":              true,
		"base.read":                true,
		"auth.session":             true,
	}}
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: az, AuditSink: sink}
	f.branches.mu.Lock()
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:alice", OwnerName: "Alice", Runtime: mysql84}
	b := f.branches.bases["prod"]
	b.Snapshots = append(b.Snapshots, domain.BaseSnapshot{Name: "s2", CreatedAt: t0})
	f.branches.bases["prod"] = b
	f.branches.mu.Unlock()

	h := f.srv.Handler(false)
	resp, loginBody := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "alice-pw"}, nil)
	cookie, csrf := resp.Cookies()[0], loginBody["csrf_token"].(string)

	// New action names are tried first, then legacy aliases allow an older auth module to authorize.
	resp, pauseBody := send(t, h, "POST", "/v1/branches/x/pause", nil, withSession(cookie, csrf))
	if resp.StatusCode != 202 {
		t.Fatalf("pause with legacy branch.update: %d", resp.StatusCode)
	}
	f.waitOp(t, pauseBody["id"].(string))
	f.branches.mu.Lock()
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Base: name("prod"), Status: domain.BranchPaused, Owner: "sso:alice", OwnerName: "Alice", Runtime: mysql84}
	f.branches.mu.Unlock()
	resp, resumeBody := send(t, h, "POST", "/v1/branches/x/resume", nil, withSession(cookie, csrf))
	if resp.StatusCode != 202 {
		t.Fatalf("resume with legacy branch.update: %d", resp.StatusCode)
	}
	f.waitOp(t, resumeBody["id"].(string))
	if resp, _ := send(t, h, "DELETE", "/v1/branches/x/checkpoints/c1", nil, withSession(cookie, csrf)); resp.StatusCode != 204 {
		t.Fatalf("delete checkpoint with legacy branch.checkpoint_delete: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "DELETE", "/v1/bases/prod/snapshots/s1", nil, withSession(cookie, csrf)); resp.StatusCode != 204 {
		t.Fatalf("delete snapshot with legacy base.snapshot_delete: %d", resp.StatusCode)
	}

	// Audit log must record the canonical new action names.
	gotAudit := strings.Join(sink.actions(), " ")
	for _, want := range []string{"branch.pause:", "branch.resume:", "branch.checkpoint.delete:", "base.snapshot.delete:"} {
		if !strings.Contains(gotAudit, want) {
			t.Errorf("audit missing new action %q in %s", want, gotAudit)
		}
	}

	// Session can & base.can expose the canonical new action names.
	_, sess := send(t, h, "GET", "/v1/auth/session", nil, withSession(cookie, csrf))
	can := sess["can"].(map[string]any)
	for _, a := range []string{"branch.pause", "branch.resume", "branch.checkpoint.delete", "branch.extend"} {
		if can[a] != true {
			t.Errorf("session.can[%q] = %v, want true", a, can[a])
		}
	}
	_, baseBody := send(t, h, "GET", "/v1/bases/prod", nil, withSession(cookie, csrf))
	baseCan := baseBody["can"].(map[string]any)
	if baseCan["base.snapshot.delete"] != true {
		t.Errorf("base.can[base.snapshot.delete] = %v, want true", baseCan["base.snapshot.delete"])
	}

	// Reverse direction: a newer auth module granting only split/dot actions still
	// keeps legacy UI `can` checks (including branch.update) working during rollout.
	f.srv.Ext.Authorizer = &legacyOnlyAuthorizer{allow: map[string]bool{
		"branch.pause":             true,
		"branch.checkpoint.delete": true,
		"auth.session":             true,
	}}
	_, newSess := send(t, f.srv.Handler(false), "GET", "/v1/auth/session", nil, withSession(cookie, csrf))
	newCan := newSess["can"].(map[string]any)
	if newCan["branch.update"] != true || newCan["branch.checkpoint_delete"] != true {
		t.Errorf("reverse aliases in session.can: %v", newCan)
	}
}
