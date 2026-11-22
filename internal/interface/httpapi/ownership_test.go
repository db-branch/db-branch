package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/pkg/ext"
)

// tokenAuth: "t-alice" and "t-bob" are people with branches-scoped tokens,
// "t-full" a full-scope one.
type tokenAuth struct{}

func (tokenAuth) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	switch {
	case c.Token == "t-alice":
		return ext.Principal{ID: "token:a", Name: "alice", Roles: []string{"scope:branches"}}, nil
	case c.Token == "t-bob":
		return ext.Principal{ID: "token:b", Name: "bob", Roles: []string{"scope:branches"}}, nil
	case c.Token == "t-full":
		return ext.Principal{ID: "token:f", Name: "ci", Roles: []string{"scope:full"}}, nil
	case c.Token == "" && c.Password == "pw":
		// People who sign in to the console (the user page).
		names := map[string]string{"alice": "alice", "bob": "bob", "carol": "Carol.Smith@Example.com", "long": "a-very-long-user-name-indeed-yes"}
		if n, ok := names[c.Username]; ok {
			return ext.Principal{ID: "sso:" + c.Username, Name: n, Roles: []string{"viewer"}}, nil
		}
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

func as(tok string) func(*http.Request) {
	return func(r *http.Request) {
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
	}
}

func ownedFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.srv.TokenAuth = tokenAuth{}
	f.branches.branches["alices"] = domain.Branch{Name: name("alices"), Base: name("prod"), Status: domain.BranchReady, Owner: "token:a", OwnerName: "alice", Runtime: mysql84, Port: 40001, User: "alices", Password: "secret"}
	f.branches.branches["opened"] = domain.Branch{Name: name("opened"), Base: name("prod"), Status: domain.BranchReady, Owner: "token:a", OwnerName: "alice", Access: domain.AccessOpen, Runtime: mysql84, Port: 40002, User: "opened", Password: "shared"}
	f.branches.branches["legacy"] = domain.Branch{Name: name("legacy"), Base: name("prod"), Status: domain.BranchReady, Runtime: mysql84}
	return f
}

func TestOwnership_自分のブランチだけを変えられる(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	for _, c := range []struct {
		tok, method, path string
		want              int
	}{
		{"t-bob", "DELETE", "/v1/branches/alices", 403},
		{"t-bob", "GET", "/v1/branches/alices/dsn", 403},
		{"t-bob", "GET", "/v1/branches/opened/dsn", 200},
		{"t-bob", "GET", "/v1/branches/opened", 200},
		{"t-bob", "GET", "/v1/branches/alices", 403},
		{"t-alice", "GET", "/v1/branches/alices", 200},
		{"t-bob", "GET", "/v1/branches", 200},
		{"t-bob", "POST", "/v1/branches/legacy/reset", 403},
		{"t-full", "POST", "/v1/branches/legacy/reset", 202},
		{"t-alice", "GET", "/v1/branches/alices/dsn", 200},
		{"t-alice", "POST", "/v1/branches/alices/reset", 202},
		{"t-full", "GET", "/v1/branches/alices/dsn", 200},
		{"t-bob", "GET", "/v1/branches/nope/dsn", 404},
	} {
		resp, body := send(t, h, c.method, c.path, nil, as(c.tok))
		if resp.StatusCode != c.want {
			t.Errorf("%s %s %s: %d %v", c.tok, c.method, c.path, resp.StatusCode, body)
		}
	}
	_, body := send(t, h, "GET", "/v1/branches/alices", nil, as("t-alice"))
	if body["owner"] != "token:a" || body["owner_name"] != "alice" || body["id"] != "alice/alices" || body["access"] != "private" {
		t.Errorf("view = %v", body)
	}
	if _, body := send(t, h, "GET", "/v1/branches/legacy", nil, as("t-full")); body["owner"] != nil || body["owner_name"] != nil || body["id"] != "legacy" {
		t.Errorf("legacy view = %v", body)
	}
}

