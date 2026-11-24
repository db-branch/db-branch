package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
)

type fakeModules struct {
	specs     []domain.ModuleSpec
	err       error
	forgetErr error
	availErr  error
}

func (f *fakeModules) Available(context.Context) (map[string]string, error) {
	return map[string]string{"proxysql": "ProxySQL routing", "audit": "Audit trail"}, f.availErr
}

func (f *fakeModules) Install(_ context.Context, name, version string) (domain.ModuleSpec, error) {
	if name == "Bad" {
		return domain.ModuleSpec{}, domain.ErrInvalidModule
	}
	if version == "" {
		version = "1.0"
	}
	m := domain.ModuleSpec{Name: name, Version: version, Wanted: true}
	f.specs = append(f.specs, m)
	return m, f.err
}

func (f *fakeModules) Remove(_ context.Context, name string) error {
	for i := range f.specs {
		if f.specs[i].Name == name {
			f.specs[i].Wanted = false
			return f.err
		}
	}
	return domain.ErrNotFound
}

func (f *fakeModules) List(context.Context) ([]domain.ModuleSpec, error) { return f.specs, f.err }

func (f *fakeModules) Forget(_ context.Context, name string) error {
	if f.forgetErr != nil {
		return f.forgetErr
	}
	for i := range f.specs {
		if f.specs[i].Name == name {
			if f.specs[i].Wanted {
				return domain.ErrConflict
			}
			f.specs = append(f.specs[:i], f.specs[i+1:]...)
			return f.err
		}
	}
	return domain.ErrNotFound
}

