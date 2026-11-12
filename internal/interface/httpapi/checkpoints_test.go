package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func (f *fakeBranches) ListCheckpoints(_ context.Context, n domain.Name) ([]domain.Checkpoint, error) {
	f.record("checkpoints " + n.String())
	return f.cps, f.cpErr
}

func (f *fakeBranches) CheckpointGraph(_ context.Context, n domain.Name) (domain.Graph, error) {
	f.record("graph " + n.String())
	if f.cpErr != nil {
		return domain.Graph{}, f.cpErr
	}
	g := f.graph
	g.Checkpoints = f.cps
	return g, nil
}

func (f *fakeBranches) CreateCheckpoint(_ context.Context, n domain.Name, cp string) (domain.Checkpoint, error) {
	f.record(fmt.Sprintf("checkpoint %s %q", n, cp))
	if f.cpErr != nil {
		return domain.Checkpoint{}, f.cpErr
	}
	if cp == "" {
		cp = "20261109-120000"
	}
	return domain.Checkpoint{Name: cp, CreatedAt: t0, Size: 4096, Current: true}, nil
}

func (f *fakeBranches) RestoreCheckpoint(_ context.Context, n domain.Name, cp string, rep dbbranch.Reporter) (domain.Branch, error) {
	f.record(fmt.Sprintf("restore %s %s", n, cp))
	if rep != nil {
		rep("cloning")
	}
	return domain.Branch{}, f.cpErr
}

func (f *fakeBranches) DeleteCheckpoint(_ context.Context, n domain.Name, cp string) error {
	f.record(fmt.Sprintf("delete-checkpoint %s %s", n, cp))
	return f.cpErr
}

func checkpointFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady}
	f.branches.cps = []domain.Checkpoint{
		{Name: "c1", CreatedAt: t0, Size: 8192, Line: 0, Current: true},
		{Name: "before-restore-20261109-120100", CreatedAt: t0.Add(time.Minute), Size: 4096, Line: 0, Auto: true},
	}
	return f
}

