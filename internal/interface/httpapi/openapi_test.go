package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// openAPIFile is the public API description, checked against the routes.
const openAPIFile = "../../../api/openapi.yaml"

// moduleAPIPrefix is where modules register their own APIs. They are
// documented by each module, not in the core description.
const moduleAPIPrefix = "/v1/ext/"

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

type openAPIDoc struct {
	Info struct {
		Version     string `yaml:"version"`
		Description string `yaml:"description"`
	} `yaml:"info"`
	Tags []struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	} `yaml:"tags"`
	Components struct {
		Parameters map[string]struct {
			Name string `yaml:"name"`
			In   string `yaml:"in"`
		} `yaml:"parameters"`
		Schemas map[string]yaml.Node `yaml:"schemas"`
	} `yaml:"components"`
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

type openAPIOperation struct {
	OperationID string                    `yaml:"operationId"`
	Summary     string                    `yaml:"summary"`
	Description string                    `yaml:"description"`
	Tags        []string                  `yaml:"tags"`
	Action      string                    `yaml:"x-dbb-action"`
	Responses   map[string]map[string]any `yaml:"responses"`
	Parameters  []map[string]any          `yaml:"parameters"`
}

func readOpenAPI(t *testing.T) (openAPIDoc, yaml.Node) {
	t.Helper()
	raw, err := os.ReadFile(openAPIFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", openAPIFile, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	return doc, root
}

// specOperations returns the operations of the description by "METHOD /path".
func specOperations(t *testing.T, doc openAPIDoc) map[string]openAPIOperation {
	t.Helper()
	ops := map[string]openAPIOperation{}
	for path, item := range doc.Paths {
		for method, node := range item {
			if !slices.Contains(httpMethods, method) {
				continue // parameters, summary, ...
			}
			var op openAPIOperation
			if err := node.Decode(&op); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			ops[strings.ToUpper(method)+" "+path] = op
		}
	}
	return ops
}

// serverRoutes returns every "METHOD /path" the core API serves: the
// patterns routes registers with the mux, and the few paths answered before
// the mux (outerPatterns). Module APIs are left out.
func serverRoutes(t *testing.T) map[string]bool {
	routes, _ := serverRoutesAndActions(t)
	return routes
}

func serverRoutesAndActions(t *testing.T) (map[string]bool, map[string]string) {
	t.Helper()
	rec := &patternRecorder{actions: map[string]string{}}
	(&Server{}).routes(rec, false)
	routes := map[string]bool{}
	for _, p := range append(rec.patterns, outerPatterns...) {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			path, method = p, ""
		}
		switch {
		case strings.HasPrefix(path, moduleAPIPrefix):
			continue
		case method == "":
			t.Errorf("route %q has no method: register it with one so that the description can say what it answers", p)
		}
		routes[method+" "+path] = true
	}
	return routes, rec.actions
}

// patternRecorder is a router that only remembers the patterns and actions it is given.
type patternRecorder struct {
	patterns []string
	actions  map[string]string
}

func (r *patternRecorder) Handle(pattern string, _ http.Handler) {
	r.patterns = append(r.patterns, pattern)
}

func (r *patternRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}

