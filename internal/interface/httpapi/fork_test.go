package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/pkg/ext"
)

func TestForkBranch_親ブランチをフォークでき親参照フィールドが返る(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["parent"] = domain.Branch{
		Name: name("parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40001, Node: "node1", User: "parent", Password: "pw",
		Owner: "admin", OwnerName: "admin", CreatedAt: t0,
	}
	resp, body := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{
		"name": "child", "checkpoint": "cp1", "clean": true, "init_sql": "SELECT 1;", "ttl": "24h", "access": "open",
	})
	if resp.StatusCode != 202 || body["kind"] != "branch.fork" {
		t.Fatalf("fork: %d %v", resp.StatusCode, body)
	}
	if target := body["target"].(map[string]any); target["name"] != "child" || target["resource"] != "branch" {
		t.Fatalf("target = %v", target)
	}
	op := f.waitOp(t, body["id"].(string))
	if op.State != "succeeded" {
		t.Fatalf("op = %+v", op)
	}
	if got := f.branches.lastFork; got.Source != name("parent") || got.Name != name("child") || got.Checkpoint != "cp1" || !got.Clean || got.InitSQL != "SELECT 1;" || got.TTL != 24*time.Hour || got.Access != "open" {
		t.Errorf("lastFork = %+v", got)
	}
	resp, gotBranch := f.do(t, "GET", "/v1/branches/child", nil)
	if resp.StatusCode != 200 || gotBranch["parent_branch"] != "parent" || gotBranch["parent_snapshot"] != "ckpt-cp1" {
		t.Errorf("GET child = %d %v", resp.StatusCode, gotBranch)
	}
	// Non-forked parent omits parent_branch / parent_snapshot.
	_, gotParent := f.do(t, "GET", "/v1/branches/parent", nil)
	if _, has := gotParent["parent_branch"]; has {
		t.Errorf("non-forked branch should omit parent_branch: %v", gotParent)
	}
}