func TestOwnership_作った人が所有者になり他人の名義は管理者だけ(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "bobs", "base": "prod", "access": "open"}, as("t-bob"))
	if resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("bobs"); return ok })
	if b, _ := f.branches.get("bobs"); b.Owner != "token:b" || b.OwnerName != "bob" || b.Access != domain.AccessOpen {
		t.Errorf("bobs = %+v", b)
	}
	// Non-admin naming themselves cannot spoof owner_name.
	if resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "bobs2", "base": "prod", "owner": "token:b", "owner_name": "spoof"}, as("t-bob")); resp.StatusCode != 202 {
		t.Fatalf("self with spoofed owner_name: %d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("bobs2"); return ok })
	if b, _ := f.branches.get("bobs2"); b.Owner != "token:b" || b.OwnerName != "bob" {
		t.Errorf("bobs2 = %+v", b)
	}
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "for-alice", "base": "prod", "owner": "token:a"}, as("t-bob")); resp.StatusCode != 403 {
		t.Errorf("bob for alice: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "for-alice", "base": "prod", "owner": "token:a", "owner_name": "alice"}, as("t-full")); resp.StatusCode != 202 {
		t.Errorf("admin for alice: %d", resp.StatusCode)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("for-alice"); return ok })
	if b, _ := f.branches.get("for-alice"); b.Owner != "token:a" || b.OwnerName != "alice" {
		t.Errorf("for-alice = %+v", b)
	}
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod", "access": "world"}, as("t-bob")); resp.StatusCode != 400 {
		t.Errorf("bad access: %d", resp.StatusCode)
	}
}

func TestAnonymous_公開スナップショットからだけログインなしで作れる(t *testing.T) {
	defer func(d time.Duration) { httpapi.PublicSettle = d }(httpapi.PublicSettle)
	httpapi.PublicSettle = 50 * time.Millisecond
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	if resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon1", "base": "prod"}, nil); resp.StatusCode != 403 {
		t.Errorf("not public: %d %v", resp.StatusCode, body)
	}
	b := f.branches.bases["prod"]
	b.SetPublic("s1", true)
	f.branches.bases["prod"] = b
	f.srv.Anonymous = func() (time.Duration, int) { return 2 * time.Hour, 2 }
	resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon1", "base": "prod", "ttl": "100h", "owner": "admin"}, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("public: %d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("anon1"); return ok })
	got, _ := f.branches.get("anon1")
	if got.Owner != "anonymous" || !got.Open() || got.ExpiresAt == nil || got.ExpiresAt.Sub(t0) != 2*time.Hour {
		t.Errorf("anon1 = %+v", got)
	}
	// The operation, the branch and its DSN are readable; nothing else.
	if resp, _ := send(t, h, "GET", "/v1/operations/"+body["id"].(string), nil, nil); resp.StatusCode != 200 {
		t.Errorf("operation: %d", resp.StatusCode)
	}
	if resp, body := send(t, h, "GET", "/v1/branches/anon1/dsn", nil, nil); resp.StatusCode != 200 || !strings.HasPrefix(body["dsn"].(string), "mysql://") {
		t.Errorf("dsn: %d %v", resp.StatusCode, body)
	}
	for _, path := range []string{"/v1/branches", "/v1/operations", "/v1/bases"} {
		if resp, _ := send(t, h, "GET", path, nil, nil); resp.StatusCode != 401 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/v1/branches/alices", "/v1/branches/alices/dsn"} {
		if resp, _ := send(t, h, "GET", path, nil, nil); resp.StatusCode != 403 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := send(t, h, "DELETE", "/v1/branches/anon1", nil, nil); resp.StatusCode != 401 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	// The cap counts existing anonymous branches.
	_, _ = send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon2", "base": "prod", "snapshot": "s1"}, nil)
	waitFor(t, func() bool { _, ok := f.branches.get("anon2"); return ok })
	if resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon3", "base": "prod"}, nil); resp.StatusCode != 409 {
		t.Errorf("over the cap: %d %v", resp.StatusCode, body)
	}
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon3", "base": "nope"}, nil); resp.StatusCode != 404 {
		t.Errorf("missing base: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon3", "base": "Bad Name"}, nil); resp.StatusCode != 400 {
		t.Errorf("bad base: %d", resp.StatusCode)
	}
}

func TestAnonymous_公開したばかりのスナップショットからも作れる(t *testing.T) {
	// Publishing is replicated: another node may learn of it a moment later.
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.branches.mu.Lock()
		b := cloneBase(f.branches.bases["prod"])
		b.SetPublic("s1", true)
		f.branches.bases["prod"] = b
		f.branches.mu.Unlock()
	}()
	if resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon1", "base": "prod"}, nil); resp.StatusCode != 202 {
		t.Errorf("just published: %d %v", resp.StatusCode, body)
	}
}

