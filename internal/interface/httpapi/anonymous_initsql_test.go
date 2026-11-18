package httpapi_test

import (
	"strings"
	"testing"
)

// SQL after the copy runs on the node. Whoever may make a branch without a
// login may not hand the node SQL to run.
func TestAnonymous_ログインなしはinit_sqlを渡せない(t *testing.T) {
	f := anonFixture(t)
	h := f.srv.Handler(false)
	resp, body := send(t, h, "POST", "/v1/branches",
		map[string]any{"name": "x1", "base": "prod", "init_sql": "COPY (SELECT 1) TO PROGRAM 'id'"}, nil)
	if resp.StatusCode != 403 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "init_sql") {
		t.Errorf("anonymous with init_sql: %d %v", resp.StatusCode, body)
	}
	if _, made := f.branches.branches["x1"]; made {
		t.Error("the branch was made")
	}
	// Nothing to run is nothing to refuse.
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "x2", "base": "prod", "init_sql": "  \n"}, nil); resp.StatusCode != 202 {
		t.Errorf("blank init_sql: %d", resp.StatusCode)
	}
	// Someone signed in may.
	if resp, _ := send(t, h, "POST", "/v1/branches", map[string]any{"name": "x3", "base": "prod", "init_sql": "UPDATE t SET a = 1"}, as("t-alice")); resp.StatusCode != 202 {
		t.Errorf("signed in with init_sql: %d", resp.StatusCode)
	}
}
