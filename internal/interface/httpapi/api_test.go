package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

var (
	t0      = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	mysql84 = domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}
)

func name(s string) domain.Name { n, _ := domain.ParseName(s); return n }

// fakeBranches keeps branches and bases in maps and finishes work instantly.
type fakeBranches struct {
	mu       sync.Mutex
	branches map[string]domain.Branch
	bases    map[string]domain.Base
	lastMask dbbranch.MaskRequest
	fail     error
	calls    []string
	// place is where PlaceBranch puts new branches ("" = this node); placeErr fails it.
	place     string
	placeErr  error
	refreshes map[string]domain.BaseRefresh
	// cps are the checkpoints every branch has; cpErr fails checkpoint calls.
	cps   []domain.Checkpoint
	cpErr error
	// noCheckpoints makes the node report no checkpoint support.
	noCheckpoints bool
}

func (f *fakeBranches) CheckpointsAvailable() bool { return !f.noCheckpoints }

func (f *fakeBranches) PlaceRecreate(_ context.Context, n domain.Name) (string, error) {
	if f.placeErr != nil {
		return "", f.placeErr
	}
	if f.place != "" {
		return f.place, nil
	}
	return "node1", nil
}

func (f *fakeBranches) RecreateBranch(_ context.Context, n domain.Name, _ dbbranch.Reporter) (domain.Branch, error) {
	f.record("recreate " + n.String())
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.branches[n.String()]
	b.Status, b.Node = domain.BranchReady, "node1"
	f.branches[n.String()] = b
	return b, f.fail
}

func (f *fakeBranches) PlaceBranch(_ context.Context, r dbbranch.CreateBranchRequest) (string, error) {
	if f.placeErr != nil {
		return "", f.placeErr
	}
	if _, err := f.GetBase(context.Background(), r.Base); err != nil {
		return "", err
	}
	if r.Node != "" {
		return r.Node, nil
	}
	if f.place != "" {
		return f.place, nil
	}
	return "node1", nil
}

func (f *fakeBranches) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeBranches) CreateBranch(_ context.Context, r dbbranch.CreateBranchRequest, rep dbbranch.Reporter) (domain.Branch, error) {
	f.record(fmt.Sprintf("create %s base=%s snap=%s ttl=%s sql=%q node=%s", r.Name, r.Base, r.Snapshot, r.TTL, r.InitSQL, r.Node))
	if rep != nil {
		rep("cloning")
	}
	if f.fail != nil {
		return domain.Branch{}, f.fail
	}
	b := domain.Branch{Name: r.Name, Base: r.Base, Runtime: mysql84, Status: domain.BranchReady, Port: 40001, User: "branch", Password: "pw", CreatedAt: t0,
		Owner: r.Owner, Access: r.Access}
	if r.TTL > 0 {
		exp := t0.Add(r.TTL)
		b.ExpiresAt = &exp
	}
	f.mu.Lock()
	f.branches[r.Name.String()] = b
	f.mu.Unlock()
	return b, nil
}

func (f *fakeBranches) ResetBranch(_ context.Context, n domain.Name, _ dbbranch.Reporter) (domain.Branch, error) {
	f.record("reset " + n.String())
	return f.GetBranch(context.Background(), n)
}

func (f *fakeBranches) DeleteBranch(_ context.Context, n domain.Name, _ dbbranch.Reporter) error {
	f.record("delete " + n.String())
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.branches, n.String())
	return nil
}

func (f *fakeBranches) ExtendBranch(_ context.Context, n domain.Name, ttl time.Duration) (domain.Branch, error) {
	f.record(fmt.Sprintf("extend %s %s", n, ttl))
	b, err := f.GetBranch(context.Background(), n)
	if err != nil {
		return b, err
	}
	if err := b.Extend(t0, ttl); err != nil {
		return b, err
	}
	return b, nil
}

func (f *fakeBranches) GetBranch(_ context.Context, n domain.Name) (domain.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.branches[n.String()]
	if !ok {
		return b, fmt.Errorf("branch %s: %w", n, domain.ErrNotFound)
	}
	return b, nil
}

func (f *fakeBranches) ListBranches(context.Context) ([]domain.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Branch
	for _, b := range f.branches {
		out = append(out, b)
	}
	return out, f.fail
}

func (f *fakeBranches) BranchStorage(context.Context, domain.Name) (domain.DatasetInfo, error) {
	return domain.DatasetInfo{Used: 1 << 20, Referenced: 500 << 20}, nil
}