func TestBases_スナップショットの公開と削除とSQL(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do(t, "PATCH", "/v1/bases/prod/snapshots/s1", map[string]any{"public": true})
	snaps := body["snapshots"].([]any)
	if resp.StatusCode != 200 || snaps[0].(map[string]any)["public"] != true {
		t.Errorf("public: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "PATCH", "/v1/bases/prod/snapshots/s1", map[string]any{}); resp.StatusCode != 400 {
		t.Errorf("nothing: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/bases/prod/snapshots/s1", nil); resp.StatusCode != 204 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/bases/prod/sql", map[string]any{"sql": "INSERT INTO t VALUES (1)"}); resp.StatusCode != 204 {
		t.Errorf("sql: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/bases/prod/sql", map[string]any{"sql": " "}); resp.StatusCode != 400 {
		t.Errorf("empty sql: %d", resp.StatusCode)
	}
	want := []string{"public prod@s1 true", "delete-snapshot prod@s1", "exec-base prod INSERT INTO t VALUES (1)"}
	if strings.Join(f.branches.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v", f.branches.calls)
	}
	f.branches.fail = errors.New("syntax error")
	if resp, body := f.do(t, "POST", "/v1/bases/prod/sql", map[string]any{"sql": "x"}); resp.StatusCode != 422 || errCode(body) != "query_failed" {
		t.Errorf("failing sql: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/bases/prod/snapshots/s1", nil); resp.StatusCode != 500 {
		t.Errorf("failing delete: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PATCH", "/v1/bases/prod/snapshots/s1", map[string]any{"public": false}); resp.StatusCode != 500 {
		t.Errorf("failing patch: %d", resp.StatusCode)
	}
	f.branches.fail = domain.ErrConflict
	if resp, _ := f.do(t, "POST", "/v1/bases/prod/sql", map[string]any{"sql": "x"}); resp.StatusCode != 409 {
		t.Errorf("maintenance: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/bases/nope/sql", map[string]any{"sql": "x"}); resp.StatusCode != 404 {
		t.Errorf("missing base: %d", resp.StatusCode)
	}
}

func (f *fakeBranches) get(n string) (domain.Branch, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.branches[n]
	return b, ok
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestBases_説明を付けて変えられる(t *testing.T) {
	f := newFixture(t)
	resp, op := f.do(t, "POST", "/v1/bases", map[string]any{"name": "demo", "runtime": "mysql@8.4.13", "description": "sample shop data"})
	if resp.StatusCode != 202 {
		t.Fatalf("create: %d %v", resp.StatusCode, op)
	}
	f.waitOp(t, op["id"].(string))
	if _, body := f.do(t, "GET", "/v1/bases/demo", nil); body["description"] != "sample shop data" {
		t.Errorf("get: %v", body)
	}
	resp, body := f.do(t, "PATCH", "/v1/bases/demo", map[string]any{"description": "for demos"})
	if resp.StatusCode != 200 || body["description"] != "for demos" {
		t.Errorf("patch: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "PATCH", "/v1/bases/demo", map[string]any{}); resp.StatusCode != 400 {
		t.Errorf("nothing to change: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PATCH", "/v1/bases/nope", map[string]any{"description": "x"}); resp.StatusCode != 404 {
		t.Errorf("missing: %d", resp.StatusCode)
	}
	f.branches.fail = errors.New("too long")
	if resp, _ := f.do(t, "PATCH", "/v1/bases/demo", map[string]any{"description": "x"}); resp.StatusCode != 400 {
		t.Errorf("rejected: %d", resp.StatusCode)
	}
}

func TestPublicBases_ログインなしで公開スナップショットのあるベースだけ見える(t *testing.T) {
	f := ownedFixture(t)
	f.branches.bases["hidden"] = domain.Base{Name: name("hidden"), Runtime: mysql84, Status: domain.BaseReady,
		Snapshots: []domain.BaseSnapshot{{Name: "h1", CreatedAt: t0}}}
	b := f.branches.bases["prod"]
	b.Description = "demo data"
	b.Snapshots = append(b.Snapshots, domain.BaseSnapshot{Name: "s2", CreatedAt: t0.Add(time.Hour), Public: true})
	f.branches.bases["prod"] = b
	f.srv.Anonymous = func() (time.Duration, int) { return 2 * time.Hour, 3 }
	resp, body := send(t, f.srv.Handler(false), "GET", "/v1/public/bases", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items %v", items)
	}
	prod := items[0].(map[string]any)
	snaps := prod["snapshots"].([]any)
	if prod["name"] != "prod" || prod["description"] != "demo data" || len(snaps) != 1 || snaps[0].(map[string]any)["name"] != "s2" || prod["runtime"] != "mysql@8.4.13" {
		t.Errorf("prod %v", prod)
	}
	anon := body["anonymous"].(map[string]any)
	if anon["max_ttl"] != "2h0m0s" || anon["max_branches"] != 3.0 {
		t.Errorf("limits %v", anon)
	}
}

func TestBranchView_メールアドレスの所有者はidでローカル部だけを見せる(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Base: name("prod"), Status: domain.BranchReady, Owner: "dev1@example.com", Runtime: mysql84}
	_, body := f.do(t, "GET", "/v1/branches/x", nil)
	if body["id"] != "dev1/x" || body["owner"] != "dev1@example.com" {
		t.Errorf("%v", body)
	}
}

// teamPolicy: alice and bob share a team; at most 3 branches; base prod only; TTL ≤ 8h.
type teamPolicy struct{ got []ext.BranchRequest }

func (p *teamPolicy) CheckBranch(_ context.Context, who ext.Principal, r ext.BranchRequest) error {
	p.got = append(p.got, r)
	if slices.Contains(who.Roles, "scope:full") {
		return nil // like the auth module, this policy does not limit admins
	}
	if r.Base != "prod" {
		return fmt.Errorf("%w: your team may branch only from prod", ext.ErrForbidden)
	}
	if r.TTL == 0 || r.TTL > 8*time.Hour {
		return fmt.Errorf("%w: your team's branches last at most 8h", ext.ErrForbidden)
	}
	n := 0
	for _, b := range r.Existing {
		if b.Owner == "token:a" || b.Owner == "token:b" {
			n++
		}
	}
	if n >= 3 {
		return fmt.Errorf("%w: your team has 3 branches already", domain.ErrConflict)
	}
	return nil
}

func (p *teamPolicy) SharesBranches(_ context.Context, who ext.Principal, owner string) bool {
	team := map[string]bool{"token:a": true, "token:b": true}
	return team[who.ID] && team[owner]
}

func TestBranchPolicy_モジュールが作成を判断しチームの人は互いのブランチを扱える(t *testing.T) {
	f := ownedFixture(t)
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql84, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s0"}, {Name: "s1"}}}
	pol := &teamPolicy{}
	f.srv.Ext = ext.Module{BranchPolicy: pol}
	h := f.srv.Handler(false)
	if resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "b1", "base": "prod", "ttl": "4h"}, as("t-bob")); resp.StatusCode != 202 {
		t.Fatalf("allowed: %d %v", resp.StatusCode, body)
	}
	if g := pol.got[0]; g.Name != "b1" || g.Owner != "token:b" || g.TTL != 4*time.Hour || len(g.Existing) < 3 {
		t.Errorf("request %+v", g)
	}
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "b2", "base": "prod"}, as("t-bob")); resp.StatusCode != 403 {
		t.Errorf("no TTL: %d", resp.StatusCode)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("b1"); return ok })
	// alice's and bob's branches now count 3 (alices, opened, b1): the team's cap.
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "b3", "base": "prod", "ttl": "1h"}, as("t-alice")); resp.StatusCode != 409 {
		t.Errorf("team cap: %d", resp.StatusCode)
	}
	// bob may reset alice's branch: same team.
	if resp, _ := send(t, h, "POST", "/v1/branches/alices/reset", nil, as("t-bob")); resp.StatusCode != 202 {
		t.Errorf("teammate reset: %d", resp.StatusCode)
	}
	// Admins are not limited by this policy, but asked too (other policies,
	// e.g. masking, apply to everyone); the latest snapshot is named.
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "b4", "base": "other"}, as("t-full")); resp.StatusCode == 403 {
		t.Errorf("admin limited: %d", resp.StatusCode)
	}
	if g := pol.got[len(pol.got)-1]; g.Name != "b4" {
		t.Errorf("admin's request not asked: %+v", g)
	}
	if g := pol.got[0]; g.Snapshot != "s1" {
		t.Errorf("latest snapshot not named: %q", g.Snapshot)
	}
}