func TestForkBranch_認可は所有者や共有メンバーや管理者のみ許可し他人のopenや匿名は拒否する(t *testing.T) {
	f := newFixture(t)
	f.srv.TokenAuth = &multiTokens{principals: map[string]ext.Principal{
		"t-alice": {ID: "sso:alice", Name: "alice", Roles: []string{"scope:branches"}},
		"t-bob":   {ID: "sso:bob", Name: "bob", Roles: []string{"scope:branches"}},
		"t-full":  {ID: "token:full", Name: "admin", Roles: []string{"scope:full"}},
	}}
	f.branches.branches["alices-open"] = domain.Branch{
		Name: name("alices-open"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40001, Node: "node1", Owner: "sso:alice", OwnerName: "alice",
		Access: domain.AccessOpen, CreatedAt: t0,
	}
	h := f.srv.Handler(false)
	// Anonymous (no auth) gets 401.
	if resp, _ := send(t, h, "POST", "/v1/branches/alices-open/fork", map[string]any{"name": "anon-c"}, nil); resp.StatusCode != 401 {
		t.Errorf("anonymous fork: %d, want 401", resp.StatusCode)
	}
	// Bob cannot fork Alice's open branch -> 403.
	if resp, _ := send(t, h, "POST", "/v1/branches/alices-open/fork", map[string]any{"name": "bob-c"}, as("t-bob")); resp.StatusCode != 403 {
		t.Errorf("bob fork alice's open branch: %d, want 403", resp.StatusCode)
	}
	// Alice cannot spoof another owner -> 403.
	if resp, _ := send(t, h, "POST", "/v1/branches/alices-open/fork", map[string]any{"name": "alice-c", "owner": "sso:bob"}, as("t-alice")); resp.StatusCode != 403 {
		t.Errorf("alice spoof owner: %d, want 403", resp.StatusCode)
	}
	// Alice can fork her own branch -> 202.
	if resp, body := send(t, h, "POST", "/v1/branches/alices-open/fork", map[string]any{"name": "alice-c"}, as("t-alice")); resp.StatusCode != 202 {
		t.Fatalf("alice own fork: %d %v", resp.StatusCode, body)
	} else {
		f.waitOp(t, body["id"].(string))
	}
	if f.branches.lastFork.Owner != "sso:alice" || f.branches.lastFork.OwnerName != "alice" {
		t.Errorf("owner = %q / %q", f.branches.lastFork.Owner, f.branches.lastFork.OwnerName)
	}
	// Admin can fork any branch for another owner -> 202.
	if resp, body := send(t, h, "POST", "/v1/branches/alices-open/fork", map[string]any{"name": "admin-c", "owner": "sso:bob", "owner_name": "bob"}, as("t-full")); resp.StatusCode != 202 {
		t.Fatalf("admin fork: %d %v", resp.StatusCode, body)
	} else {
		f.waitOp(t, body["id"].(string))
	}
	if f.branches.lastFork.Owner != "sso:bob" || f.branches.lastFork.OwnerName != "bob" {
		t.Errorf("admin-chosen owner = %q / %q", f.branches.lastFork.Owner, f.branches.lastFork.OwnerName)
	}
	// Ownerless legacy branch: non-admin refused with 403.
	f.branches.branches["legacy"] = domain.Branch{
		Name: name("legacy"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40003, Node: "node1", CreatedAt: t0,
	}
	if resp, _ := send(t, h, "POST", "/v1/branches/legacy/fork", map[string]any{"name": "bob-leg"}, as("t-bob")); resp.StatusCode != 403 {
		t.Errorf("non-admin fork ownerless branch: %d, want 403", resp.StatusCode)
	}
	// Shared team member (SharesBranches) is allowed to fork Alice's branch -> 202.
	f.srv.Ext.BranchPolicy = &forkTestPolicy{share: map[string]string{"sso:bob": "sso:alice"}}
	if resp, body := send(t, h, "POST", "/v1/branches/alices-open/fork", map[string]any{"name": "bob-shared"}, as("t-bob")); resp.StatusCode != 202 {
		t.Fatalf("shared team member fork: %d %v", resp.StatusCode, body)
	} else {
		f.waitOp(t, body["id"].(string))
	}
	// BranchPolicy denial refuses fork -> 403.
	f.srv.Ext.BranchPolicy = &forkTestPolicy{err: ext.ErrForbidden}
	if resp, _ := send(t, h, "POST", "/v1/branches/alices-open/fork", map[string]any{"name": "alice-pol"}, as("t-alice")); resp.StatusCode != 403 {
		t.Errorf("branch policy denial: %d, want 403", resp.StatusCode)
	}
}

func TestForkBranch_混在クラスタでは転送前にcluster_upgradingを返しRecord検証はチェックポイントのランタイムを使う(t *testing.T) {
	t.Run("混在クラスタ(Protocol 24)では転送せず503 cluster_upgradingを返す", func(t *testing.T) {
		f := newFixture(t)
		f.branches.branches["remote-parent"] = domain.Branch{
			Name: name("remote-parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
			Status: domain.BranchReady, Port: 40001, Node: "node2", Owner: "admin", OwnerName: "admin", CreatedAt: t0,
		}
		forwarded := false
		f.srv.Cluster = &forkForwardCluster{
			fakeCluster: &fakeCluster{status: httpapi.ClusterStatus{Protocol: domain.ProtocolBranchFork - 1}},
			onForward: func(string) http.Handler {
				forwarded = true
				return http.NotFoundHandler()
			},
		}
		resp, body := f.do(t, "POST", "/v1/branches/remote-parent/fork", map[string]any{"name": "child"})
		if resp.StatusCode != 503 || errCode(body) != "cluster_upgrading" || forwarded {
			t.Fatalf("mixed cluster: status=%d body=%v forwarded=%v", resp.StatusCode, body, forwarded)
		}
	})

	t.Run("チェックポイント起点のRecord検証はチェックポイント時のランタイムを渡す", func(t *testing.T) {
		f := newFixture(t)
		mysql80 := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}
		mysql94 := domain.RuntimeRef{Engine: domain.MySQL, Version: "9.4.0"}
		f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql80, Status: domain.BaseReady}
		f.branches.branches["parent"] = domain.Branch{
			Name: name("parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql94,
			Status: domain.BranchReady, Port: 40001, Node: "node1", Owner: "admin", OwnerName: "admin", CreatedAt: t0,
		}
		// Checkpoint runtime (8.4.13) differs from both base (8.0.46) and parent (9.4.0).
		f.branches.cps = []domain.Checkpoint{
			{Name: "mid-84", Runtime: mysql84.String()},
		}
		opt := &captureRecordOptioner{}
		f.srv.Ext.ServerOptioner = opt
		resp, body := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{
			"name": "c-cp", "checkpoint": "mid-84", "record": true,
		})
		if resp.StatusCode != 202 {
			t.Fatalf("checkpoint record fork: %d %v", resp.StatusCode, body)
		}
		f.waitOp(t, body["id"].(string))
		if opt.last.Version != "8.4.13" {
			t.Errorf("ServerOptions received version %q, want 8.4.13", opt.last.Version)
		}
		// @reset uses base runtime (8.0.46) rather than upgraded parent runtime (9.4.0).
		resp, body = f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{
			"name": "c-reset", "checkpoint": domain.ResetPoint, "record": true,
		})
		if resp.StatusCode != 202 {
			t.Fatalf("@reset record fork: %d %v", resp.StatusCode, body)
		}
		f.waitOp(t, body["id"].(string))
		if opt.last.Version != "8.0.46" {
			t.Errorf("@reset ServerOptions version = %q, want 8.0.46", opt.last.Version)
		}
	})

	t.Run("リモート親の名前付きチェックポイントに対するRecord検証は所有者ノードへ転送する", func(t *testing.T) {
		f := newFixture(t)
		f.branches.branches["remote-parent"] = domain.Branch{
			Name: name("remote-parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
			Status: domain.BranchReady, Port: 40001, Node: "node2", Owner: "admin", OwnerName: "admin", CreatedAt: t0,
		}
		forwarded := false
		f.srv.Cluster = &forkForwardCluster{
			fakeCluster: &fakeCluster{status: httpapi.ClusterStatus{Protocol: domain.ProtocolBranchFork}},
			onForward: func(string) http.Handler {
				forwarded = true
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"id":"op_fwd","kind":"branch.fork"}`))
				})
			},
		}
		// Without ServerOptioner -> 400 before forwarding.
		if resp, _ := f.do(t, "POST", "/v1/branches/remote-parent/fork", map[string]any{"name": "c1", "checkpoint": "cp1", "record": true}); resp.StatusCode != 400 || forwarded {
			t.Fatalf("missing history module: status=%d forwarded=%v", resp.StatusCode, forwarded)
		}
		// With ServerOptioner -> forwards without invoking local version-specific check.
		opt := &captureRecordOptioner{}
		f.srv.Ext.ServerOptioner = opt
		if resp, _ := f.do(t, "POST", "/v1/branches/remote-parent/fork", map[string]any{"name": "c1", "checkpoint": "cp1", "record": true}); resp.StatusCode != 202 || !forwarded {
			t.Fatalf("remote checkpoint record fork: status=%d forwarded=%v", resp.StatusCode, forwarded)
		}
		if opt.last.Version != "" {
			t.Errorf("local node should defer version check to owner node, got %+v", opt.last)
		}
	})

	t.Run("単一ノードのフォーク失敗時SSEは子の実在有無に応じて正しく配信する", func(t *testing.T) {
		f := newFixture(t)
		f.branches.branches["parent"] = domain.Branch{
			Name: name("parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
			Status: domain.BranchReady, Port: 40001, Node: "node1", Owner: "admin", OwnerName: "admin", CreatedAt: t0,
		}
		sub := f.srv.Bus.Subscribe(0)
		defer sub.Close()
		drain := func() string {
			var seen []string
			for {
				select {
				case ev := <-sub.Events():
					if strings.HasPrefix(ev.Type, "branch.") {
						b, _ := json.Marshal(ev.Data)
						seen = append(seen, ev.Type+":"+string(b))
					}
				default:
					return strings.Join(seen, "\n")
				}
			}
		}

		// Case 1: Fork fails after leaving failed child in store (returns empty Branch).
		f.branches.forkFailKeep = true
		resp, body := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{"name": "c-failed"})
		if resp.StatusCode != 202 {
			t.Fatalf("fork: %d %v", resp.StatusCode, body)
		}
		f.waitOp(t, body["id"].(string))
		joined := drain()
		if !strings.Contains(joined, `"name":"c-failed"`) || !strings.Contains(joined, `"name":"parent"`) {
			t.Errorf("expected branch.updated for both c-failed and parent, got:\n%s", joined)
		}

		// Case 2: Fork fails before keeping child (reservation cancelled) -> no branch.deleted for child.
		f.branches.forkFailKeep = false
		f.branches.forkFailCancel = true
		resp, body = f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{"name": "c-cancelled"})
		if resp.StatusCode != 202 {
			t.Fatalf("fork cancel: %d %v", resp.StatusCode, body)
		}
		f.waitOp(t, body["id"].(string))
		joined = drain()
		if strings.Contains(joined, "c-cancelled") {
			t.Errorf("cancelled child must not emit branch.deleted/updated, got:\n%s", joined)
		}
		if !strings.Contains(joined, `"name":"parent"`) {
			t.Errorf("expected parent update, got:\n%s", joined)
		}
	})
}

func TestForkBranch_他ノードの親へは所有者とプレフィックスを確定して転送する(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["remote-parent"] = domain.Branch{
		Name: name("remote-parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40001, Node: "node2", Owner: "sso:alice", OwnerName: "alice", CreatedAt: t0,
	}
	var gotBody map[string]any
	var gotNode string
	f.srv.Cluster = &forkForwardCluster{
		fakeCluster: &fakeCluster{},
		onForward: func(node string) http.Handler {
			gotNode = node
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"op_fwd","kind":"branch.fork"}`))
			})
		},
	}
	f.srv.Ext.Authenticator = &aliceAuth{}
	h := f.srv.Handler(false)
	// Sign in as non-admin alice via console session so userBranchName adds "alice-" prefix.
	resp, loginBody := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "pw"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %v", resp.StatusCode, loginBody)
	}
	csrf := loginBody["csrf_token"].(string)
	cookies := resp.Cookies()
	resp, _ = send(t, h, "POST", "/v1/branches/remote-parent/fork", map[string]any{"name": "try"}, func(r *http.Request) {
		for _, c := range cookies {
			r.AddCookie(c)
		}
		r.Header.Set("X-CSRF-Token", csrf)
	})
	if resp.StatusCode != 202 || gotNode != "node2" {
		t.Fatalf("forwarded: status=%d node=%q body=%v", resp.StatusCode, gotNode, gotBody)
	}
	if gotBody["name"] != "alice-try" || gotBody["owner"] != "sso:alice" || gotBody["owner_name"] != "alice" {
		t.Errorf("forwarded body = %v, want prefixed name and resolved owner", gotBody)
	}
}