func (f *fakeBranches) CreateBase(_ context.Context, r dbbranch.CreateBaseRequest, _ dbbranch.Reporter) (domain.Base, error) {
	f.record(fmt.Sprintf("create-base %s %s dump=%s", r.Name, r.Runtime, r.Dump))
	b := domain.Base{Name: r.Name, Runtime: r.Runtime, Status: domain.BaseReady, Description: r.Description, ReplicaNodes: r.ReplicaNodes}
	f.mu.Lock()
	f.bases[r.Name.String()] = b
	f.mu.Unlock()
	return b, nil
}

func (f *fakeBranches) MaskBase(_ context.Context, req dbbranch.MaskRequest, _ dbbranch.Reporter) (domain.BaseSnapshot, error) {
	f.record("mask " + req.Target.String())
	f.mu.Lock()
	f.lastMask = req
	f.mu.Unlock()
	return domain.BaseSnapshot{Name: "s3"}, nil
}

func (f *fakeBranches) SnapshotBase(_ context.Context, n domain.Name, _ dbbranch.Reporter) (domain.BaseSnapshot, error) {
	f.record("snapshot " + n.String())
	return domain.BaseSnapshot{Name: "s2"}, nil
}

func (f *fakeBranches) GetBase(_ context.Context, n domain.Name) (domain.Base, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bases[n.String()]
	if !ok {
		return b, fmt.Errorf("base %s: %w", n, domain.ErrNotFound)
	}
	return cloneBase(b), nil
}

// cloneBase copies what a caller could change in place (the snapshots), so a
// test that edits the stored base while a handler reads it does not race.
func cloneBase(b domain.Base) domain.Base {
	b.Snapshots = slices.Clone(b.Snapshots)
	return b
}

func (f *fakeBranches) ListBases(context.Context) ([]domain.Base, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Base
	for _, b := range f.bases {
		out = append(out, b)
	}
	return out, nil
}

func (f *fakeBranches) DeleteBase(_ context.Context, n domain.Name) error {
	f.record("delete-base " + n.String())
	if f.fail == nil {
		f.mu.Lock()
		delete(f.bases, n.String())
		f.mu.Unlock()
	}
	return f.fail
}

func (f *fakeBranches) StartBase(_ context.Context, n domain.Name) error {
	f.record("start-base " + n.String())
	return nil
}

func (f *fakeBranches) DescribeBase(_ context.Context, n domain.Name, d string) (domain.Base, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bases[n.String()]
	if !ok {
		return b, domain.ErrNotFound
	}
	if f.fail != nil {
		return b, f.fail
	}
	b.Description = d
	f.bases[n.String()] = b
	return b, nil
}

func (f *fakeBranches) LoadBase(_ context.Context, n domain.Name, dump string, _ dbbranch.Reporter) error {
	f.record("load-base " + n.String() + " " + dump)
	return f.fail
}

func (f *fakeBranches) ExecBase(_ context.Context, n domain.Name, sql string) error {
	f.record("exec-base " + n.String() + " " + sql)
	return f.fail
}

func (f *fakeBranches) DeleteSnapshot(_ context.Context, n domain.Name, snap string) error {
	f.record("delete-snapshot " + n.String() + "@" + snap)
	return f.fail
}

func (f *fakeBranches) SetSnapshotPublic(ctx context.Context, n domain.Name, snap string, public bool) (domain.Base, error) {
	f.record(fmt.Sprintf("public %s@%s %v", n, snap, public))
	b, err := f.GetBase(ctx, n)
	if err != nil {
		return b, err
	}
	if f.fail != nil {
		return b, f.fail
	}
	b.SetPublic(snap, public)
	f.mu.Lock()
	f.bases[n.String()] = b
	f.mu.Unlock()
	return b, nil
}

func (f *fakeBranches) StopBase(_ context.Context, n domain.Name) error {
	f.record("stop-base " + n.String())
	return nil
}

type fakeRuntimes struct {
	installed  []domain.RuntimeRef
	listErr    error
	removeErr  error
	installErr error
}

func (r *fakeRuntimes) Install(_ context.Context, ref domain.RuntimeRef) error {
	if r.installErr != nil {
		return r.installErr
	}
	r.installed = append(r.installed, ref)
	return nil
}
func (r *fakeRuntimes) Remove(context.Context, domain.RuntimeRef) error { return r.removeErr }
func (r *fakeRuntimes) List(context.Context) ([]domain.RuntimeRef, error) {
	return []domain.RuntimeRef{mysql84}, r.listErr
}
func (r *fakeRuntimes) Available(e domain.Engine) []domain.RuntimeRef {
	return []domain.RuntimeRef{{Engine: e, Version: "1.0"}}
}

