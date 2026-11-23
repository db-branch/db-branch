package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

func (f *fakeBranches) ValidateMerge(_ context.Context, req dbbranch.MergeBranchRequest) (domain.Branch, error) {
	f.record(fmt.Sprintf("validate-merge %s line=%v cp=%q dry=%v", req.Name, req.SourceLine, req.Checkpoint, req.DryRun))
	if f.mergeErr != nil {
		return domain.Branch{}, f.mergeErr
	}
	b, err := f.GetBranch(context.Background(), req.Name)
	if err != nil {
		return b, err
	}
	if err := b.CanMerge(); err != nil {
		return b, err
	}
	if (req.SourceLine == nil && req.Checkpoint == "") || (req.SourceLine != nil && req.Checkpoint != "") {
		return b, domain.ErrInvalidMergeSource
	}
	return b, nil
}

func (f *fakeBranches) MergeBranch(_ context.Context, req dbbranch.MergeBranchRequest, rep dbbranch.Reporter) (domain.MergeResult, error) {
	f.record(fmt.Sprintf("merge %s line=%v cp=%q dry=%v tables=%v ddl=%v trig=%v",
		req.Name, req.SourceLine, req.Checkpoint, req.DryRun, req.Tables, req.AllowDDL, req.IgnoreTriggers))
	f.mu.Lock()
	f.lastMerge = req
	res := f.mergeResult
	rerr := f.mergeResultErr
	f.mu.Unlock()
	if rep != nil {
		rep("diffing")
	}
	if res.Branch == "" {
		res.Branch = req.Name.String()
		res.TargetLine = 1
		if req.SourceLine != nil {
			res.SourceLine = *req.SourceLine
		}
		res.SourceCheckpoint = req.Checkpoint
		res.DryRun = req.DryRun
		if req.DryRun {
			res.Status = domain.MergePreview
		} else {
			res.Status = domain.MergeMerged
			res.Checkpoint = "before-merge-test"
		}
		res.Inserted = 2
	}
	if rerr != nil && res.Error == "" {
		res.Error = rerr.Error()
	}
	return res, rerr
}

func mergeFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql84, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s1"}}}
	f.branches.branches["x"] = domain.Branch{
		Name: name("x"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40001, Node: "node1", Owner: "admin", OwnerName: "admin", CreatedAt: t0,
	}
	return f
}

func TestMergeBranch_受け付けてオペレーションにMergeResultを残す(t *testing.T) {
	f := mergeFixture(t)
	resp, body := f.do(t, "POST", "/v1/branches/x/merge", map[string]any{
		"source_line": 0, "tables": []string{"shop.orders"}, "allow_ddl": true, "ignore_triggers": true,
	})
	if resp.StatusCode != 202 || body["kind"] != "branch.merge" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	op := f.waitOp(t, body["id"].(string))
	if op.State != "succeeded" || op.Target.Name != "x" {
		t.Fatalf("op = %+v", op)
	}
	var res domain.MergeResult
	if err := json.Unmarshal(op.Result, &res); err != nil || res.Status != domain.MergeMerged || res.Inserted != 2 {
		t.Errorf("result = %s (%v)", op.Result, err)
	}
	if f.branches.lastMerge.SourceLine == nil || *f.branches.lastMerge.SourceLine != 0 || !f.branches.lastMerge.AllowDDL || !f.branches.lastMerge.IgnoreTriggers {
		t.Errorf("lastMerge = %+v", f.branches.lastMerge)
	}
}