func TestBases_レプリカ先を作成時に指定し後から変えられ保持の状態が見える(t *testing.T) {
	f := newFixture(t)
	resp, op := f.do(t, "POST", "/v1/bases", map[string]any{"name": "big", "runtime": "mysql@8.4.13", "replica_nodes": []string{"n2"}})
	if resp.StatusCode != 202 {
		t.Fatalf("create: %d %v", resp.StatusCode, op)
	}
	f.waitOp(t, op["id"].(string))
	if _, body := f.do(t, "GET", "/v1/bases/big", nil); fmt.Sprint(body["replica_nodes"]) != "[n2]" {
		t.Errorf("replica_nodes: %v", body["replica_nodes"])
	}
	if _, body := f.do(t, "GET", "/v1/bases/prod", nil); body["replica_nodes"] != nil {
		t.Errorf("default must be null: %v", body["replica_nodes"])
	}
	resp, body := f.do(t, "PUT", "/v1/bases/prod/replicas", map[string]any{"replica_nodes": []string{}})
	if resp.StatusCode != 200 || fmt.Sprint(body["replica_nodes"]) != "[]" {
		t.Errorf("origin only: %d %v", resp.StatusCode, body)
	}
	if resp, body = f.do(t, "PUT", "/v1/bases/prod/replicas", map[string]any{"replica_nodes": nil}); resp.StatusCode != 200 || body["replica_nodes"] != nil {
		t.Errorf("all: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "PUT", "/v1/bases/prod/replicas", map[string]any{}); resp.StatusCode != 400 {
		t.Errorf("missing field: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/bases/prod/replicas", map[string]any{"replica_nodes": "n2"}); resp.StatusCode != 400 {
		t.Errorf("wrong type: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/bases/nope/replicas", map[string]any{"replica_nodes": nil}); resp.StatusCode != 404 {
		t.Errorf("missing base: %d", resp.StatusCode)
	}
	f.branches.fail = fmt.Errorf("%w: node x", domain.ErrInvalidReplicas)
	if resp, _ := f.do(t, "PUT", "/v1/bases/prod/replicas", map[string]any{"replica_nodes": []string{"x"}}); resp.StatusCode != 400 {
		t.Errorf("unknown node: %d", resp.StatusCode)
	}
}

func TestHoldersOf_保持中と取得中と撤去中をノードごとに示す(t *testing.T) {
	f := newFixture(t)
	b := domain.Base{Name: name("prod"), Runtime: mysql84, Status: domain.BaseReady, Node: "a",
		Snapshots: []domain.BaseSnapshot{{Name: "s1", CreatedAt: t0}, {Name: "s2", CreatedAt: t0}},
		Replicas:  map[string][]string{"s1": {"b", "c", "d"}, "s2": {"b"}}}
	only := []string{"b", "e"}
	b.ReplicaNodes = &only
	f.branches.bases["prod"] = b
	_, body := f.do(t, "GET", "/v1/bases/prod", nil)
	got := fmt.Sprint(body["holders"])
	want := "[map[node:a origin:true state:held] map[node:b origin:false state:held] map[node:c origin:false state:draining] map[node:d origin:false state:draining] map[node:e origin:false state:pulling]]"
	if got != want {
		t.Errorf("holders = %s", got)
	}
}

// createAs makes a branch the way the user page does: signed in to the
// console as user (alice, bob, carol, long), through a login session.
func createAs(t *testing.T, h http.Handler, user string, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	resp, in := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": user, "password": "pw"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login %s: %d %v", user, resp.StatusCode, in)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "dbb_session" {
			cookie = c
		}
	}
	body["base"] = "prod"
	return send(t, h, "POST", "/v1/branches", body, withSession(cookie, in["csrf_token"].(string)))
}

func TestCreateBranch_利用者が作る名前には所有者名が前に付く(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	if resp, body := createAs(t, h, "alice", map[string]any{"name": "test"}); resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("alice-test"); return ok })
	if b, _ := f.branches.get("alice-test"); b.Owner != "sso:alice" || b.OwnerName != "alice" {
		t.Errorf("owner = (%q, %q)", b.Owner, b.OwnerName)
	}
	if _, ok := f.branches.get("test"); ok {
		t.Error("unprefixed branch was made")
	}
}

