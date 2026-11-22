package httpapi_test

import (
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/pkg/ext"
)

func TestUploads_HTTPS経由でuploadプレフィックス以外のローカルパス指定を拒否する(t *testing.T) {
	f, _ := uploadFixture(t)
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/bases/prod/load", map[string]any{"dump": "/etc/passwd"}},
		{"/v1/bases", map[string]any{"name": "fresh", "runtime": "mysql@8.4.13", "dump": "/etc/shadow"}},
	} {
		resp, body := send(t, h, "POST", tc.path, tc.body, withSession(cookie, csrf))
		if resp.StatusCode != 400 || errCode(body) != "invalid_request" {
			t.Errorf("%s dump=%v: %d %v, want 400 invalid_request", tc.path, tc.body["dump"], resp.StatusCode, body)
		}
	}
	// upload: references still reach the upload store (unknown id → 404).
	resp, _ := send(t, h, "POST", "/v1/bases/prod/load", map[string]any{"dump": "upload:00000000000000000000000000000000"}, withSession(cookie, csrf))
	if resp.StatusCode != 404 {
		t.Errorf("upload ref over HTTPS: %d, want 404", resp.StatusCode)
	}
}

func TestHTTPAPI_ExtPublicへのCookie認証POSTでCSRFトークン欠如およびMustChange状態を403拒否する(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	cap := &captureExtHandler{}
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}, AuditSink: sink}
	f.srv.ExtHandlers = map[string]ext.Handler{"sso": cap}
	f.srv.ExtPublic = map[string][]string{"sso": {"/public"}}
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)

	if resp, body := send(t, h, "POST", "/v1/ext/sso/public/x", nil, withSession(cookie, "")); resp.StatusCode != 403 || errCode(body) != "csrf_failed" || len(cap.principals()) != 0 {
		t.Errorf("no csrf: %d %v seen=%+v, want 403 csrf_failed and no module call", resp.StatusCode, body, cap.principals())
	}
	if resp, _ := send(t, h, "GET", "/v1/ext/sso/public/x", nil, withSession(cookie, "")); resp.StatusCode != 200 {
		t.Errorf("GET without csrf: %d, want 200", resp.StatusCode)
	}
	cap.reset()
	if resp, _ := send(t, h, "POST", "/v1/ext/sso/public/x", nil, withSession(cookie, csrf)); resp.StatusCode != 200 || len(cap.principals()) != 1 || cap.principals()[0].ID == "" {
		t.Errorf("with csrf: %d seen=%+v, want 200 with the session principal", resp.StatusCode, cap.principals())
	}
	if got := strings.Join(sink.actions(), " "); !strings.Contains(got, "ext.sso.write") {
		t.Errorf("audit = %q, want ext.sso.write recorded", got)
	}
	cap.reset()
	if resp, _ := send(t, h, "POST", "/v1/ext/sso/public/x", nil, nil); resp.StatusCode != 200 || cap.principals()[0].ID != httpapi.Anonymous.ID {
		t.Errorf("anonymous POST: %d seen=%+v, want 200 anonymous", resp.StatusCode, cap.principals())
	}

	// A session that must change its password may not use module paths either.
	g := newFixture(t)
	g.srv.Ext = f.srv.Ext
	g.srv.ExtHandlers = f.srv.ExtHandlers
	g.srv.ExtPublic = f.srv.ExtPublic
	if err := g.srv.Auth.SetInitialPassword("admin"); err != nil {
		t.Fatal(err)
	}
	c2, csrf2 := loginWith(t, g, "admin")
	cap.reset()
	if resp, body := send(t, g.srv.Handler(false), "GET", "/v1/ext/sso/public/x", nil, withSession(c2, csrf2)); resp.StatusCode != 403 || errCode(body) != "password_change_required" || len(cap.principals()) != 0 {
		t.Errorf("must change: %d %v seen=%+v", resp.StatusCode, body, cap.principals())
	}
}
