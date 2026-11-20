package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// manifestJSON is the example of the design (§5.2) with the digests the signature covers.
func manifestJSON(mutate func(string) string) []byte {
	s := `{
  "schema_version": 1,
  "engine": "clickhouse",
  "display_name": "ClickHouse",
  "module_version": "0.1.0",
  "api_version": 1,
  "binary": "/usr/lib/db-branch/runtimes/clickhouse/db-branch-rt-clickhouse",
  "binary_sha256": "` + sha + `",
  "catalog": "/usr/lib/db-branch/runtimes/clickhouse/runtimes.json",
  "catalog_sha256": "` + sha + `",
  "run_as_user": "db-branch-rt-clickhouse",
  "collation_bound": false,
  "zfs_props": { "recordsize": "128K", "compression": "lz4" },
  "protocols": [ { "name": "http", "primary": true }, { "name": "native", "primary": false } ],
  "capabilities": { "query": true, "checkpoints": true, "live_freeze": true, "upgrade": true, "source_dump": false,
                    "branch_merge": false, "history_record": false, "update_stats": false, "instance_metrics": true },
  "systemd": { "service_type": "simple", "kill_signal": "SIGTERM", "timeout_stop_sec": 300 }
}`
	if mutate != nil {
		s = mutate(s)
	}
	return []byte(s)
}

func TestParseManifest_設計書の例を読んでEngineSpecにする(t *testing.T) {
	m, err := domain.ParseManifest(manifestJSON(nil))
	if err != nil {
		t.Fatal(err)
	}
	spec := m.Spec()
	if spec.Engine != "clickhouse" || spec.DisplayName != "ClickHouse" || spec.RunAsUser != "db-branch-rt-clickhouse" || spec.CollationBound {
		t.Errorf("spec = %+v", spec)
	}
	if len(spec.Protocols) != 2 || spec.Protocols[0].Name != "http" || !spec.Protocols[0].Primary || spec.Protocols[1].Primary {
		t.Errorf("protocols = %+v", spec.Protocols)
	}
	c := spec.Capabilities
	if !c.Query || !c.Checkpoints || !c.LiveFreeze || !c.Upgrade || !c.InstanceMetrics || c.SourceDump || c.BranchMerge || c.HistoryRecord || c.UpdateStats {
		t.Errorf("capabilities = %+v", c)
	}
	if spec.ZFSProps["recordsize"] != "128K" {
		t.Errorf("zfs props = %v", spec.ZFSProps)
	}
	if m.BinarySHA256 != sha || m.Systemd.TimeoutStopSec != 300 {
		t.Errorf("manifest = %+v", m)
	}
	if err := domain.NewRuntimeRegistry().Register(spec); err != nil {
		t.Errorf("a spec read from a valid manifest must register: %v", err)
	}
}

func TestParseManifest_run_as_userは空かエンジン専用ユーザーだけ(t *testing.T) {
	for _, user := range []string{"root", "db-branch", "db-branch-admin", "db-branch-rt-mysql", "db-branch-rt-clickhouse2", "keisuke", "DB-BRANCH-RT-CLICKHOUSE"} {
		_, err := domain.ParseManifest(manifestJSON(func(s string) string {
			return strings.Replace(s, `"run_as_user": "db-branch-rt-clickhouse"`, `"run_as_user": "`+user+`"`, 1)
		}))
		if !errors.Is(err, domain.ErrInvalidManifest) {
			t.Errorf("run_as_user %q: err = %v, want ErrInvalidManifest", user, err)
		}
	}
	m, err := domain.ParseManifest(manifestJSON(func(s string) string {
		return strings.Replace(s, `"run_as_user": "db-branch-rt-clickhouse",`, ``, 1)
	}))
	if err != nil || m.Spec().RunAsUser != "db-branch-rt-clickhouse" {
		t.Errorf("an omitted run_as_user must become the engine's own user: %v %+v", err, m.Spec().RunAsUser)
	}
}