func (r *patternRecorder) recordAction(pattern, action string) {
	r.actions[pattern] = action
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestOpenAPI_記述とサーバーのルートが過不足なく一致する(t *testing.T) {
	doc, _ := readOpenAPI(t)
	spec := specOperations(t, doc)
	for path := range doc.Paths {
		if strings.HasPrefix(path, moduleAPIPrefix) {
			t.Errorf("%s: module APIs are documented by each module, not in api/openapi.yaml", path)
		}
	}
	served := serverRoutes(t)
	for _, route := range keys(served) {
		if _, ok := spec[route]; !ok {
			t.Errorf("%s is served but missing from api/openapi.yaml", route)
		}
	}
	for _, route := range keys(spec) {
		if !served[route] {
			t.Errorf("%s is in api/openapi.yaml but the server does not serve it", route)
		}
	}
}

func TestOpenAPI_すべての操作にID_タグ_概要_エラー応答がある(t *testing.T) {
	doc, _ := readOpenAPI(t)
	declared := map[string]bool{}
	for _, tag := range doc.Tags {
		if strings.TrimSpace(tag.Description) == "" {
			t.Errorf("tag %q has no description", tag.Name)
		}
		declared[tag.Name] = true
	}
	ids := map[string]string{}
	for _, route := range keys(specOperations(t, doc)) {
		op := specOperations(t, doc)[route]
		if op.OperationID == "" {
			t.Errorf("%s: operationId is missing", route)
		} else if other, dup := ids[op.OperationID]; dup {
			t.Errorf("%s: operationId %q is also used by %s", route, op.OperationID, other)
		}
		ids[op.OperationID] = route
		if strings.TrimSpace(op.Summary) == "" {
			t.Errorf("%s: summary is missing", route)
		}
		if len(op.Tags) == 0 {
			t.Errorf("%s: has no tag", route)
		}
		for _, tag := range op.Tags {
			if !declared[tag] {
				t.Errorf("%s: tag %q is not declared in the top-level tags", route, tag)
			}
		}
		if !documentsError(op) {
			t.Errorf("%s: documents no error response (a 4xx/5xx status or default)", route)
		}
	}
}

func documentsError(op openAPIOperation) bool {
	for status := range op.Responses {
		if n, err := strconv.Atoi(status); status == "default" || (err == nil && n >= 400) {
			return true
		}
	}
	return false
}

func TestOpenAPI_パスのパラメータがすべて宣言されている(t *testing.T) {
	doc, _ := readOpenAPI(t)
	wildcard := regexp.MustCompile(`\{([^}]+)\}`)
	// pathParams names the path parameters a list of parameters declares,
	// directly or through a reference to components.parameters.
	pathParams := func(list []map[string]any) map[string]bool {
		names := map[string]bool{}
		for _, p := range list {
			if ref, ok := p["$ref"].(string); ok {
				c := doc.Components.Parameters[strings.TrimPrefix(ref, "#/components/parameters/")]
				if c.In == "path" {
					names[c.Name] = true
				}
			}
			if p["in"] == "path" {
				name, _ := p["name"].(string)
				names[name] = true
			}
		}
		return names
	}
	for path, item := range doc.Paths {
		var shared []map[string]any
		if n, ok := item["parameters"]; ok {
			_ = n.Decode(&shared)
		}
		for method, node := range item {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			var op openAPIOperation
			_ = node.Decode(&op)
			declared := pathParams(append(append([]map[string]any{}, shared...), op.Parameters...))
			for _, m := range wildcard.FindAllStringSubmatch(path, -1) {
				if !declared[m[1]] {
					t.Errorf("%s %s: path parameter {%s} is not declared", strings.ToUpper(method), path, m[1])
				}
			}
		}
	}
}

func TestOpenAPI_すべての参照が解決できる(t *testing.T) {
	_, root := readOpenAPI(t)
	var top map[string]any
	if err := root.Content[0].Decode(&top); err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if !resolves(top, ref) {
					t.Errorf("unresolved $ref %s", ref)
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(top)
}

// resolves follows a local JSON pointer ("#/components/schemas/Branch").
func resolves(top map[string]any, ref string) bool {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return false
	}
	var cur any = top
	for _, part := range strings.Split(rest, "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[part]; !ok {
			return false
		}
	}
	return true
}

func TestOpenAPI_版は下書きではない(t *testing.T) {
	doc, _ := readOpenAPI(t)
	if doc.Info.Version == "" || strings.Contains(doc.Info.Version, "draft") {
		t.Errorf("info.version = %q: set the release the description is for", doc.Info.Version)
	}
}