func TestForkBranch_各種バリデーションエラーと親削除保護を同期的に返す(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["parent"] = domain.Branch{
		Name: name("parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40001, Node: "node1", CreatedAt: t0,
	}
	f.branches.branches["busy"] = domain.Branch{
		Name: name("busy"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchResetting, Node: "node1", CreatedAt: t0,
	}
	// Missing parent -> 404.
	if resp, _ := f.do(t, "POST", "/v1/branches/nope/fork", map[string]any{"name": "c"}); resp.StatusCode != 404 {
		t.Errorf("missing parent: %d", resp.StatusCode)
	}
	// Busy parent -> 409 conflict.
	if resp, _ := f.do(t, "POST", "/v1/branches/busy/fork", map[string]any{"name": "c"}); resp.StatusCode != 409 {
		t.Errorf("busy parent: %d", resp.StatusCode)
	}
	// Invalid body / bad name / bad checkpoint / bad ttl / bad access -> 400.
	for label, payload := range map[string]any{
		"unknown field": map[string]any{"name": "c", "unknown": 1},
		"bad name":      map[string]any{"name": "BAD"},
		"bad cp":        map[string]any{"name": "c", "checkpoint": "-bad-"},
		"bad ttl":       map[string]any{"name": "c", "ttl": "nope"},
		"bad access":    map[string]any{"name": "c", "access": "public"},
	} {
		if resp, _ := f.do(t, "POST", "/v1/branches/parent/fork", payload); resp.StatusCode != 400 {
			t.Errorf("%s: status = %d, want 400", label, resp.StatusCode)
		}
	}
	// Different node -> 409 conflict.
	if resp, body := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{"name": "c", "node": "node2"}); resp.StatusCode != 409 || errCode(body) != "conflict" {
		t.Errorf("wrong node: %d %v", resp.StatusCode, body)
	}
	// Existing child name -> 409 already_exists.
	if resp, body := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{"name": "parent"}); resp.StatusCode != 409 || errCode(body) != "already_exists" {
		t.Errorf("duplicate name: %d %v", resp.StatusCode, body)
	}
	// Checkpoints unavailable -> 409 checkpoints_unavailable.
	f.branches.noCheckpoints = true
	if resp, body := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{"name": "c"}); resp.StatusCode != 409 || errCode(body) != "checkpoints_unavailable" {
		t.Errorf("no checkpoints: %d %v", resp.StatusCode, body)
	}
	f.branches.noCheckpoints = false
	// Cluster upgrading -> 503 cluster_upgrading.
	f.branches.forkErr = domain.ErrClusterUpgrading
	if resp, body := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{"name": "c"}); resp.StatusCode != 503 || errCode(body) != "cluster_upgrading" {
		t.Errorf("cluster upgrading: %d %v", resp.StatusCode, body)
	}
	f.branches.forkErr = nil
	// Record without history module -> 400.
	if resp, _ := f.do(t, "POST", "/v1/branches/parent/fork", map[string]any{"name": "c", "record": true}); resp.StatusCode != 400 {
		t.Errorf("record without module: %d", resp.StatusCode)
	}
	// Synchronous delete protection on parent when a forked child exists -> 409 in_use.
	f.branches.branches["child"] = domain.Branch{
		Name: name("child"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Node: "node1", ParentBranch: name("parent"), CreatedAt: t0,
	}
	if resp, body := f.do(t, "DELETE", "/v1/branches/parent", nil); resp.StatusCode != 409 || errCode(body) != "in_use" {
		t.Errorf("delete parent with child: %d %v", resp.StatusCode, body)
	}
}

