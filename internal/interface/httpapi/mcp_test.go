package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/pkg/ext"
)

// fakeTokens accepts "t-full" and "t-branches".
type fakeTokens struct {
	created []string
	err     error
}

func (f *fakeTokens) Authenticate(_ context.Context, c ext.Credentials) (ext.Principal, error) {
	switch c.Token {
	case "t-full":
		return ext.Principal{ID: "token:full", Roles: []string{"scope:full"}}, nil
	case "t-branches":
		return ext.Principal{ID: "token:dev", Name: "dev", Roles: []string{"scope:branches"}}, nil
	}
	return ext.Principal{}, ext.ErrUnauthenticated
}

func (f *fakeTokens) Create(_ context.Context, name, scope string, ttl time.Duration) (string, domain.APIToken, error) {
	f.created = append(f.created, name+"/"+scope+"/"+ttl.String())
	return "dbb_x_secret", domain.APIToken{ID: "x", Name: name, Scope: scope, CreatedAt: t0}, f.err
}
func (f *fakeTokens) CreateService(ctx context.Context, name, scope string, ttl time.Duration, bases []string, team string) (string, domain.APIToken, error) {
	sec, rec, err := f.Create(ctx, name, scope, ttl)
	rec.Bases, rec.Team = bases, team
	return sec, rec, err
}
func (f *fakeTokens) CreatePersonal(_ context.Context, p ext.Principal, name string, ttl time.Duration) (string, domain.APIToken, error) {
	return f.Create(ctx, name, "branches", ttl)
}
func (f *fakeTokens) ListOwned(ctx context.Context, _ string) ([]domain.APIToken, error) {
	return f.ListService(ctx)
}
func (f *fakeTokens) RevokeOwned(ctx context.Context, _, id string) error { return f.Revoke(ctx, id) }
func (f *fakeTokens) RevokeByOwner(context.Context, string) error         { return f.err }
func (f *fakeTokens) ListService(context.Context) ([]domain.APIToken, error) {
	return []domain.APIToken{{ID: "x", Name: "dev", Scope: "branches", Hash: "secret-hash", CreatedAt: t0}}, f.err
}
func (f *fakeTokens) Revoke(_ context.Context, id string) error {
	if id != "x" {
		return domain.ErrNotFound
	}
	return f.err
}

// fakeQuery is the runtime provider of mysql that answers every statement with 42.
type fakeQuery struct {
	domain.RuntimeProvider
	gotInst    domain.InstanceSpec
	got        domain.QueryRequest
	err        error
	noSQL      bool // the engine declares no query support
	noUpgrade  bool
	histRecord bool // the provider declares history_record whatever the registry says
	describe   error
}

func (q *fakeQuery) Describe(context.Context) (domain.EngineSpec, error) {
	spec, _ := domain.DefaultRuntimes.Spec(domain.MySQL)
	spec.Capabilities.Query = !q.noSQL
	spec.Capabilities.Upgrade = spec.Capabilities.Upgrade && !q.noUpgrade
	spec.Capabilities.HistoryRecord = spec.Capabilities.HistoryRecord || q.histRecord
	return spec, q.describe
}

func (q *fakeQuery) Query(_ context.Context, s domain.InstanceSpec, r domain.QueryRequest) (domain.QueryResult, error) {
	q.gotInst, q.got = s, r
	v := "42"
	return domain.QueryResult{Columns: []string{"answer"}, Rows: [][]*string{{&v}}}, q.err
}

func providersOf(q *fakeQuery) domain.ProviderSet { return domain.ProviderSet{domain.MySQL: q} }