func TestOpenAPI_全ルートとアクション名とエラーコードがサーバー実装と一致しEngineとOperationKindが拡張可能である(t *testing.T) {
	doc, _ := readOpenAPI(t)
	if doc.Info.Version != "1.0.0" {
		t.Errorf("info.version = %q, want 1.0.0", doc.Info.Version)
	}
	raw, err := os.ReadFile(openAPIFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "db-branch group") {
		t.Error("openapi.yaml still refers to db-branch group instead of db-branch-admin")
	}
	if regexp.MustCompile(`(?i)licence`).Match(raw) {
		t.Error("openapi.yaml still contains British spelling 'licence'")
	}

	// 1. All routes' x-dbb-action must match server.go routes(...).
	_, actions := serverRoutesAndActions(t)
	ops := specOperations(t, doc)
	for _, route := range keys(ops) {
		wantAction := actions[route]
		if ops[route].Action != wantAction {
			t.Errorf("%s: x-dbb-action = %q, server has %q", route, ops[route].Action, wantAction)
		}
	}

	// 2. Every writeError code in httpapi/*.go must be documented in openapi.yaml # Errors.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 3 {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "writeError" {
				return true
			}
			lit, ok := call.Args[2].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			code, _ := strconv.Unquote(lit.Value)
			if !strings.Contains(doc.Info.Description, "`"+code+"`") {
				t.Errorf("%s: error code %q is not documented in openapi.yaml info.description", e.Name(), code)
			}
			return true
		})
	}

	// 3. Engine, RuntimeRef, and Operation.kind must be extensible (no fixed enum).
	var engSchema struct {
		Type    string   `yaml:"type"`
		Enum    []string `yaml:"enum"`
		Pattern string   `yaml:"pattern"`
	}
	engNode := doc.Components.Schemas["Engine"]
	if err := engNode.Decode(&engSchema); err != nil {
		t.Fatal(err)
	}
	if len(engSchema.Enum) > 0 {
		t.Errorf("Engine schema must not have fixed enum %v", engSchema.Enum)
	}
	if engSchema.Pattern == "" || !regexp.MustCompile(engSchema.Pattern).MatchString("clickhouse") {
		t.Errorf("Engine pattern %q should accept external engines such as clickhouse", engSchema.Pattern)
	}

	var rtSchema struct {
		Pattern string `yaml:"pattern"`
	}
	rtNode := doc.Components.Schemas["RuntimeRef"]
	if err := rtNode.Decode(&rtSchema); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(rtSchema.Pattern).MatchString("clickhouse@26.3.19.3") {
		t.Errorf("RuntimeRef pattern %q must accept external engine runtimes", rtSchema.Pattern)
	}

	var opSchema struct {
		Properties map[string]struct {
			Type string   `yaml:"type"`
			Enum []string `yaml:"enum"`
		} `yaml:"properties"`
	}
	opNode := doc.Components.Schemas["Operation"]
	if err := opNode.Decode(&opSchema); err != nil {
		t.Fatal(err)
	}
	if kind := opSchema.Properties["kind"]; len(kind.Enum) > 0 {
		t.Errorf("Operation.kind must not have fixed enum %v", kind.Enum)
	}

	// 4. All list schemas must include next_cursor.
	for name, node := range doc.Components.Schemas {
		if !strings.HasSuffix(name, "List") && name != "LoginMethods" {
			continue
		}
		var listSchema struct {
			Properties map[string]any `yaml:"properties"`
			Required   []string       `yaml:"required"`
		}
		if err := node.Decode(&listSchema); err != nil {
			t.Fatal(err)
		}
		if _, ok := listSchema.Properties["next_cursor"]; !ok || !slices.Contains(listSchema.Required, "next_cursor") {
			t.Errorf("schema %s must include required next_cursor", name)
		}
	}

	// 5. Auth doc accuracy: GET /v1/auth/session is session-only, POST /v1/mcp mentions fork_branch & merge_line.
	if strings.Contains(ops["GET /v1/auth/session"].Description, "the local socket, or a `full` token") {
		t.Error("GET /v1/auth/session description falsely says local socket or full token works")
	}
	mcpDesc := ops["POST /v1/mcp"].Description
	for _, tool := range []string{"fork_branch", "merge_line"} {
		if !strings.Contains(mcpDesc, tool) {
			t.Errorf("POST /v1/mcp description missing tool %s", tool)
		}
	}
	_ = filepath.Base
}
