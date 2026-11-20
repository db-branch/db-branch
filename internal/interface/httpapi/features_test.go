package httpapi_test

import (
	"context"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// mysqlWith makes the test's registry say mysql supports exactly what edit leaves.
func mysqlWith(t *testing.T, edit func(*domain.RuntimeCaps)) {
	t.Helper()
	old := domain.DefaultRuntimes
	spec, _ := old.Spec(domain.MySQL)
	edit(&spec.Capabilities)
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	domain.DefaultRuntimes = reg
	t.Cleanup(func() { domain.DefaultRuntimes = old })
}

func Testチェックポイントはエンジンがcheckpointsを宣言していなければ409_feature_not_supported(t *testing.T) {
	mysqlWith(t, func(c *domain.RuntimeCaps) { c.Checkpoints = false })
	f := checkpointFixture(t)

	for name, call := range map[string]func() (int, map[string]any){
		"list": func() (int, map[string]any) {
			r, b := f.do(t, "GET", "/v1/branches/x/checkpoints", nil)
			return r.StatusCode, b
		},
		"create": func() (int, map[string]any) {
			r, b := f.do(t, "POST", "/v1/branches/x/checkpoints", nil)
			return r.StatusCode, b
		},
		"restore": func() (int, map[string]any) {
			r, b := f.do(t, "POST", "/v1/branches/x/checkpoints/c1/restore", nil)
			return r.StatusCode, b
		},
	} {
		if code, body := call(); code != 409 || errCode(body) != "feature_not_supported" {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
	if len(f.branches.calls) != 0 {
		t.Errorf("the use case was called: %v", f.branches.calls)
	}
}

func Testアップグレードはエンジンがupgradeを宣言していなければ409_feature_not_supported(t *testing.T) {
	mysqlWith(t, func(c *domain.RuntimeCaps) { c.Upgrade = false })
	f := checkpointFixture(t)
	for _, in := range []map[string]any{{"runtime": "mysql@8.4.13"}, {"runtime": "mysql@8.4.13", "check_only": true}} {
		resp, body := f.do(t, "POST", "/v1/branches/x/upgrade", in)
		if resp.StatusCode != 409 || errCode(body) != "feature_not_supported" {
			t.Errorf("%v: %d %v", in, resp.StatusCode, body)
		}
	}
	// checkpoints are unaffected
	if resp, _ := f.do(t, "GET", "/v1/branches/x/checkpoints", nil); resp.StatusCode != 200 {
		t.Errorf("list checkpoints: %d", resp.StatusCode)
	}
}

func Testプロバイダの宣言がレジストリより優先される(t *testing.T) {
	f := checkpointFixture(t)
	q := &fakeQuery{noUpgrade: true}
	f.srv.Providers = providersOf(q)
	resp, body := f.do(t, "POST", "/v1/branches/x/upgrade", map[string]any{"runtime": "mysql@8.4.13", "check_only": true})
	if resp.StatusCode != 409 || errCode(body) != "feature_not_supported" {
		t.Errorf("provider without upgrade: %d %v", resp.StatusCode, body)
	}
	q.describe = context.DeadlineExceeded
	if resp, _ := f.do(t, "GET", "/v1/branches/x/checkpoints", nil); resp.StatusCode != 500 {
		t.Errorf("provider that cannot describe itself: %d", resp.StatusCode)
	}
}

func TestCapabilities_エンジン別の機能を返す(t *testing.T) {
	f := newFixture(t)
	_, body := f.do(t, "GET", "/v1/capabilities", nil)
	engines, ok := body["engines"].(map[string]any)
	if !ok || len(engines) != 2 {
		t.Fatalf("engines = %v", body["engines"])
	}
	my := engines["mysql"].(map[string]any)
	feat := my["features"].(map[string]any)
	if my["display_name"] != "MySQL" || feat["query"] != true || feat["checkpoints"] != true || feat["upgrade"] != true || feat["branch_merge"] != false || feat["live_freeze"] != false {
		t.Errorf("mysql = %v", my)
	}
	if len(feat) != len(domain.FeatureNames) {
		t.Errorf("features = %v: every feature is listed, true or false", feat)
	}
	ports := my["protocols"].([]any)
	if len(ports) != 1 || ports[0].(map[string]any)["name"] != "mysql" || ports[0].(map[string]any)["primary"] != true {
		t.Errorf("protocols = %v", ports)
	}
	pg := engines["postgres"].(map[string]any)
	if pg["collation_bound"] != true || pg["features"].(map[string]any)["history_record"] != false {
		t.Errorf("postgres = %v", pg)
	}
}

func TestCapabilities_登録された外部エンジンも載り_プロバイダの宣言を反映する(t *testing.T) {
	reg := swapRuntimes(t)
	if err := reg.Register(domain.EngineSpec{Engine: "clickhouse", DisplayName: "ClickHouse", Capabilities: domain.RuntimeCaps{Query: true, LiveFreeze: true}}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.srv.Providers = providersOf(&fakeQuery{noSQL: true}) // the mysql provider says it cannot run SQL
	_, body := f.do(t, "GET", "/v1/capabilities", nil)
	engines := body["engines"].(map[string]any)
	ch := engines["clickhouse"].(map[string]any)
	if ch["display_name"] != "ClickHouse" || ch["features"].(map[string]any)["live_freeze"] != true {
		t.Errorf("clickhouse = %v", ch)
	}
	if my := engines["mysql"].(map[string]any)["features"].(map[string]any); my["query"] != false {
		t.Errorf("the provider's declaration must win over the registry's: %v", my)
	}
}

func TestCapabilities_history_recordはモジュールが受け付けるときだけ真(t *testing.T) {
	f := newFixture(t)
	rec := &recorder{}
	f.srv.Ext = ext.Module{ServerOptioner: rec}
	_, body := f.do(t, "GET", "/v1/capabilities", nil)
	engines := body["engines"].(map[string]any)
	if engines["mysql"].(map[string]any)["features"].(map[string]any)["history_record"] != true {
		t.Errorf("mysql history_record = %v", engines["mysql"])
	}
	if engines["postgres"].(map[string]any)["features"].(map[string]any)["history_record"] != false {
		t.Errorf("postgres history_record = %v", engines["postgres"])
	}
	rec.err = ext.ErrForbidden // unlicensed
	_, body = f.do(t, "GET", "/v1/capabilities", nil)
	if body["engines"].(map[string]any)["mysql"].(map[string]any)["features"].(map[string]any)["history_record"] != false {
		t.Error("history_record must follow the licence")
	}
	f.srv.Ext = ext.Module{} // no module
	_, body = f.do(t, "GET", "/v1/capabilities", nil)
	if body["engines"].(map[string]any)["mysql"].(map[string]any)["features"].(map[string]any)["history_record"] != false {
		t.Error("history_record without a module")
	}
}
