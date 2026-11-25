package httpapi_test

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

// sseTypes opens /v1/events as tok and returns a function that reads event
// types until the one named stop arrives (or time runs out).
func sseTypes(t *testing.T, srv *httptest.Server, tok, lastID string) func(stop string) []string {
	t.Helper()
	return sseStream(t, srv, tok, lastID)
}

func sseStream(t *testing.T, srv *httptest.Server, tok, lastID string) func(stop string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/events", nil)
	as(tok)(req)
	req.Header.Set("Last-Event-ID", lastID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("events as %q: %d", tok, resp.StatusCode)
	}
	rd := bufio.NewReader(resp.Body)
	return func(stop string) []string {
		var got []string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended before %s (got %v): %v", stop, got, err)
			}
			if typ, ok := strings.CutPrefix(strings.TrimSpace(line), "event: "); ok {
				if typ == stop {
					return got
				}
				got = append(got, typ)
			}
		}
	}
}

func TestHTTPAPI_SSEは権限のない人へ他人や管理用のイベントを流さない(t *testing.T) {
	f := ownedFixture(t)
	bus := f.srv.Bus
	bus.Publish("first", nil) // ID 1: the clients replay everything after it
	bus.Publish("setting.updated", map[string]string{"scope": "mysql"})
	bus.PublishTo(operation.Audience{Owner: "token:a"}, "branch.updated", "alices")
	bus.PublishTo(operation.Audience{Owner: "token:b"}, "operation.updated", "bobs op")
	bus.PublishTo(operation.Audience{Owner: "token:a", Open: true}, "branch.updated", "opened")
	bus.PublishTo(operation.Audience{Action: "base.read"}, "base.updated", "prod")
	bus.PublishTo(operation.Audience{Open: true}, "stop", nil)

	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()

	for _, c := range []struct {
		tok  string
		want []string
	}{
		{"t-bob", []string{"operation.updated", "branch.updated", "base.updated"}},
		{"t-alice", []string{"branch.updated", "branch.updated", "base.updated"}},
		{"t-full", []string{"setting.updated", "branch.updated", "operation.updated", "branch.updated", "base.updated"}},
	} {
		got := sseTypes(t, srv, c.tok, "1")("stop")
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s receives %v, want %v", c.tok, got, c.want)
		}
	}
}

func TestHTTPAPI_ブランチの更新と削除は所有者宛てに届き他人には届かない(t *testing.T) {
	f := ownedFixture(t)
	ctx := context.Background()
	f.srv.Bus.Publish("first", nil)
	f.srv.PublishBranch(ctx, name("alices")) // private, token:a
	f.srv.PublishBranch(ctx, name("opened")) // open, token:a
	f.srv.PublishBranch(ctx, name("legacy")) // no owner
	f.branches.mu.Lock()
	delete(f.branches.branches, "alices")
	f.branches.mu.Unlock()
	f.srv.PublishBranch(ctx, name("alices")) // now deleted
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)

	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()
	for _, c := range []struct {
		tok  string
		want string
	}{
		{"t-bob", "branch.updated"},                                 // only the open one
		{"t-alice", "branch.updated,branch.updated,branch.deleted"}, // not the legacy one
		{"t-full", "branch.updated,branch.updated,branch.updated,branch.deleted"},
	} {
		if got := strings.Join(sseTypes(t, srv, c.tok, "1")("stop"), ","); got != c.want {
			t.Errorf("%s receives %s, want %s", c.tok, got, c.want)
		}
	}
}

func TestHTTPAPI_SSEはチームの人へ互いのイベントを流しログインなしには流さない(t *testing.T) {
	f := ownedFixture(t)
	f.srv.Ext = ext.Module{BranchPolicy: &teamPolicy{}}
	f.srv.Bus.Publish("first", nil)
	f.srv.Bus.PublishTo(operation.Audience{Owner: "token:a"}, "operation.updated", "alice's")
	f.srv.Bus.PublishTo(operation.Audience{Owner: "anonymous"}, "operation.updated", "anonymous'")
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)

	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()
	if got := strings.Join(sseTypes(t, srv, "t-bob", "1")("stop"), ","); got != "operation.updated" {
		t.Errorf("teammate receives %q", got)
	}
	// Without a login there is no stream at all.
	resp, err := http.Get(srv.URL + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("anonymous: %d", resp.StatusCode)
	}
}

