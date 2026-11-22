package httpapi_test

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func login(t *testing.T, f *fixture) (cookie *http.Cookie, csrf string) {
	t.Helper()
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/auth/login",
		map[string]any{"username": "admin", "password": "correct horse battery"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "dbb_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", cookie)
	}
	return cookie, body["csrf_token"].(string)
}

func withSession(c *http.Cookie, csrf string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(c)
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
	}
}

func TestConsole_ログインしていなければ401(t *testing.T) {
	f := newFixture(t)
	resp, body := send(t, f.srv.Handler(false), "GET", "/v1/branches", nil, nil)
	if resp.StatusCode != 401 || errCode(body) != "unauthenticated" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestConsole_ログインすれば読めて書き込みにはCSRFトークンが要る(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)

	if resp, _ := send(t, h, "GET", "/v1/branches", nil, withSession(cookie, "")); resp.StatusCode != 200 {
		t.Errorf("read: %d", resp.StatusCode)
	}
	resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod"}, withSession(cookie, ""))
	if resp.StatusCode != 403 || errCode(body) != "csrf_failed" {
		t.Errorf("no csrf: %d %v", resp.StatusCode, body)
	}
	resp, body = send(t, h, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod"}, withSession(cookie, "wrong"))
	if resp.StatusCode != 403 {
		t.Errorf("wrong csrf: %d %v", resp.StatusCode, body)
	}
	resp, body = send(t, h, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod"}, withSession(cookie, csrf))
	if resp.StatusCode != 202 {
		t.Errorf("with csrf: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
}

func TestConsole_セッション確認とログアウト(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)

	resp, body := send(t, h, "GET", "/v1/auth/session", nil, withSession(cookie, ""))
	if resp.StatusCode != 200 || body["csrf_token"] != csrf || body["user"].(map[string]any)["name"] != "admin" {
		t.Errorf("session: %d %v", resp.StatusCode, body)
	}
	if resp, _ := send(t, h, "POST", "/v1/auth/logout", nil, withSession(cookie, csrf)); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/auth/session", nil, withSession(cookie, "")); resp.StatusCode != 401 {
		t.Errorf("after logout: %d", resp.StatusCode)
	}
}

func TestConsole_パスワードを間違えると401で続けば429(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Handler(false)
	var last *http.Response
	for i := 0; i < 6; i++ {
		last, _ = send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "nope"}, nil)
	}
	if last.StatusCode != 429 || last.Header.Get("Retry-After") == "" {
		t.Errorf("status %d retry-after %q", last.StatusCode, last.Header.Get("Retry-After"))
	}
	resp, body := send(t, newFixture(t).srv.Handler(false), "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "nope"}, nil)
	if resp.StatusCode != 401 || errCode(body) != "invalid_credentials" {
		t.Errorf("first failure: %d %v", resp.StatusCode, body)
	}
}

func TestConsole_パスワードを変更する(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)

	resp, body := send(t, h, "PUT", "/v1/auth/password",
		map[string]any{"current_password": "correct horse battery", "new_password": "short"}, withSession(cookie, csrf))
	if resp.StatusCode != 400 || errCode(body) != "weak_password" {
		t.Errorf("weak: %d %v", resp.StatusCode, body)
	}
	resp, body = send(t, h, "PUT", "/v1/auth/password",
		map[string]any{"current_password": "wrong", "new_password": "a much better passphrase"}, withSession(cookie, csrf))
	if resp.StatusCode != 401 {
		t.Errorf("wrong current: %d %v", resp.StatusCode, body)
	}
	resp, _ = send(t, h, "PUT", "/v1/auth/password",
		map[string]any{"current_password": "correct horse battery", "new_password": "a much better passphrase"}, withSession(cookie, csrf))
	if resp.StatusCode != 204 {
		t.Errorf("change: %d", resp.StatusCode)
	}
}

func TestConsole_画面はログインなしで配信する(t *testing.T) {
	f := newFixture(t)
	f.srv.Console = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>console</html>")) })
	req := httptest.NewRequest("GET", "/branches", nil)
	rec := httptest.NewRecorder()
	f.srv.Handler(false).ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "console") {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("security headers missing: %v", rec.Header())
	}
}

