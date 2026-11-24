package httpapi_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
)

func scrape(t *testing.T, f *fixture) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/metrics", nil)
	rec := httptest.NewRecorder()
	f.srv.Handler(true).ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestMetrics_Prometheus形式でクラスタの状態を出す(t *testing.T) {
	f, c := remoteFixture(t)
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "node1", Nodes: []httpapi.NodeStatus{
		{Name: "node1", Version: "1.2.0", Leader: true, PoolSize: 100, PoolFree: 40},
		{Name: "n2", Version: "1.2.0", PoolSize: 50, PoolFree: 10, ModuleProblems: map[string]string{"auth": "no licence"}},
		{Name: "n3", Down: true},
	}}
	b := f.branches.bases["far"]
	b.Snapshots = []domain.BaseSnapshot{{Name: "s1"}, {Name: "s2"}}
	b.Replicas = map[string][]string{"s2": {"node1"}}
	f.branches.bases["far"] = b
	_, _ = f.do(t, "GET", "/v1/branches", nil) // counted
	code, out := scrape(t, f)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	for _, want := range []string{
		"# TYPE dbbranch_node_up gauge",
		`dbbranch_node_up{node="node1"} 1`,
		`dbbranch_node_up{node="n3"} 0`,
		`dbbranch_node_leader{node="node1"} 1`,
		`dbbranch_pool_free_bytes{node="n2"} 10`,
		`dbbranch_pool_size_bytes{node="node1"} 100`,
		`dbbranch_branches{node="n2",status="ready"} 1`,
		`dbbranch_bases{status="ready"} 2`,
		`dbbranch_base_snapshots{base="far"} 2`,
		// far's latest snapshot is on n2 (origin) and node1; n3 is down, so none missing among up nodes.
		`dbbranch_base_replicas_missing{base="far"} 0`,
		`dbbranch_module_problem{module="auth",node="n2"} 1`,
		`dbbranch_build_info{node="node1",version="test"} 1`,
		"# TYPE dbbranch_api_requests_total counter",
		`dbbranch_api_requests_total{action="branch.read",outcome="ok"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	c.err = domain.ErrNoLeader
	if code, out := scrape(t, f); code != 503 || !strings.Contains(out, `"no_leader"`) {
		t.Errorf("no leader: %d %s", code, out)
	}
	c.err = errors.New("open /var/lib/db-branch/raft.db: permission denied")
	if code, out := scrape(t, f); code != 500 || !strings.Contains(out, `"internal"`) || strings.Contains(out, "/var/lib/db-branch") {
		t.Errorf("internal error: %d %s", code, out)
	}
}

func TestMetrics_単体ノードでも出す(t *testing.T) {
	f := newFixture(t)
	code, out := scrape(t, f)
	if code != 200 || !strings.Contains(out, `dbbranch_node_up{node="node1"} 1`) || !strings.Contains(out, `dbbranch_node_leader{node="node1"} 1`) {
		t.Errorf("%d %s", code, out)
	}
}

func TestMetrics_単一ノードでpool容量とmoduleProblemメトリクスが出力されmodules_pending中でも200を返す(t *testing.T) {
	f := newFixture(t)
	f.srv.PoolStats = func(context.Context) (uint64, uint64) { return 200, 80 }
	f.srv.ModuleProblems = func() map[string]string { return map[string]string{"auth": "licence expired"} }
	f.srv.PendingModules = func() []string { return []string{"auth"} }

	// Unauthenticated /v1/metrics is still rejected with 401.
	unauthRec := httptest.NewRecorder()
	f.srv.Handler(false).ServeHTTP(unauthRec, httptest.NewRequest("GET", "/v1/metrics", nil))
	if unauthRec.Code != 401 {
		t.Fatalf("unauthenticated metrics: %d, want 401", unauthRec.Code)
	}

	cookie, csrf := login(t, f)
	req := httptest.NewRequest("GET", "/v1/metrics", nil)
	withSession(cookie, csrf)(req)
	rec := httptest.NewRecorder()
	f.srv.Handler(false).ServeHTTP(rec, req)
	out := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("metrics while modules_pending: %d %s", rec.Code, out)
	}
	for _, want := range []string{
		`dbbranch_pool_size_bytes{node="node1"} 200`,
		`dbbranch_pool_free_bytes{node="node1"} 80`,
		`dbbranch_module_problem{module="auth",node="node1"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Cluster mode: /v1/metrics also returns 200 while modules_pending, and GET /v1/cluster does not invoke PoolStats.
	fc, c := remoteFixture(t)
	poolCalled := false
	fc.srv.PoolStats = func(context.Context) (uint64, uint64) {
		poolCalled = true
		return 1, 1
	}
	fc.srv.PendingModules = func() []string { return []string{"auth"} }
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "node1", Nodes: []httpapi.NodeStatus{{Name: "node1", Leader: true}}}
	cc, ccsrf := login(t, fc)
	creq := httptest.NewRequest("GET", "/v1/metrics", nil)
	withSession(cc, ccsrf)(creq)
	crec := httptest.NewRecorder()
	fc.srv.Handler(false).ServeHTTP(crec, creq)
	if crec.Code != 200 {
		t.Fatalf("cluster metrics while modules_pending: %d %s", crec.Code, crec.Body.String())
	}
	clReq := httptest.NewRequest("GET", "/v1/cluster", nil)
	withSession(cc, ccsrf)(clReq)
	clRec := httptest.NewRecorder()
	fc.srv.Handler(false).ServeHTTP(clRec, clReq)
	if clRec.Code != 200 {
		t.Fatalf("cluster status: %d %s", clRec.Code, clRec.Body.String())
	}
	if poolCalled {
		t.Error("PoolStats was called in cluster mode")
	}
}

