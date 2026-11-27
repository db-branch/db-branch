package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

var mysql80 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.0.46"}

func (f *fakeBranches) ValidateUpgrade(_ context.Context, n domain.Name, to domain.RuntimeRef, checkOnly bool) (domain.Branch, error) {
	f.record(fmt.Sprintf("validate-upgrade %s %s check=%v", n, to, checkOnly))
	if f.upErr != nil {
		return domain.Branch{}, f.upErr
	}
	b, err := f.GetBranch(context.Background(), n)
	if err != nil {
		return b, err
	}
	return b, domain.CheckUpgradePath(b.Runtime, to)
}

func (f *fakeBranches) UpgradeBranch(_ context.Context, req dbbranch.UpgradeRequest, rep dbbranch.Reporter) (domain.UpgradeResult, error) {
	f.record(fmt.Sprintf("upgrade %s %s check=%v norollback=%v force=%v free=%d", req.Name, req.To, req.CheckOnly, req.NoRollback, req.Force, req.PoolFree))
	if rep != nil {
		rep("upgrading")
	}
	res := domain.UpgradeResult{Branch: req.Name.String(), From: mysql80, To: req.To, Status: domain.UpgradeUpgraded, CheckOnly: req.CheckOnly,
		PreCheck: []domain.Finding{{Level: domain.LevelWarning, Code: "native_password_users", Message: "1 account"}}, Log: []domain.Finding{}}
	if req.CheckOnly {
		res.Status = domain.UpgradeChecked
	}
	if f.upResultErr != nil {
		res.Status = domain.UpgradeRolledBack
	}
	return res, f.upResultErr
}

func (f *fakeBranches) ValidateTarget(_ context.Context, from, to domain.RuntimeRef) error {
	f.record(fmt.Sprintf("validate-target %s %s", from, to))
	return f.upErr
}

func (f *fakeBranches) CreateBranchUpgraded(ctx context.Context, r dbbranch.CreateBranchRequest, rep dbbranch.Reporter) (domain.Branch, *domain.UpgradeResult, error) {
	b, err := f.CreateBranch(ctx, r, rep)
	f.record(fmt.Sprintf("create-upgraded %s %s force=%v", r.Name, r.Runtime, r.Force))
	res := domain.UpgradeResult{Branch: r.Name.String(), To: r.Runtime, Status: domain.UpgradeUpgraded, PreCheck: []domain.Finding{}, Log: []domain.Finding{}}
	if f.upResultErr != nil {
		res.Status = domain.UpgradeRolledBack
		return b, &res, f.upResultErr
	}
	return b, &res, err
}

func upgradeFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql80, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s1"}}}
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Base: name("prod"), Runtime: mysql80, Status: domain.BranchReady}
	return f
}

func TestUpgradeBranch_受け付けてオペレーションに結果を残す(t *testing.T) {
	f := upgradeFixture(t)
	resp, body := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13", "no_rollback": true, "force": true})
	if resp.StatusCode != 202 || body["kind"] != "branch.upgrade" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	op := f.waitOp(t, body["id"].(string))
	if op.State != "succeeded" || op.Target.Name != "x" {
		t.Fatalf("op = %+v", op)
	}
	var res domain.UpgradeResult
	if err := json.Unmarshal(op.Result, &res); err != nil || res.Status != domain.UpgradeUpgraded || res.To != mysql84 || len(res.PreCheck) != 1 || res.PreCheck[0].Level != domain.LevelWarning {
		t.Errorf("result = %s (%v)", op.Result, err)
	}
	if got := f.branches.calls[len(f.branches.calls)-1]; got != "upgrade x mysql@8.4.13 check=false norollback=true force=true free=0" {
		t.Errorf("calls = %v", f.branches.calls)
	}
}

func TestUpgradeBranch_確認だけの依頼も同じ形で結果を返す(t *testing.T) {
	f := upgradeFixture(t)
	f.srv.PoolFree = func() uint64 { return 7 << 30 }
	_, body := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13", "check_only": true})
	op := f.waitOp(t, body["id"].(string))
	var res domain.UpgradeResult
	_ = json.Unmarshal(op.Result, &res)
	if res.Status != domain.UpgradeChecked || !res.CheckOnly {
		t.Errorf("result = %s", op.Result)
	}
	if got := f.branches.calls[len(f.branches.calls)-1]; !strings.HasSuffix(got, "check=true norollback=false force=false free=7516192768") {
		t.Errorf("calls = %v", f.branches.calls)
	}
}