func TestMergeBranch_ドライランと競合失敗でも結果を残す(t *testing.T) {
	f := mergeFixture(t)
	_, body := f.do(t, "POST", "/v1/branches/x/merge", map[string]any{"checkpoint": "cp1", "dry_run": true})
	op := f.waitOp(t, body["id"].(string))
	var res domain.MergeResult
	_ = json.Unmarshal(op.Result, &res)
	if op.State != "succeeded" || res.Status != domain.MergePreview || !res.DryRun {
		t.Errorf("dry-run = %+v %+v", op, res)
	}

	f.branches.mergeResult = domain.MergeResult{
		Branch: "x", Status: domain.MergeConflicted, Code: "merge_conflict", TargetLine: 1, SourceLine: 0,
		TotalConflicts: 1,
		Conflicts:      []domain.MergeConflict{{Type: domain.ConflictUpdateUpdate, Database: "shop", Table: "orders", Key: map[string]any{"id": 1}}},
	}
	f.branches.mergeResultErr = fmt.Errorf("%w: 1 conflict(s) detected", domain.ErrMergeConflict)
	_, body = f.do(t, "POST", "/v1/branches/x/merge", map[string]any{"source_line": 0})
	op = f.waitOp(t, body["id"].(string))
	_ = json.Unmarshal(op.Result, &res)
	if op.State != "failed" || res.Status != domain.MergeConflicted || len(res.Conflicts) != 1 || !strings.Contains(op.Error, "conflict") {
		t.Errorf("conflict = %+v res=%+v", op, res)
	}
}

func TestMergeBranch_不正な依頼や状態や機能非対応は同期で拒否する(t *testing.T) {
	f := mergeFixture(t)
	// Unknown field -> 400 invalid_request
	if resp, body := f.do(t, "POST", "/v1/branches/x/merge", map[string]any{"source_line": 0, "bogus": 1}); resp.StatusCode != 400 || errCode(body) != "invalid_request" {
		t.Errorf("unknown field: %d %v", resp.StatusCode, body)
	}
	// Missing source_line & checkpoint -> 400 invalid_request
	if resp, body := f.do(t, "POST", "/v1/branches/x/merge", map[string]any{}); resp.StatusCode != 400 || errCode(body) != "invalid_request" {
		t.Errorf("empty body: %d %v", resp.StatusCode, body)
	}
	// Missing branch -> 404
	if resp, _ := f.do(t, "POST", "/v1/branches/missing/merge", map[string]any{"source_line": 0}); resp.StatusCode != 404 {
		t.Errorf("missing branch: %d", resp.StatusCode)
	}
	// ErrNoCapacity -> 503
	f.branches.mergeErr = domain.ErrNoCapacity
	if resp, body := f.do(t, "POST", "/v1/branches/x/merge", map[string]any{"source_line": 0}); resp.StatusCode != 503 || errCode(body) != "no_capacity" {
		t.Errorf("no capacity: %d %v", resp.StatusCode, body)
	}
	f.branches.mergeErr = nil
	// Feature disabled -> 409 feature_not_supported
	mysqlWith(t, func(c *domain.RuntimeCaps) { c.BranchMerge = false })
	if resp, body := f.do(t, "POST", "/v1/branches/x/merge", map[string]any{"source_line": 0}); resp.StatusCode != 409 || errCode(body) != "feature_not_supported" {
		t.Errorf("feature disabled: %d %v", resp.StatusCode, body)
	}
}

