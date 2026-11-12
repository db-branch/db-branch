package httpapi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

func refreshBody() map[string]any {
	return map[string]any{
		"source": map[string]any{"host": "db.example.com", "port": 3306, "user": "dumper", "password": "hunter2", "databases": []string{"app"}},
		"every":  "24h",
	}
}

func TestRefresh_設定して読み戻しパスワードは返さない(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/bases/prod/refresh", nil); resp.StatusCode != 404 {
		t.Fatalf("before set: %d", resp.StatusCode)
	}
	resp, body := f.do(t, "PUT", "/v1/bases/prod/refresh", refreshBody())
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %v", resp.StatusCode, body)
	}
	resp, body = f.do(t, "GET", "/v1/bases/prod/refresh", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d %v", resp.StatusCode, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("password leaked: %s", raw)
	}
	src := body["source"].(map[string]any)
	if body["base"] != "prod" || body["every"] != "24h0m0s" || src["host"] != "db.example.com" || src["password_set"] != true || body["engine"] != "mysql" {
		t.Errorf("body = %v", body)
	}
	if body["last_run"] != nil || body["next_run"] == nil || body["operation"] != nil {
		t.Errorf("run fields = %v", body)
	}
}

func TestRefresh_最後の実行結果と実行中の操作を返す(t *testing.T) {
	f := newFixture(t)
	f.branches.refreshes = map[string]domain.BaseRefresh{"prod": {
		Base: name("prod"), Every: time.Hour, UpdatedAt: t0,
		Source:  domain.RefreshSource{Host: "h", Port: 1, User: "u", Databases: []string{"a"}},
		LastRun: &domain.RefreshRun{StartedAt: t0, FinishedAt: t0.Add(time.Minute), OK: false, Error: "boom"},
	}}
	_, body := f.do(t, "GET", "/v1/bases/prod/refresh", nil)
	run := body["last_run"].(map[string]any)
	if run["ok"] != false || run["error"] != "boom" || run["finished_at"] != "2026-11-09T12:01:00Z" || body["next_run"] != "2026-11-09T13:01:00Z" {
		t.Errorf("body = %v", body)
	}
	if body["source"].(map[string]any)["password_set"] != false {
		t.Errorf("source = %v", body["source"])
	}
}

func TestRefresh_不正な設定は400で通らない(t *testing.T) {
	f := newFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"every":   func(b map[string]any) { b["every"] = "soon" },
		"short":   func(b map[string]any) { b["every"] = "5m" },
		"missing": func(b map[string]any) { delete(b, "every") },
		"host":    func(b map[string]any) { b["source"].(map[string]any)["host"] = "-x" },
		"unknown": func(b map[string]any) { b["bogus"] = 1 },
	} {
		b := refreshBody()
		mutate(b)
		if resp, body := f.do(t, "PUT", "/v1/bases/prod/refresh", b); resp.StatusCode != 400 {
			t.Errorf("%s: %d %v", name, resp.StatusCode, body)
		}
	}
	if resp, _ := f.do(t, "PUT", "/v1/bases/nope/refresh", refreshBody()); resp.StatusCode != 404 {
		t.Errorf("missing base: %d", resp.StatusCode)
	}
}

func TestRefresh_オフにすると消える(t *testing.T) {
	f := newFixture(t)
	f.do(t, "PUT", "/v1/bases/prod/refresh", refreshBody())
	if resp, _ := f.do(t, "DELETE", "/v1/bases/prod/refresh", nil); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/bases/prod/refresh", nil); resp.StatusCode != 404 {
		t.Errorf("delete twice: %d", resp.StatusCode)
	}
}

func TestRefresh_今すぐ実行は202で操作を返す(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "POST", "/v1/bases/prod/refresh/run", nil); resp.StatusCode != 404 {
		t.Fatalf("without schedule: %d", resp.StatusCode)
	}
	f.do(t, "PUT", "/v1/bases/prod/refresh", refreshBody())
	resp, body := f.do(t, "POST", "/v1/bases/prod/refresh/run", nil)
	if resp.StatusCode != 202 || body["kind"] != "base.refresh" {
		t.Fatalf("run: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	if !strings.Contains(strings.Join(f.branches.calls, "|"), "refresh-base prod") {
		t.Errorf("calls = %q", f.branches.calls)
	}
}

func TestRefresh_今すぐ実行はベースを持つノードへ転送する(t *testing.T) {
	f, c := remoteFixture(t)
	f.branches.refreshes = map[string]domain.BaseRefresh{"far": {Base: name("far"), Every: time.Hour}}
	resp, _ := f.do(t, "POST", "/v1/bases/far/refresh/run", nil)
	if resp.StatusCode != 202 || len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 POST /v1/bases/far/refresh/run") {
		t.Errorf("%d %v", resp.StatusCode, c.forwarded)
	}
	if len(f.branches.calls) != 0 {
		t.Errorf("ran here: %v", f.branches.calls)
	}
}

// perBase allows base actions only on the base "prod".
type perBase struct{}

func (perBase) Authorize(_ context.Context, _ ext.Principal, action string, r ext.Resource) error {
	if r.Kind == "base" && r.Name != "" && r.Name != "prod" && strings.HasPrefix(action, "base.") && action != "base.read" {
		return ext.ErrForbidden
	}
	return nil
}

func TestBases_ベースごとの権限canを一覧と個別の応答に載せる(t *testing.T) {
	f := newFixture(t)
	f.branches.bases["other"] = domain.Base{Name: name("other"), Runtime: mysql84, Status: domain.BaseReady}
	f.srv.Ext = ext.Module{Authorizer: perBase{}}
	_, body := f.do(t, "GET", "/v1/bases", nil)
	got := map[string]map[string]any{}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		got[m["name"].(string)] = m["can"].(map[string]any)
	}
	for _, a := range []string{"base.snapshot", "base.refresh", "base.refresh.read", "base.delete", "base.load", "base.sql", "base.snapshot_delete"} {
		if got["prod"][a] != true || got["other"][a] != false {
			t.Errorf("%s: prod=%v other=%v", a, got["prod"][a], got["other"][a])
		}
	}
	_, one := f.do(t, "GET", "/v1/bases/other", nil)
	if one["can"].(map[string]any)["base.snapshot"] != false {
		t.Errorf("single = %v", one["can"])
	}
}
