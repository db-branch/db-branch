package httpapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

type fakeRouting struct {
	users []domain.RouteUser
	err   error
}

func (f *fakeRouting) AddUser(_ context.Context, u domain.RouteUser) error {
	if u.Password == "" {
		return domain.ErrInvalidRouteUser
	}
	f.users = append(f.users, u)
	return f.err
}

func (f *fakeRouting) RemoveUser(_ context.Context, name string) error {
	if name == "nope" {
		return domain.ErrNotFound
	}
	return f.err
}

func (f *fakeRouting) ListUsers(context.Context) ([]domain.RouteUser, error) { return f.users, f.err }

func TestRouteUsers_登録と一覧と削除でパスワードは返さない(t *testing.T) {
	f := newFixture(t)
	if resp, body := f.do(t, "GET", "/v1/route-users", nil); resp.StatusCode != 409 || errCode(body) != "no_proxy" {
		t.Errorf("without proxy: %d %v", resp.StatusCode, body)
	}
	rt := &fakeRouting{}
	f.srv.Routing = rt
	resp, body := f.do(t, "PUT", "/v1/route-users/app", map[string]any{"password": "secret", "default_branch": "prod"})
	if resp.StatusCode != 200 || body["default_branch"] != "prod" || len(rt.users) != 1 || rt.users[0].Password != "secret" {
		t.Fatalf("%d %v %+v", resp.StatusCode, body, rt.users)
	}
	resp, body = f.do(t, "GET", "/v1/route-users", nil)
	item := body["items"].([]any)[0].(map[string]any)
	if resp.StatusCode != 200 || item["name"] != "app" || item["password"] != nil {
		t.Errorf("list: %v", body)
	}
	if resp, _ := f.do(t, "PUT", "/v1/route-users/app", map[string]any{"password": "", "default_branch": "prod"}); resp.StatusCode != 400 {
		t.Errorf("invalid: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/route-users/app", map[string]any{"password": "p", "default_branch": "Bad"}); resp.StatusCode != 400 {
		t.Errorf("bad default: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/route-users/app", nil); resp.StatusCode != 204 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/route-users/nope", nil); resp.StatusCode != 404 {
		t.Errorf("delete missing: %d", resp.StatusCode)
	}
	rt.err = errors.New("raft down")
	if resp, _ := f.do(t, "GET", "/v1/route-users", nil); resp.StatusCode != 500 {
		t.Errorf("list failure: %d", resp.StatusCode)
	}
	f.srv.Routing = nil
	for _, m := range []string{"PUT", "DELETE"} {
		if resp, _ := f.do(t, m, "/v1/route-users/app", map[string]any{}); resp.StatusCode != 409 {
			t.Errorf("%s without proxy: %d", m, resp.StatusCode)
		}
	}
}

func TestBranch_プロキシ経由の接続先を示す(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["feature-x"] = domain.Branch{Name: name("feature-x"), Runtime: mysql84, Status: domain.BranchReady, Port: 40001, User: "feature-x", Password: "pw"}
	f.branches.branches["legacy"] = domain.Branch{Name: name("legacy"), Runtime: mysql84, Status: domain.BranchReady, Port: 40002, User: "branch", Password: "pw"}
	if _, body := f.do(t, "GET", "/v1/branches/feature-x", nil); body["proxy"] != nil {
		t.Errorf("no proxy: %v", body["proxy"])
	}
	if resp, body := f.do(t, "GET", "/v1/branches/feature-x/dsn?via=proxy", nil); resp.StatusCode != 409 || errCode(body) != "not_routable" {
		t.Errorf("no proxy dsn: %d %v", resp.StatusCode, body)
	}
	f.srv.ProxyPorts = map[domain.Engine]int{domain.MySQL: 6033, domain.Postgres: 6133}
	f.branches.branches["pg-x"] = domain.Branch{Name: name("pg-x"), Runtime: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}, Status: domain.BranchReady, Port: 40010, User: "pg-x", Password: "pw"}
	if _, body := f.do(t, "GET", "/v1/branches/pg-x/dsn?via=proxy", nil); !strings.HasPrefix(body["dsn"].(string), "postgres://pg-x:pw@192.0.2.10:6133/postgres") {
		t.Errorf("pg dsn = %v", body["dsn"])
	}
	_, body := f.do(t, "GET", "/v1/branches/feature-x", nil)
	if p := body["proxy"].(map[string]any); p["port"] != 6033.0 || p["user"] != "feature-x" || p["host"] != "192.0.2.10" {
		t.Errorf("proxy = %v", p)
	}
	_, body = f.do(t, "GET", "/v1/branches/feature-x/dsn?via=proxy", nil)
	if dsn := body["dsn"].(string); !strings.HasPrefix(dsn, "mysql://feature-x:pw@192.0.2.10:6033/") {
		t.Errorf("dsn = %s", dsn)
	}
	if resp, _ := f.do(t, "GET", "/v1/branches/legacy/dsn?via=proxy", nil); resp.StatusCode != 409 {
		t.Errorf("legacy: %d", resp.StatusCode)
	}
}
