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
	if got != "base.delete:token:viewer:base/prod:denied base.stop:token:admin:base/prod:ok" {
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
	f.branches.branches["here"] = domain.Branch{Name: name("here"), Status: domain.BranchReady}
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
	_, body = send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil)
	can, _ = body["can"].(map[string]any)
	for _, a := range []string{"base.create", "runtime.install", "setting.write", "module.install"} {
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
