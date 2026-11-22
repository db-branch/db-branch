package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpInstructions tell an LLM agent how to use the tools.
const mcpInstructions = `db-branch gives you disposable copies (branches) of real databases.
Create a branch from a base, work in it freely (query, change data or schema),
reset it to start over, and delete it when done. Branches are isolated: nothing
you do reaches the base or other branches. Prefer a short ttl (e.g. "4h").
Before risky work (a migration, a bulk update) take a checkpoint with
create_checkpoint; if it goes wrong, restore_checkpoint puts the branch back.
Restoring replaces the current state and saves nothing: call create_checkpoint first
if the current state may be needed.
A branch can be moved to a newer database version (upgrade_branch; check_upgrade
only checks). A checkpoint is saved first and a failed upgrade rolls back by itself.`

// internalMux serves tool calls through the same routes (and so the same
// authorization and audit) as HTTP clients.
type internalMux struct {
	once sync.Once
	mux  *http.ServeMux
}

func (s *Server) internal() *http.ServeMux {
	s.inner.once.Do(func() {
		s.inner.mux = http.NewServeMux()
		s.routes(s.inner.mux, false)
	})
	return s.inner.mux
}

// call runs an API request as the principal of from.
func (s *Server) call(ctx context.Context, from *http.Request, method, path string, body any) (int, []byte) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequestWithContext(withPrincipal(ctx, principalOf(from)), method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = from.RemoteAddr
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	s.internal().ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes()
}

// apiError turns an error envelope into an error.
func apiError(status int, body []byte) error {
	return fmt.Errorf("%s (HTTP %d)", errorMessage(body), status)
}

func (s *Server) get(ctx context.Context, from *http.Request, path string, out any) error {
	status, body := s.call(ctx, from, http.MethodGet, path, nil)
	if status >= 300 {
		return apiError(status, body)
	}
	return json.Unmarshal(body, out)
}

