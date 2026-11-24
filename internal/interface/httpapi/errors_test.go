package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

var branchEndpoints = [][2]string{
	{"GET", "/v1/branches"}, {"POST", "/v1/branches"}, {"GET", "/v1/branches/x"}, {"PATCH", "/v1/branches/x"},
	{"DELETE", "/v1/branches/x"}, {"POST", "/v1/branches/x/reset"}, {"GET", "/v1/branches/x/dsn"},
	{"GET", "/v1/bases"}, {"POST", "/v1/bases"}, {"GET", "/v1/bases/x"}, {"DELETE", "/v1/bases/x"},
	{"POST", "/v1/bases/x/snapshots"}, {"POST", "/v1/bases/x/start"}, {"POST", "/v1/bases/x/stop"},
}

func Test初期化前はどのブランチ系エンドポイントも503(t *testing.T) {
	f := newFixture(t)
	f.srv.Branches = func() (httpapi.Branches, error) { return nil, errors.New("not initialized") }
	for _, e := range branchEndpoints {
		resp, body := f.do(t, e[0], e[1], map[string]any{})
		if resp.StatusCode != 503 || errCode(body) != "not_initialized" {
			t.Errorf("%s %s: %d %v", e[0], e[1], resp.StatusCode, body)
		}
	}
}

func Testパスの名前が不正なら400(t *testing.T) {
	f := newFixture(t)
	for _, e := range branchEndpoints {
		if !strings.Contains(e[1], "/x") {
			continue
		}
		path := strings.Replace(e[1], "/x", "/Bad_Name", 1)
		resp, body := f.do(t, e[0], path, map[string]any{})
		if resp.StatusCode != 400 || errCode(body) != "invalid_request" {
			t.Errorf("%s %s: %d %v", e[0], path, resp.StatusCode, body)
		}
	}
}

func Test存在しないものは404(t *testing.T) {
	f := newFixture(t)
	for _, e := range [][2]string{
		{"GET", "/v1/branches/nope/dsn"}, {"PATCH", "/v1/branches/nope"}, {"GET", "/v1/bases/nope"},
		{"POST", "/v1/bases/nope/snapshots"},
	} {
		resp, body := f.do(t, e[0], e[1], map[string]any{"ttl": "1h"})
		if resp.StatusCode != 404 {
			t.Errorf("%s %s: %d %v", e[0], e[1], resp.StatusCode, body)
		}
	}
}

func Test壊れたJSONは400(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Status: domain.BranchReady}
	for _, e := range [][2]string{
		{"POST", "/v1/branches"}, {"PATCH", "/v1/branches/x"}, {"POST", "/v1/bases"}, {"POST", "/v1/runtimes"},
		{"POST", "/v1/auth/login"}, {"PUT", "/v1/auth/password"},
	} {
		resp, _ := send(t, f.srv.Handler(true), e[0], e[1], nil, func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("{")) })
		if resp.StatusCode != 400 {
			t.Errorf("%s %s: %d", e[0], e[1], resp.StatusCode)
		}
	}
	for _, body := range []map[string]any{{"name": "x", "runtime": "mysql@8.4.13", "extra": 1}, {"name": "Bad", "runtime": "mysql@8.4.13"}} {
		if resp, _ := f.do(t, "POST", "/v1/bases", body); resp.StatusCode != 400 {
			t.Errorf("%v: %d", body, resp.StatusCode)
		}
	}
	if resp, _ := f.do(t, "POST", "/v1/branches", map[string]any{"name": "ok", "base": "Bad"}); resp.StatusCode != 400 {
		t.Errorf("bad base name: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql"}); resp.StatusCode != 400 {
		t.Errorf("bad runtime: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/runtimes/oracle/1.0", nil); resp.StatusCode != 400 {
		t.Errorf("bad runtime path: %d", resp.StatusCode)
	}
}

func Testユースケースのエラーを決まったコードに対応させる(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrNotFound, 404, "not_found"},
		{domain.ErrAlreadyExists, 409, "already_exists"},
		{domain.ErrInUse, 409, "in_use"},
		{domain.ErrUncleanShutdown, 409, "unclean_shutdown"},
		{domain.ErrFeatureNotSupported, 409, "feature_not_supported"},
		{domain.ErrUnsupportedEngine, 409, "feature_not_supported"},
		{domain.ErrConflict, 409, "conflict"},
		{domain.ErrRuntimeNotInstalled, 404, "runtime_not_installed"},
		{domain.ErrRuntimeNotFound, 422, "runtime_unavailable"},
		{domain.ErrRuntimeInstalled, 409, "already_exists"},
		{domain.ErrInvalidTTL, 400, "invalid_request"},
		{auth.ErrInvalidCredentials, 401, "invalid_credentials"},
		{auth.ErrNoSession, 401, "unauthenticated"},
		{auth.ErrWeakPassword, 400, "weak_password"},
		{errors.New("disk on fire"), 500, "internal"},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.branches.fail = fmt.Errorf("wrapped: %w", c.err)
		resp, body := f.do(t, "DELETE", "/v1/bases/prod", nil)
		if resp.StatusCode != c.status || errCode(body) != c.code {
			t.Errorf("%v: %d %v", c.err, resp.StatusCode, body)
		}
	}
}

func Test一覧の失敗は500(t *testing.T) {
	f := newFixture(t)
	f.branches.fail = errors.New("boom")
	if resp, _ := f.do(t, "GET", "/v1/branches", nil); resp.StatusCode != 500 {
		t.Errorf("branches: %d", resp.StatusCode)
	}
	f.runtimes.listErr = errors.New("boom")
	if resp, _ := f.do(t, "GET", "/v1/runtimes", nil); resp.StatusCode != 500 {
		t.Errorf("runtimes: %d", resp.StatusCode)
	}
}

