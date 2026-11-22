package httpapi_test

import (
	"context"
	"errors"
	"fmt"
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

func secretFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.srv.Settings = &settings.Service{
		Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "s.json")},
		Core: []ext.Setting{
			{Key: "license.key", Label: "License key", Type: "secret"},
			{Key: "branch.open_password", Label: "Open password", Type: "secret", Default: "dbbranch"},
			{Key: "name", Label: "Name", Type: "string"},
		},
	}
	return f
}

func settingOf(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	for _, sc := range body["items"].([]any) {
		for _, s := range sc.(map[string]any)["settings"].([]any) {
			if s.(map[string]any)["key"] == key {
				return s.(map[string]any)
			}
		}
	}
	t.Fatalf("no setting %s in %v", key, body)
	return nil
}

func TestSettings_GETレスポンスでSecret型設定値が平文返却されずsecret_setフラグで返る(t *testing.T) {
	f := secretFixture(t)
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"license.key": "TOPSECRET", "name": "plain"}})
	resp, body := f.do(t, "GET", "/v1/settings", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(fmt.Sprint(body), "TOPSECRET") {
		t.Errorf("secret leaked: %v", body)
	}
	if s := settingOf(t, body, "license.key"); s["value"] != "" || s["secret_set"] != true {
		t.Errorf("set secret = %v", s)
	}
	if s := settingOf(t, body, "branch.open_password"); s["value"] != "" || s["secret_set"] != false {
		t.Errorf("default-only secret = %v", s)
	}
	if s := settingOf(t, body, "name"); s["value"] != "plain" {
		t.Errorf("plain setting = %v", s)
	} else if _, ok := s["secret_set"]; ok {
		t.Errorf("non-secret has secret_set: %v", s)
	}
}

func TestSettings_PUT時に空または未指定のSecret型は既存値を保持し明示クリアで削除できる(t *testing.T) {
	f := secretFixture(t)
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"license.key": "TOPSECRET"}})
	stored := func() string {
		v, _ := f.srv.Settings.Values(context.Background(), "core")
		return v["license.key"]
	}
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"license.key": "", "name": "x"}})
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"name": "y"}})
	if stored() != "TOPSECRET" {
		t.Fatalf("secret lost: %q", stored())
	}
	resp, body := f.do(t, "PUT", "/v1/settings/core", map[string]any{"clear": []string{"license.key"}})
	if resp.StatusCode != 200 || stored() != "" {
		t.Errorf("clear: %d %q", resp.StatusCode, stored())
	}
	if s := body["settings"].([]any)[0].(map[string]any); s["secret_set"] != false {
		t.Errorf("answer after clear = %v", s)
	}
	// Clearing a non-secret or unknown key is refused.
	for _, key := range []string{"name", "ghost"} {
		if resp, _ := f.do(t, "PUT", "/v1/settings/core", map[string]any{"clear": []string{key}}); resp.StatusCode != 400 {
			t.Errorf("clear %s: %d", key, resp.StatusCode)
		}
	}
}

func TestSettings_setting_updatedイベントに平文のSecretを含めない(t *testing.T) {
	f := secretFixture(t)
	sub := f.srv.Bus.Subscribe(0)
	defer sub.Close()
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"license.key": "TOPSECRET"}})
	ev := <-sub.Events()
	if ev.Type != "setting.updated" || strings.Contains(fmt.Sprint(ev.Data), "TOPSECRET") {
		t.Errorf("event = %+v", ev)
	}
}

func TestSettings_clearの境界(t *testing.T) {
	f := secretFixture(t)
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"license.key": "OLD"}})
	// A new value sent with the same key in clear wins.
	f.do(t, "PUT", "/v1/settings/core", map[string]any{"values": map[string]string{"license.key": "NEW"}, "clear": []string{"license.key"}})
	if v, _ := f.srv.Settings.Values(context.Background(), "core"); v["license.key"] != "NEW" {
		t.Errorf("value = %q", v["license.key"])
	}
	// An unknown scope is not found, with or without clear.
	if resp, _ := f.do(t, "PUT", "/v1/settings/ghost", map[string]any{"clear": []string{"license.key"}}); resp.StatusCode != 404 {
		t.Errorf("unknown scope: %d", resp.StatusCode)
	}
}

func TestSettings_モジュールのsecretもマスクされる(t *testing.T) {
	f := newFixture(t)
	f.srv.Settings = &settings.Service{
		Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "s.json")},
		Modules: func() map[string][]ext.Setting {
			return map[string][]ext.Setting{"auth": {{Key: "oidc.secret", Label: "Secret", Type: "secret"}}}
		},
	}
	f.do(t, "PUT", "/v1/settings/auth", map[string]any{"values": map[string]string{"oidc.secret": "MODSECRET"}})
	_, body := f.do(t, "GET", "/v1/settings", nil)
	if strings.Contains(fmt.Sprint(body), "MODSECRET") || settingOf(t, body, "oidc.secret")["secret_set"] != true {
		t.Errorf("module secret = %v", body)
	}
}