// startAndWait runs a request that answers with an operation and waits for it.
func (s *Server) startAndWait(ctx context.Context, from *http.Request, method, path string, body any) error {
	status, out := s.call(ctx, from, method, path, body)
	if status >= 300 {
		return apiError(status, out)
	}
	var op struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(out, &op) != nil || op.ID == "" {
		return nil
	}
	for {
		var cur struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		if err := s.get(ctx, from, "/v1/operations/"+op.ID, &cur); err != nil {
			return err
		}
		switch cur.State {
		case "succeeded":
			return nil
		case "failed":
			return errors.New(cur.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(MCPPoll):
		}
	}
}

// finished is a finished operation as the tools report it.
type finished struct {
	State  string          `json:"state"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// startAndCollect is startAndWait that also returns the operation's result,
// failed or not. The error is for requests the API refused.
func (s *Server) startAndCollect(ctx context.Context, from *http.Request, method, path string, body any) (finished, error) {
	status, out := s.call(ctx, from, method, path, body)
	if status >= 300 {
		return finished{}, apiError(status, out)
	}
	var op struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out, &op)
	for {
		var cur finished
		if err := s.get(ctx, from, "/v1/operations/"+op.ID, &cur); err != nil {
			return cur, err
		}
		if cur.State == "succeeded" || cur.State == "failed" {
			return cur, nil
		}
		select {
		case <-ctx.Done():
			return cur, ctx.Err()
		case <-time.After(MCPPoll):
		}
	}
}

// operationResult is the tool answer for a finished operation: the result
// either way, and an error flag when the operation failed.
func operationResult(f finished) *mcp.CallToolResult {
	res := text(f)
	res.IsError = f.State == "failed"
	return res
}

type upgradeIn struct {
	Branch     string `json:"branch" jsonschema:"the branch to upgrade"`
	Runtime    string `json:"runtime" jsonschema:"the target, engine@version, e.g. mysql@8.4.13 (see list_runtimes on the CLI; it must be installed)"`
	NoRollback bool   `json:"no_rollback,omitempty" jsonschema:"keep a failed upgrade as it is instead of rolling back"`
	Force      bool   `json:"force,omitempty" jsonschema:"upgrade although the pre-check found errors"`
}

type checkUpgradeIn struct {
	Branch  string `json:"branch" jsonschema:"the branch to check"`
	Runtime string `json:"runtime" jsonschema:"the target, engine@version, e.g. mysql@8.4.13"`
}

// MCPMaxBody is the largest MCP request body (bytes) POST /v1/mcp accepts.
const MCPMaxBody = 1 << 20

// MCPPoll is how often tools check a running operation.
var MCPPoll = 500 * time.Millisecond

func text(v any) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

type nameIn struct {
	Name string `json:"name" jsonschema:"the branch name"`
}

type createIn struct {
	Name     string `json:"name" jsonschema:"new branch name: lowercase letters, digits and hyphens"`
	Base     string `json:"base" jsonschema:"the base to copy (see list_bases)"`
	Snapshot string `json:"snapshot,omitempty" jsonschema:"base snapshot to copy; default the latest"`
	TTL      string `json:"ttl,omitempty" jsonschema:"delete automatically after this long, e.g. 4h"`
	InitSQL  string `json:"init_sql,omitempty" jsonschema:"SQL run once after copying, e.g. to mask data; reset returns here"`
	Access   string `json:"access,omitempty" jsonschema:"private (default: a password of its own, only you may connect) or open (anyone may connect, shared password)"`
	Record   bool   `json:"record,omitempty" jsonschema:"record the branch's changes so it can be restored to any point (needs the history module)"`
	Runtime  string `json:"runtime,omitempty" jsonschema:"create the branch on a newer version of the base's engine, e.g. mysql@8.4.13 (upgraded right after creation)"`
}

type extendIn struct {
	Name string `json:"name" jsonschema:"the branch name"`
	TTL  string `json:"ttl" jsonschema:"new time to live from now, e.g. 24h"`
}

type branchIn struct {
	Branch string `json:"branch" jsonschema:"the branch"`
}

type checkpointIn struct {
	Branch string `json:"branch" jsonschema:"the branch to take a checkpoint of"`
	Name   string `json:"name,omitempty" jsonschema:"checkpoint name: lowercase letters, digits and hyphens; default the current time"`
}

type restoreIn struct {
	Branch     string `json:"branch" jsonschema:"the branch to restore"`
	Checkpoint string `json:"checkpoint" jsonschema:"the checkpoint name (see list_checkpoints)"`
}

type queryIn struct {
	Branch  string `json:"branch" jsonschema:"the branch to run on"`
	SQL     string `json:"sql" jsonschema:"one SQL statement"`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"at most this many rows (default and limit 1000)"`
}

// branchInfo is a branch with how to connect to it.
func (s *Server) branchInfo(ctx context.Context, from *http.Request, name string) (any, error) {
	var b map[string]any
	if err := s.get(ctx, from, "/v1/branches/"+url.PathEscape(name), &b); err != nil {
		return nil, err
	}
	var dsn struct {
		DSN string `json:"dsn"`
	}
	if err := s.get(ctx, from, "/v1/branches/"+url.PathEscape(name)+"/dsn", &dsn); err == nil {
		b["dsn"] = dsn.DSN
	}
	return b, nil
}

// mcpServer builds the tools for one request (stateless): they act as the
// request's principal.
func (s *Server) mcpServer(r *http.Request) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "db-branch", Version: s.Version}, &mcp.ServerOptions{Instructions: mcpInstructions})
	list := func(path string) mcp.ToolHandlerFor[struct{}, any] {
		return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			var out map[string]any
			if err := s.get(ctx, r, path, &out); err != nil {
				return nil, nil, err
			}
			return text(out["items"]), nil, nil
		}
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "list_bases", Description: "List the bases branches can be copied from, with their snapshots."}, list("/v1/bases"))
	mcp.AddTool(srv, &mcp.Tool{Name: "list_branches", Description: "List existing branches."}, list("/v1/branches"))
	mcp.AddTool(srv, &mcp.Tool{Name: "create_branch", Description: "Copy a base into a new branch (a full database of its own, ready in seconds) and return how to connect."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createIn) (*mcp.CallToolResult, any, error) {
			body := map[string]any{"name": in.Name, "base": in.Base, "ttl": in.TTL, "init_sql": in.InitSQL}
			if in.Access != "" {
				body["access"] = in.Access
			}
			if in.Record {
				body["record"] = true
			}
			if in.Snapshot != "" {
				body["snapshot"] = in.Snapshot
			}
			if in.Runtime != "" {
				body["runtime"] = in.Runtime
			}
			if err := s.startAndWait(ctx, r, http.MethodPost, "/v1/branches", body); err != nil {
				return nil, nil, err
			}
			info, err := s.branchInfo(ctx, r, in.Name)
			if err != nil {
				return nil, nil, err
			}
			return text(info), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "get_branch", Description: "Show a branch and how to connect to it (dsn)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in nameIn) (*mcp.CallToolResult, any, error) {
			info, err := s.branchInfo(ctx, r, in.Name)
			if err != nil {
				return nil, nil, err
			}
			return text(info), nil, nil
		})
	action := func(method, suffix, done string) mcp.ToolHandlerFor[nameIn, any] {
		return func(ctx context.Context, _ *mcp.CallToolRequest, in nameIn) (*mcp.CallToolResult, any, error) {
			if err := s.startAndWait(ctx, r, method, "/v1/branches/"+url.PathEscape(in.Name)+suffix, nil); err != nil {
				return nil, nil, err
			}
			return text(map[string]string{"branch": in.Name, "result": done}), nil, nil
		}
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "reset_branch", Description: "Throw away every change in a branch: back to the state right after it was created (and its init SQL ran)."}, action(http.MethodPost, "/reset", "reset"))
	mcp.AddTool(srv, &mcp.Tool{Name: "delete_branch", Description: "Delete a branch and its data."}, action(http.MethodDelete, "", "deleted"))
	mcp.AddTool(srv, &mcp.Tool{Name: "extend_branch", Description: "Keep a branch longer: it now expires ttl from now."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in extendIn) (*mcp.CallToolResult, any, error) {
			status, body := s.call(ctx, r, http.MethodPatch, "/v1/branches/"+url.PathEscape(in.Name), map[string]string{"ttl": in.TTL})
			if status >= 300 {
				return nil, nil, apiError(status, body)
			}
			var out any
			_ = json.Unmarshal(body, &out)
			return text(out), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "create_checkpoint", Description: "Save the branch's current state as a checkpoint (instant, the branch keeps running). Take one before risky work so restore_checkpoint can undo it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in checkpointIn) (*mcp.CallToolResult, any, error) {
			status, body := s.call(ctx, r, http.MethodPost, "/v1/branches/"+url.PathEscape(in.Branch)+"/checkpoints", map[string]string{"name": in.Name})
			if status >= 300 {
				return nil, nil, apiError(status, body)
			}
			var out any
			_ = json.Unmarshal(body, &out)
			return text(out), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "list_checkpoints", Description: "List a branch's checkpoints, oldest first. current marks the one its data descends from; auto marks those taken before a restore."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in branchIn) (*mcp.CallToolResult, any, error) {
			var out map[string]any
			if err := s.get(ctx, r, "/v1/branches/"+url.PathEscape(in.Branch)+"/checkpoints", &out); err != nil {
				return nil, nil, err
			}
			return text(out["items"]), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "restore_checkpoint", Description: "Put a branch back to a checkpoint (it restarts briefly). The current state is replaced and not saved: call create_checkpoint first if it may be needed. Every checkpoint already taken stays restorable."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in restoreIn) (*mcp.CallToolResult, any, error) {
			path := "/v1/branches/" + url.PathEscape(in.Branch) + "/checkpoints/" + url.PathEscape(in.Checkpoint) + "/restore"
			if err := s.startAndWait(ctx, r, http.MethodPost, path, nil); err != nil {
				return nil, nil, err
			}
			return text(map[string]string{"branch": in.Branch, "checkpoint": in.Checkpoint, "result": "restored"}), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "check_upgrade", Description: "Check what upgrading a branch to another version of its database engine would run into (accounts, configuration, free space), without changing anything. Returns findings with level error, warning or info."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in checkUpgradeIn) (*mcp.CallToolResult, any, error) {
			f, err := s.startAndCollect(ctx, r, http.MethodPost, "/v1/branches/"+url.PathEscape(in.Branch)+"/upgrade", map[string]any{"runtime": in.Runtime, "check_only": true})
			if err != nil {
				return nil, nil, err
			}
			return operationResult(f), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "upgrade_branch", Description: "Move one branch to a newer version of its database engine (MySQL 8.0 to 8.4 and so on); its base and other branches are untouched. A checkpoint is saved first and a failed upgrade rolls back by itself. The result lists the pre-check findings and the server log's warnings. Upgrades only; to go back, restore the before-upgrade checkpoint."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in upgradeIn) (*mcp.CallToolResult, any, error) {
			f, err := s.startAndCollect(ctx, r, http.MethodPost, "/v1/branches/"+url.PathEscape(in.Branch)+"/upgrade", map[string]any{"runtime": in.Runtime, "no_rollback": in.NoRollback, "force": in.Force})
			if err != nil {
				return nil, nil, err
			}
			return operationResult(f), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "query", Description: "Run one SQL statement in a branch and return its rows (as text) or the number of affected rows. Changes stay in the branch."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, any, error) {
			status, body := s.call(ctx, r, http.MethodPost, "/v1/branches/"+url.PathEscape(in.Branch)+"/query", map[string]any{"sql": in.SQL, "max_rows": in.MaxRows})
			if status >= 300 {
				return nil, nil, apiError(status, body)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
		})
	return srv
}

// mcpHandler serves MCP (streamable HTTP, stateless) to principals allowed
// "mcp.use" (routes wraps it in guard, which authorizes and audits);
// API clients authenticate with a bearer token.
func (s *Server) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(s.mcpServer, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A JSON-RPC message is small; read it with a cap before the SDK parses it.
		body, err := io.ReadAll(io.LimitReader(r.Body, MCPMaxBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "cannot read the request")
			return
		}
		if len(body) > MCPMaxBody {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the MCP request is larger than "+strconv.Itoa(MCPMaxBody)+" bytes")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r)
	})
}