func TestCreateBranch_別々の利用者が同じ名前でそれぞれ作れる(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	for _, tok := range []string{"alice", "bob", "carol"} {
		if resp, body := createAs(t, h, tok, map[string]any{"name": "test"}); resp.StatusCode != 202 {
			t.Fatalf("%s: %d %v", tok, resp.StatusCode, body)
		}
	}
	for _, want := range []string{"alice-test", "bob-test", "carol-smith-test"} {
		waitFor(t, func() bool { _, ok := f.branches.get(want); return ok })
	}
	if b, _ := f.branches.get("carol-smith-test"); b.Owner != "sso:carol" || b.OwnerName != "Carol.Smith@Example.com" {
		t.Errorf("owner = (%q, %q)", b.Owner, b.OwnerName)
	}
}

func TestCreateBranch_すでに所有者名が付いた名前は重ねない(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	if resp, body := createAs(t, h, "alice", map[string]any{"name": "alice-test"}); resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("alice-test"); return ok })
	if _, ok := f.branches.get("alice-alice-test"); ok {
		t.Error("prefix was doubled")
	}
}

func TestCreateBranch_同じ人の同じ名前は分かる言葉で重複を伝える(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	f.branches.branches["alice-test"] = domain.Branch{Name: name("alice-test"), Base: name("prod"), Status: domain.BranchReady, Owner: "alice", Runtime: mysql84}
	resp, body := createAs(t, h, "alice", map[string]any{"name": "test"})
	if resp.StatusCode != 409 || errCode(body) != "already_exists" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if msg := fmt.Sprint(body); !strings.Contains(msg, "alice-test") {
		t.Errorf("message does not name the full branch: %v", body)
	}
}