func TestModules_有効化と一覧と無効化(t *testing.T) {
	f, c := remoteFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/modules", nil); resp.StatusCode != 503 {
		t.Errorf("without admin: %d", resp.StatusCode)
	}
	mods := &fakeModules{}
	f.srv.ModuleAdmin = mods
	c.status = httpapi.ClusterStatus{Mode: "cluster", Nodes: []httpapi.NodeStatus{{Name: "n1", Modules: []string{"proxysql 1.0"}},
		{Name: "n2", Modules: []string{"proxysql 0.9"}, ModuleProblems: map[string]string{"proxysql": "licence expired"}}}}
	resp, body := f.do(t, "POST", "/v1/modules", map[string]any{"name": "proxysql"})
	if resp.StatusCode != 200 || body["version"] != "1.0" {
		t.Fatalf("install: %d %v", resp.StatusCode, body)
	}
	_, body = f.do(t, "GET", "/v1/modules", nil)
	item := body["items"].([]any)[0].(map[string]any)
	nodes := item["nodes"].([]any)
	n1, n2 := nodes[0].(map[string]any), nodes[1].(map[string]any)
	if item["wanted"] != true || n1["current"] != true || n2["current"] != false || n2["loaded"] != "0.9" || n2["problem"] != "licence expired" || body["available"].(map[string]any)["audit"] != "Audit trail" {
		t.Errorf("list = %v", body)
	}
	if v, ok := body["next_cursor"]; !ok || v != nil {
		t.Errorf("modules next_cursor = %v (present=%v), want null", v, ok)
	}
	mods.availErr = errors.New("offline")
	if _, body := f.do(t, "GET", "/v1/modules", nil); len(body["available"].(map[string]any)) != 0 {
		t.Errorf("offline: %v", body["available"])
	}
	mods.availErr = nil
	if resp, _ := f.do(t, "POST", "/v1/modules", map[string]any{"name": "Bad"}); resp.StatusCode != 400 {
		t.Errorf("bad name: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/modules/proxysql", nil); resp.StatusCode != 204 {
		t.Errorf("remove: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/modules/nope", nil); resp.StatusCode != 404 {
		t.Errorf("remove missing: %d", resp.StatusCode)
	}
	mods.err = errors.New("raft down")
	if resp, _ := f.do(t, "GET", "/v1/modules", nil); resp.StatusCode != 500 {
		t.Errorf("list failure: %d", resp.StatusCode)
	}
	f.srv.ModuleAdmin = nil
	for _, m := range []string{"POST", "DELETE"} {
		if resp, _ := f.do(t, m, "/v1/modules/x", map[string]any{"name": "x"}); resp.StatusCode != 503 && resp.StatusCode != 405 {
			t.Errorf("%s without admin: %d", m, resp.StatusCode)
		}
	}
}

func TestModules_単一ノードではこのノードの読み込み状況を返す(t *testing.T) {
	f := newFixture(t)
	f.srv.ModuleAdmin = &fakeModules{specs: []domain.ModuleSpec{{Name: "proxysql", Version: "1.0", Wanted: true}}}
	f.srv.Modules = []string{"proxysql 1.0"}
	f.srv.ModuleProblems = func() map[string]string { return map[string]string{"proxysql": "no licence"} }
	_, body := f.do(t, "GET", "/v1/modules", nil)
	n := body["items"].([]any)[0].(map[string]any)["nodes"].([]any)[0].(map[string]any)
	if n["node"] != "node1" || n["current"] != true || n["problem"] != "no licence" {
		t.Errorf("nodes = %v", n)
	}
	if resp, _ := f.do(t, "POST", "/v1/modules", map[string]any{"name": "x", "extra": 1}); resp.StatusCode != 400 {
		t.Errorf("bad body: %d", resp.StatusCode)
	}
}

func Testモジュールが揃わないノードは確認以外を断る(t *testing.T) {
	f := newFixture(t)
	f.srv.PendingModules = func() []string { return []string{"auth"} }
	cookie, csrf := login(t, f)
	h := f.srv.Handler(false)
	resp, body := send(t, h, "GET", "/v1/branches", nil, withSession(cookie, csrf))
	if resp.StatusCode != 503 || errCode(body) != "modules_pending" {
		t.Errorf("branches: %d %v", resp.StatusCode, body)
	}
	if resp, _ := send(t, h, "GET", "/v1/cluster", nil, withSession(cookie, csrf)); resp.StatusCode != 200 {
		t.Errorf("cluster status stays readable: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "GET", "/v1/branches", nil); resp.StatusCode != 200 {
		t.Errorf("the local socket stays usable: %d", resp.StatusCode)
	}
	_ = http.StatusOK
}

func TestModules_一覧にランタイムモジュールの種類と配布元を出す(t *testing.T) {
	f, c := remoteFixture(t)
	mods := &fakeModules{specs: []domain.ModuleSpec{
		{Name: "auth", Version: "1.0", Wanted: true},
		{Name: "rt-clickhouse", Version: "0.3.0", Wanted: true, Kind: domain.ModuleKindRuntime, Source: domain.SourceLocal},
	}}
	f.srv.ModuleAdmin = mods
	c.status = httpapi.ClusterStatus{Mode: "single", Nodes: []httpapi.NodeStatus{{Name: "n1"}}}
	_, body := f.do(t, "GET", "/v1/modules", nil)
	items := body["items"].([]any)
	plain, rt := items[0].(map[string]any), items[1].(map[string]any)
	if _, has := plain["kind"]; has {
		t.Errorf("a plain module shows a kind: %v", plain)
	}
	if rt["kind"] != "runtime" || rt["source"] != "local" || rt["name"] != "rt-clickhouse" {
		t.Errorf("runtime module = %v", rt)
	}
}

func TestModules_忘却APIのステータスコードと認可と監査(t *testing.T) {
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.Ext.Authenticator = moduleAuth{}
	f.srv.Ext.Authorizer = viewerReadsOnly{}
	f.srv.Ext.AuditSink = sink
	mods := &fakeModules{specs: []domain.ModuleSpec{
		{Name: "proxysql", Version: "1.0", Wanted: true},
		{Name: "audit", Version: "1.0", Wanted: false},
	}}
	f.srv.ModuleAdmin = mods

	// Non-admin (viewer) is denied with 403 and audited as denied.
	h := f.srv.Handler(false)
	if resp, _ := send(t, h, "POST", "/v1/modules/audit/forget", nil, bearer("t-viewer")); resp.StatusCode != 403 {
		t.Fatalf("viewer forget: %d, want 403", resp.StatusCode)
	}

	// Still wanted -> 409 conflict.
	if resp, body := send(t, h, "POST", "/v1/modules/proxysql/forget", nil, bearer("t-admin")); resp.StatusCode != 409 || errCode(body) != "conflict" {
		t.Fatalf("wanted forget: %d %v, want 409 conflict", resp.StatusCode, body)
	}

	// Missing -> 404 not_found.
	if resp, body := send(t, h, "POST", "/v1/modules/nope/forget", nil, bearer("t-admin")); resp.StatusCode != 404 || errCode(body) != "not_found" {
		t.Fatalf("missing forget: %d %v, want 404 not_found", resp.StatusCode, body)
	}

	// Cluster upgrading -> 503 cluster_upgrading.
	mods.forgetErr = domain.ErrClusterUpgrading
	if resp, body := send(t, h, "POST", "/v1/modules/audit/forget", nil, bearer("t-admin")); resp.StatusCode != 503 || errCode(body) != "cluster_upgrading" {
		t.Fatalf("upgrading forget: %d %v, want 503 cluster_upgrading", resp.StatusCode, body)
	}
	mods.forgetErr = nil

	// Works even while other modules are pending (module.* is exempt).
	f.srv.PendingModules = func() []string { return []string{"auth"} }
	if resp, _ := send(t, h, "POST", "/v1/modules/audit/forget", nil, bearer("t-admin")); resp.StatusCode != 204 {
		t.Fatalf("removed forget: %d, want 204", resp.StatusCode)
	}
	if len(mods.specs) != 1 || mods.specs[0].Name != "proxysql" {
		t.Errorf("specs after forget = %+v", mods.specs)
	}

	// Audit events recorded for module.forget (denied, failed, ok).
	actions := sink.actions()
	wantEvents := []string{
		"module.forget:token:viewer:module/audit:denied",
		"module.forget:token:admin:module/proxysql:failed",
		"module.forget:token:admin:module/audit:ok",
	}
	for _, want := range wantEvents {
		found := false
		for _, got := range actions {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing audit event %q in %v", want, actions)
		}
	}

	// Without ModuleAdmin -> 503 not_initialized.
	f.srv.ModuleAdmin = nil
	if resp, body := f.do(t, "POST", "/v1/modules/audit/forget", nil); resp.StatusCode != 503 || errCode(body) != "not_initialized" {
		t.Errorf("without admin: %d %v", resp.StatusCode, body)
	}
}