func TestEvents_変化をSSEで流す(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.srv.Handler(true))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	f.srv.Bus.Publish("branch.updated", map[string]string{"name": "x"})

	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "" && len(lines) > 1 {
			break
		}
	}
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "id: 1") || !strings.Contains(got, "event: branch.updated") || !strings.Contains(got, `data: {"name":"x"}`) {
		t.Errorf("stream = %q", got)
	}
}

func loginWith(t *testing.T, f *fixture, password string) (*http.Cookie, string) {
	t.Helper()
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": password}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "dbb_session" {
			return c, body["csrf_token"].(string)
		}
	}
	t.Fatal("no session cookie")
	return nil, ""
}

func TestConsole_初期パスワードのままなら変更するまで他の操作を断る(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Handler(false)
	if err := f.srv.Auth.SetInitialPassword("admin"); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginWith(t, f, "admin")

	resp, body := send(t, h, "GET", "/v1/auth/session", nil, withSession(cookie, ""))
	if resp.StatusCode != 200 || body["must_change_password"] != true {
		t.Errorf("session: %d %v", resp.StatusCode, body)
	}
	for _, tc := range []struct{ method, path string }{{"GET", "/v1/branches"}, {"POST", "/v1/branches"}, {"GET", "/v1/bases"}} {
		resp, body := send(t, h, tc.method, tc.path, map[string]any{"name": "x", "base": "prod"}, withSession(cookie, csrf))
		if resp.StatusCode != 403 || errCode(body) != "password_change_required" {
			t.Errorf("%s %s: %d %v", tc.method, tc.path, resp.StatusCode, body)
		}
	}
	resp, _ = send(t, h, "PUT", "/v1/auth/password",
		map[string]any{"current_password": "admin", "new_password": "a much better passphrase"}, withSession(cookie, csrf))
	if resp.StatusCode != 204 {
		t.Fatalf("change: %d", resp.StatusCode)
	}
	cookie, _ = loginWith(t, f, "a much better passphrase")
	if resp, _ := send(t, h, "GET", "/v1/branches", nil, withSession(cookie, "")); resp.StatusCode != 200 {
		t.Errorf("after the change: %d", resp.StatusCode)
	}
	if _, body := send(t, h, "GET", "/v1/auth/session", nil, withSession(cookie, "")); body["must_change_password"] != false {
		t.Errorf("session after the change: %v", body)
	}
}

func TestConsole_初期パスワードのままでもログアウトはできる(t *testing.T) {
	f := newFixture(t)
	_ = f.srv.Auth.SetInitialPassword("admin")
	cookie, csrf := loginWith(t, f, "admin")
	if resp, _ := send(t, f.srv.Handler(false), "POST", "/v1/auth/logout", nil, withSession(cookie, csrf)); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
}

func TestLogin_特定IPの連続失敗は別IPからの正規ログインを止めない(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Handler(false)
	from := func(addr, pw string) int {
		resp, _ := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": pw},
			func(r *http.Request) { r.RemoteAddr = addr })
		return resp.StatusCode
	}
	for i := 0; i < 5; i++ {
		from("203.0.113.9:4000", "wrong")
	}
	if got := from("203.0.113.9:4001", "correct horse battery"); got != http.StatusTooManyRequests {
		t.Errorf("attacker address: %d, want 429", got)
	}
	if got := from("198.51.100.1:5000", "correct horse battery"); got != http.StatusOK {
		t.Errorf("other address: %d, want 200", got)
	}
}

type failingRevocations struct{ err error }

func (f failingRevocations) Revoke(string, time.Time, time.Time) error { return f.err }
func (failingRevocations) Revoked(string) bool                         { return false }

func TestConsole_ログアウトが他ノードに記録できなければ503で知らせクラスタ更新中は従来どおり204(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{errors.New("no leader"), 503}, {domain.ErrClusterUpgrading, 204}} {
		f := newFixture(t)
		f.srv.Auth.Revocations = failingRevocations{err: tc.err}
		cookie, csrf := login(t, f)
		resp, _ := send(t, f.srv.Handler(false), "POST", "/v1/auth/logout", nil, withSession(cookie, csrf))
		if resp.StatusCode != tc.want {
			t.Errorf("%v: logout = %d, want %d", tc.err, resp.StatusCode, tc.want)
		}
		if after, _ := send(t, f.srv.Handler(false), "GET", "/v1/auth/session", nil, withSession(cookie, "")); after.StatusCode != 401 {
			t.Errorf("%v: session after logout = %d, want 401 (ended on this node)", tc.err, after.StatusCode)
		}
	}
}
