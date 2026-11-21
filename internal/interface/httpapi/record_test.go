package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/pkg/ext"
)

// recorder is a module that records for owners of its choosing.
type recorder struct {
	err  error
	seen []ext.ServerBranch
}

func (r *recorder) ServerOptions(_ context.Context, b ext.ServerBranch) (ext.ServerOptions, error) {
	r.seen = append(r.seen, b)
	return ext.ServerOptions{}, r.err
}

func TestCreateBranch_記録つきの作成はモジュールが受け付けたときだけ通す(t *testing.T) {
	f := newFixture(t)
	rec := &recorder{}
	f.srv.Ext = ext.Module{ServerOptioner: rec}
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod", "record": true})
	if resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	if len(rec.seen) != 1 || !rec.seen[0].Record || rec.seen[0].Engine != "mysql" || rec.seen[0].Name != "x" {
		t.Errorf("asked = %+v", rec.seen)
	}
	if !f.branches.lastCreate.Record {
		t.Error("the request should carry record")
	}
	_, got := f.do(t, "GET", "/v1/branches/x", nil)
	if got["record"] != true {
		t.Errorf("branch = %v", got)
	}
}

func TestCreateBranch_記録できない場合は作らず理由を返す(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod", "record": true})
	if resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(body), "history module") {
		t.Errorf("no module: %d %v", resp.StatusCode, body)
	}
	rec := &recorder{err: fmt.Errorf("%w: no licence", ext.ErrForbidden)}
	f.srv.Ext = ext.Module{ServerOptioner: rec}
	if resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod", "record": true}); resp.StatusCode != 403 || !strings.Contains(fmt.Sprint(body), "no licence") {
		t.Errorf("unlicensed: %d %v", resp.StatusCode, body)
	}
	rec.err = errors.New("PostgreSQL cannot be recorded yet")
	if resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod", "record": true}); resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(body), "PostgreSQL") {
		t.Errorf("engine: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "nope", "record": true}); resp.StatusCode != 404 {
		t.Errorf("unknown base: %d", resp.StatusCode)
	}
	if len(f.branches.calls) != 0 {
		t.Errorf("created anyway: %v", f.branches.calls)
	}
}

func TestCreateBranch_ログインなしの呼び出しは記録できない(t *testing.T) {
	f := anonFixture(t)
	f.srv.Ext = ext.Module{ServerOptioner: &recorder{}}
	resp, body := send(t, f.srv.Handler(false), "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod", "record": true}, nil)
	if resp.StatusCode != 403 || !strings.Contains(fmt.Sprint(body), "login") {
		t.Errorf("anonymous: %d %v", resp.StatusCode, body)
	}
}

func TestCapabilities_記録できるのはモジュールが入っていてライセンスがあるときだけ(t *testing.T) {
	f := newFixture(t)
	if _, body := f.do(t, "GET", "/v1/capabilities", nil); body["record"] != false {
		t.Errorf("no module: %v", body["record"])
	}
	rec := &recorder{}
	f.srv.Ext = ext.Module{ServerOptioner: rec}
	if _, body := f.do(t, "GET", "/v1/capabilities", nil); body["record"] != true {
		t.Errorf("module: %v", body["record"])
	}
	rec.err = ext.ErrForbidden
	if _, body := f.do(t, "GET", "/v1/capabilities", nil); body["record"] != false {
		t.Errorf("unlicensed: %v", body["record"])
	}
}

func TestExt_ブランチ配下のパスはそのブランチのノードで答える(t *testing.T) {
	f, c := remoteFixture(t)
	f.srv.ExtHandlers = map[string]ext.Handler{"history": &moduleAPI{}}
	f.do(t, "GET", "/v1/ext/history/branches/there/events", nil)
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 GET /v1/ext/history/branches/there/events") {
		t.Errorf("forwarded = %v", c.forwarded)
	}
	for _, path := range []string{"/v1/ext/history/branches/nope/events", "/v1/ext/history/branches/BAD_NAME/events", "/v1/ext/history/status", "/v1/ext/history/branches"} {
		c.forwarded = nil
		f.do(t, "GET", path, nil)
		if len(c.forwarded) != 0 {
			t.Errorf("%s forwarded: %v", path, c.forwarded)
		}
	}
	f.srv.Branches = func() (httpapi.Branches, error) { return nil, errors.New("not initialized") }
	f.do(t, "GET", "/v1/ext/history/branches/there/events", nil)
	if len(c.forwarded) != 0 {
		t.Errorf("no service: %v", c.forwarded)
	}
}

var _ = domain.Branch{}

func TestPatchBranch_作成後に記録の有無を変える手段は無い(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady, Record: true, CreatedAt: t0}
	for _, body := range []map[string]any{{"record": false}, {"ttl": "1h", "record": false}} {
		if resp, _ := f.do(t, "PATCH", "/v1/branches/x", body); resp.StatusCode != 400 {
			t.Errorf("%v: status %d, want 400", body, resp.StatusCode)
		}
	}
	for _, m := range []string{"PUT", "POST"} {
		if resp, _ := f.do(t, m, "/v1/branches/x", map[string]any{"record": false}); resp.StatusCode < 400 {
			t.Errorf("%s changed a branch: %d", m, resp.StatusCode)
		}
	}
	if _, got := f.do(t, "GET", "/v1/branches/x", nil); got["record"] != true {
		t.Errorf("branch = %v", got)
	}
}

func TestCreateBranch_記録を宣言しないエンジンのベースは409で作らない(t *testing.T) {
	f := newFixture(t)
	rec := &recorder{}
	f.srv.Ext = ext.Module{ServerOptioner: rec}
	f.branches.bases["pgbase"] = domain.Base{Name: name("pgbase"), Runtime: pg18, Status: domain.BaseReady}
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "pgbase", "record": true})
	if resp.StatusCode != 409 || fmt.Sprint(body["error"].(map[string]any)["code"]) != "feature_not_supported" || !strings.Contains(fmt.Sprint(body), "history_record") {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if len(rec.seen) != 0 || len(f.branches.calls) != 0 {
		t.Errorf("the module was asked or the branch created: %v %v", rec.seen, f.branches.calls)
	}
}
