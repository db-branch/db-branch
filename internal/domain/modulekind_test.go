package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestModuleSpec_従来のモジュールの保存形式は変わらず_新しい項目は空のとき書かない(t *testing.T) {
	b, _ := json.Marshal(domain.ModuleSpec{Name: "auth", Version: "1.0", Wanted: true})
	if string(b) != `{"name":"auth","version":"1.0","wanted":true}` {
		t.Errorf("a plain module is stored as %s", b)
	}
	var old domain.ModuleSpec
	if err := json.Unmarshal([]byte(`{"name":"auth","version":"1.0","wanted":true}`), &old); err != nil || old.IsRuntime() || old.Kind != "" || old.Source != "" {
		t.Errorf("state written before runtime modules = %+v, %v", old, err)
	}
	rt := domain.ModuleSpec{Name: "rt-clickhouse", Version: "0.1.0", Wanted: true, Kind: domain.ModuleKindRuntime, Source: domain.SourceLocal}
	b, _ = json.Marshal(rt)
	if string(b) != `{"name":"rt-clickhouse","version":"0.1.0","wanted":true,"kind":"runtime","source":"local"}` {
		t.Errorf("a runtime module is stored as %s", b)
	}
	var back domain.ModuleSpec
	if err := json.Unmarshal(b, &back); err != nil || back != rt || !back.IsRuntime() || back.Engine() != "clickhouse" {
		t.Errorf("round trip = %+v, %v", back, err)
	}
}

func TestModuleSpec_ランタイムモジュールのパッケージは_db_branch_rt_エンジン名(t *testing.T) {
	if got := (domain.ModuleSpec{Name: "auth"}).Package(); got != "db-branch-auth" {
		t.Errorf("module package = %q", got)
	}
	rt := domain.ModuleSpec{Name: domain.RuntimeModuleName("clickhouse"), Kind: domain.ModuleKindRuntime}
	if rt.Name != "rt-clickhouse" || rt.Package() != "db-branch-rt-clickhouse" || rt.Engine() != "clickhouse" {
		t.Errorf("runtime module = %+v, package %q, engine %q", rt, rt.Package(), rt.Engine())
	}
	if (domain.ModuleSpec{Name: "auth"}).Engine() != "" {
		t.Error("a plain module has no engine")
	}
}

func TestValidModuleName_rt_で始まる名前はランタイムモジュール用に予約する(t *testing.T) {
	for _, n := range []string{"rt-clickhouse", "rt-", "rt-x"} {
		if err := domain.ValidModuleName(n); !errors.Is(err, domain.ErrInvalidModule) {
			t.Errorf("ValidModuleName(%q) = %v, want ErrInvalidModule", n, err)
		}
	}
	for _, n := range []string{"auth", "rtx", "art-x", "proxysql"} {
		if err := domain.ValidModuleName(n); err != nil {
			t.Errorf("ValidModuleName(%q) = %v", n, err)
		}
	}
}

func TestValidRuntimeModuleEngine_組み込み_長すぎる_不正な名前を拒否する(t *testing.T) {
	for _, e := range []string{"mysql", "postgres", "Click House", "", "a/b", "abcdefghijklmnopqrstu", "1db"} {
		if err := domain.ValidRuntimeModuleEngine(e); !errors.Is(err, domain.ErrInvalidModule) {
			t.Errorf("ValidRuntimeModuleEngine(%q) = %v, want ErrInvalidModule", e, err)
		}
	}
	for _, e := range []string{"clickhouse", "duckdb", "abcdefghijklmnopqrs"} {
		if err := domain.ValidRuntimeModuleEngine(e); err != nil {
			t.Errorf("ValidRuntimeModuleEngine(%q) = %v", e, err)
		}
	}
}

func TestPendingModules_ランタイムモジュールはプラグインの一覧と突き合わせない(t *testing.T) {
	specs := []domain.ModuleSpec{
		{Name: "auth", Version: "1", Wanted: true},
		{Name: "rt-clickhouse", Version: "1", Wanted: true, Kind: domain.ModuleKindRuntime},
	}
	// the plugin list never holds an engine: a runtime module must not lock the API out
	if got := domain.PendingModules(specs, []string{"auth"}); len(got) != 0 {
		t.Errorf("PendingModules = %v", got)
	}
	if got := domain.PendingModules(specs, nil); len(got) != 1 || got[0] != "auth" {
		t.Errorf("PendingModules = %v", got)
	}
}

func TestPendingRuntimeModules_読み込まれていない希望のエンジンを返す(t *testing.T) {
	specs := []domain.ModuleSpec{
		{Name: "auth", Version: "1", Wanted: true},
		{Name: "rt-clickhouse", Version: "1", Wanted: true, Kind: domain.ModuleKindRuntime},
		{Name: "rt-duckdb", Version: "1", Wanted: true, Kind: domain.ModuleKindRuntime, Source: domain.SourceLocal},
		{Name: "rt-gone", Version: "1", Wanted: false, Kind: domain.ModuleKindRuntime},
	}
	got := domain.PendingRuntimeModules(specs, []string{"clickhouse"})
	if len(got) != 1 || got[0] != "duckdb" {
		t.Errorf("PendingRuntimeModules = %v, want [duckdb] (a local one is waited for too)", got)
	}
	if got := domain.PendingRuntimeModules(specs, nil); len(got) != 2 {
		t.Errorf("PendingRuntimeModules = %v", got)
	}
}

func TestProtocolRuntimeModules_設計書が予約した21(t *testing.T) {
	if domain.ProtocolRuntimeModules != 21 {
		t.Errorf("ProtocolRuntimeModules = %d", domain.ProtocolRuntimeModules)
	}
}