func TestUpgradeBranch_失敗してもオペレーションに結果が残る(t *testing.T) {
	f := upgradeFixture(t)
	f.branches.upResultErr = errors.New("rolled back")
	_, body := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13"})
	op := f.waitOp(t, body["id"].(string))
	if op.State != "failed" || !strings.Contains(string(op.Result), `"rolled_back"`) {
		t.Errorf("op = %+v", op)
	}
}

func TestUpgradeBranch_受け付けられない依頼は同期で断る(t *testing.T) {
	f := upgradeFixture(t)
	for _, c := range []struct {
		label string
		body  any
		path  string
		want  int
		code  string
	}{
		{"bad runtime", map[string]any{"runtime": "latest"}, "/v1/branches/x/upgrade", 400, "invalid_request"},
		{"downgrade", map[string]any{"runtime": "mysql@8.0.30"}, "/v1/branches/x/upgrade", 422, "upgrade_not_allowed"},
		{"unknown branch", map[string]any{"runtime": "mysql@8.4.13"}, "/v1/branches/nope/upgrade", 404, "not_found"},
		{"no body", nil, "/v1/branches/x/upgrade", 400, "invalid_request"},
	} {
		resp, body := f.do(t, "POST", c.path, c.body)
		if resp.StatusCode != c.want || errCode(body) != c.code {
			t.Errorf("%s: %d %v", c.label, resp.StatusCode, body)
		}
	}
	f.branches.upErr = fmt.Errorf("%w: mysql@8.4.13 is not installed on node n1; install it with: db-branch runtime install mysql@8.4.13", domain.ErrRuntimeNotInstalled)
	resp, body := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13"})
	if resp.StatusCode != 404 || errCode(body) != "runtime_not_installed" || !strings.Contains(fmt.Sprint(body), "runtime install") {
		t.Errorf("not installed: %d %v", resp.StatusCode, body)
	}
	f.branches.upErr = domain.ErrClusterUpgrading
	if resp, body := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13"}); resp.StatusCode != 503 || errCode(body) != "cluster_upgrading" {
		t.Errorf("old nodes: %d %v", resp.StatusCode, body)
	}
}

func Test他のノードのブランチのアップグレードは担当ノードへ転送する(t *testing.T) {
	f, c := remoteFixture(t)
	f.do(t, "POST", "/v1/branches/there/upgrade", map[string]any{"runtime": "mysql@8.4.13", "check_only": true})
	if len(c.forwarded) != 1 || !strings.HasPrefix(c.forwarded[0], "n2 POST /v1/branches/there/upgrade via=node1 {") || !strings.Contains(c.forwarded[0], `"check_only":true`) {
		t.Errorf("forwarded = %v", c.forwarded)
	}
	if len(f.branches.calls) != 0 {
		t.Errorf("ran here: %v", f.branches.calls)
	}
}

func TestUpgradeBranch_所有者と管理者だけが使える(t *testing.T) {
	f := ownedFixture(t)
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql80, Status: domain.BaseReady}
	for _, n := range []string{"alices", "opened", "legacy"} {
		b := f.branches.branches[n]
		b.Runtime = mysql80
		f.branches.branches[n] = b
	}
	h := f.srv.Handler(false)
	body := map[string]any{"runtime": "mysql@8.4.13"}
	for _, c := range []struct {
		tok, path string
		want      int
	}{
		{"t-bob", "/v1/branches/alices/upgrade", 403},
		{"t-bob", "/v1/branches/opened/upgrade", 403}, // open lets anyone connect, not change the branch
		{"t-alice", "/v1/branches/alices/upgrade", 202},
		{"t-full", "/v1/branches/alices/upgrade", 202},
		{"t-bob", "/v1/branches/legacy/upgrade", 403},
		{"t-full", "/v1/branches/legacy/upgrade", 202},
		{"", "/v1/branches/opened/upgrade", 401},
	} {
		resp, b := send(t, h, "POST", c.path, body, as(c.tok))
		if resp.StatusCode != c.want {
			t.Errorf("%q %s: %d %v", c.tok, c.path, resp.StatusCode, b)
		}
	}
}