func TestMergeBranch_混在クラスタは転送前に503を返し同一プロトコルなら転送し認可を強制する(t *testing.T) {
	f := mergeFixture(t)
	f.branches.branches["remote"] = domain.Branch{
		Name: name("remote"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40002, Node: "node2", Owner: "sso:alice", OwnerName: "alice", CreatedAt: t0,
	}
	forwarded := false
	f.srv.Cluster = &forkForwardCluster{
		fakeCluster: &fakeCluster{status: httpapi.ClusterStatus{Protocol: domain.ProtocolBranchMerge - 1}},
		onForward: func(string) http.Handler {
			forwarded = true
			return http.NotFoundHandler()
		},
	}
	resp, body := f.do(t, "POST", "/v1/branches/remote/merge", map[string]any{"source_line": 0})
	if resp.StatusCode != 503 || errCode(body) != "cluster_upgrading" || forwarded {
		t.Fatalf("mixed cluster: %d %v forwarded=%v", resp.StatusCode, body, forwarded)
	}

	// Same protocol -> forwards to node2.
	f.srv.Cluster = &forkForwardCluster{
		fakeCluster: &fakeCluster{status: httpapi.ClusterStatus{Protocol: domain.ProtocolBranchMerge}},
		onForward: func(string) http.Handler {
			forwarded = true
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"op_fwd","kind":"branch.merge"}`))
			})
		},
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/remote/merge", map[string]any{"source_line": 0}); resp.StatusCode != 202 || !forwarded {
		t.Fatalf("forward: status=%d forwarded=%v", resp.StatusCode, forwarded)
	}

	// Authorization: anon -> 401, bob -> 403, alice -> 202, legacy non-admin -> 403.
	f.srv.Cluster = nil
	f.srv.TokenAuth = &multiTokens{principals: map[string]ext.Principal{
		"t-alice": {ID: "sso:alice", Name: "alice", Roles: []string{"scope:branches"}},
		"t-bob":   {ID: "sso:bob", Name: "bob", Roles: []string{"scope:branches"}},
	}}
	f.branches.branches["alices"] = domain.Branch{
		Name: name("alices"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40003, Node: "node1", Owner: "sso:alice", OwnerName: "alice", Access: domain.AccessOpen, CreatedAt: t0,
	}
	f.branches.branches["legacy"] = domain.Branch{
		Name: name("legacy"), Base: name("prod"), Snapshot: "s1", Runtime: mysql84,
		Status: domain.BranchReady, Port: 40004, Node: "node1", CreatedAt: t0,
	}
	h := f.srv.Handler(false)
	if r, _ := send(t, h, "POST", "/v1/branches/alices/merge", map[string]any{"source_line": 0}, nil); r.StatusCode != 401 {
		t.Errorf("anon: %d", r.StatusCode)
	}
	if r, _ := send(t, h, "POST", "/v1/branches/alices/merge", map[string]any{"source_line": 0}, as("t-bob")); r.StatusCode != 403 {
		t.Errorf("bob on alice branch: %d", r.StatusCode)
	}
	if r, _ := send(t, h, "POST", "/v1/branches/legacy/merge", map[string]any{"source_line": 0}, as("t-bob")); r.StatusCode != 403 {
		t.Errorf("bob on legacy branch: %d", r.StatusCode)
	}
	if r, b := send(t, h, "POST", "/v1/branches/alices/merge", map[string]any{"source_line": 0}, as("t-alice")); r.StatusCode != 202 {
		t.Errorf("alice on own branch: %d %v", r.StatusCode, b)
	} else {
		f.waitOp(t, b["id"].(string))
	}
}

func TestMergeBranch_オペレーション記録失敗は500台(t *testing.T) {
	f := mergeFixture(t)
	f.srv.Ops = &operation.Manager{Store: &refusingOps{}, Clock: time.Now}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/merge", map[string]any{"source_line": 0}); resp.StatusCode < 500 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestMCP_merge_lineでマージ実行とドライランと競合とエラーを扱う(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := mergeFixture(t)
	bx := f.branches.branches["x"]
	bx.Owner, bx.OwnerName = "token:dev", "dev"
	f.branches.branches["x"] = bx
	f.srv.TokenAuth = &fakeTokens{}
	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	out, isErr := callText(t, s, "merge_line", map[string]any{"branch": "x", "source_line": 0, "dry_run": true})
	if isErr || !strings.Contains(out, `"dry_run"`) {
		t.Errorf("dry_run: %v %s", isErr, out)
	}
	out, isErr = callText(t, s, "merge_line", map[string]any{"branch": "x", "checkpoint": "cp1"})
	if isErr || !strings.Contains(out, `"merged"`) {
		t.Errorf("merge: %v %s", isErr, out)
	}
	f.branches.mergeResult = domain.MergeResult{
		Branch: "x", Status: domain.MergeConflicted, Code: "merge_conflict",
		TotalConflicts: 1,
		Conflicts:      []domain.MergeConflict{{Type: domain.ConflictInsertInsert, Database: "shop", Table: "t"}},
	}
	f.branches.mergeResultErr = fmt.Errorf("%w: 1 conflict(s)", domain.ErrMergeConflict)
	out, isErr = callText(t, s, "merge_line", map[string]any{"branch": "x", "source_line": 0})
	if !isErr || !strings.Contains(out, `"conflicted"`) || !strings.Contains(out, `"insert_insert"`) {
		t.Errorf("conflict: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "merge_line", map[string]any{"branch": "x"}); !isErr || !strings.Contains(out, "invalid") {
		t.Errorf("invalid args: %v %s", isErr, out)
	}
}