func TestTokens_発行は一度だけ秘密を返し一覧には出さない(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/tokens", nil); resp.StatusCode != 503 {
		t.Errorf("without tokens: %d", resp.StatusCode)
	}
	for _, req := range [][2]string{{"POST", "/v1/tokens"}, {"DELETE", "/v1/tokens/x"}} {
		if resp, _ := f.do(t, req[0], req[1], map[string]any{}); resp.StatusCode != 503 {
			t.Errorf("%v without tokens: %d", req, resp.StatusCode)
		}
	}
	tk := &fakeTokens{}
	f.srv.Tokens = tk
	resp, body := f.do(t, "POST", "/v1/tokens", map[string]any{"name": "claude", "ttl": "720h"})
	if resp.StatusCode != 201 || body["token"] != "dbb_x_secret" || tk.created[0] != "claude/branches/720h0m0s" {
		t.Errorf("create: %d %v %v", resp.StatusCode, body, tk.created)
	}
	_, body = f.do(t, "GET", "/v1/tokens", nil)
	if item := body["items"].([]any)[0].(map[string]any); item["token"] != nil || item["hash"] != nil || item["name"] != "dev" {
		t.Errorf("list = %v", body)
	}
	if resp, _ := f.do(t, "POST", "/v1/tokens", map[string]any{"name": "x", "ttl": "soon"}); resp.StatusCode != 400 {
		t.Errorf("bad ttl: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/tokens/x", nil); resp.StatusCode != 204 {
		t.Errorf("revoke: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/tokens/nope", nil); resp.StatusCode != 404 {
		t.Errorf("revoke missing: %d", resp.StatusCode)
	}
	tk.err = errors.New("down")
	if resp, _ := f.do(t, "GET", "/v1/tokens", nil); resp.StatusCode != 500 {
		t.Errorf("list failure: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/tokens", map[string]any{"name": "y"}); resp.StatusCode != 500 {
		t.Errorf("create failure: %d", resp.StatusCode)
	}
}

func TestQuery_ブランチ自身のユーザーで実行する(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady, Port: 40001, User: "x", Password: "pw"}
	f.branches.branches["busy"] = domain.Branch{Name: name("busy"), Runtime: mysql84, Status: domain.BranchResetting}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 42"}); resp.StatusCode != 503 {
		t.Errorf("without querier: %d", resp.StatusCode)
	}
	q := &fakeQuery{}
	f.srv.Providers = providersOf(q)
	resp, body := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 42"})
	if resp.StatusCode != 200 || body["columns"].([]any)[0] != "answer" || q.got.Host != "192.0.2.10" || q.got.User != "x" || q.got.Password != "pw" || q.got.SQL != "SELECT 42" {
		t.Errorf("%d %v %+v", resp.StatusCode, body, q.got)
	}
	if q.gotInst.Runtime != mysql84 || q.gotInst.Ports["mysql"] != 40001 || q.gotInst.ID != "branch-x" {
		t.Errorf("instance = %+v", q.gotInst)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/busy/query", map[string]any{"sql": "SELECT 1"}); resp.StatusCode != 409 {
		t.Errorf("not ready: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"bad": 1}); resp.StatusCode != 400 {
		t.Errorf("bad body: %d", resp.StatusCode)
	}
	q.err = errors.New("syntax error")
	if resp, body := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELEC"}); resp.StatusCode != 422 || errCode(body) != "query_failed" {
		t.Errorf("failure: %d %v", resp.StatusCode, body)
	}
}

func TestQuery_任意database指定をRESTとMCPの両方からプロバイダへ渡せる(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady, Port: 40001, User: "x", Password: "pw", Access: domain.AccessOpen}
	q := &fakeQuery{}
	f.srv.Providers = providersOf(q)
	f.srv.TokenAuth = &fakeTokens{}

	resp, _ := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 1", "database": "  app  "})
	if resp.StatusCode != 200 || q.got.Database != "app" {
		t.Fatalf("REST database: status=%d got=%+v", resp.StatusCode, q.got)
	}
	for _, bad := range []string{"app?allowAllFiles=true", "bad/db", "bad\ndb"} {
		q.got.Database = "untouched"
		resp, body := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 1", "database": bad})
		if resp.StatusCode != 400 || errCode(body) != "invalid_request" || q.got.Database != "untouched" {
			t.Fatalf("bad database %q: status=%d body=%v got=%+v", bad, resp.StatusCode, body, q.got)
		}
	}
	resp, _ = f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 1"})
	if resp.StatusCode != 200 || q.got.Database != "" {
		t.Fatalf("REST default database: status=%d got=%+v", resp.StatusCode, q.got)
	}

	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out, isErr := callText(t, s, "query", map[string]any{"branch": "x", "sql": "SELECT 1", "database": "analytics"}); isErr || !strings.Contains(out, "42") {
		t.Fatalf("MCP query: isErr=%v out=%s", isErr, out)
	}
	if q.got.Database != "analytics" {
		t.Errorf("MCP database not forwarded: got=%+v", q.got)
	}
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpSession(t *testing.T, f *fixture, token string) (*mcp.ClientSession, error) {
	t.Helper()
	srv := httptest.NewServer(f.srv.Handler(false))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/v1/mcp", HTTPClient: &http.Client{Transport: bearerRT{token}}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
}

