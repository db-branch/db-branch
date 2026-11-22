package httpapi_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

// sseTypes opens /v1/events as tok and returns a function that reads event
// types until the one named stop arrives (or time runs out).
func sseTypes(t *testing.T, srv *httptest.Server, tok, lastID string) func(stop string) []string {
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