func TestMCP_fork_branchで既存ブランチから子ブランチを作りDSNを得る(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := newFixture(t)
	f.branches.branches["parent"] = domain.Branch{
		Name: name("parent"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40001, Node: "node1", User: "parent", Password: "pw",
		Owner: "token:dev", OwnerName: "dev", CreatedAt: t0,
	}
	sink := &recordingSink{}
	f.srv.TokenAuth, f.srv.Ext = &fakeTokens{}, ext.Module{AuditSink: sink}
	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callText(t, s, "fork_branch", map[string]any{
		"source": "parent", "name": "child", "checkpoint": "cp1", "clean": true, "ttl": "4h", "init_sql": "SELECT 1;",
	})
	if isErr || !strings.Contains(out, `"parent_branch": "parent"`) || !strings.Contains(out, `"dsn": "mysql://child:pw@192.0.2.10:40002/"`) {
		t.Fatalf("fork_branch: isErr=%v out=%s", isErr, out)
	}
	if got := strings.Join(sink.actions(), " "); !strings.Contains(got, "branch.fork:token:dev:branch/parent:ok") {
		t.Errorf("audit lacks branch.fork: %s", got)
	}
	if out, isErr := callText(t, s, "fork_branch", map[string]any{"source": "nope", "name": "c2"}); !isErr || !strings.Contains(out, "not found") {
		t.Errorf("fork missing: %v %s", isErr, out)
	}
}

type multiTokens struct {
	principals map[string]ext.Principal
}

func (m *multiTokens) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	if p, ok := m.principals[c.Token]; ok {
		return p, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

type forkForwardCluster struct {
	*fakeCluster
	onForward func(node string) http.Handler
}

func (c *forkForwardCluster) Forward(node string) (http.Handler, error) {
	return c.onForward(node), nil
}

type aliceAuth struct{}

func (*aliceAuth) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	if c.Username == "alice" {
		return ext.Principal{ID: "sso:alice", Name: "alice", Roles: []string{"user"}}, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

// Keep httptest import used if needed.
var _ = httptest.NewRecorder

type forkTestPolicy struct {
	share map[string]string
	err   error
}

func (p *forkTestPolicy) CheckBranch(context.Context, ext.Principal, ext.BranchRequest) error {
	return p.err
}

func (p *forkTestPolicy) SharesBranches(_ context.Context, principal ext.Principal, owner string) bool {
	return p.share[principal.ID] == owner
}

type captureRecordOptioner struct {
	last ext.ServerBranch
}

func (c *captureRecordOptioner) ServerOptions(_ context.Context, b ext.ServerBranch) (ext.ServerOptions, error) {
	c.last = b
	return ext.ServerOptions{}, nil
}
