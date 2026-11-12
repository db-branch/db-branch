package httpapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

func TestModuleAPI_モジュールが自分のAPIを利用者として呼べる(t *testing.T) {
	f := newFixture(t)
	api := &moduleAPI{}
	sink := &recordingSink{}
	f.srv.ExtHandlers = map[string]ext.Handler{"diff": api}
	f.srv.Ext = ext.Module{AuditSink: sink, Authorizer: viewerReadsOnly{}}
	call := f.srv.ModuleAPI("diff")
	alice := ext.Principal{ID: "t-viewer", Name: "alice", Roles: []string{"viewer"}}
	resp, err := call.Call(context.Background(), ext.Request{Method: "GET", Path: "/stuff", RawQuery: "a=1", Principal: alice})
	if err != nil || resp.Status != 201 || !strings.Contains(string(resp.Body), `"who":"t-viewer"`) || resp.ContentType != "application/json" {
		t.Fatalf("%+v %v", resp, err)
	}
	if g := api.got[0]; g.RawQuery != "a=1" || g.Principal.ID != "t-viewer" {
		t.Errorf("request = %+v", g)
	}
	// The module's own authorization applies to the principal it names.
	resp, _ = call.Call(context.Background(), ext.Request{Method: "POST", Path: "/stuff", Principal: alice})
	if resp.Status != 403 {
		t.Errorf("a viewer's write: %d", resp.Status)
	}
	// An empty method is a GET; a bad method fails before anything runs.
	if resp, _ := call.Call(context.Background(), ext.Request{Path: "/x", Principal: alice}); resp.Status != 201 {
		t.Errorf("default method: %d", resp.Status)
	}
	if _, err := call.Call(context.Background(), ext.Request{Method: "BAD METHOD", Path: "/x"}); err == nil {
		t.Error("a bad method should fail")
	}
	// Writes are audited like a client's.
	admin := ext.Principal{ID: "admin", Name: "admin", Roles: []string{"admin"}}
	if resp, _ := call.Call(context.Background(), ext.Request{Method: "POST", Path: "/stuff", Body: []byte(`{"a":1}`), Principal: admin}); resp.Status != 201 {
		t.Errorf("admin write: %d", resp.Status)
	}
	if got := sink.actions(); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "ext.diff.write:") {
		t.Errorf("audit = %v", got)
	}
	if _, err := f.srv.ModuleAPI("nothing").Call(context.Background(), ext.Request{Path: "/x"}); err == nil {
		t.Error("a module without an API should fail")
	}
}

func TestModuleAPI_ブランチを持つノードへ転送する(t *testing.T) {
	f := newFixture(t)
	f.srv.ExtHandlers = map[string]ext.Handler{"diff": &moduleAPI{}}
	fc := &fakeCluster{}
	f.srv.Cluster = fc
	f.branches.branches["there"] = domain.Branch{Name: name("there"), Runtime: mysql84, Status: domain.BranchReady, Owner: "alice", Node: "n2"}
	admin := ext.Principal{ID: "admin", Name: "admin", Roles: []string{"admin"}}
	resp, err := f.srv.ModuleAPI("diff").Call(context.Background(), ext.Request{Method: "GET", Path: "/branches/there/info", Principal: admin})
	if err != nil || resp.Status != 202 || string(resp.Body) != `{"id":"op_remote"}` {
		t.Fatalf("%+v %v", resp, err)
	}
	if len(fc.forwarded) != 1 || !strings.HasPrefix(fc.forwarded[0], "n2 GET /v1/ext/diff/branches/there/info via=node1") || !strings.Contains(fc.principals[0], `"admin"`) {
		t.Errorf("forwarded = %v %v", fc.forwarded, fc.principals)
	}
}
