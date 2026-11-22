package httpapi_test

import (
	"io"
	"net/http/httptest"

	"errors"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"net/http"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/pkg/ext"
)

func TestAudit_401認証失敗とCSRF拒否とMCP拒否がAuditSinkに記録される(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}, AuditSink: sink}
	h := f.srv.Handler(false)
	cookie, _ := login(t, f)
	sink.reset()

	if resp, _ := send(t, h, "GET", "/v1/branches", nil, bearer("nope")); resp.StatusCode != 401 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, withSession(&http.Cookie{Name: "dbb_session", Value: "stale"}, "")); resp.StatusCode != 401 {
		t.Fatalf("bad session: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/bases/prod/stop", nil, withSession(cookie, "")); resp.StatusCode != 403 {
		t.Fatalf("csrf: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/mcp", map[string]any{}, bearer("t-viewer")); resp.StatusCode != 403 {
		t.Fatalf("mcp: %d", resp.StatusCode)
	}
	got := strings.Join(sink.actions(), " ")
	want := "auth.token:anonymous:auth/:denied auth.session:anonymous:auth/:denied auth.csrf:admin:auth/:denied mcp.use:token:viewer:mcp/:denied"
	if got != want {
		t.Errorf("audit = %s\nwant    %s", got, want)
	}
}

func TestMCP_modules_pending中はMCPも断る(t *testing.T) {
	f := newFixture(t)
	f.srv.PendingModules = func() []string { return []string{"audit"} }
	f.srv.TokenAuth = &fakeTokens{}
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/mcp", map[string]any{}, bearer("t-full"))
	if resp.StatusCode != 503 || errCode(body) != "modules_pending" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestAudit_tokens削除やsettings更新やbackup取得のリソースIDとプレフィックス解決後ブランチ名が記録される(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{AuditSink: sink}
	f.srv.Tokens = &fakeTokens{}
	f.srv.Backups = &fakeBackups{}
	f.do(t, "DELETE", "/v1/tokens/x", nil)
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{}})
	f.do(t, "GET", "/v1/backups/db-branch-n1-20261110T030000Z.json.gz", nil)
	f.do(t, "DELETE", "/v1/runtimes/mysql/8.4.13", nil)
	got := strings.Join(sink.actions(), " ")
	for _, want := range []string{
		"token.write:local:token/x:ok",
		"setting.write:local:setting/core:",
		"backup.download:local:backup/db-branch-n1-20261110T030000Z.json.gz:",
		"runtime.remove:local:runtime/mysql@8.4.13:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("audit lacks %q: %s", want, got)
		}
	}

	o := ownedFixture(t)
	osink := &recordingSink{}
	o.srv.Ext.AuditSink = osink
	if resp, body := createAs(t, o.srv.Handler(false), "alice", map[string]any{"name": "test"}); resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if got := strings.Join(osink.actions(), " "); !strings.Contains(got, "branch.create:sso:alice:branch/alice-test:") {
		t.Errorf("the resolved name is audited: %s", got)
	}
}

func TestAudit_Fail_closed設定では記録できない変更を実行せず503で断る(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{err: errors.New("siem down")}
	f.srv.Ext = ext.Module{AuditSink: sink}
	closed := true
	f.srv.AuditFailClosed = func() bool { return closed }

	resp, body := f.do(t, "POST", "/v1/bases/prod/stop", nil)
	if resp.StatusCode != 503 || errCode(body) != "audit_unavailable" {
		t.Fatalf("closed: %d %v", resp.StatusCode, body)
	}
	if len(f.branches.calls) != 0 {
		t.Error("the change ran although it could not be audited")
	}
	if resp, _ := f.do(t, "GET", "/v1/branches", nil); resp.StatusCode != 200 {
		t.Errorf("unaudited reads still work: %d", resp.StatusCode)
	}
	closed = false
	if resp, _ := f.do(t, "POST", "/v1/bases/prod/stop", nil); resp.StatusCode != 204 {
		t.Errorf("fail-open (the default) keeps going: %d", resp.StatusCode)
	}
}

func TestMCP_サイズ上限超過ボディを413で拒否する(t *testing.T) {
	f := newFixture(t)
	f.srv.TokenAuth = &fakeTokens{}
	h := f.srv.Handler(false)
	big := strings.Repeat("x", httpapi.MCPMaxBody+1)
	for name, body := range map[string]io.Reader{
		"Content-Length あり": strings.NewReader(big),
		"chunked":           io.NopCloser(strings.NewReader(big)),
	} {
		req := httptest.NewRequest("POST", "/v1/mcp", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer t-full")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 413 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}