type memCreds struct{ c *auth.Credentials }

func (m *memCreds) Load() (auth.Credentials, error) {
	if m.c == nil {
		return auth.Credentials{}, auth.ErrNoCredentials
	}
	return *m.c, nil
}
func (m *memCreds) Save(c auth.Credentials) error { m.c = &c; return nil }

type sha struct{}

func (sha) Hash(p string, salt []byte) []byte { s := sha256.Sum256(append(salt, p...)); return s[:] }

type fixture struct {
	srv      *httpapi.Server
	branches *fakeBranches
	runtimes *fakeRuntimes
	ops      *operation.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	bus := operation.NewBus(100)
	a := &auth.Service{Store: &memCreds{}, Hasher: sha{}, Clock: time.Now}
	if err := a.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		branches: &fakeBranches{
			branches: map[string]domain.Branch{},
			bases: map[string]domain.Base{"prod": {Name: name("prod"), Runtime: mysql84, Status: domain.BaseReady,
				Snapshots: []domain.BaseSnapshot{{Name: "s1", CreatedAt: t0}}}},
		},
		runtimes: &fakeRuntimes{removeErr: domain.ErrRuntimeNotInstalled},
		ops:      &operation.Manager{Bus: bus, Clock: time.Now},
	}
	f.srv = &httpapi.Server{
		Branches: func() (httpapi.Branches, error) { return f.branches, nil },
		Runtimes: f.runtimes,
		Ops:      f.ops,
		Bus:      bus,
		Auth:     a,
		Host:     "192.0.2.10",
		Node:     "node1",
		Version:  "test",
	}
	return f
}

// do sends a request to the local (socket) handler, which needs no login.
func (f *fixture) do(t *testing.T, method, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	return send(t, f.srv.Handler(true), method, path, body, nil)
}

func send(t *testing.T, h http.Handler, method, path string, body any, mutate func(*http.Request)) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Result(), out
}

