package httpapi_test

import (
	"context"
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/update"
)

type fakeUpdates struct {
	view   update.View
	err    error
	target string
	by     string
	cancel int
}

func (f *fakeUpdates) View(context.Context) (update.View, error)  { return f.view, f.err }
func (f *fakeUpdates) Check(context.Context) (update.View, error) { return f.view, f.err }
func (f *fakeUpdates) Apply(_ context.Context, target, by string) (domain.Rollout, error) {
	f.target, f.by = target, by
	return domain.Rollout{ID: "r1", Target: target, Status: domain.RolloutRunning}, f.err
}
func (f *fakeUpdates) Cancel(context.Context) error { f.cancel++; return f.err }

func TestUpdates_一覧と確認と開始と取り消し(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/updates", nil); resp.StatusCode != 503 {
		t.Errorf("without updates: %d", resp.StatusCode)
	}
	u := &fakeUpdates{view: update.View{LatestMinor: "1.2.0", Nodes: []update.NodeView{{Name: "n1", Version: "1.0.0"}},
		Packages: []update.PackageView{{Name: "db-branch", Installed: "1.0.0", Available: []update.Candidate{{Version: "1.2.0", Kind: "minor"}}}}}}
	f.srv.Updates = u
	_, body := f.do(t, "GET", "/v1/updates", nil)
	if body["latest_minor"] != "1.2.0" || body["packages"].([]any)[0].(map[string]any)["available"].([]any)[0].(map[string]any)["kind"] != "minor" {
		t.Errorf("get = %v", body)
	}
	if resp, body := f.do(t, "POST", "/v1/updates/check", nil); resp.StatusCode != 200 || body["latest_minor"] != "1.2.0" {
		t.Errorf("check: %d %v", resp.StatusCode, body)
	}
	resp, body := f.do(t, "POST", "/v1/updates", map[string]any{"target": "1.2.0"})
	if resp.StatusCode != 202 || body["id"] != "r1" || u.target != "1.2.0" || u.by != "local" {
		t.Errorf("apply: %d %v %q %q", resp.StatusCode, body, u.target, u.by)
	}
	if resp, _ := f.do(t, "POST", "/v1/updates", map[string]any{"target": "1.2.0", "x": 1}); resp.StatusCode != 400 {
		t.Errorf("unknown field: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/v1/updates", map[string]any{}); resp.StatusCode != 400 {
		t.Errorf("no target: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/v1/updates", nil); resp.StatusCode != 204 || u.cancel != 1 {
		t.Errorf("cancel: %d", resp.StatusCode)
	}
	for err, want := range map[error]int{
		domain.ErrInvalidUpdate: 400, domain.ErrConflict: 409, domain.ErrRolloutRunning: 409, domain.ErrNoLeader: 503, errors.New("boom"): 500,
	} {
		u.err = err
		for _, c := range []struct{ method, path string }{{"GET", "/v1/updates"}, {"POST", "/v1/updates/check"}, {"DELETE", "/v1/updates"}} {
			if resp, _ := f.do(t, c.method, c.path, nil); resp.StatusCode != want {
				t.Errorf("%s %s with %v: %d, want %d", c.method, c.path, err, resp.StatusCode, want)
			}
		}
		if resp, _ := f.do(t, "POST", "/v1/updates", map[string]any{"target": "1.2.0"}); resp.StatusCode != want {
			t.Errorf("apply with %v: %d, want %d", err, resp.StatusCode, want)
		}
	}
}

func TestUpdates_走っているロールアウトはrollout_runningで断る(t *testing.T) {
	f := newFixture(t)
	f.srv.Updates = &fakeUpdates{err: domain.ErrRolloutRunning}
	resp, body := f.do(t, "POST", "/v1/updates", map[string]any{"target": "1.2.0"})
	if resp.StatusCode != 409 || errCode(body) != "rollout_running" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}