func TestHealth_生存とリーダーの有無を返す(t *testing.T) {
	f, c := remoteFixture(t)
	h := f.srv.Handler(false) // remote: no login needed for these
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code
	}
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "n2"}
	if get("/healthz") != 200 || get("/readyz") != 200 {
		t.Errorf("healthy: %d %d", get("/healthz"), get("/readyz"))
	}
	c.status.Leader = ""
	if get("/readyz") != 503 || get("/healthz") != 200 {
		t.Errorf("no leader: ready %d", get("/readyz"))
	}
	c.err = errors.New("raft down")
	if get("/readyz") != 503 {
		t.Errorf("status error: %d", get("/readyz"))
	}
	f.srv.Branches = func() (httpapi.Branches, error) { return nil, errors.New("not initialized") }
	c.err = nil
	c.status.Leader = "n2"
	if get("/readyz") != 503 {
		t.Errorf("not initialized: %d", get("/readyz"))
	}
}

func TestReadyz_modules_pending時に503汎用メッセージを返し内部エラー詳細を露出しない(t *testing.T) {
	f, c := remoteFixture(t)
	h := f.srv.Handler(false)
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}
	c.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "n2"}

	// Pending modules -> 503 with generic message (no module names leaked); /healthz stays 200.
	f.srv.PendingModules = func() []string { return []string{"auth", "audit"} }
	if code, body := get("/readyz"); code != 503 || strings.TrimSpace(body) != "not ready" || strings.Contains(body, "auth") || strings.Contains(body, "audit") {
		t.Errorf("modules_pending readyz: %d %q", code, body)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Errorf("modules_pending healthz: %d, want 200", code)
	}
	f.srv.PendingModules = func() []string { return nil }
	if code, body := get("/readyz"); code != 200 || strings.TrimSpace(body) != "ready" {
		t.Errorf("ready after modules loaded: %d %q", code, body)
	}

	// Internal error from Branches() -> 503 with generic message (no path or internal detail leaked).
	f.srv.Branches = func() (httpapi.Branches, error) {
		return nil, errors.New("open /var/lib/db-branch/state.json: permission denied")
	}
	if code, body := get("/readyz"); code != 503 || strings.TrimSpace(body) != "not ready" || strings.Contains(body, "/var/lib/db-branch") || strings.Contains(body, "permission denied") {
		t.Errorf("branches error readyz: %d %q", code, body)
	}

	// Single-node /readyz must never call PoolStats (no unauthenticated zfs fork DoS),
	// returns 503 on modules_pending or empty Node, and 200 when ready.
	sf := newFixture(t)
	poolCalled := false
	sf.srv.PoolStats = func(context.Context) (uint64, uint64) {
		poolCalled = true
		return 100, 50
	}
	sh := sf.srv.Handler(false)
	sget := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		sh.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := sget("/readyz"); code != 200 || strings.TrimSpace(body) != "ready" {
		t.Errorf("single-node healthy readyz: %d %q", code, body)
	}
	if poolCalled {
		t.Error("single-node /readyz invoked PoolStats")
	}
	sf.srv.PendingModules = func() []string { return []string{"auth"} }
	if code, body := sget("/readyz"); code != 503 || strings.TrimSpace(body) != "not ready" || strings.Contains(body, "auth") {
		t.Errorf("single-node modules_pending readyz: %d %q", code, body)
	}
	sf.srv.PendingModules = nil
	sf.srv.Node = ""
	if code, body := sget("/readyz"); code != 503 || strings.TrimSpace(body) != "not ready" {
		t.Errorf("single-node empty node readyz: %d %q", code, body)
	}
}