func callText(t *testing.T, s *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestMCP_復元は今の状態を保存しないと先に伝える(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := checkpointFixture(t)
	f.srv.TokenAuth = &fakeTokens{}
	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	text := s.InitializeResult().Instructions
	for _, bad := range []string{"loses nothing", "Nothing is lost", "saved as an automatic checkpoint", "kept as another checkpoint"} {
		if strings.Contains(text, bad) {
			t.Errorf("the instructions still say %q", bad)
		}
	}
	if !strings.Contains(text, "Restoring replaces the current state and saves nothing") || !strings.Contains(text, "create_checkpoint first") {
		t.Errorf("the instructions do not tell the agent to save first: %s", text)
	}
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "restore_checkpoint" && (strings.Contains(tool.Description, "Nothing is lost") || !strings.Contains(tool.Description, "create_checkpoint first")) {
			t.Errorf("restore_checkpoint says %q", tool.Description)
		}
	}
}

func TestMCP_トークンで接続しブランチを作って使える(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := newFixture(t)
	sink := &recordingSink{}
	f.srv.TokenAuth, f.srv.Providers, f.srv.Ext = &fakeTokens{}, providersOf(&fakeQuery{}), ext.Module{AuditSink: sink}
	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 15 {
		t.Fatalf("tools: %v %v", err, tools)
	}
	out, isErr := callText(t, s, "create_branch", map[string]any{"name": "feature-x", "base": "prod", "ttl": "4h", "access": "open"})
	if isErr || !strings.Contains(out, `"dsn": "mysql://branch:pw@192.0.2.10:40001/"`) || !strings.Contains(out, `"access": "open"`) {
		t.Errorf("create: %v %s", isErr, out)
	}
	f.srv.Ext = ext.Module{AuditSink: sink, ServerOptioner: &recorder{}}
	if out, isErr := callText(t, s, "create_branch", map[string]any{"name": "rec-x", "base": "prod", "record": true}); isErr || !strings.Contains(out, "rec-x") {
		t.Errorf("create with record: %v %s", isErr, out)
	}
	if !f.branches.lastCreate.Record {
		t.Error("the MCP request should carry record")
	}
	if out, isErr := callText(t, s, "list_branches", map[string]any{}); isErr || !strings.Contains(out, "feature-x") {
		t.Errorf("list: %s", out)
	}
	if out, isErr := callText(t, s, "list_bases", map[string]any{}); isErr || !strings.Contains(out, "prod") {
		t.Errorf("bases: %s", out)
	}
	if out, isErr := callText(t, s, "get_branch", map[string]any{"branch": "feature-x"}); isErr || !strings.Contains(out, "dsn") {
		t.Errorf("get with branch: %s", out)
	}
	if out, isErr := callText(t, s, "get_branch", map[string]any{"name": "feature-x"}); isErr || !strings.Contains(out, "dsn") {
		t.Errorf("get with deprecated name alias: %s", out)
	}
	if out, isErr := callText(t, s, "query", map[string]any{"branch": "feature-x", "sql": "SELECT 42"}); isErr || !strings.Contains(out, "42") {
		t.Errorf("query: %s", out)
	}
	if out, isErr := callText(t, s, "extend_branch", map[string]any{"branch": "feature-x", "ttl": "8h"}); isErr || !strings.Contains(out, "feature-x") {
		t.Errorf("extend with branch: %s", out)
	}
	if out, isErr := callText(t, s, "extend_branch", map[string]any{"name": "feature-x", "ttl": "8h"}); isErr || !strings.Contains(out, "feature-x") {
		t.Errorf("extend with deprecated name alias: %s", out)
	}
	if out, isErr := callText(t, s, "reset_branch", map[string]any{"branch": "feature-x"}); isErr || !strings.Contains(out, "reset") {
		t.Errorf("reset: %s", out)
	}
	if out, isErr := callText(t, s, "reset_branch", map[string]any{"name": "feature-x"}); isErr || !strings.Contains(out, "reset") {
		t.Errorf("reset with deprecated name alias: %s", out)
	}
	if out, isErr := callText(t, s, "delete_branch", map[string]any{"branch": "feature-x"}); isErr || !strings.Contains(out, "deleted") {
		t.Errorf("delete: %s", out)
	}
	if out, isErr := callText(t, s, "delete_branch", map[string]any{"name": "rec-x"}); isErr || !strings.Contains(out, "deleted") {
		t.Errorf("delete with deprecated name alias: %s", out)
	}
	if out, isErr := callText(t, s, "get_branch", map[string]any{"branch": "nope"}); !isErr || !strings.Contains(out, "not found") {
		t.Errorf("missing: %v %s", isErr, out)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "upgrade_branch" {
			raw, _ := json.Marshal(tool.InputSchema)
			if strings.Contains(string(raw), "list_runtimes") || !strings.Contains(string(raw), "db-branch runtime list") {
				t.Errorf("upgrade_branch schema still has wrong command name: %s", raw)
			}
		}
	}
	// Every tool call was authorized and audited as the token's principal.
	got := strings.Join(sink.actions(), " ")
	for _, want := range []string{"branch.create:token:dev:branch/feature-x:ok", "branch.query:token:dev:branch/feature-x:ok", "branch.delete:token:dev:branch/feature-x:ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit lacks %s: %s", want, got)
		}
	}
}