func TestCreateBranch_ランタイムを付けると作ってすぐ上げて結果を残す(t *testing.T) {
	f := upgradeFixture(t)
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "y", "base": "prod", "runtime": "mysql@8.4.13"})
	if resp.StatusCode != 202 || body["kind"] != "branch.create" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	op := f.waitOp(t, body["id"].(string))
	var res domain.UpgradeResult
	if err := json.Unmarshal(op.Result, &res); err != nil || op.State != "succeeded" || res.Status != domain.UpgradeUpgraded || res.To != mysql84 {
		t.Errorf("op = %+v", op)
	}
	if !contains(f.branches.calls, "validate-target mysql@8.0.46 mysql@8.4.13") || !contains(f.branches.calls, "create-upgraded y mysql@8.4.13 force=false") {
		t.Errorf("calls = %v", f.branches.calls)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestCreateBranch_ランタイム付きの失敗でも結果が残る(t *testing.T) {
	f := upgradeFixture(t)
	f.branches.upResultErr = errors.New("rolled back")
	_, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "y", "base": "prod", "runtime": "mysql@8.4.13"})
	op := f.waitOp(t, body["id"].(string))
	if op.State != "failed" || !strings.Contains(string(op.Result), "rolled_back") {
		t.Errorf("op = %+v", op)
	}
}

func TestCreateBranch_ランタイムの誤りは作る前に断る(t *testing.T) {
	f := upgradeFixture(t)
	for _, c := range []struct {
		label, runtime, base string
		want                 int
		code                 string
	}{
		{"malformed", "8.4", "prod", 400, "invalid_request"},
		{"downgrade", "mysql@8.0.30", "prod", 422, "upgrade_not_allowed"},
		{"lts skip", "mysql@9.1.0", "prod", 422, "upgrade_not_allowed"},
		{"no base", "mysql@8.4.13", "nope", 404, "not_found"},
	} {
		resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "y", "base": c.base, "runtime": c.runtime})
		if resp.StatusCode != c.want || errCode(body) != c.code {
			t.Errorf("%s: %d %v", c.label, resp.StatusCode, body)
		}
	}
	f.branches.upErr = domain.ErrRuntimeNotInstalled
	if resp, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "y", "base": "prod", "runtime": "mysql@8.4.13"}); resp.StatusCode != 404 || errCode(body) != "runtime_not_installed" {
		t.Errorf("not installed here: %d %v", resp.StatusCode, body)
	}
	if _, ok := f.branches.branches["y"]; ok {
		t.Error("a refused request created the branch")
	}
}

func TestCreateBranch_ログインなしではランタイムを選べない(t *testing.T) {
	f := ownedFixture(t)
	f.branches.bases["prod"] = domain.Base{Name: name("prod"), Runtime: mysql80, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{{Name: "s1", Public: true}}}
	h := f.srv.Handler(true)
	resp, body := send(t, h, "POST", "/v1/branches", map[string]any{"name": "anon-y", "base": "prod", "runtime": "mysql@8.4.13"}, func(r *http.Request) {
		r.Header.Set("X-Db-Branch-Forwarded", "n9")
		r.Header.Set("X-Db-Branch-Principal", `{"id":"anonymous","name":"anonymous","roles":["anonymous"]}`)
	})
	if resp.StatusCode != 403 {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestBranch_ベースのランタイムと違うことをJSONで返す(t *testing.T) {
	f := upgradeFixture(t)
	b := f.branches.branches["x"]
	b.Runtime = mysql84
	f.branches.branches["x"] = b
	_, body := f.do(t, "GET", "/v1/branches/x", nil)
	if body["runtime"] != "mysql@8.4.13" || body["base_runtime"] != "mysql@8.0.46" {
		t.Errorf("branch = %v", body)
	}
	delete(f.branches.bases, "prod")
	_, body = f.do(t, "GET", "/v1/branches/x", nil)
	if body["base_runtime"] != "mysql@8.4.13" {
		t.Errorf("without the base: %v", body)
	}
}

func TestRestoreCheckpoint_アップグレードに失敗して残ったブランチからも戻せる(t *testing.T) {
	f := checkpointFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchFailed, Error: domain.UpgradeFailedPrefix + "boom"}
	resp, body := f.do(t, "POST", "/v1/branches/x/checkpoints/c1/restore", nil)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchFailed, Error: "disk full"}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/checkpoints/c1/restore", nil); resp.StatusCode != 409 {
		t.Errorf("another failure: %d", resp.StatusCode)
	}
}

