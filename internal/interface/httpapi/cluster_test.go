package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

type fakeCluster struct {
	status     httpapi.ClusterStatus
	err        error
	forwardErr error
	// forwarded records what reached the other node.
	forwarded []string
	// replies overrides the forwarded answer per node.
	replies   map[string]int
	added     []string
	removed   []string
	replyBody string
	// n2NoCheckpoints makes n2 report no checkpoint support.
	n2NoCheckpoints bool
	// principals are the principal headers that reached the other node.
	principals []string
}

func (f *fakeCluster) AddRuntime(_ context.Context, ref domain.RuntimeRef) error {
	f.added = append(f.added, ref.String())
	return f.err
}

func (f *fakeCluster) RemoveRuntime(_ context.Context, ref domain.RuntimeRef) error {
	f.removed = append(f.removed, ref.String())
	return f.err
}

func (f *fakeCluster) Forward(node string) (http.Handler, error) {
	if f.forwardErr != nil {
		return nil, f.forwardErr
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.principals = append(f.principals, r.Header.Get("X-Db-Branch-Principal"))
		f.forwarded = append(f.forwarded, fmt.Sprintf("%s %s %s via=%s %s", node, r.Method, r.URL.Path, r.Header.Get("X-Db-Branch-Forwarded"), bytes.TrimSpace(body)))
		if code, ok := f.replies[node]; ok {
			msg := "runtime in use"
			if f.replyBody != "" {
				msg = f.replyBody
			}
			http.Error(w, msg, code)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"op_remote"}`))
	}), nil
}

func (f *fakeCluster) BranchUsage(node, branch string) (domain.Usage, bool) {
	if node == "n2" && branch == "there" {
		return domain.Usage{Used: 5, Referenced: 900}, true
	}
	return domain.Usage{}, false
}

func (f *fakeCluster) NodeCheckpoints(node string) bool { return node == "n2" && !f.n2NoCheckpoints }

func (f *fakeCluster) NodeHost(node string) string {
	if node == "n2" {
		return "192.0.2.11"
	}
	return ""
}

func (f *fakeCluster) Status(context.Context) (httpapi.ClusterStatus, error) { return f.status, f.err }
func (f *fakeCluster) NewToken(context.Context) (string, error)              { return "dbb1.token", f.err }

func TestCluster_ノードとリーダーを返す(t *testing.T) {
	f := newFixture(t)
	seen := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	f.srv.Cluster = &fakeCluster{status: httpapi.ClusterStatus{Mode: "cluster", Leader: "n1", Protocol: 3, Nodes: []httpapi.NodeStatus{
		{Name: "n1", Host: "192.0.2.10", Leader: true, Voter: true, LastSeen: seen, PoolSize: 100, PoolFree: 60},
		{Name: "n2", Host: "192.0.2.11", Down: true, LastSeen: seen},
	}}}
	resp, body := f.do(t, "GET", "/v1/cluster", nil)
	if resp.StatusCode != 200 || body["leader"] != "n1" || body["mode"] != "cluster" || body["protocol"] != 3.0 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	nodes := body["nodes"].([]any)
	n2 := nodes[1].(map[string]any)
	if nodes[0].(map[string]any)["voter"] != true || n2["voter"] != false {
		t.Errorf("voter flags = %v", nodes)
	}
	if len(nodes) != 2 || n2["down"] != true || n2["last_seen"] != "2026-11-09T12:00:00Z" {
		t.Errorf("nodes = %v", nodes)
	}
}

func TestCluster_単一ノードならこのノードだけを返す(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "GET", "/v1/cluster", nil)
	nodes := body["nodes"].([]any)
	if body["mode"] != "single" || len(nodes) != 1 || nodes[0].(map[string]any)["name"] != "node1" || nodes[0].(map[string]any)["voter"] != true || body["leader"] != "node1" {
		t.Errorf("body = %v", body)
	}
}

func TestClusterToken_ソケットからだけ発行できる(t *testing.T) {
	f := newFixture(t)
	f.srv.Cluster = &fakeCluster{}
	resp, body := f.do(t, "POST", "/v1/cluster/token", nil)
	if resp.StatusCode != 200 || body["token"] != "dbb1.token" {
		t.Errorf("socket: %d %v", resp.StatusCode, body)
	}
	cookie, csrf := login(t, f)
	resp, body = send(t, f.srv.Handler(false), "POST", "/v1/cluster/token", nil, withSession(cookie, csrf))
	if resp.StatusCode != 403 || errCode(body) != "socket_only" {
		t.Errorf("console: %d %v", resp.StatusCode, body)
	}
}

func TestCluster_単一ノードでのトークン発行と失敗(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do(t, "POST", "/v1/cluster/token", nil)
	if resp.StatusCode != 409 || errCode(body) != "not_clustered" {
		t.Errorf("single: %d %v", resp.StatusCode, body)
	}
	f.srv.Cluster = &fakeCluster{err: errors.New("raft down")}
	if resp, _ := f.do(t, "GET", "/v1/cluster", nil); resp.StatusCode != 500 {
		t.Errorf("status error: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/cluster/token", nil); resp.StatusCode != 500 {
		t.Errorf("token error: %d", resp.StatusCode)
	}
}

var ctx = context.Background()

// remoteFixture is node node1 of a cluster where n2 holds base "far"
// and branch "there".
func remoteFixture(t *testing.T) (*fixture, *fakeCluster) {
	f := newFixture(t)
	c := &fakeCluster{}
	f.srv.Cluster = c
	// The fake never replicates its operations here; do not wait the full time.
	old := httpapi.ForwardWait
	httpapi.ForwardWait = 300 * time.Millisecond
	t.Cleanup(func() { httpapi.ForwardWait = old })
	f.branches.bases["far"] = domain.Base{Name: name("far"), Node: "n2", Runtime: mysql84, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s1"}}}
	f.branches.branches["there"] = domain.Branch{Name: name("there"), Base: name("far"), Node: "n2", Runtime: mysql84, Status: domain.BranchReady, Port: 40000, User: "branch", Password: "pw"}
	return f, c
}

func TestCreateBranch_配置先が他のノードならそのノードへ転送する(t *testing.T) {
	f, c := remoteFixture(t)
	f.branches.place = "n2"
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "far"})
	if resp.StatusCode != 202 || body["id"] != "op_remote" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 POST /v1/branches via=node1 {") || !strings.Contains(c.forwarded[0], `"node":"n2"`) {
		t.Errorf("forwarded = %v", c.forwarded)
	}
	if len(f.branches.calls) != 0 {
		t.Errorf("ran here: %v", f.branches.calls)
	}
}

func TestCreateBranch_置けるノードがなければ503(t *testing.T) {
	f, _ := remoteFixture(t)
	f.branches.placeErr = domain.ErrNoCapacity
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "far"})
	if resp.StatusCode != 503 || errCode(body) != "no_capacity" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func Test他のノードが持つブランチとベースの操作はそのノードへ転送する(t *testing.T) {
	f, c := remoteFixture(t)
	for _, req := range [][2]string{
		{"POST", "/v1/branches/there/reset"},
		{"DELETE", "/v1/branches/there"},
		{"POST", "/v1/bases/far/snapshots"},
		{"POST", "/v1/bases/far/start"},
		{"POST", "/v1/bases/far/stop"},
		{"DELETE", "/v1/bases/far"},
	} {
		c.forwarded = nil
		resp, _ := f.do(t, req[0], req[1], nil)
		if resp.StatusCode != 202 || len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 "+req[0]+" "+req[1]+" via=node1") {
			t.Errorf("%v: %d %v", req, resp.StatusCode, c.forwarded)
		}
	}
	if len(f.branches.calls) != 0 {
		t.Errorf("ran here: %v", f.branches.calls)
	}
}

func TestCreateBase_ノードを指定すればそのノードで作る(t *testing.T) {
	f, c := remoteFixture(t)
	resp, _ := f.do(t, "POST", "/v1/bases", map[string]any{"name": "new", "runtime": "mysql@8.4.13", "dump": "/srv/d.sql", "node": "n2"})
	if resp.StatusCode != 202 || len(c.forwarded) != 1 || !strings.Contains(c.forwarded[0], `"dump":"/srv/d.sql"`) {
		t.Errorf("%d %v", resp.StatusCode, c.forwarded)
	}
}

func Test転送されてきたのに担当が他ならもう転送しない(t *testing.T) {
	f, c := remoteFixture(t)
	resp, body := send(t, f.srv.Handler(true), "POST", "/v1/branches/there/reset", nil, func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n3")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
	})
	if resp.StatusCode != 409 || errCode(body) != "other_node" || len(c.forwarded) != 0 {
		t.Errorf("%d %v %v", resp.StatusCode, body, c.forwarded)
	}
}

func Test転送先が分からなければエラー(t *testing.T) {
	f, c := remoteFixture(t)
	c.forwardErr = errors.New("unknown node n2")
	if resp, _ := f.do(t, "POST", "/v1/branches/there/reset", nil); resp.StatusCode != 500 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/bases/nope/start", nil); resp.StatusCode != 404 {
		t.Errorf("missing base: %d", resp.StatusCode)
	}
}

func Test他のノードのブランチは担当ノードのホストで接続先を示す(t *testing.T) {
	f, _ := remoteFixture(t)
	_, body := f.do(t, "GET", "/v1/branches/there", nil)
	conn := body["connection"].(map[string]any)
	storage, _ := body["storage"].(map[string]any)
	if body["node"] != "n2" || conn["host"] != "192.0.2.11" || storage["referenced"] != 900.0 {
		t.Errorf("view = %v", body)
	}
	_, body = f.do(t, "GET", "/v1/branches/there/dsn", nil)
	if !strings.Contains(body["dsn"].(string), "@192.0.2.11:40000") {
		t.Errorf("dsn = %v", body["dsn"])
	}
	_, body = f.do(t, "GET", "/v1/bases/far", nil)
	if body["node"] != "n2" {
		t.Errorf("base node = %v", body["node"])
	}
}

func Testクラスタでは変更通知を複製された状態から流す(t *testing.T) {
	f, _ := remoteFixture(t)
	sub := f.srv.Bus.Subscribe(0)
	defer sub.Close()
	if resp, _ := f.do(t, "PATCH", "/v1/branches/there", map[string]any{"ttl": "1h"}); resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	f.srv.PublishBranch(ctx, name("there"))
	f.srv.PublishBranch(ctx, name("gone"))
	f.srv.PublishBase(ctx, name("far"))
	var types []string
	for len(types) < 3 {
		select {
		case ev := <-sub.Events():
			types = append(types, ev.Type)
		case <-time.After(time.Second):
			t.Fatalf("events = %v", types)
		}
	}
	if strings.Join(types, ",") != "branch.updated,branch.deleted,base.updated" {
		t.Errorf("events = %v (the PATCH itself must not publish)", types)
	}
}

type refusingOps struct{ operation.MemoryStore }

func (*refusingOps) PutOperation(context.Context, operation.Operation) error {
	return domain.ErrNoLeader
}

func Test操作を記録できなければ始めない(t *testing.T) {
	f := newFixture(t)
	f.srv.Ops = &operation.Manager{Store: &refusingOps{}, Clock: time.Now}
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Status: domain.BranchReady}
	resp, body := f.do(t, "POST", "/v1/branches/x/reset", nil)
	if resp.StatusCode != 503 || errCode(body) != "no_leader" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func Test転送先で始まった操作がこのノードに届くまで待って返す(t *testing.T) {
	f, _ := remoteFixture(t)
	store := &operation.MemoryStore{}
	f.srv.Ops = &operation.Manager{Store: store, Clock: time.Now}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = store.PutOperation(ctx, operation.Operation{ID: "op_remote", State: operation.Running})
	}()
	began := time.Now()
	resp, body := f.do(t, "POST", "/v1/branches/there/reset", nil)
	if resp.StatusCode != 202 || body["id"] != "op_remote" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if time.Since(began) < 50*time.Millisecond {
		t.Error("answered before the operation replicated here")
	}
	if _, err := f.srv.Ops.Get("op_remote"); err != nil {
		t.Error(err)
	}
}

func Test届かなくても待つのは一定時間まで(t *testing.T) {
	f, _ := remoteFixture(t)
	began := time.Now()
	if resp, _ := f.do(t, "POST", "/v1/branches/there/reset", nil); resp.StatusCode != 202 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("waited %v", d)
	}
}

func Test他のノードの使用量が未報告なら空にする(t *testing.T) {
	f, _ := remoteFixture(t)
	f.branches.branches["quiet"] = domain.Branch{Name: name("quiet"), Node: "n2", Runtime: mysql84}
	if _, body := f.do(t, "GET", "/v1/branches/quiet", nil); body["storage"] != nil {
		t.Errorf("storage = %v", body["storage"])
	}
}

func Test他のノードのエンジンの一覧と削除はそのノードへ転送する(t *testing.T) {
	f, c := remoteFixture(t)
	for _, req := range []struct{ method, path, want string }{
		{"GET", "/v1/runtimes?node=n2", "n2 GET /v1/runtimes"},
		{"DELETE", "/v1/runtimes/mysql/8.0.46?node=n2", "n2 DELETE /v1/runtimes/mysql/8.0.46"},
	} {
		c.forwarded = nil
		f.do(t, req.method, req.path, nil)
		if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], req.want) {
			t.Errorf("%s %s: forwarded %v", req.method, req.path, c.forwarded)
		}
	}
	if len(f.runtimes.installed) != 0 {
		t.Errorf("installed here: %v", f.runtimes.installed)
	}
}

func Testエンジンを入れ替えたらクラスタへ知らせる(t *testing.T) {
	f := newFixture(t)
	changed := make(chan struct{}, 2)
	f.srv.OnRuntimesChanged = func() { changed <- struct{}{} }
	f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql@8.0.46"})
	f.runtimes.removeErr = nil
	f.do(t, "DELETE", "/v1/runtimes/mysql/8.0.46", nil)
	for i := 0; i < 2; i++ {
		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatalf("notified %d times", i)
		}
	}
}

func TestCluster_ノードごとのエンジンを返す(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "GET", "/v1/cluster", nil)
	rts := body["nodes"].([]any)[0].(map[string]any)["runtimes"].([]any)
	if len(rts) != 1 || rts[0] != "mysql@8.4.13" {
		t.Errorf("single runtimes = %v", rts)
	}
	f.runtimes.listErr = errors.New("disk")
	if resp, _ := f.do(t, "GET", "/v1/cluster", nil); resp.StatusCode != 500 {
		t.Errorf("list error: %d", resp.StatusCode)
	}
	f.srv.Cluster = &fakeCluster{status: httpapi.ClusterStatus{Mode: "cluster", Nodes: []httpapi.NodeStatus{{Name: "n1"}}}}
	_, body = f.do(t, "GET", "/v1/cluster", nil)
	if rts, ok := body["nodes"].([]any)[0].(map[string]any)["runtimes"].([]any); !ok || len(rts) != 0 {
		t.Errorf("cluster runtimes = %v", body["nodes"])
	}
}

func TestResetPassword_ソケットからだけ新しいパスワードを発行する(t *testing.T) {
	f := newFixture(t)
	reset := false
	f.srv.OnPasswordReset = func() { reset = true }
	cookie, csrf := login(t, f)
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/auth/reset", nil, withSession(cookie, csrf))
	if resp.StatusCode != 403 || errCode(body) != "socket_only" {
		t.Errorf("console: %d %v", resp.StatusCode, body)
	}
	resp, body = f.do(t, "POST", "/v1/auth/reset", nil)
	pw, _ := body["password"].(string)
	if resp.StatusCode != 200 || len(pw) < 12 || !reset {
		t.Fatalf("socket: %d %v reset=%v", resp.StatusCode, body, reset)
	}
	if _, err := f.srv.Auth.Login("admin", pw); err != nil {
		t.Errorf("new password: %v", err)
	}
	if resp, _ := send(t, f.srv.Handler(false), "GET", "/v1/branches", nil, withSession(cookie, csrf)); resp.StatusCode != 401 {
		t.Errorf("old session still works: %d", resp.StatusCode)
	}
}

type brokenCreds struct{}

func (brokenCreds) Load() (auth.Credentials, error) {
	return auth.Credentials{}, errors.New("raft down")
}
func (brokenCreds) Save(auth.Credentials) error { return errors.New("raft down") }

func TestResetPassword_保存できなければエラー(t *testing.T) {
	f := newFixture(t)
	f.srv.Auth = &auth.Service{Store: brokenCreds{}, Hasher: sha{}, Clock: time.Now}
	if resp, _ := f.do(t, "POST", "/v1/auth/reset", nil); resp.StatusCode != 500 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestBase_スナップショットごとのコピーを返す(t *testing.T) {
	f, _ := remoteFixture(t)
	b := f.branches.bases["far"]
	b.Replicas = map[string][]string{"s1": {"node1"}}
	f.branches.bases["far"] = b
	_, body := f.do(t, "GET", "/v1/bases/far", nil)
	r := body["replicas"].(map[string]any)["s1"].([]any)
	if len(r) != 1 || r[0] != "node1" {
		t.Errorf("replicas = %v", body["replicas"])
	}
	_, body = f.do(t, "GET", "/v1/bases/prod", nil)
	if m, ok := body["replicas"].(map[string]any); !ok || len(m) != 0 {
		t.Errorf("no replicas: %v", body["replicas"])
	}
}

func Testクラスタの更新待ちは503で理由を返す(t *testing.T) {
	f := newFixture(t)
	f.branches.fail = fmt.Errorf("record: %w", domain.ErrClusterUpgrading)
	f.branches.placeErr = f.branches.fail
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod"})
	if resp.StatusCode != 503 || errCode(body) != "cluster_upgrading" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

var v10 = domain.RuntimeRef{Engine: domain.MySQL, Version: "1.0"}

func Testクラスタではノード指定なしのインストールを全ノードに広げる(t *testing.T) {
	f, c := remoteFixture(t)
	resp, body := f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql@1.0"})
	if resp.StatusCode != 202 || len(c.added) != 1 || c.added[0] != "mysql@1.0" {
		t.Fatalf("%d %v added %v", resp.StatusCode, body, c.added)
	}
	op := waitOp(t, f, body["id"].(string))
	if op["state"] != "succeeded" || len(f.runtimes.installed) != 1 || f.runtimes.installed[0] != v10 {
		t.Errorf("op %v installed %v", op, f.runtimes.installed)
	}
	f.runtimes.installErr = domain.ErrRuntimeInstalled
	_, body = f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql@1.0"})
	if op := waitOp(t, f, body["id"].(string)); op["state"] != "succeeded" {
		t.Errorf("already here: %v", op)
	}
	if resp, body := f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql@9.9"}); resp.StatusCode != 422 {
		t.Errorf("not offered: %d %v", resp.StatusCode, body)
	}
	c.err = errors.New("raft down")
	if resp, _ := f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql@1.0"}); resp.StatusCode != 500 {
		t.Errorf("record failure: %d", resp.StatusCode)
	}
}

func Testクラスタではノード指定なしの削除を全ノードで行い使っているノードを示す(t *testing.T) {
	f, c := remoteFixture(t)
	c.status = httpapi.ClusterStatus{Mode: "cluster", Nodes: []httpapi.NodeStatus{{Name: "node1"}, {Name: "n2"}, {Name: "n3"}}}
	f.runtimes.removeErr = nil
	c.replies = map[string]int{"n3": http.StatusNotFound} // not installed there: fine
	resp, _ := f.do(t, "DELETE", "/v1/runtimes/mysql/1.0", nil)
	if resp.StatusCode != 204 || len(c.removed) != 1 || len(c.forwarded) != 2 || !strings.Contains(c.forwarded[0], "n2 DELETE /v1/runtimes/mysql/1.0") {
		t.Fatalf("%d removed %v forwarded %v", resp.StatusCode, c.removed, c.forwarded)
	}
	c.replies = map[string]int{"n2": http.StatusConflict}
	c.status.Nodes = append(c.status.Nodes, httpapi.NodeStatus{Name: "n4", Down: true})
	f.runtimes.removeErr = errors.New("disk")
	resp, body := f.do(t, "DELETE", "/v1/runtimes/mysql/1.0", nil)
	msg := body["error"].(map[string]any)["message"].(string)
	if resp.StatusCode != 409 || !strings.Contains(msg, "n2: runtime in use") || !strings.Contains(msg, "n4: down") || !strings.Contains(msg, "node1: disk") {
		t.Errorf("%d %s", resp.StatusCode, msg)
	}
	c.forwardErr = errors.New("unknown node")
	c.status.Nodes = c.status.Nodes[1:2]
	if resp, _ := f.do(t, "DELETE", "/v1/runtimes/mysql/1.0", nil); resp.StatusCode != 409 {
		t.Errorf("forward failure: %d", resp.StatusCode)
	}
	c.err = errors.New("raft down")
	if resp, _ := f.do(t, "DELETE", "/v1/runtimes/mysql/1.0", nil); resp.StatusCode != 500 {
		t.Errorf("status failure: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/runtimes/oracle/1", nil); resp.StatusCode != 400 {
		t.Errorf("bad runtime: %d", resp.StatusCode)
	}
}

func waitOp(t *testing.T, f *fixture, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, op := f.do(t, "GET", "/v1/operations/"+id, nil)
		if op["state"] != "running" {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return nil
}

func Test他のノードの削除失敗はメッセージだけを示す(t *testing.T) {
	f, c := remoteFixture(t)
	c.status = httpapi.ClusterStatus{Mode: "cluster", Nodes: []httpapi.NodeStatus{{Name: "n2"}}}
	c.replies = map[string]int{"n2": http.StatusConflict}
	c.replyBody = `{"error":{"code":"in_use","message":"a branch runs it"}}`
	_, body := f.do(t, "DELETE", "/v1/runtimes/mysql/1.0", nil)
	if msg := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "n2: a branch runs it") || strings.Contains(msg, "{") {
		t.Errorf("message = %s", msg)
	}
}

func TestRecreate_失われたブランチを置ける先で作り直す(t *testing.T) {
	f, c := remoteFixture(t)
	b := f.branches.branches["there"]
	b.Status = domain.BranchLost
	f.branches.branches["there"] = b

	resp, body := f.do(t, "POST", "/v1/branches/there/recreate", nil)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if op := waitOp(t, f, body["id"].(string)); op["state"] != "succeeded" || len(c.forwarded) != 0 {
		t.Errorf("op %v forwarded %v", op, c.forwarded)
	}
	b.Status = domain.BranchLost
	f.branches.branches["there"] = b
	f.branches.place = "n3"
	f.do(t, "POST", "/v1/branches/there/recreate", nil)
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n3 POST /v1/branches/there/recreate") {
		t.Errorf("forwarded %v", c.forwarded)
	}
	f.branches.placeErr = domain.ErrNoCapacity
	if resp, _ := f.do(t, "POST", "/v1/branches/there/recreate", nil); resp.StatusCode != 503 {
		t.Errorf("no place: %d", resp.StatusCode)
	}
	b.Status = domain.BranchReady
	f.branches.branches["there"] = b
	if resp, body := f.do(t, "POST", "/v1/branches/there/recreate", nil); resp.StatusCode != 409 {
		t.Errorf("not lost: %d %v", resp.StatusCode, body)
	}
}

func Test失われたブランチの削除は転送せずこのノードで受け付ける(t *testing.T) {
	f, c := remoteFixture(t)
	b := f.branches.branches["there"]
	b.Status = domain.BranchLost
	f.branches.branches["there"] = b
	resp, _ := f.do(t, "DELETE", "/v1/branches/there", nil)
	if resp.StatusCode != 202 || len(c.forwarded) != 0 {
		t.Errorf("%d forwarded %v", resp.StatusCode, c.forwarded)
	}
}

func Testノードを指定したインストールは受け付けない(t *testing.T) {
	f, _ := remoteFixture(t)
	if resp, _ := f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql@1.0", "node": "n2"}); resp.StatusCode != 400 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestMetrics_ノードの推移と最新値(t *testing.T) {
	f := newFixture(t)
	if _, body := f.do(t, "GET", "/v1/nodes/node1/metrics", nil); len(body["items"].([]any)) != 0 {
		t.Errorf("none yet: %v", body)
	}
	f.srv.Metrics = func() []domain.Metrics { return []domain.Metrics{{CPU: 10}, {CPU: 20, MemUsed: 5}} }
	_, body := f.do(t, "GET", "/v1/nodes/node1/metrics", nil)
	if list := body["items"].([]any); len(list) != 2 || list[1].(map[string]any)["cpu"] != 20.0 {
		t.Errorf("history = %v", body)
	}
	_, body = f.do(t, "GET", "/v1/cluster", nil)
	if m := body["nodes"].([]any)[0].(map[string]any)["metrics"].(map[string]any); m["cpu"] != 20.0 {
		t.Errorf("latest = %v", m)
	}
	if resp, _ := f.do(t, "GET", "/v1/nodes/other/metrics", nil); resp.StatusCode != 404 {
		t.Errorf("unknown node: %d", resp.StatusCode)
	}
	g, c := remoteFixture(t)
	g.do(t, "GET", "/v1/nodes/n2/metrics", nil)
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 GET /v1/nodes/n2/metrics") {
		t.Errorf("forwarded %v", c.forwarded)
	}
}

func TestStartMaskOn_マスク先のノードへ実行者refreshとして転送し監査に残す(t *testing.T) {
	f, c := remoteFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{AuditSink: sink}
	req := dbbranch.MaskRequest{Target: name("dev"), Source: name("prod"), Snapshot: "s2", Masker: "greenmask", By: "refresh"}
	if err := f.srv.StartMaskOn(ctx, "n2", req); err != nil {
		t.Fatal(err)
	}
	want := `n2 POST /v1/bases/dev/mask via=node1 {"masker":"greenmask","snapshot":"s2","source":"prod"}`
	if len(c.forwarded) != 1 || c.forwarded[0] != want {
		t.Errorf("forwarded = %q", c.forwarded)
	}
	if len(c.principals) != 1 || !strings.Contains(c.principals[0], `"name":"refresh"`) {
		t.Errorf("principal = %v", c.principals)
	}
	if len(sink.events) != 1 || sink.events[0].Action != "base.mask" || sink.events[0].Resource.Name != "dev" || sink.events[0].Principal.Name != "refresh" || sink.events[0].Outcome != ext.OutcomeOK {
		t.Errorf("audit = %+v", sink.events)
	}
}

func TestStartMaskOn_転送できない受け付けられないときは理由を返し監査は失敗で残す(t *testing.T) {
	f, c := remoteFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{AuditSink: sink}
	req := dbbranch.MaskRequest{Target: name("dev"), Source: name("prod"), Masker: "greenmask"}
	c.replies = map[string]int{"n2": 409}
	c.replyBody = "base dev is busy"
	if err := f.srv.StartMaskOn(ctx, "n2", req); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("refused: %v", err)
	}
	c.forwardErr = errors.New("node n2 is not a known cluster member")
	if err := f.srv.StartMaskOn(ctx, "n2", req); err == nil || !strings.Contains(err.Error(), "known cluster member") {
		t.Errorf("unknown node: %v", err)
	}
	if len(sink.events) != 2 || sink.events[0].Outcome != ext.OutcomeFailed {
		t.Errorf("audit = %+v", sink.events)
	}
}

func TestStartMaskOn_クラスタでなければ使えない(t *testing.T) {
	f := newFixture(t)
	if err := f.srv.StartMaskOn(ctx, "n2", dbbranch.MaskRequest{Target: name("dev")}); err == nil {
		t.Error("no cluster")
	}
}

func TestBranch_他ノードのブランチはそのノードの報告でチェックポイント可否を返す(t *testing.T) {
	f, c := remoteFixture(t)
	// This node cannot do checkpoints, but n2 (which holds the branch) can.
	f.branches.noCheckpoints = true
	_, body := f.do(t, "GET", "/v1/branches/there", nil)
	if body["checkpoints"] != true {
		t.Errorf("checkpoints = %v, want true from n2", body["checkpoints"])
	}
	c.n2NoCheckpoints = true
	_, body = f.do(t, "GET", "/v1/branches/there", nil)
	if body["checkpoints"] != false {
		t.Errorf("checkpoints = %v, want false from n2", body["checkpoints"])
	}
}

func TestCluster_ノードのOSとカーネルとZFSを返し未報告はunknownにする(t *testing.T) {
	f := newFixture(t)
	f.srv.Cluster = &fakeCluster{status: httpapi.ClusterStatus{Mode: "cluster", Leader: "n1", Nodes: []httpapi.NodeStatus{
		{Name: "n1", System: domain.NodeSystem{Distro: "ubuntu-26.04", OS: "Ubuntu 26.04.1 LTS", Kernel: "7.0.0-14-generic", ZFS: "2.4.0-1ubuntu1"}},
		{Name: "n2"},
	}}}
	_, body := f.do(t, "GET", "/v1/cluster", nil)
	nodes := body["nodes"].([]any)
	n1 := nodes[0].(map[string]any)["system"].(map[string]any)
	n2 := nodes[1].(map[string]any)["system"].(map[string]any)
	if n1["distro"] != "ubuntu-26.04" || n1["os"] != "Ubuntu 26.04.1 LTS" || n1["kernel"] != "7.0.0-14-generic" || n1["zfs"] != "2.4.0-1ubuntu1" {
		t.Errorf("n1 system = %v", n1)
	}
	for _, k := range []string{"distro", "os", "kernel", "zfs"} {
		if n2[k] != "unknown" {
			t.Errorf("n2 %s = %v, want unknown", k, n2[k])
		}
	}
}

func TestCluster_単一ノードでも自分のシステム情報を返す(t *testing.T) {
	f := newFixture(t)
	f.srv.System = func(context.Context) domain.NodeSystem {
		return domain.NodeSystem{Distro: "ubuntu-24.04", Kernel: "6.8.0-48-generic"}
	}
	_, body := f.do(t, "GET", "/v1/cluster", nil)
	sys := body["nodes"].([]any)[0].(map[string]any)["system"].(map[string]any)
	if sys["distro"] != "ubuntu-24.04" || sys["kernel"] != "6.8.0-48-generic" || sys["zfs"] != "unknown" {
		t.Errorf("system = %v", sys)
	}
}

func TestCluster_クラスタのdbbranchのidと食い違うノードを理由つきで返す(t *testing.T) {
	f := newFixture(t)
	f.srv.Cluster = &fakeCluster{status: httpapi.ClusterStatus{Mode: "cluster", Leader: "n1", ServiceIDs: domain.ServiceIDs{UID: 109, GID: 112}, Nodes: []httpapi.NodeStatus{
		{Name: "n1", ServiceIDs: domain.ServiceIDs{UID: 109, GID: 112}},
		{Name: "n2", ServiceIDs: domain.ServiceIDs{UID: 103, GID: 109}, IDProblem: "ids differ"},
	}}}
	_, body := f.do(t, "GET", "/v1/cluster", nil)
	if ids := body["service_ids"].(map[string]any); ids["uid"] != float64(109) || ids["gid"] != float64(112) {
		t.Errorf("cluster ids = %v", ids)
	}
	nodes := body["nodes"].([]any)
	if _, has := nodes[0].(map[string]any)["id_problem"]; has {
		t.Errorf("n1 = %v", nodes[0])
	}
	n2 := nodes[1].(map[string]any)
	if n2["id_problem"] != "ids differ" || n2["service_ids"].(map[string]any)["uid"] != float64(103) {
		t.Errorf("n2 = %v", n2)
	}
}

func TestStorage_ノードの使用量を返し_別のノードへは転送する(t *testing.T) {
	f := newFixture(t)
	if resp, body := f.do(t, "GET", "/v1/nodes/node1/storage", nil); resp.StatusCode != 501 || body["error"].(map[string]any)["code"] != "not_implemented" {
		t.Errorf("a server without storage: %d %v", resp.StatusCode, body)
	}
	f.srv.Storage = func(context.Context) (domain.StorageReport, error) {
		return domain.StorageReport{Pool: "tank", PoolSize: 100, PoolFree: 60, Root: "tank/db-branch",
			Nodes: []domain.SpaceNode{{Name: "tank/db-branch/bases/orders", Kind: "filesystem", Used: 30}}}, nil
	}
	resp, body := f.do(t, "GET", "/v1/nodes/node1/storage", nil)
	if resp.StatusCode != 200 || body["node"] != "node1" || body["pool_free"] != 60.0 || len(body["nodes"].([]any)) != 1 {
		t.Errorf("report = %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "GET", "/v1/nodes/other/storage", nil); resp.StatusCode != 404 {
		t.Errorf("unknown node: %d", resp.StatusCode)
	}
	f.srv.Storage = func(context.Context) (domain.StorageReport, error) {
		return domain.StorageReport{}, errors.New("zfs is gone")
	}
	if resp, _ := f.do(t, "GET", "/v1/nodes/node1/storage", nil); resp.StatusCode != 500 {
		t.Errorf("a failing zfs: %d", resp.StatusCode)
	}
	g, c := remoteFixture(t)
	g.do(t, "GET", "/v1/nodes/n2/storage", nil)
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 GET /v1/nodes/n2/storage") {
		t.Errorf("forwarded %v", c.forwarded)
	}
}

func TestHTTPAPI_偽装Forwardedヘッダでノード転送とライセンスリーダー転送を妨害できない(t *testing.T) {
	// 1. Spoofed X-Db-Branch-Forwarded on the public listener must not block node forwarding
	//    or leak a forged principal, and forwardTo must not mutate the incoming request headers.
	f, c := remoteFixture(t)
	sink := &recordingSink{}
	f.srv.Ext = ext.Module{AuditSink: sink}
	cookie, csrf := login(t, f)
	h := f.srv.Handler(false)

	req := httptest.NewRequest("POST", "/v1/branches/there/reset", nil)
	withSession(cookie, csrf)(req)
	req.Header.Set("X-Db-Branch-Forwarded", "spoof")
	req.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 202 || len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 POST /v1/branches/there/reset via=node1") {
		t.Fatalf("spoofed node forward: %d %s forwarded=%v", rec.Code, rec.Body.String(), c.forwarded)
	}
	if len(c.principals) != 1 || !strings.Contains(c.principals[0], `"id":"admin"`) {
		t.Errorf("forwarded principal = %v, want admin", c.principals)
	}
	if got := sink.actions(); len(got) != 2 || got[1] != "branch.reset:admin:branch/there:ok" {
		t.Errorf("origin audit = %v", got)
	}

	// Normal (non-spoofed) forwarding must also keep the original request headers untouched.
	plainReq := httptest.NewRequest("POST", "/v1/branches/there/reset", nil)
	withSession(cookie, csrf)(plainReq)
	plainRec := httptest.NewRecorder()
	h.ServeHTTP(plainRec, plainReq)
	if plainRec.Code != 202 {
		t.Fatalf("plain forward: %d %s", plainRec.Code, plainRec.Body.String())
	}
	if plainReq.Header.Get("X-Db-Branch-Forwarded") != "" || plainReq.Header.Get("X-Db-Branch-Principal") != "" {
		t.Errorf("forwardTo mutated incoming request headers: forwarded=%q principal=%q",
			plainReq.Header.Get("X-Db-Branch-Forwarded"), plainReq.Header.Get("X-Db-Branch-Principal"))
	}

	// 2. Spoofed X-Db-Branch-Forwarded on the public listener must not block license leader forwarding.
	lf := newFixture(t)
	lf.srv.Licensing = &fakeLicensing{stored: "good"}
	lf.srv.Renewal = &fakeRenewal{leading: false}
	lc := &leaderCluster{replyCode: 200, reply: `{"enabled":true,"last_result":"not_yet"}`}
	lc.status = httpapi.ClusterStatus{Mode: "cluster", Leader: "n2"}
	lf.srv.Cluster = lc
	lCookie, lCSRF := login(t, lf)
	lh := lf.srv.Handler(false)
	spoofSession := func(r *http.Request) {
		withSession(lCookie, lCSRF)(r)
		r.Header.Set("X-Db-Branch-Forwarded", "spoof")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
	}

	resp, body := send(t, lh, "GET", "/v1/license", nil, spoofSession)
	if resp.StatusCode != 200 || body["renewal"].(map[string]any)["last_result"] != "not_yet" {
		t.Fatalf("GET /v1/license: %d %v", resp.StatusCode, body)
	}
	if len(lc.seen) != 1 || lc.seen[0] != "n2 GET /v1/license/renewal via=node1" {
		t.Errorf("license seen = %v", lc.seen)
	}
	lc.seen = nil
	resp, _ = send(t, lh, "GET", "/v1/license/renewal", nil, spoofSession)
	if resp.StatusCode != 200 || len(lc.seen) != 1 || lc.seen[0] != "n2 GET /v1/license/renewal via=node1" {
		t.Errorf("GET /v1/license/renewal: %d seen=%v", resp.StatusCode, lc.seen)
	}
	lc.seen = nil
	resp, _ = send(t, lh, "POST", "/v1/license/renew", nil, spoofSession)
	if resp.StatusCode != 200 || len(lc.seen) != 1 || lc.seen[0] != "n2 POST /v1/license/renew via=node1" {
		t.Errorf("POST /v1/license/renew: %d seen=%v", resp.StatusCode, lc.seen)
	}
}

type proxyCluster struct {
	fakeCluster
	target *url.URL
}

func (p *proxyCluster) Forward(string) (http.Handler, error) {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(p.target)
			r.Out.Header.Del("Cookie")
		},
	}, nil
}

type captureExtHandler struct {
	mu   sync.Mutex
	seen []ext.Principal
}

func (h *captureExtHandler) Serve(_ context.Context, r ext.Request) (ext.Response, error) {
	h.mu.Lock()
	h.seen = append(h.seen, r.Principal)
	h.mu.Unlock()
	return ext.Response{Status: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
}

func (h *captureExtHandler) principals() []ext.Principal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ext.Principal(nil), h.seen...)
}

func (h *captureExtHandler) reset() {
	h.mu.Lock()
	h.seen = nil
	h.mu.Unlock()
}

func TestHTTPAPI_Connectionヘッダ悪用でReverseProxy経由の転送先PrincipalをLocalPrincipalに昇格できない(t *testing.T) {
	// Receiving node (n2) wired behind a real httputil.ReverseProxy -> Handler(true).
	n2 := newFixture(t)
	n2.srv.Node = "n2"
	n2Sink := &recordingSink{}
	n2Ext := &captureExtHandler{}
	n2.srv.Ext = ext.Module{Authorizer: viewerReadsOnly{}, AuditSink: n2Sink}
	n2.srv.ExtHandlers = map[string]ext.Handler{"sso": n2Ext}
	n2.srv.ExtPublic = map[string][]string{"sso": {"/public"}}
	n2.branches.branches["there"] = domain.Branch{Name: name("there"), Base: name("prod"), Node: "n2", Status: domain.BranchReady}
	ts := httptest.NewServer(n2.srv.Handler(true))
	defer ts.Close()
	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Origin node (node1) serving the public listener (Handler(false)).
	n1 := newFixture(t)
	n1.srv.Node = "node1"
	n1.srv.Cluster = &proxyCluster{target: target}
	n1.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}}
	n1.srv.ExtHandlers = map[string]ext.Handler{"sso": &captureExtHandler{}}
	n1.srv.ExtPublic = map[string][]string{"sso": {"/public"}}
	n1.branches.branches["there"] = domain.Branch{Name: name("there"), Base: name("prod"), Node: "n2", Status: domain.BranchReady}
	h := n1.srv.Handler(false)

	// 1. Unauthenticated caller to ExtPublic ?node=n2 with Connection smuggling
	//    (single, combined, multi-line, lowercase) must arrive at n2 as anonymous, never LocalPrincipal.
	connVariants := [][]string{
		{"X-Db-Branch-Principal"},
		{"X-Db-Branch-Forwarded"},
		{"X-Db-Branch-Forwarded, X-Db-Branch-Principal"},
		{"keep-alive", "x-db-branch-principal", "x-db-branch-forwarded"},
	}
	for _, conns := range connVariants {
		n2Ext.reset()
		resp, _ := send(t, h, "GET", "/v1/ext/sso/public/info?node=n2", nil, func(r *http.Request) {
			for _, c := range conns {
				r.Header.Add("Connection", c)
			}
		})
		if got := n2Ext.principals(); resp.StatusCode != 200 || len(got) != 1 || got[0].ID != httpapi.Anonymous.ID {
			t.Fatalf("ExtPublic Connection=%q: status=%d seen=%+v, want anonymous", conns, resp.StatusCode, got)
		}
	}

	// 2. Viewer calling a read-only ext route forwarded to n2 with Connection smuggling
	//    must arrive at n2 as token:viewer (not local) and must not duplicate audit.
	n2Ext.reset()
	resp, _ := send(t, h, "GET", "/v1/ext/sso/private/info?node=n2", nil, func(r *http.Request) {
		bearer("t-viewer")(r)
		r.Header.Add("Connection", "keep-alive")
		r.Header.Add("Connection", "x-db-branch-forwarded, x-db-branch-principal")
	})
	if got := n2Ext.principals(); resp.StatusCode != 200 || len(got) != 1 || got[0].ID != "token:viewer" {
		t.Fatalf("viewer ext forward: status=%d seen=%+v, want token:viewer", resp.StatusCode, got)
	}
	if got := n2Sink.actions(); len(got) != 0 {
		t.Errorf("receiving node duplicated audit: %v", got)
	}
}