func (f *fixture) waitOp(t *testing.T, id string) operation.Operation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, _ := f.ops.Get(id)
		if op.State != operation.Running {
			return op
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation still running")
	return operation.Operation{}
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestCreateBranch_受け付けてオペレーションを返し裏で作る(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do(t, "POST", "/v1/branches", map[string]any{
		"name": "feature-x", "base": "prod", "init_sql": "SELECT 1", "ttl": "72h",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if resp.Header.Get("Location") != "/v1/operations/"+id || body["kind"] != "branch.create" {
		t.Errorf("headers %v body %v", resp.Header, body)
	}
	op := f.waitOp(t, id)
	if op.State != operation.Succeeded {
		t.Errorf("op = %+v", op)
	}
	if f.branches.calls[0] != `create feature-x base=prod snap= ttl=72h0m0s sql="SELECT 1" node=node1` {
		t.Errorf("calls = %q", f.branches.calls)
	}
}

func TestCreateBranch_受け付ける前に分かる誤りはすぐ返す(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["taken"] = domain.Branch{Name: name("taken"), Status: domain.BranchReady}
	cases := []struct {
		body   map[string]any
		status int
		code   string
	}{
		{map[string]any{"name": "Bad Name", "base": "prod"}, 400, "invalid_request"},
		{map[string]any{"name": "ok", "base": "prod", "ttl": "soon"}, 400, "invalid_request"},
		{map[string]any{"name": "ok", "base": "prod", "ttl": "0s"}, 400, "invalid_request"},
		{map[string]any{"name": "taken", "base": "prod"}, 409, "already_exists"},
		{map[string]any{"name": "ok", "base": "nope"}, 404, "not_found"},
	}
	for _, c := range cases {
		resp, body := f.do(t, "POST", "/v1/branches", c.body)
		if resp.StatusCode != c.status || errCode(body) != c.code {
			t.Errorf("%v: %d %v", c.body, resp.StatusCode, body)
		}
	}
	resp, _ := send(t, f.srv.Handler(true), "POST", "/v1/branches", nil, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader("{"))
	})
	if resp.StatusCode != 400 {
		t.Errorf("broken json: %d", resp.StatusCode)
	}
}

func TestGetBranch_コンソールが描くための情報を返す(t *testing.T) {
	f := newFixture(t)
	exp := t0.Add(72 * time.Hour)
	f.branches.branches["feature-x"] = domain.Branch{Name: name("feature-x"), Base: name("prod"), Snapshot: "s1",
		Runtime: mysql84, Status: domain.BranchReady, Port: 40001, User: "branch", Password: "secret", CreatedAt: t0, ExpiresAt: &exp}

	resp, body := f.do(t, "GET", "/v1/branches/feature-x", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if body["status"] != "ready" || body["runtime"] != "mysql@8.4.13" || body["node"] != "node1" ||
		body["expires_at"] != "2026-11-12T12:00:00Z" || body["created_at"] != "2026-11-09T12:00:00Z" {
		t.Errorf("body = %v", body)
	}
	conn := body["connection"].(map[string]any)
	if conn["host"] != "192.0.2.10" || conn["port"] != float64(40001) || conn["user"] != "branch" { // the fake keeps the old shared name
		t.Errorf("connection = %v", conn)
	}
	storage := body["storage"].(map[string]any)
	if storage["used"] != float64(1<<20) || storage["referenced"] != float64(500<<20) {
		t.Errorf("storage = %v", storage)
	}
	b, _ := json.Marshal(body)
	if strings.Contains(string(b), "secret") {
		t.Error("the password must only be returned by the dsn endpoint")
	}

	_, dsn := f.do(t, "GET", "/v1/branches/feature-x/dsn", nil)
	if dsn["dsn"] != "mysql://branch:secret@192.0.2.10:40001/" {
		t.Errorf("dsn = %v", dsn)
	}
	resp, body = f.do(t, "GET", "/v1/branches/nope", nil)
	if resp.StatusCode != 404 || errCode(body) != "not_found" {
		t.Errorf("missing: %d %v", resp.StatusCode, body)
	}
}

func TestListBranches_一覧を返す(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["b"] = domain.Branch{Name: name("b"), Runtime: mysql84, Status: domain.BranchReady}
	f.branches.branches["a"] = domain.Branch{Name: name("a"), Runtime: mysql84, Status: domain.BranchReady}

	_, body := f.do(t, "GET", "/v1/branches", nil)
	items := body["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["name"] != "a" {
		t.Errorf("items = %v", items)
	}
}

func TestResetとDelete_状態を確かめてから受け付ける(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady}
	f.branches.branches["busy"] = domain.Branch{Name: name("busy"), Runtime: mysql84, Status: domain.BranchProvisioning}

	resp, body := f.do(t, "POST", "/v1/branches/x/reset", nil)
	if resp.StatusCode != 202 {
		t.Fatalf("reset: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	resp, body = f.do(t, "POST", "/v1/branches/busy/reset", nil)
	if resp.StatusCode != 409 || errCode(body) != "conflict" {
		t.Errorf("busy reset: %d %v", resp.StatusCode, body)
	}
	resp, body = f.do(t, "DELETE", "/v1/branches/busy", nil)
	if resp.StatusCode != 409 {
		t.Errorf("busy delete: %d %v", resp.StatusCode, body)
	}
	resp, body = f.do(t, "DELETE", "/v1/branches/x", nil)
	if resp.StatusCode != 202 {
		t.Fatalf("delete: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	if _, err := f.branches.GetBranch(context.Background(), name("x")); err == nil {
		t.Error("branch should be deleted")
	}
	resp, _ = f.do(t, "POST", "/v1/branches/nope/reset", nil)
	if resp.StatusCode != 404 {
		t.Errorf("missing reset: %d", resp.StatusCode)
	}
}

func TestPatchBranch_TTLを延長する(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady}

	resp, body := f.do(t, "PATCH", "/v1/branches/x", map[string]any{"ttl": "24h"})
	if resp.StatusCode != 200 || body["expires_at"] != "2026-11-10T12:00:00Z" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
	resp, body = f.do(t, "PATCH", "/v1/branches/x", map[string]any{"ttl": "-1h"})
	if resp.StatusCode != 400 {
		t.Errorf("negative: %d %v", resp.StatusCode, body)
	}
}

func TestBases_作成とスナップショットと起動停止と削除(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do(t, "POST", "/v1/bases", map[string]any{"name": "shop", "runtime": "mysql@8.4.13", "dump": "/srv/shop.sql.gz"})
	if resp.StatusCode != 202 {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	resp, body = f.do(t, "POST", "/v1/bases/shop/snapshots", nil)
	if resp.StatusCode != 202 {
		t.Fatalf("snapshot: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	for _, p := range []string{"/v1/bases/shop/start", "/v1/bases/shop/stop"} {
		if resp, _ := f.do(t, "POST", p, nil); resp.StatusCode != 204 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := f.do(t, "DELETE", "/v1/bases/shop", nil); resp.StatusCode != 204 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	want := []string{"create-base shop mysql@8.4.13 dump=/srv/shop.sql.gz", "snapshot shop", "start-base shop", "stop-base shop", "delete-base shop"}
	if strings.Join(f.branches.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q", f.branches.calls)
	}
	_, body = f.do(t, "GET", "/v1/bases", nil)
	if len(body["items"].([]any)) != 1 {
		t.Errorf("bases = %v", body)
	}
	_, body = f.do(t, "GET", "/v1/bases/prod", nil)
	if body["latest_snapshot"] != "s1" {
		t.Errorf("base = %v", body)
	}
	resp, body = f.do(t, "POST", "/v1/bases", map[string]any{"name": "prod", "runtime": "mysql@8.4.13"})
	if resp.StatusCode != 409 {
		t.Errorf("duplicate: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.do(t, "POST", "/v1/bases", map[string]any{"name": "x", "runtime": "mysql@latest"})
	if resp.StatusCode != 400 {
		t.Errorf("bad runtime: %d", resp.StatusCode)
	}
}

func TestDeleteBase_使われていれば409(t *testing.T) {
	f := newFixture(t)
	f.branches.fail = fmt.Errorf("%w: base prod is used by branch x", domain.ErrInUse)
	resp, body := f.do(t, "DELETE", "/v1/bases/prod", nil)
	if resp.StatusCode != 409 || errCode(body) != "in_use" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestRuntimes_一覧と取得可能な版とインストール(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "GET", "/v1/runtimes", nil)
	if items := body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["runtime"] != "mysql@8.4.13" {
		t.Errorf("installed = %v", body)
	}
	_, body = f.do(t, "GET", "/v1/runtimes/available", nil)
	if len(body["items"].([]any)) != 2 {
		t.Errorf("available = %v", body)
	}
	resp, body := f.do(t, "POST", "/v1/runtimes", map[string]any{"runtime": "mysql@8.0.46"})
	if resp.StatusCode != 202 {
		t.Fatalf("install: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	if len(f.runtimes.installed) != 1 || f.runtimes.installed[0].Version != "8.0.46" {
		t.Errorf("installed %v", f.runtimes.installed)
	}
	resp, body = f.do(t, "DELETE", "/v1/runtimes/mysql/8.0.46", nil)
	if resp.StatusCode != 404 || errCode(body) != "runtime_not_installed" {
		t.Errorf("remove: %d %v", resp.StatusCode, body)
	}
}

func TestOperations_一覧と取得(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod"})
	id := body["id"].(string)
	f.waitOp(t, id)

	_, got := f.do(t, "GET", "/v1/operations/"+id, nil)
	if got["state"] != "succeeded" {
		t.Errorf("op = %v", got)
	}
	_, list := f.do(t, "GET", "/v1/operations", nil)
	if len(list["items"].([]any)) != 1 {
		t.Errorf("list = %v", list)
	}
	if resp, _ := f.do(t, "GET", "/v1/operations/op_nope", nil); resp.StatusCode != 404 {
		t.Errorf("missing: %d", resp.StatusCode)
	}
}

func Test失敗したオペレーションは理由を持つ(t *testing.T) {
	f := newFixture(t)
	f.branches.fail = errors.New("clone failed")
	_, body := f.do(t, "POST", "/v1/branches", map[string]any{"name": "x", "base": "prod"})
	op := f.waitOp(t, body["id"].(string))
	if op.State != operation.Failed || op.Error != "clone failed" {
		t.Errorf("op = %+v", op)
	}
}

func Test初期化前はブランチ操作が503になる(t *testing.T) {
	f := newFixture(t)
	f.srv.Branches = func() (httpapi.Branches, error) { return nil, errors.New("not initialized: run `db-branch init`") }
	resp, body := f.do(t, "GET", "/v1/branches", nil)
	if resp.StatusCode != 503 || errCode(body) != "not_initialized" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestCapabilities_OSS版は有償モジュールなし(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "GET", "/v1/capabilities", nil)
	if mods := body["modules"].([]any); len(mods) != 0 || body["version"] != "test" {
		t.Errorf("body = %v", body)
	}
}

func Test知らないパスやメソッドは404か405(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/nope", nil); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/branches", nil); resp.StatusCode != 405 {
		t.Errorf("wrong method: %d", resp.StatusCode)
	}
}

func TestBases_マスクの実行を受け付けて誰がしたかを渡す(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "POST", "/v1/bases", map[string]any{"name": "dev", "runtime": "mysql@8.4.13"})
	f.waitOp(t, body["id"].(string))
	resp, body := f.do(t, "POST", "/v1/bases/dev/mask", map[string]any{"source": "prod", "masker": "greenmask", "rules": "x: y"})
	if resp.StatusCode != 202 {
		t.Fatalf("mask: %d %v", resp.StatusCode, body)
	}
	f.waitOp(t, body["id"].(string))
	if got := f.branches.lastMask; got.Source.String() != "prod" || got.Target.String() != "dev" || got.Masker != "greenmask" || got.Rules != "x: y" || got.By == "" {
		t.Errorf("request %+v", got)
	}
	for _, bad := range []map[string]any{{"masker": "greenmask"}, {"source": "Bad", "masker": "m"}, {"source": "prod"}} {
		if resp, _ := f.do(t, "POST", "/v1/bases/dev/mask", bad); resp.StatusCode != 400 {
			t.Errorf("%v: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := f.do(t, "POST", "/v1/bases/dev/mask", "nope"); resp.StatusCode != 400 {
		t.Errorf("bad body: %d", resp.StatusCode)
	}
}

func TestBases_マスクの記録をスナップショットに出す(t *testing.T) {
	f := newFixture(t)
	f.branches.bases["dev"] = domain.Base{Name: name("dev"), Runtime: mysql84, Status: domain.BaseReady, Snapshots: []domain.BaseSnapshot{
		{Name: "s1"}, {Name: "s2", Mask: &domain.MaskInfo{Source: name("prod"), SourceSnapshot: "p1", Masker: "greenmask", RulesSHA256: "ab", By: "ann"}}}}
	_, body := f.do(t, "GET", "/v1/bases/dev", nil)
	snaps := body["snapshots"].([]any)
	if snaps[0].(map[string]any)["mask"] != nil {
		t.Errorf("plain snapshot %v", snaps[0])
	}
	m, _ := snaps[1].(map[string]any)["mask"].(map[string]any)
	if m["source"] != "prod" || m["masker"] != "greenmask" || m["rules_sha256"] != "ab" {
		t.Errorf("masked snapshot %v", snaps[1])
	}
}

func (f *fakeBranches) GetRefresh(_ context.Context, n domain.Name) (domain.BaseRefresh, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.refreshes[n.String()]
	if !ok {
		return r, fmt.Errorf("refresh of base %s: %w", n, domain.ErrNotFound)
	}
	return r, nil
}

func (f *fakeBranches) SetRefresh(ctx context.Context, n domain.Name, req dbbranch.RefreshRequest) (domain.BaseRefresh, error) {
	b, err := f.GetBase(ctx, n)
	if err != nil {
		return domain.BaseRefresh{}, err
	}
	r := domain.BaseRefresh{Base: n, Source: req.Source, Every: req.Every, UpdatedAt: t0}
	if err := r.Validate(b.Runtime.Engine); err != nil {
		return r, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshes == nil {
		f.refreshes = map[string]domain.BaseRefresh{}
	}
	f.refreshes[n.String()] = r
	return r, nil
}

func (f *fakeBranches) ClearRefresh(_ context.Context, n domain.Name) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.refreshes[n.String()]; !ok {
		return domain.ErrNotFound
	}
	delete(f.refreshes, n.String())
	return nil
}

func (f *fakeBranches) RefreshBase(_ context.Context, n domain.Name, _ dbbranch.Reporter) error {
	f.record("refresh-base " + n.String())
	return f.fail
}

func (f *fakeBranches) SetReplicaNodes(_ context.Context, n domain.Name, nodes *[]string) (domain.Base, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bases[n.String()]
	if !ok {
		return b, domain.ErrNotFound
	}
	if f.fail != nil {
		return b, f.fail
	}
	b.ReplicaNodes = nodes
	f.bases[n.String()] = b
	return b, nil
}

func (f *fakeBranches) SetAnonymous(_ context.Context, n domain.Name, l domain.AnonymousLimits) (domain.Base, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bases[n.String()]
	if !ok {
		return b, domain.ErrNotFound
	}
	if f.fail != nil {
		return b, f.fail
	}
	if err := l.Validate(); err != nil {
		return b, err
	}
	b.Anonymous = l
	f.bases[n.String()] = b
	return b, nil
}