func TestHTTPAPI_再起動後でも削除イベントは所有者とopenの閲覧者に届く(t *testing.T) {
	f := ownedFixture(t) // alices (private) and opened (open) exist; nothing was published since "boot"
	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()
	bob, alice := sseStream(t, srv, "t-bob", ""), sseStream(t, srv, "t-alice", "")

	ctx := context.Background()
	f.branches.mu.Lock()
	delete(f.branches.branches, "alices")
	delete(f.branches.branches, "opened")
	f.branches.mu.Unlock()
	f.srv.PublishBranch(ctx, name("alices"))
	f.srv.PublishBranch(ctx, name("opened"))
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)

	if got := strings.Join(bob("stop"), ","); got != "branch.deleted" { // only opened
		t.Errorf("bob receives %q", got)
	}
	if got := strings.Join(alice("stop"), ","); got != "branch.deleted,branch.deleted" {
		t.Errorf("alice receives %q", got)
	}
}

func TestHTTPAPI_openから非公開にしたブランチは閲覧者の一覧から消える(t *testing.T) {
	f := ownedFixture(t)
	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()
	bob := sseStream(t, srv, "t-bob", "")

	f.branches.mu.Lock()
	b := f.branches.branches["opened"]
	b.Access = domain.AccessPrivate
	f.branches.branches["opened"] = b
	f.branches.mu.Unlock()
	f.srv.PublishBranch(context.Background(), name("opened"))
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)

	if got := strings.Join(bob("stop"), ","); got != "branch.deleted" {
		t.Errorf("bob receives %q, want the branch dropped from his view", got)
	}
}

func TestHTTPAPI_初期化前のストリームは次の接続で宛先を覚え直す(t *testing.T) {
	f := ownedFixture(t)
	ready := f.srv.Branches
	f.srv.Branches = func() (httpapi.Branches, error) { return nil, errors.New("not initialized") }
	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()
	first := sseStream(t, srv, "t-full", "")
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)
	first("stop")

	f.srv.Branches = ready
	alice := sseStream(t, srv, "t-alice", "")
	f.branches.mu.Lock()
	delete(f.branches.branches, "alices")
	f.branches.mu.Unlock()
	f.srv.PublishBranch(context.Background(), name("alices"))
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)
	if got := strings.Join(alice("stop"), ","); got != "branch.deleted" {
		t.Errorf("alice receives %q", got)
	}
}

func TestHTTPAPI_宛先にアクションを持つイベントはそのアクションが許された人にだけ届く(t *testing.T) {
	f := ownedFixture(t)
	f.srv.Bus.Publish("first", nil)
	f.srv.Bus.PublishTo(operation.Audience{Action: "setting.read"}, "setting.updated", "x") // not for branches tokens
	f.srv.Bus.PublishTo(operation.Audience{Action: "runtime.read"}, "runtime.updated", "x")
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)
	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()
	if got := strings.Join(sseTypes(t, srv, "t-bob", "1")("stop"), ","); got != "runtime.updated" {
		t.Errorf("bob receives %q", got)
	}
}

type atomicResolver struct{ active atomic.Bool }

func (r *atomicResolver) ResolvePrincipal(_ context.Context, p ext.Principal) (ext.Principal, error) {
	if !r.active.Load() {
		return ext.Principal{}, ext.ErrUnauthenticated
	}
	return p, nil
}

func TestHTTPAPI_SSE中に無効化された利用者のストリームは閉じる(t *testing.T) {
	f := ownedFixture(t)
	res := &atomicResolver{}
	res.active.Store(true)
	f.srv.Ext = ext.Module{PrincipalResolver: res}
	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/events", nil)
	as("t-bob")(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events: %v %d", err, resp.StatusCode)
	}
	defer resp.Body.Close()

	res.active.Store(false)
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "runtime.updated", "x")
	rd := bufio.NewReader(resp.Body)
	for {
		line, rerr := rd.ReadString('\n')
		if strings.Contains(line, "runtime.updated") {
			t.Fatal("disabled user received event after deactivation")
		}
		if rerr != nil {
			break // stream closed cleanly
		}
	}
}