func TestCreateBranch_前置きを付けると長すぎる名前は入力できる長さを伝えて拒否する(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	resp, body := createAs(t, h, "alice", map[string]any{"name": strings.Repeat("a", 27)})
	if resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(body), "at most 26 characters") {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
	if resp, body := createAs(t, h, "long", map[string]any{"name": "x"}); resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(body), "too long") {
		t.Errorf("long owner: %d %v", resp.StatusCode, body)
	}
}

func TestCreateBranch_管理者が作る名前はそのまま使う(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	if resp, body := tokenCreate(t, h, "t-full", map[string]any{"name": "test"}); resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("test"); return ok })
	if resp, body := tokenCreate(t, h, "t-full", map[string]any{"name": "x-for-bob", "owner": "bob"}); resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("x-for-bob"); return ok })
}

func TestSession_利用者には名前の前置きを伝え管理者には伝えない(t *testing.T) {
	f := newFixture(t)
	f.srv.Ext = ext.Module{Authenticator: moduleAuth{}, Authorizer: viewerReadsOnly{}}
	h := f.srv.Handler(false)
	_, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "alice", "password": "alice-pw"}, nil)
	if body["branch_prefix"] != "alice-" {
		t.Errorf("alice prefix = %v", body["branch_prefix"])
	}
	_, body = send(t, h, "POST", "/v1/auth/login", map[string]any{"username": "admin", "password": "correct horse battery"}, nil)
	if body["branch_prefix"] != "" {
		t.Errorf("admin prefix = %v", body["branch_prefix"])
	}
}

func tokenCreate(t *testing.T, h http.Handler, tok string, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	body["base"] = "prod"
	return send(t, h, "POST", "/v1/branches", body, as(tok))
}

func TestCreateBranch_管理者のセッションで作る名前もそのまま使う(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Handler(false)
	cookie, csrf := login(t, f)
	if resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "adm", "base": "prod"}, withSession(cookie, csrf)); resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("adm"); return ok })
}

func TestCreateBranch_トークンで作る名前は利用者でもそのまま使う(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	if resp, body := tokenCreate(t, h, "t-alice", map[string]any{"name": "ci-run"}); resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("ci-run"); return ok })
}

func TestOwnership_同名サービストークンで他ユーザーのprivateブランチを操作およびDSN取得できない(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	// SSO user alice (ID: "sso:alice", Name: "alice") creates a private branch.
	resp, body := createAs(t, h, "alice", map[string]any{"name": "priv"})
	if resp.StatusCode != 202 {
		t.Fatalf("alice create: %d %v", resp.StatusCode, body)
	}
	waitFor(t, func() bool { _, ok := f.branches.get("alice-priv"); return ok })
	b, _ := f.branches.get("alice-priv")
	if b.Owner != "sso:alice" || b.OwnerName != "alice" {
		t.Fatalf("stored owner = (%q, %q), want (sso:alice, alice)", b.Owner, b.OwnerName)
	}

	// Service token "t-alice" has the SAME display Name ("alice"), but a different
	// immutable ID ("token:a"). It must NOT be able to operate on or get DSN for
	// sso:alice's private branch!
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/branches/alice-priv/dsn"},
		{"DELETE", "/v1/branches/alice-priv"},
		{"POST", "/v1/branches/alice-priv/reset"},
		{"PATCH", "/v1/branches/alice-priv"},
		{"POST", "/v1/branches/alice-priv/pause"},
		{"POST", "/v1/branches/alice-priv/query"},
	} {
		var reqBody any
		if c.method == "PATCH" {
			reqBody = map[string]any{"ttl": "1h"}
		} else if strings.HasSuffix(c.path, "/query") {
			reqBody = map[string]any{"sql": "SELECT 1"}
		}
		if r, rb := send(t, h, c.method, c.path, reqBody, as("t-alice")); r.StatusCode != 403 {
			t.Errorf("service token 'alice' %s %s: got %d %v, want 403", c.method, c.path, r.StatusCode, rb)
		}
	}

	// Admin/owner view returns immutable owner ID and display owner_name separately.
	_, view := send(t, h, "GET", "/v1/branches/alice-priv", nil, as("t-full"))
	if view["owner"] != "sso:alice" || view["owner_name"] != "alice" || view["id"] != "alice/alice-priv" {
		t.Errorf("view = %v", view)
	}
}

type shareEverythingPolicy struct{ calledWithEmpty bool }

func (p *shareEverythingPolicy) CheckBranch(context.Context, ext.Principal, ext.BranchRequest) error {
	return nil
}

func (p *shareEverythingPolicy) SharesBranches(_ context.Context, _ ext.Principal, owner string) bool {
	if owner == "" {
		p.calledWithEmpty = true
	}
	return true
}