func TestListCheckpoints_全てのチェックポイントを返す(t *testing.T) {
	f := checkpointFixture(t)
	resp, body := f.do(t, "GET", "/v1/branches/x/checkpoints", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	items := body["items"].([]any)
	first := items[0].(map[string]any)
	if len(items) != 2 || first["name"] != "c1" || first["created_at"] != "2026-11-09T12:00:00Z" || first["size"] != float64(8192) ||
		first["line"] != float64(0) || first["auto"] != false || first["current"] != true {
		t.Errorf("items = %v", items)
	}
	if items[1].(map[string]any)["auto"] != true {
		t.Errorf("auto = %v", items[1])
	}
	if resp, _ := f.do(t, "GET", "/v1/branches/nope/checkpoints", nil); resp.StatusCode != 404 {
		t.Errorf("unknown branch: %d", resp.StatusCode)
	}
	f.branches.cps = nil
	if _, body := f.do(t, "GET", "/v1/branches/x/checkpoints", nil); body["items"] == nil {
		t.Errorf("no checkpoints must be an empty list: %v", body)
	}
	f.branches.cpErr = fmt.Errorf("%w: no zfs", domain.ErrConflict)
	if resp, _ := f.do(t, "GET", "/v1/branches/x/checkpoints", nil); resp.StatusCode != 409 {
		t.Errorf("unsupported: %d", resp.StatusCode)
	}
}

func TestCreateCheckpoint_名前ありでもなしでも201で返す(t *testing.T) {
	f := checkpointFixture(t)
	resp, body := f.do(t, "POST", "/v1/branches/x/checkpoints", map[string]any{"name": "before-migration"})
	if resp.StatusCode != 201 || body["name"] != "before-migration" || body["size"] != float64(4096) {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	resp, body = f.do(t, "POST", "/v1/branches/x/checkpoints", nil)
	if resp.StatusCode != 201 || body["name"] != "20261109-120000" {
		t.Fatalf("no body: %d %v", resp.StatusCode, body)
	}
	if got := f.branches.calls; len(got) != 2 || got[0] != `checkpoint x "before-migration"` || got[1] != `checkpoint x ""` {
		t.Errorf("calls = %v", got)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/checkpoints", map[string]any{"nme": "typo"}); resp.StatusCode != 400 {
		t.Errorf("unknown field: %d", resp.StatusCode)
	}
}

func TestCreateCheckpoint_上限や状態の衝突は409で伝える(t *testing.T) {
	f := checkpointFixture(t)
	f.branches.cpErr = fmt.Errorf("%w: branch x has 20 checkpoints (limit 20); delete one first", domain.ErrConflict)
	resp, body := f.do(t, "POST", "/v1/branches/x/checkpoints", map[string]any{"name": "c"})
	if resp.StatusCode != 409 || errCode(body) != "conflict" || !strings.Contains(fmt.Sprint(body), "limit 20") {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
	f.branches.cpErr = domain.ErrAlreadyExists
	if resp, _ := f.do(t, "POST", "/v1/branches/x/checkpoints", map[string]any{"name": "c1"}); resp.StatusCode != 409 {
		t.Errorf("duplicate: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/nope/checkpoints", nil); resp.StatusCode != 404 {
		t.Errorf("unknown branch: %d", resp.StatusCode)
	}
}

func TestRestoreCheckpoint_受け付けてオペレーションで戻す(t *testing.T) {
	f := checkpointFixture(t)
	resp, body := f.do(t, "POST", "/v1/branches/x/checkpoints/c1/restore", nil)
	if resp.StatusCode != 202 || body["kind"] != "branch.restore" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	op := f.waitOp(t, body["id"].(string))
	if op.State != "succeeded" || op.Target.Name != "x" {
		t.Errorf("op = %+v", op)
	}
	if got := f.branches.calls[len(f.branches.calls)-1]; got != "restore x c1" {
		t.Errorf("calls = %v", f.branches.calls)
	}
}

func TestRestoreCheckpoint_受け付けられない場合は同期で断る(t *testing.T) {
	f := checkpointFixture(t)
	f.branches.branches["busy"] = domain.Branch{Name: name("busy"), Runtime: mysql84, Status: domain.BranchResetting}
	if resp, body := f.do(t, "POST", "/v1/branches/busy/checkpoints/c1/restore", nil); resp.StatusCode != 409 || errCode(body) != "conflict" {
		t.Errorf("busy: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/checkpoints/nope/restore", nil); resp.StatusCode != 404 {
		t.Errorf("unknown checkpoint: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/checkpoints/Bad%20Name/restore", nil); resp.StatusCode != 400 {
		t.Errorf("invalid name: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/nope/checkpoints/c1/restore", nil); resp.StatusCode != 404 {
		t.Errorf("unknown branch: %d", resp.StatusCode)
	}
	f.branches.cpErr = domain.ErrConflict
	if resp, _ := f.do(t, "POST", "/v1/branches/x/checkpoints/c1/restore", nil); resp.StatusCode != 409 {
		t.Errorf("listing fails: %d", resp.StatusCode)
	}
}

func TestDeleteCheckpoint_消して204を返す(t *testing.T) {
	f := checkpointFixture(t)
	if resp, _ := f.do(t, "DELETE", "/v1/branches/x/checkpoints/c1", nil); resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := f.branches.calls; got[len(got)-1] != "delete-checkpoint x c1" {
		t.Errorf("calls = %v", got)
	}
	f.branches.cpErr = fmt.Errorf("checkpoint c9: %w", domain.ErrNotFound)
	if resp, _ := f.do(t, "DELETE", "/v1/branches/x/checkpoints/c9", nil); resp.StatusCode != 404 {
		t.Errorf("missing: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/branches/nope/checkpoints/c1", nil); resp.StatusCode != 404 {
		t.Errorf("unknown branch: %d", resp.StatusCode)
	}
}

func Test他のノードのブランチのチェックポイント操作は担当ノードへ転送する(t *testing.T) {
	f, c := remoteFixture(t)
	for _, req := range [][2]string{
		{"GET", "/v1/branches/there/checkpoints"},
		{"POST", "/v1/branches/there/checkpoints"},
		{"POST", "/v1/branches/there/checkpoints/c1/restore"},
		{"DELETE", "/v1/branches/there/checkpoints/c1"},
	} {
		c.forwarded = nil
		f.do(t, req[0], req[1], nil)
		if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 "+req[0]+" "+req[1]+" via=node1") {
			t.Errorf("%v: %v", req, c.forwarded)
		}
	}
	if len(f.branches.calls) != 0 {
		t.Errorf("ran here: %v", f.branches.calls)
	}
}

func TestCheckpoint_所有者と管理者だけが操作できる(t *testing.T) {
	f := ownedFixture(t)
	f.branches.cps = []domain.Checkpoint{{Name: "c1", CreatedAt: t0}}
	h := f.srv.Handler(false)
	for _, c := range []struct {
		tok, method, path string
		want              int
	}{
		{"t-bob", "GET", "/v1/branches/alices/checkpoints", 403},
		{"t-bob", "POST", "/v1/branches/alices/checkpoints", 403},
		{"t-bob", "POST", "/v1/branches/alices/checkpoints/c1/restore", 403},
		{"t-bob", "DELETE", "/v1/branches/alices/checkpoints/c1", 403},
		// An open branch lets anyone connect, not change its history.
		{"t-bob", "GET", "/v1/branches/opened/checkpoints", 403},
		{"t-bob", "POST", "/v1/branches/opened/checkpoints", 403},
		{"t-alice", "GET", "/v1/branches/alices/checkpoints", 200},
		{"t-alice", "POST", "/v1/branches/alices/checkpoints", 201},
		{"t-alice", "POST", "/v1/branches/alices/checkpoints/c1/restore", 202},
		{"t-alice", "DELETE", "/v1/branches/alices/checkpoints/c1", 204},
		{"t-full", "POST", "/v1/branches/alices/checkpoints", 201},
		{"t-bob", "POST", "/v1/branches/legacy/checkpoints", 201},
		// Without credentials: nothing, not even on an open branch (401: log in).
		{"", "GET", "/v1/branches/opened/checkpoints", 401},
		{"", "POST", "/v1/branches/opened/checkpoints", 401},
		{"", "POST", "/v1/branches/opened/checkpoints/c1/restore", 401},
		{"", "DELETE", "/v1/branches/opened/checkpoints/c1", 401},
	} {
		resp, body := send(t, h, c.method, c.path, nil, as(c.tok))
		if resp.StatusCode != c.want {
			t.Errorf("%q %s %s: %d %v", c.tok, c.method, c.path, resp.StatusCode, body)
		}
	}
}

func TestCheckpoint_アクション名は操作ごとに分かれる(t *testing.T) {
	f := ownedFixture(t)
	sink := &recordingSink{}
	f.srv.Ext.AuditSink = sink
	h := f.srv.Handler(false)
	send(t, h, "POST", "/v1/branches/alices/checkpoints", nil, as("t-alice"))
	send(t, h, "POST", "/v1/branches/alices/checkpoints/c1/restore", nil, as("t-alice"))
	send(t, h, "DELETE", "/v1/branches/alices/checkpoints/c1", nil, as("t-alice"))
	send(t, h, "POST", "/v1/branches/alices/checkpoints", nil, as("t-bob"))
	got := strings.Join(sink.actions(), " ")
	for _, want := range []string{"branch.checkpoint:", "branch.restore:", "branch.checkpoint_delete:", "denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit %q lacks %q", got, want)
		}
	}
}

func TestCheckpoint_ログインなしの主体は開いたブランチでも断る(t *testing.T) {
	f := ownedFixture(t)
	h := f.srv.Handler(true)
	for _, c := range [][2]string{{"GET", "/v1/branches/opened/checkpoints"}, {"POST", "/v1/branches/opened/checkpoints"},
		{"POST", "/v1/branches/opened/checkpoints/c1/restore"}, {"DELETE", "/v1/branches/opened/checkpoints/c1"}} {
		resp, body := send(t, h, c[0], c[1], nil, func(r *http.Request) {
			r.Header.Set("X-Db-Branch-Forwarded", "n9")
			r.Header.Set("X-Db-Branch-Principal", `{"id":"anonymous","name":"anonymous","roles":["anonymous"]}`)
		})
		if resp.StatusCode != 403 {
			t.Errorf("%v: %d %v", c, resp.StatusCode, body)
		}
	}
}

func TestListCheckpoints_線の構造と現在地も返す(t *testing.T) {
	f := checkpointFixture(t)
	zero := 0
	f.branches.graph = domain.Graph{Live: 1, Head: "c1", Lines: []domain.LineView{{Line: 0}, {Line: 1, Live: true, Parent: &zero, From: "c1"}}}
	resp, body := f.do(t, "GET", "/v1/branches/x/checkpoints", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	lines := body["lines"].([]any)
	l0, l1 := lines[0].(map[string]any), lines[1].(map[string]any)
	if len(lines) != 2 || l0["parent"] != nil || l0["live"] != false || l0["from"] != "" ||
		l1["line"] != float64(1) || l1["live"] != true || l1["parent"] != float64(0) || l1["from"] != "c1" {
		t.Errorf("lines = %v", lines)
	}
	if body["live"] != float64(1) || body["head"] != "c1" {
		t.Errorf("live/head = %v %v", body["live"], body["head"])
	}
	f.branches.graph = domain.Graph{}
	if _, body := f.do(t, "GET", "/v1/branches/x/checkpoints", nil); body["head"] != nil || len(body["lines"].([]any)) != 0 {
		t.Errorf("no history: %v", body)
	}
}

func TestBranch_チェックポイントの可否をノードの能力から返す(t *testing.T) {
	f := checkpointFixture(t)
	_, body := f.do(t, "GET", "/v1/branches/x", nil)
	if body["checkpoints"] != true {
		t.Errorf("checkpoints = %v, want true", body["checkpoints"])
	}
	f.branches.noCheckpoints = true
	_, body = f.do(t, "GET", "/v1/branches/x", nil)
	if body["checkpoints"] != false {
		t.Errorf("checkpoints = %v, want false on module storage", body["checkpoints"])
	}
	_, list := f.do(t, "GET", "/v1/branches", nil)
	if list["items"].([]any)[0].(map[string]any)["checkpoints"] != false {
		t.Errorf("list = %v", list)
	}
}

func TestCheckpoints_ストレージが対応しないと専用コードの409を返す(t *testing.T) {
	f := checkpointFixture(t)
	f.branches.cpErr = fmt.Errorf("%w: %w", domain.ErrCheckpointsUnavailable, domain.ErrConflict)
	resp, body := f.do(t, "POST", "/v1/branches/x/checkpoints", nil)
	if resp.StatusCode != 409 || errCode(body) != "checkpoints_unavailable" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestCreateCheckpoint_ブランチの使用量がプール空きの半分を超えたら警告を付ける(t *testing.T) {
	f := checkpointFixture(t)
	f.srv.PoolFree = func() uint64 { return 1 << 20 } // the branch uses 1 MiB (live and history)
	resp, body := f.do(t, "POST", "/v1/branches/x/checkpoints", map[string]any{"name": "c9"})
	if resp.StatusCode != 201 || body["name"] != "c9" || !strings.Contains(fmt.Sprint(body["warning"]), "half of the pool's free space") {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
	f.srv.PoolFree = func() uint64 { return 10 << 20 }
	if _, body = f.do(t, "POST", "/v1/branches/x/checkpoints", map[string]any{"name": "c10"}); body["warning"] != nil {
		t.Errorf("plenty of space: %v", body)
	}
	f.srv.PoolFree = nil
	if _, body = f.do(t, "POST", "/v1/branches/x/checkpoints", map[string]any{"name": "c11"}); body["warning"] != nil {
		t.Errorf("pool size unknown: %v", body)
	}
}
