package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/interface/httpapi"
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