func TestMCP_危ない作業の前にチェックポイントを取り失敗したら戻せる(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := checkpointFixture(t)
	bx := f.branches.branches["x"]
	bx.Owner, bx.OwnerName = "token:dev", "dev"
	f.branches.branches["x"] = bx
	sink := &recordingSink{}
	f.srv.TokenAuth, f.srv.Ext = &fakeTokens{}, ext.Module{AuditSink: sink}
	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, isErr := callText(t, s, "create_checkpoint", map[string]any{"branch": "x", "name": "before-migration"})
	if isErr || !strings.Contains(out, `"name": "before-migration"`) {
		t.Errorf("create: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "create_checkpoint", map[string]any{"branch": "x"}); isErr || !strings.Contains(out, "20261109-120000") {
		t.Errorf("create without a name: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "list_checkpoints", map[string]any{"branch": "x"}); isErr || !strings.Contains(out, `"name": "c1"`) || !strings.Contains(out, `"current": true`) {
		t.Errorf("list: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "restore_checkpoint", map[string]any{"branch": "x", "checkpoint": "c1"}); isErr || !strings.Contains(out, "restored") {
		t.Errorf("restore: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "restore_checkpoint", map[string]any{"branch": "x", "checkpoint": "nope"}); !isErr || !strings.Contains(out, "not found") {
		t.Errorf("restore unknown: %v %s", isErr, out)
	}
	got := strings.Join(sink.actions(), " ")
	for _, want := range []string{"branch.checkpoint:token:dev:branch/x:ok", "branch.restore:token:dev:branch/x:ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit lacks %s: %s", want, got)
		}
	}
}

func TestMCP_チェックポイントに対応しないストレージでは分かりやすく断る(t *testing.T) {
	httpapi.MCPPoll = time.Millisecond
	f := checkpointFixture(t)
	bx := f.branches.branches["x"]
	bx.Owner, bx.OwnerName = "token:dev", "dev"
	f.branches.branches["x"] = bx
	f.branches.noCheckpoints = true
	f.branches.cpErr = domain.ErrCheckpointsUnavailable
	f.srv.TokenAuth = &fakeTokens{}
	s, err := mcpSession(t, f, "t-branches")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for tool, args := range map[string]map[string]any{
		"create_checkpoint":  {"branch": "x"},
		"list_checkpoints":   {"branch": "x"},
		"restore_checkpoint": {"branch": "x", "checkpoint": "c1"},
	} {
		if out, isErr := callText(t, s, tool, args); !isErr || !strings.Contains(out, "not available on this storage") {
			t.Errorf("%s: %v %q", tool, isErr, out)
		}
	}
}

func TestMCP_範囲外の操作や認証のない接続は断る(t *testing.T) {
	f := newFixture(t)
	f.srv.TokenAuth = &fakeTokens{}
	if _, err := mcpSession(t, f, "wrong"); err == nil {
		t.Error("bad token: want error")
	}
	f.branches.fail = errors.New("clone failed")
	s, err := mcpSession(t, f, "t-full")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out, isErr := callText(t, s, "create_branch", map[string]any{"name": "x", "base": "prod"}); !isErr || !strings.Contains(out, "clone failed") {
		t.Errorf("failed operation: %v %s", isErr, out)
	}
	if out, isErr := callText(t, s, "create_branch", map[string]any{"name": "Bad", "base": "prod"}); !isErr {
		t.Errorf("bad name: %s", out)
	}
	if out, isErr := callText(t, s, "extend_branch", map[string]any{"name": "nope", "ttl": "1h"}); !isErr {
		t.Errorf("extend missing: %s", out)
	}
	if out, isErr := callText(t, s, "query", map[string]any{"branch": "nope", "sql": "SELECT 1"}); !isErr {
		t.Errorf("query missing: %s", out)
	}
	if out, isErr := callText(t, s, "list_bases", map[string]any{}); isErr {
		t.Errorf("full token lists: %s", out)
	}
	f.srv.Ext = ext.Module{Authorizer: viewerReadsOnly{}}
	f.srv.TokenAuth = moduleAuth{}
	if _, err := mcpSession(t, f, "t-viewer"); err == nil {
		t.Error("a principal not allowed mcp.use: want error")
	}
}

func TestTLSCert_証明書を配る(t *testing.T) {
	f := newFixture(t)
	f.srv.TLSCertPEM = []byte("-----BEGIN CERTIFICATE-----\n")
	rec := httptest.NewRecorder()
	f.srv.Handler(false).ServeHTTP(rec, httptest.NewRequest("GET", "/tls.pem", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "-----BEGIN CERTIFICATE") {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}

	current := []byte("-----BEGIN CERTIFICATE-----\nFIRST\n")
	f.srv.TLSCert = func() []byte { return current }
	rec = httptest.NewRecorder()
	f.srv.Handler(false).ServeHTTP(rec, httptest.NewRequest("GET", "/tls.pem", nil))
	if rec.Code != 200 || rec.Body.String() != string(current) {
		t.Errorf("dynamic first: %d %q", rec.Code, rec.Body.String())
	}
	current = []byte("-----BEGIN CERTIFICATE-----\nSECOND\n")
	rec = httptest.NewRecorder()
	f.srv.Handler(false).ServeHTTP(rec, httptest.NewRequest("GET", "/tls.pem", nil))
	if rec.Code != 200 || rec.Body.String() != string(current) {
		t.Errorf("dynamic second: %d %q", rec.Code, rec.Body.String())
	}
}

func TestQuery_エンジンがqueryを宣言していなければ409_feature_not_supported(t *testing.T) {
	f := newFixture(t)
	f.branches.branches["x"] = domain.Branch{Name: name("x"), Runtime: mysql84, Status: domain.BranchReady, Port: 40001, User: "x", Password: "pw"}
	q := &fakeQuery{noSQL: true}
	f.srv.Providers = providersOf(q)

	resp, body := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 1"})
	if resp.StatusCode != 409 || errCode(body) != "feature_not_supported" {
		t.Errorf("no query support: %d %v", resp.StatusCode, body)
	}
	if q.got.SQL != "" {
		t.Error("the statement was sent to an engine that declares no query support")
	}
	f.srv.Providers = domain.ProviderSet{}
	if resp, body := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 1"}); resp.StatusCode != 409 || errCode(body) != "feature_not_supported" {
		t.Errorf("no provider: %d %v", resp.StatusCode, body)
	}
	f.srv.Providers = providersOf(&fakeQuery{describe: errors.New("module down")})
	if resp, _ := f.do(t, "POST", "/v1/branches/x/query", map[string]any{"sql": "SELECT 1"}); resp.StatusCode != 500 {
		t.Errorf("describe failure: %d", resp.StatusCode)
	}
}

// swapRuntimes gives the test its own engine registry (the built-in engines).
func swapRuntimes(t *testing.T) *domain.RuntimeRegistry {
	t.Helper()
	old := domain.DefaultRuntimes
	domain.DefaultRuntimes = domain.NewBuiltinRuntimes()
	t.Cleanup(func() { domain.DefaultRuntimes = old })
	return domain.DefaultRuntimes
}

func TestRuntimesAvailable_登録されたすべてのエンジンを列挙する(t *testing.T) {
	reg := swapRuntimes(t)
	if err := reg.Register(domain.EngineSpec{Engine: "clickhouse"}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	_, body := f.do(t, "GET", "/v1/runtimes/available", nil)
	var engines []string
	for _, it := range body["items"].([]any) {
		engines = append(engines, it.(map[string]any)["engine"].(string))
	}
	if strings.Join(engines, ",") != "clickhouse,mysql,postgres" {
		t.Errorf("engines = %v", engines)
	}
}

func TestCapabilities_記録の有無は履歴を記録できると宣言したエンジンで確かめる(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	_ = reg.Register(domain.EngineSpec{Engine: "mysql"})
	_ = reg.Register(domain.EngineSpec{Engine: "journaled", Capabilities: domain.RuntimeCaps{HistoryRecord: true}})
	old := domain.DefaultRuntimes
	domain.DefaultRuntimes = reg
	defer func() { domain.DefaultRuntimes = old }()

	f := newFixture(t)
	rec := &recorder{}
	f.srv.Ext = ext.Module{ServerOptioner: rec}
	if _, body := f.do(t, "GET", "/v1/capabilities", nil); body["record"] != true {
		t.Errorf("record = %v", body["record"])
	}
	if len(rec.seen) != 1 || rec.seen[0].Engine != "journaled" {
		t.Errorf("probed = %+v, want only the engine that records", rec.seen)
	}

	domain.DefaultRuntimes = domain.NewRuntimeRegistry()
	if _, body := f.do(t, "GET", "/v1/capabilities", nil); body["record"] != false {
		t.Errorf("no engine records: %v", body["record"])
	}
}

func TestFail_ErrFeatureNotSupportedは409_feature_not_supported(t *testing.T) {
	f := newFixture(t)
	f.branches.fail = domain.FeatureNotSupported("branch_merge")
	resp, body := f.do(t, "DELETE", "/v1/bases/prod", nil)
	if resp.StatusCode != 409 || errCode(body) != "feature_not_supported" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestCapabilities_記録できるエンジンが複数あれば受け付けたものがあれば真(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	caps := domain.RuntimeCaps{HistoryRecord: true}
	_ = reg.Register(domain.EngineSpec{Engine: "aaa", Capabilities: caps})
	_ = reg.Register(domain.EngineSpec{Engine: "bbb", Capabilities: caps})
	old := domain.DefaultRuntimes
	domain.DefaultRuntimes = reg
	defer func() { domain.DefaultRuntimes = old }()

	f := newFixture(t)
	f.srv.Ext = ext.Module{ServerOptioner: &pickyOptioner{accepts: "bbb"}}
	if _, body := f.do(t, "GET", "/v1/capabilities", nil); body["record"] != true {
		t.Errorf("the second engine records: %v", body["record"])
	}
	f.srv.Ext = ext.Module{ServerOptioner: &pickyOptioner{accepts: "none"}}
	if _, body := f.do(t, "GET", "/v1/capabilities", nil); body["record"] != false {
		t.Errorf("no engine is accepted: %v", body["record"])
	}
}

// pickyOptioner accepts a recorded branch of one engine only.
type pickyOptioner struct{ accepts string }

func (p *pickyOptioner) ServerOptions(_ context.Context, b ext.ServerBranch) (ext.ServerOptions, error) {
	if b.Engine != p.accepts {
		return ext.ServerOptions{}, errors.New("cannot record " + b.Engine)
	}
	return ext.ServerOptions{}, nil
}