func TestListCheckpoints_取得時のランタイムを返す(t *testing.T) {
	f := checkpointFixture(t)
	f.branches.cps[0].Runtime = "mysql@8.0.46"
	_, body := f.do(t, "GET", "/v1/branches/x/checkpoints", nil)
	first := body["items"].([]any)[0].(map[string]any)
	if first["runtime"] != "mysql@8.0.46" {
		t.Errorf("checkpoint = %v", first)
	}
}

// denyUpgrade lets a role do everything but upgrade.
type denyUpgrade struct{}

func (denyUpgrade) Authorize(_ context.Context, _ ext.Principal, action string, _ ext.Resource) error {
	if action == "branch.upgrade" {
		return fmt.Errorf("%w: no", ext.ErrForbidden)
	}
	return nil
}

func TestCreateBranch_アップグレード権限のない役割はランタイムを選べない(t *testing.T) {
	f := upgradeFixture(t)
	f.srv.Ext = ext.Module{Authorizer: denyUpgrade{}}
	resp, _ := f.do(t, "POST", "/v1/branches", map[string]any{"name": "y", "base": "prod", "runtime": "mysql@8.4.13"})
	if resp.StatusCode != 403 {
		t.Errorf("status %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches", map[string]any{"name": "y", "base": "prod"}); resp.StatusCode != 202 {
		t.Errorf("plain create: %d", resp.StatusCode)
	}
}

func TestUpgradeBranch_空き容量は不明ならゼロで渡す(t *testing.T) {
	f := upgradeFixture(t)
	f.srv.PoolFree = func() uint64 { return math.MaxUint64 }
	_, body := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13"})
	f.waitOp(t, body["id"].(string))
	if got := f.branches.calls[len(f.branches.calls)-1]; !strings.HasSuffix(got, "free=0") {
		t.Errorf("calls = %v", f.branches.calls)
	}
}

func TestUpgradeBranch_オペレーションを記録できなければ失敗を返す(t *testing.T) {
	f := upgradeFixture(t)
	f.srv.Ops = &operation.Manager{Store: &refusingOps{}, Clock: time.Now}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13"}); resp.StatusCode < 500 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestMCP_アップグレードの確認と実行と結果(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := upgradeFixture(t)
	bx := f.branches.branches["x"]
	bx.Owner, bx.OwnerName = "token:dev", "dev"
	f.branches.branches["x"] = bx
	f.srv.TokenAuth = &fakeTokens{}
	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callText(t, s, "check_upgrade", map[string]any{"branch": "x", "runtime": "mysql@8.4.13"})
	if isErr || !strings.Contains(out, `"checked"`) || !strings.Contains(out, "native_password_users") {
		t.Errorf("check: %v %s", isErr, out)
	}
	out, isErr = callText(t, s, "upgrade_branch", map[string]any{"branch": "x", "runtime": "mysql@8.4.13", "force": true})
	if isErr || !strings.Contains(out, `"upgraded"`) {
		t.Errorf("upgrade: %v %s", isErr, out)
	}
	f.branches.upResultErr = errors.New("rolled back")
	out, isErr = callText(t, s, "upgrade_branch", map[string]any{"branch": "x", "runtime": "mysql@8.4.13"})
	if !isErr || !strings.Contains(out, "rolled_back") || !strings.Contains(out, `"error": "rolled back"`) {
		t.Errorf("failed upgrade: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "upgrade_branch", map[string]any{"branch": "x", "runtime": "mysql@8.0.30"}); !isErr || !strings.Contains(out, "downgrade") {
		t.Errorf("refused: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "create_branch", map[string]any{"name": "y", "base": "prod", "runtime": "mysql@8.4.13"}); isErr || !strings.Contains(out, `"y"`) {
		f.branches.upResultErr = nil
		t.Logf("create with runtime (expected failure by the fake): %v %s", isErr, out)
	}
	if !contains(f.branches.calls, "validate-target mysql@8.0.46 mysql@8.4.13") {
		t.Errorf("create_branch did not pass the runtime: %v", f.branches.calls)
	}
}

func TestBranch_統計更新中と警告をJSONで返す(t *testing.T) {
	f := upgradeFixture(t)
	b := f.branches.branches["x"]
	b.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	b.StatsUpdating = true
	b.StatsWarning = "analyze timed out"
	f.branches.branches["x"] = b
	_, body := f.do(t, "GET", "/v1/branches/x", nil)
	if body["stats_updating"] != true || body["stats_warning"] != "analyze timed out" {
		t.Errorf("branch = %v", body)
	}
}