func TestHTTPAPI_SSEはBase制限サービスTokenへ許可Base以外のイベントを流さない(t *testing.T) {
	f := ownedFixture(t)
	toks := &token.Service{Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "t.json")}, Clock: func() time.Time { return t0 }}
	f.srv.Tokens, f.srv.TokenAuth = toks, toks
	f.srv.Ext = ext.Module{BranchPolicy: &shareForTeamPolicy{team: "payments", userID: "user:alice@x.io"}}

	f.branches.mu.Lock()
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql84, Status: domain.BaseReady}
	f.branches.bases["analytics"] = domain.Base{Name: name("analytics"), Runtime: mysql84, Status: domain.BaseReady}
	f.branches.branches["prod-open"] = domain.Branch{Name: name("prod-open"), Base: name("prod"), Status: domain.BranchReady, Owner: "user:bob@x.io", Access: domain.AccessOpen, Runtime: mysql84}
	f.branches.branches["ana-open"] = domain.Branch{Name: name("ana-open"), Base: name("analytics"), Status: domain.BranchReady, Owner: "user:bob@x.io", Access: domain.AccessOpen, Runtime: mysql84}
	f.branches.branches["prod-alice"] = domain.Branch{Name: name("prod-alice"), Base: name("prod"), Status: domain.BranchReady, Owner: "user:alice@x.io", Runtime: mysql84}
	f.branches.branches["ana-alice"] = domain.Branch{Name: name("ana-alice"), Base: name("analytics"), Status: domain.BranchReady, Owner: "user:alice@x.io", Runtime: mysql84}
	f.branches.mu.Unlock()

	ctx := context.Background()
	tokProd, _, err := toks.CreateService(ctx, "ci-prod", domain.ScopeBranches, 0, []string{"prod"}, "payments")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(f.srv.Handler(false))
	defer srv.Close()

	// Connect SSE client BEFORE events happen so seedBranchAudiences also records ana-open / ana-alice.
	sCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sCtx, "GET", srv.URL+"/v1/events", nil)
	as(tokProd)(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events: %v %d", err, resp.StatusCode)
	}
	defer resp.Body.Close()

	// Emit base, branch (open/teammate/open->private/deleted), and teammate operation events on prod & analytics.
	f.srv.PublishBase(ctx, name("prod"))
	f.srv.PublishBase(ctx, name("analytics"))
	f.srv.PublishBranch(ctx, name("prod-open"))
	f.srv.PublishBranch(ctx, name("ana-open"))
	f.srv.PublishBranch(ctx, name("prod-alice"))
	f.srv.PublishBranch(ctx, name("ana-alice"))

	f.branches.mu.Lock()
	ao := f.branches.branches["ana-open"]
	ao.Access = domain.AccessPrivate
	f.branches.branches["ana-open"] = ao
	delete(f.branches.branches, "ana-alice")
	f.branches.mu.Unlock()
	f.srv.PublishBranch(ctx, name("ana-open"))
	f.srv.PublishBranch(ctx, name("ana-alice"))

	if _, err := f.srv.Ops.Start("base.refresh", operation.Target{Resource: "base", Name: "analytics"}, func(context.Context, func(string)) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Teammate-owned operation targeting disallowed base "analytics" via Bus:
	f.srv.Bus.PublishTo(operation.Audience{Owner: "user:alice@x.io"}, "operation.updated", operation.Operation{
		ID:     "op_ana",
		Kind:   "base.refresh",
		Owner:  "user:alice@x.io",
		Target: operation.Target{Resource: "base", Name: "analytics"},
	})
	f.srv.Bus.PublishTo(operation.Audience{Owner: "user:alice@x.io"}, "operation.updated", operation.Operation{
		ID:     "op_prod",
		Kind:   "base.refresh",
		Owner:  "user:alice@x.io",
		Target: operation.Target{Resource: "base", Name: "prod"},
	})
	f.srv.Bus.PublishTo(operation.Audience{Open: true}, "stop", nil)

	rd := bufio.NewReader(resp.Body)
	var lines []string
	var curTyp, curData string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early (got %v): %v", lines, err)
		}
		line = strings.TrimSpace(line)
		if tName, ok := strings.CutPrefix(line, "event: "); ok {
			curTyp = tName
		} else if d, ok := strings.CutPrefix(line, "data: "); ok {
			curData = d
		} else if line == "" && curTyp != "" {
			if curTyp == "stop" {
				break
			}
			lines = append(lines, curTyp+":"+curData)
			curTyp, curData = "", ""
		}
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "analytics") || strings.Contains(joined, "ana-open") || strings.Contains(joined, "ana-alice") || strings.Contains(joined, "op_ana") {
		t.Fatalf("restricted token leaked disallowed base events:\n%s", joined)
	}
	for _, want := range []string{`"name":"prod"`, `"name":"prod-open"`, `"name":"prod-alice"`, `"id":"op_prod"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing expected prod event %s in:\n%s", want, joined)
		}
	}
}
