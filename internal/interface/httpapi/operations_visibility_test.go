package httpapi_test

import (
	"context"
	"slices"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

// startAs makes bob (or whoever tok names) start an operation and returns its ID.
func startAs(t *testing.T, f *fixture, tok, branch string) string {
	t.Helper()
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/branches", map[string]any{"name": branch, "base": "prod"}, as(tok))
	if resp.StatusCode != 202 {
		t.Fatalf("create %s: %d %v", branch, resp.StatusCode, body)
	}
	return body["id"].(string)
}

func TestHTTPAPI_他人のOperationIDを知っていても取得できない(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(false)
	id := startAs(t, f, "t-bob", "bobs")

	for _, c := range []struct {
		who  string
		tok  string
		want int
	}{
		{"anonymous", "", 404},
		{"alice", "t-alice", 404},
		{"bob", "t-bob", 200},
		{"admin", "t-full", 200},
	} {
		if resp, body := send(t, h, "GET", "/v1/operations/"+id, nil, as(c.tok)); resp.StatusCode != c.want {
			t.Errorf("%s: %d %v, want %d", c.who, resp.StatusCode, body, c.want)
		}
	}
}

func operationIDs(t *testing.T, f *fixture, tok string) []string {
	t.Helper()
	resp, body := send(t, f.srv.Handler(false), "GET", "/v1/operations", nil, as(tok))
	if resp.StatusCode != 200 {
		t.Fatalf("list as %s: %d %v", tok, resp.StatusCode, body)
	}
	ids := []string{}
	for _, it := range body["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

func TestHTTPAPI_Operations一覧には自分が始めた操作だけが載る(t *testing.T) {
	f := ownedFixture(t)
	bobOp := startAs(t, f, "t-bob", "bobs")
	aliceOp := startAs(t, f, "t-alice", "alices2")

	for _, c := range []struct {
		tok  string
		want []string
	}{
		{"t-bob", []string{bobOp}},
		{"t-alice", []string{aliceOp}},
	} {
		if got := operationIDs(t, f, c.tok); !slices.Equal(got, c.want) {
			t.Errorf("%s sees %v, want %v", c.tok, got, c.want)
		}
	}
	if got := operationIDs(t, f, "t-full"); len(got) != 2 {
		t.Errorf("admin sees %v, want both", got)
	}
}

func TestHTTPAPI_チームの人は互いのOperationを取得できる(t *testing.T) {
	f := ownedFixture(t)
	f.srv.Ext = ext.Module{BranchPolicy: &teamPolicy{}}
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql84, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s0"}}}
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/branches", map[string]any{"name": "bobs", "base": "prod", "ttl": "4h"}, as("t-bob"))
	if resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	id := body["id"].(string)
	if resp, _ := send(t, f.srv.Handler(false), "GET", "/v1/operations/"+id, nil, as("t-alice")); resp.StatusCode != 200 {
		t.Errorf("teammate: %d", resp.StatusCode)
	}
}

func TestHTTPAPI_システムが始めたOperationは管理者だけが見られる(t *testing.T) {
	f := ownedFixture(t)
	op, err := f.srv.Ops.Start("runtime.install", operation.Target{Resource: "runtime", Name: "mysql@8.4"},
		func(context.Context, func(string)) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	h := f.srv.Handler(false)
	if resp, _ := send(t, h, "GET", "/v1/operations/"+op.ID, nil, as("t-bob")); resp.StatusCode != 404 {
		t.Errorf("bob: %d", resp.StatusCode)
	}
	if resp, _ := send(t, h, "GET", "/v1/operations/"+op.ID, nil, as("t-full")); resp.StatusCode != 200 {
		t.Errorf("admin: %d", resp.StatusCode)
	}
}