func TestOwnership_Owner空レガシーブランチは非管理者が変更削除できない(t *testing.T) {
	f := ownedFixture(t)
	pol := &shareEverythingPolicy{}
	f.srv.Ext = ext.Module{BranchPolicy: pol}
	f.branches.branches["legacy-open"] = domain.Branch{
		Name: name("legacy-open"), Base: name("prod"), Status: domain.BranchReady,
		Access: domain.AccessOpen, Runtime: mysql84, Port: 40003, User: "legacy-open", Password: "shared",
	}
	h := f.srv.Handler(false)

	for _, c := range []struct {
		tok, method, path string
		body              any
		want              int
	}{
		// Private legacy branch (Owner == ""): non-admin denied for mutations and connect/query.
		{"t-bob", "DELETE", "/v1/branches/legacy", nil, 403},
		{"t-bob", "POST", "/v1/branches/legacy/reset", nil, 403},
		{"t-bob", "PATCH", "/v1/branches/legacy", map[string]any{"ttl": "1h"}, 403},
		{"t-bob", "POST", "/v1/branches/legacy/pause", nil, 403},
		{"t-bob", "POST", "/v1/branches/legacy/checkpoints", nil, 403},
		{"t-bob", "POST", "/v1/branches/legacy/upgrade", map[string]any{"runtime": "mysql@8.4.13"}, 403},
		{"t-bob", "GET", "/v1/branches/legacy/dsn", nil, 403},
		{"t-bob", "POST", "/v1/branches/legacy/query", map[string]any{"sql": "SELECT 1"}, 403},
		// Open legacy branch (Owner == "", Access == "open"): non-admin can connect, NOT mutate.
		{"t-bob", "GET", "/v1/branches/legacy-open/dsn", nil, 200},
		{"t-bob", "DELETE", "/v1/branches/legacy-open", nil, 403},
		{"t-bob", "POST", "/v1/branches/legacy-open/reset", nil, 403},
		// Admin can manage legacy branches.
		{"t-full", "GET", "/v1/branches/legacy/dsn", nil, 200},
		{"t-full", "POST", "/v1/branches/legacy/reset", nil, 202},
		{"t-full", "DELETE", "/v1/branches/legacy", nil, 202},
	} {
		resp, body := send(t, h, c.method, c.path, c.body, as(c.tok))
		if resp.StatusCode != c.want {
			t.Errorf("%s %s %s: got %d %v, want %d", c.tok, c.method, c.path, resp.StatusCode, body, c.want)
		}
	}
	if pol.calledWithEmpty {
		t.Error("SharesBranches must not be called for a branch with empty owner")
	}
}