func TestParseManifest_不正なマニフェストを拒否する(t *testing.T) {
	replace := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				panic("test bug: " + old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	cases := map[string]func(string) string{
		"未来のschema":            replace(`"schema_version": 1`, `"schema_version": 2`),
		"api_versionが未対応":      replace(`"api_version": 1`, `"api_version": 2`),
		"組み込みのmysqlを名乗る":       replace(`"engine": "clickhouse"`, `"engine": "mysql"`),
		"組み込みのpostgresを名乗る":    replace(`"engine": "clickhouse"`, `"engine": "postgres"`),
		"エンジン名が不正":             replace(`"engine": "clickhouse"`, `"engine": "Click House"`),
		"表示名が空":                replace(`"display_name": "ClickHouse"`, `"display_name": ""`),
		"表示名に制御文字":             replace(`"display_name": "ClickHouse"`, `"display_name": "Click\nHouse"`),
		"binaryが相対パス":          replace(`"binary": "/usr/lib/db-branch/runtimes/clickhouse/db-branch-rt-clickhouse"`, `"binary": "db-branch-rt-clickhouse"`),
		"binaryが別のエンジンのディレクトリ": replace(`runtimes/clickhouse/db-branch-rt-clickhouse"`, `runtimes/other/db-branch-rt-clickhouse"`),
		"binaryに..":            replace(`runtimes/clickhouse/db-branch-rt-clickhouse"`, `runtimes/clickhouse/../../../../bin/sh"`),
		"binaryがディレクトリ自身":      replace(`"binary": "/usr/lib/db-branch/runtimes/clickhouse/db-branch-rt-clickhouse"`, `"binary": "/usr/lib/db-branch/runtimes/clickhouse/"`),
		"catalogが範囲外":          replace(`"catalog": "/usr/lib/db-branch/runtimes/clickhouse/runtimes.json"`, `"catalog": "/etc/passwd"`),
		"binary_sha256が空":      replace(`"binary_sha256": "`+sha+`"`, `"binary_sha256": ""`),
		"catalog_sha256が短い":    replace(`"catalog_sha256": "`+sha+`"`, `"catalog_sha256": "abc"`),
		"sha256が大文字":           replace(`"binary_sha256": "`+sha+`"`, `"binary_sha256": "`+strings.ToUpper(sha)+`"`),
		"未知のcapability":        replace(`"query": true`, `"teleport": true, "query": true`),
		"primaryなし":            replace(`{ "name": "http", "primary": true }`, `{ "name": "http", "primary": false }`),
		"許可外のZFSプロパティ":         replace(`"compression": "lz4"`, `"sharenfs": "on"`),
		"未知のservice_type":      replace(`"service_type": "simple"`, `"service_type": "oneshot"`),
		"未知のkill_signal":       replace(`"kill_signal": "SIGTERM"`, `"kill_signal": "SIGUSR3"`),
		"負のtimeout":            replace(`"timeout_stop_sec": 300`, `"timeout_stop_sec": -1`),
		"未知のトップレベル項目":          replace(`"collation_bound": false,`, `"collation_bound": false, "exec_start_pre": "/bin/sh",`),
		"JSONではない":             func(string) string { return "not json" },
		"JSONの後ろにゴミ":           func(s string) string { return s + `{"x":1}` },
		"空":                    func(string) string { return "" },
	}
	for name, mutate := range cases {
		if _, err := domain.ParseManifest(manifestJSON(mutate)); !errors.Is(err, domain.ErrInvalidManifest) {
			t.Errorf("%s: err = %v, want ErrInvalidManifest", name, err)
		}
	}
}

func TestCapsFromFeatures_宣言された機能名を機能フラグにする(t *testing.T) {
	caps, err := domain.CapsFromFeatures(map[string]bool{"query": true, "branch_merge": false, "live_freeze": true})
	if err != nil || !caps.Query || !caps.LiveFreeze || caps.BranchMerge || caps.Checkpoints {
		t.Errorf("caps = %+v, %v", caps, err)
	}
	if _, err := domain.CapsFromFeatures(map[string]bool{"nope": true}); !errors.Is(err, domain.ErrInvalidManifest) {
		t.Errorf("unknown feature = %v", err)
	}
	// the round trip with Features()
	round, err := domain.CapsFromFeatures(domain.RuntimeCaps{Query: true, UpdateStats: true}.Features())
	if err != nil || !round.Query || !round.UpdateStats || round.Upgrade {
		t.Errorf("round trip = %+v, %v", round, err)
	}
}

func TestParseManifestIn_別のルートの下のパスだけを受理する(t *testing.T) {
	in := manifestJSON(func(s string) string { return strings.ReplaceAll(s, "/usr/lib/db-branch/runtimes", "/opt/rt") })
	if _, err := domain.ParseManifestIn("/opt/rt", in); err != nil {
		t.Errorf("under its own root = %v", err)
	}
	if _, err := domain.ParseManifestIn("/opt/rt", manifestJSON(nil)); !errors.Is(err, domain.ErrInvalidManifest) {
		t.Errorf("the default root's paths under /opt/rt = %v", err)
	}
	if _, err := domain.ParseManifest(in); !errors.Is(err, domain.ErrInvalidManifest) {
		t.Errorf("/opt/rt paths under the real root = %v", err)
	}
	for _, root := range []string{"", "relative/rt", "/opt/rt/", "/opt/../rt"} {
		if _, err := domain.ParseManifestIn(root, in); !errors.Is(err, domain.ErrInvalidManifest) {
			t.Errorf("root %q = %v, want ErrInvalidManifest", root, err)
		}
	}
}
