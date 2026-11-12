package httpapi_test

import (
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
	c.err = errors.New("no leader")
	if code, _ := scrape(t, f); code != 503 {
		t.Errorf("cluster status failing: %d", code)
	}
}

func TestMetrics_単体ノードでも出す(t *testing.T) {
	f := newFixture(t)
	code, out := scrape(t, f)
	if code != 200 || !strings.Contains(out, `dbbranch_node_up{node="node1"} 1`) || !strings.Contains(out, `dbbranch_node_leader{node="node1"} 1`) {
		t.Errorf("%d %s", code, out)
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
