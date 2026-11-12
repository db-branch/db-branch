package httpapi_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/settings"
	"github.com/db-branch/db-branch/pkg/ext"
)

func TestSettings_一覧と変更(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/v1/settings", nil); resp.StatusCode != 503 {
		t.Errorf("without settings: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{}}); resp.StatusCode != 503 {
		t.Errorf("put without settings: %d", resp.StatusCode)
	}
	f.srv.Settings = &settings.Service{
		Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "s.json")},
		Core:  []ext.Setting{{Key: "branch.memory_mb", Label: "Memory", Type: "int", Default: "128"}},
		Modules: func() map[string][]ext.Setting {
			return map[string][]ext.Setting{"proxysql": {{Key: "comment_routing", Label: "Comment routing", Type: "bool", Default: "true",
				Translations: map[string]ext.SettingText{"ja": {Label: "コメントで振り分ける", Help: "SQL のコメントで接続先を選ぶ"}}}}}
		},
	}
	_, body := f.do(t, "GET", "/v1/settings", nil)
	scopes := body["items"].([]any)
	core := scopes[0].(map[string]any)["settings"].([]any)[0].(map[string]any)
	if len(scopes) != 2 || core["value"] != "128" || core["label"] != "Memory" {
		t.Errorf("settings = %v", body)
	}
	mod := scopes[1].(map[string]any)["settings"].([]any)[0].(map[string]any)
	if ja, _ := mod["translations"].(map[string]any)["ja"].(map[string]any); ja["label"] != "コメントで振り分ける" || ja["help"] == "" {
		t.Errorf("module translations = %v", mod)
	}
	resp, body := f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"branch.memory_mb": "256"}})
	if resp.StatusCode != 200 || body["settings"].([]any)[0].(map[string]any)["value"] != "256" {
		t.Errorf("put: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"branch.memory_mb": "lots"}}); resp.StatusCode != 400 {
		t.Errorf("invalid: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/settings/ghost", map[string]any{"values": map[string]string{}}); resp.StatusCode != 404 {
		t.Errorf("unknown scope: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "PUT", "/v1/settings/core", map[string]any{"bad": 1}); resp.StatusCode != 400 {
		t.Errorf("bad body: %d", resp.StatusCode)
	}
	f.srv.Settings.(*settings.Service).Store = brokenSettings{}
	if resp, _ := f.do(t, "GET", "/v1/settings", nil); resp.StatusCode != 500 {
		t.Errorf("store failure: %d", resp.StatusCode)
	}
}

func TestCreateBranch_TTLを省くと既定のTTLを使う(t *testing.T) {
	f := newFixture(t)
	f.srv.DefaultTTL = func() time.Duration { return 48 * time.Hour }
	for _, in := range []map[string]any{{"name": "x", "base": "prod"}, {"name": "y", "base": "prod", "ttl": "1h"}} {
		_, body := f.do(t, "POST", "/v1/branches", in)
		waitOp(t, f, body["id"].(string))
	}
	got := strings.Join(f.branches.calls, "\n")
	if !strings.Contains(got, "create x base=prod snap= ttl=48h0m0s") || !strings.Contains(got, "create y base=prod snap= ttl=1h0m0s") {
		t.Errorf("calls:\n%s", got)
	}
}

type brokenSettings struct{}

func (brokenSettings) GetSettings(_ context.Context, _ string) (map[string]string, error) {
	return nil, errors.New("raft down")
}
func (brokenSettings) PutSettings(context.Context, string, map[string]string) error { return nil }