func Test空の一覧は空配列で返し失敗したブランチは理由を持つ(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "GET", "/v1/branches", nil)
	if items, ok := body["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("empty = %v", body)
	}
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Status: domain.BranchFailed, Error: "clone failed"}
	_, body = f.do(t, "GET", "/v1/branches/x", nil)
	if body["status_reason"] != "clone failed" {
		t.Errorf("body = %v", body)
	}
}

func Testランタイムの削除とベースの削除は変化を知らせる(t *testing.T) {
	f := newFixture(t)
	sub := f.srv.Bus.Subscribe(0)
	defer sub.Close()
	f.runtimes.removeErr = nil
	if resp, _ := f.do(t, "DELETE", "/v1/runtimes/mysql/8.4.13", nil); resp.StatusCode != 204 {
		t.Errorf("remove runtime: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/bases/prod", nil); resp.StatusCode != 204 {
		t.Errorf("delete base: %d", resp.StatusCode)
	}
	var types []string
	for len(types) < 2 {
		select {
		case ev := <-sub.Events():
			types = append(types, ev.Type)
		case <-time.After(time.Second):
			t.Fatalf("events = %v", types)
		}
	}
	if strings.Join(types, ",") != "runtime.deleted,base.deleted" {
		t.Errorf("events = %v", types)
	}
}

func Testソケット経由ではコンソールのセッション操作はできない(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/auth/session", nil); resp.StatusCode != 401 {
		t.Errorf("session: %d", resp.StatusCode)
	}
	resp, _ := f.do(t, "PUT", "/v1/auth/password", map[string]any{"current_password": "a", "new_password": "b"})
	if resp.StatusCode != 401 {
		t.Errorf("password: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/auth/logout", nil); resp.StatusCode != 204 {
		t.Errorf("logout: %d", resp.StatusCode)
	}
}

func Test画面がなければ404で期限切れのセッションは401(t *testing.T) {
	f := newFixture(t)
	if resp, _ := send(t, f.srv.Handler(false), "GET", "/", nil, nil); resp.StatusCode != 404 {
		t.Errorf("no console: %d", resp.StatusCode)
	}
	resp, _ := send(t, f.srv.Handler(false), "GET", "/v1/branches", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "dbb_session", Value: "forged"})
	})
	if resp.StatusCode != 401 {
		t.Errorf("forged session: %d", resp.StatusCode)
	}
}

func TestEvents_古すぎる続きはresyncを送る(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 150; i++ {
		f.srv.Bus.Publish("x", i)
	}
	srv := httptest.NewServer(f.srv.Handler(true))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/events", nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "event: resync\n" {
		t.Errorf("first line = %q", line)
	}
}

type noFlush struct{ http.ResponseWriter }

func TestEvents_ストリームできない接続では500(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.srv.Handler(true).ServeHTTP(noFlush{rec}, httptest.NewRequest("GET", "/v1/events", nil))
	if rec.Code != 500 {
		t.Errorf("status %d", rec.Code)
	}
}

type failingBases struct {
	*fakeBranches
}

func (failingBases) ListBases(context.Context) ([]domain.Base, error) { return nil, errors.New("boom") }

func Testベース一覧の失敗とTTL延長の失敗(t *testing.T) {
	f := newFixture(t)
	f.srv.Branches = func() (httpapi.Branches, error) { return failingBases{f.branches}, nil }
	if resp, _ := f.do(t, "GET", "/v1/bases", nil); resp.StatusCode != 500 {
		t.Errorf("bases: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PATCH", "/v1/branches/x", map[string]any{"ttl": "forever"}); resp.StatusCode != 400 {
		t.Errorf("bad ttl: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches", map[string]any{"name": "ok", "base": "prod", "snapshot": "s1"}); resp.StatusCode != 202 {
		t.Errorf("with snapshot: %d", resp.StatusCode)
	}
}

func TestOperations_時刻は秒までのUTCで返す(t *testing.T) {
	f := newFixture(t)
	f.ops.Clock = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 123456789, time.FixedZone("JST", 9*3600)) }
	_, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod"})
	if body["created_at"] != "2026-11-09T03:00:00Z" {
		t.Errorf("created_at = %v", body["created_at"])
	}
	f.waitOp(t, body["id"].(string))
	_, got := f.do(t, "GET", "/v1/operations/"+body["id"].(string), nil)
	if got["finished_at"] != "2026-11-09T03:00:00Z" || got["updated_at"] != "2026-11-09T03:00:00Z" {
		t.Errorf("op = %v", got)
	}
}

func TestHTTPAPI_500エラー応答に内部エラー文字列を含めずログに残す(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	f := newFixture(t)
	f.srv.Storage = func(context.Context) (domain.StorageReport, error) {
		return domain.StorageReport{}, errors.New("zfs list tank/db-branch/secret-path: dataset busy")
	}
	f.branches.fail = errors.New("open /var/lib/db-branch/raft.db: permission denied")
	for _, path := range []string{"/v1/nodes/node1/storage", "/v1/branches"} {
		resp, body := f.do(t, "GET", path, nil)
		if resp.StatusCode != 500 || errCode(body) != "internal" {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, body)
		}
		msg := fmt.Sprint(body["error"])
		if strings.Contains(msg, "/var/lib") || strings.Contains(msg, "zfs list") {
			t.Errorf("%s leaks the internal error: %s", path, msg)
		}
	}
	if !strings.Contains(logs.String(), "dataset busy") || !strings.Contains(logs.String(), "permission denied") {
		t.Errorf("internal errors must be logged: %s", logs.String())
	}
}