func TestHTTPAPI_branchesスコープトークンでpublicBasesを取得できる(t *testing.T) {
	f := ownedFixture(t)
	b := f.branches.bases["prod"]
	b.SetPublic("s1", true)
	f.branches.bases["prod"] = b
	h := f.srv.Handler(false)

	resp, body := send(t, h, "GET", "/v1/public/bases", nil, as("t-bob"))
	if resp.StatusCode != 200 {
		t.Fatalf("branches token GET /v1/public/bases: got %d %v, want 200", resp.StatusCode, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "prod" {
		t.Errorf("items = %v", items)
	}
}

func TestOwnership_branchReadは自分とopenとSharesBranchesのブランチだけ閲覧および一覧できる(t *testing.T) {
	f := ownedFixture(t)
	f.branches.branches["bobs"] = domain.Branch{Name: name("bobs"), Base: name("prod"), Status: domain.BranchReady, Owner: "token:b", OwnerName: "bob", Runtime: mysql84}
	f.branches.branches["carols"] = domain.Branch{Name: name("carols"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:carol", OwnerName: "Carol.Smith@Example.com", Runtime: mysql84}
	f.branches.branches["legacy-open"] = domain.Branch{Name: name("legacy-open"), Base: name("prod"), Status: domain.BranchReady, Access: domain.AccessOpen, Runtime: mysql84}
	h := f.srv.Handler(false)

	listNames := func(tok string) []string {
		resp, body := send(t, h, "GET", "/v1/branches", nil, as(tok))
		if resp.StatusCode != 200 {
			t.Fatalf("%s GET /v1/branches: %d %v", tok, resp.StatusCode, body)
		}
		var names []string
		for _, item := range body["items"].([]any) {
			names = append(names, item.(map[string]any)["name"].(string))
		}
		return names
	}

	// Without SharesBranches: bob sees only his own ("bobs") and open branches ("legacy-open", "opened").
	// Bob must NOT see alice's private ("alices"), carol's private ("carols"), or private legacy ("legacy").
	if got, want := listNames("t-bob"), []string{"bobs", "legacy-open", "opened"}; !slices.Equal(got, want) {
		t.Errorf("bob list without policy = %v, want %v", got, want)
	}
	for _, c := range []struct {
		tok, path string
		want      int
	}{
		{"t-bob", "/v1/branches/bobs", 200},
		{"t-bob", "/v1/branches/opened", 200},
		{"t-bob", "/v1/branches/legacy-open", 200},
		{"t-bob", "/v1/branches/alices", 403},
		{"t-bob", "/v1/branches/carols", 403},
		{"t-bob", "/v1/branches/legacy", 403},
		{"t-bob", "/v1/branches/nope", 404},
		// Admin sees every branch including private and legacy.
		{"t-full", "/v1/branches/alices", 200},
		{"t-full", "/v1/branches/carols", 200},
		{"t-full", "/v1/branches/legacy", 200},
	} {
		if resp, body := send(t, h, "GET", c.path, nil, as(c.tok)); resp.StatusCode != c.want {
			t.Errorf("%s GET %s: got %d %v, want %d", c.tok, c.path, resp.StatusCode, body, c.want)
		}
	}
	if got, want := listNames("t-full"), []string{"alices", "bobs", "carols", "legacy", "legacy-open", "opened"}; !slices.Equal(got, want) {
		t.Errorf("admin list = %v, want %v", got, want)
	}

	// With SharesBranches (alice + bob share a team): bob can now also read and list "alices",
	// but still NOT carol's private branch or legacy private branch.
	f.srv.Ext = ext.Module{BranchPolicy: &teamPolicy{}}
	if got, want := listNames("t-bob"), []string{"alices", "bobs", "legacy-open", "opened"}; !slices.Equal(got, want) {
		t.Errorf("bob list with team policy = %v, want %v", got, want)
	}
	if resp, body := send(t, h, "GET", "/v1/branches/alices", nil, as("t-bob")); resp.StatusCode != 200 {
		t.Errorf("teammate GET alices: got %d %v, want 200", resp.StatusCode, body)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches/carols", nil, as("t-bob")); resp.StatusCode != 403 {
		t.Errorf("non-teammate GET carols: got %d, want 403", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/branches/legacy", nil, as("t-bob")); resp.StatusCode != 403 {
		t.Errorf("teammate policy must not expose legacy: got %d, want 403", resp.StatusCode)
	}
}

// loginCookie signs user in to the console and returns a request modifier that
// carries the session.
func loginCookie(t *testing.T, h http.Handler, user string) func(*http.Request) {
	t.Helper()
	resp, body := send(t, h, "POST", "/v1/auth/login", map[string]any{"username": user, "password": "pw"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login %s: %d %v", user, resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "dbb_session" {
			return func(r *http.Request) { r.AddCookie(c) }
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestOwnership_表示名で作られ移行できなかった旧ブランチは本人のセッションだけが管理できる(t *testing.T) {
	f := ownedFixture(t)
	// Created before owners were IDs by a console user shown as "Carol.Smith@Example.com";
	// migration could not resolve an ID, so OwnerName stays empty.
	f.branches.branches["carols"] = domain.Branch{Name: name("carols"), Base: name("prod"), Status: domain.BranchReady,
		Owner: "Carol.Smith@Example.com", Runtime: mysql84, Port: 40009, User: "carols", Password: "secret"}
	// A service token that is merely named like the old owner must not take it over.
	f.branches.branches["alices-old"] = domain.Branch{Name: name("alices-old"), Base: name("prod"), Status: domain.BranchReady,
		Owner: "alice", Runtime: mysql84, Port: 40010, User: "alices-old", Password: "secret"}
	h := f.srv.Handler(false)

	carol := loginCookie(t, h, "carol")
	if r, b := send(t, h, "GET", "/v1/branches/carols/dsn", nil, carol); r.StatusCode != 200 {
		t.Errorf("carol on her unmigrated branch: %d %v, want 200", r.StatusCode, b)
	}
	if r, b := send(t, h, "GET", "/v1/branches/carols/dsn", nil, loginCookie(t, h, "bob")); r.StatusCode != 403 {
		t.Errorf("bob on carol's unmigrated branch: %d %v, want 403", r.StatusCode, b)
	}
	if r, b := send(t, h, "GET", "/v1/branches/alices-old/dsn", nil, as("t-alice")); r.StatusCode != 403 {
		t.Errorf("service token named alice on an unmigrated branch: %d %v, want 403", r.StatusCode, b)
	}
	if r, b := send(t, h, "GET", "/v1/branches/alices-old/dsn", nil, loginCookie(t, h, "alice")); r.StatusCode != 200 {
		t.Errorf("alice's console session on her unmigrated branch: %d %v, want 200", r.StatusCode, b)
	}
}
