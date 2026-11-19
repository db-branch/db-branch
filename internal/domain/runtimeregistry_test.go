package domain_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestRuntimeRegistry_登録された任意のエンジンをParseRuntimeRefで受理し未登録エンジンを拒否できる(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: "clickhouse", DisplayName: "ClickHouse"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := reg.Parse("clickhouse@25.3.1")
	if err != nil {
		t.Fatalf("Parse(clickhouse@25.3.1): %v", err)
	}
	if want := (domain.RuntimeRef{Engine: "clickhouse", Version: "25.3.1"}); got != want {
		t.Errorf("Parse = %+v, want %+v", got, want)
	}

	if _, err := reg.Parse("oracle@19.0"); !errors.Is(err, domain.ErrInvalidRuntime) {
		t.Errorf("Parse(oracle@19.0) error = %v, want ErrInvalidRuntime", err)
	}
}

func TestRuntimeRegistry_不正なEngineSpecの登録を拒否する(t *testing.T) {
	cases := map[string]domain.EngineSpec{
		"空の名前":       {},
		"大文字":        {Engine: "ClickHouse"},
		"数字始まり":      {Engine: "1db"},
		"長すぎる名前":     {Engine: domain.Engine("a" + strings.Repeat("b", 31))},
		"@を含む":       {Engine: "my@sql"},
		"rootで動かす":   {Engine: "evil", RunAsUser: "root"},
		"Primaryなし":  {Engine: "x", Protocols: []domain.ProtocolPort{{Name: "http"}}},
		"Primaryが2つ": {Engine: "x", Protocols: []domain.ProtocolPort{{Name: "a", Primary: true}, {Name: "b", Primary: true}}},
		"プロトコル名の重複":  {Engine: "x", Protocols: []domain.ProtocolPort{{Name: "a", Primary: true}, {Name: "a"}}},
		"プロトコル名が空":   {Engine: "x", Protocols: []domain.ProtocolPort{{Primary: true}}},
		"正規表現が壊れている": {Engine: "x", VersionPattern: "("},
	}
	for name, spec := range cases {
		reg := domain.NewRuntimeRegistry()
		if err := reg.Register(spec); !errors.Is(err, domain.ErrInvalidEngineSpec) {
			t.Errorf("%s: Register error = %v, want ErrInvalidEngineSpec", name, err)
		}
		if _, ok := reg.Spec(spec.Engine); ok {
			t.Errorf("%s: rejected spec was registered", name)
		}
	}
}

func TestRuntimeRegistry_同じエンジンの二重登録を拒否する(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: "clickhouse"}); err != nil {
		t.Fatal(err)
	}
	err := reg.Register(domain.EngineSpec{Engine: "clickhouse", DisplayName: "other"})
	if !errors.Is(err, domain.ErrInvalidEngineSpec) {
		t.Errorf("duplicate Register error = %v, want ErrInvalidEngineSpec", err)
	}
	if s, _ := reg.Spec("clickhouse"); s.DisplayName != "" {
		t.Errorf("duplicate replaced the first spec: %+v", s)
	}
}

func TestDefaultRuntimes_MySQLとPostgreSQLが組み込みで登録されている(t *testing.T) {
	my, ok := domain.DefaultRuntimes.Spec(domain.MySQL)
	if !ok || my.DisplayName != "MySQL" || my.CollationBound {
		t.Errorf("mysql spec = %+v, %v", my, ok)
	}
	pg, ok := domain.DefaultRuntimes.Spec(domain.Postgres)
	if !ok || pg.DisplayName != "PostgreSQL" || !pg.CollationBound {
		t.Errorf("postgres spec = %+v, %v", pg, ok)
	}
	for _, s := range []domain.EngineSpec{my, pg} {
		if len(s.Protocols) != 1 || !s.Protocols[0].Primary {
			t.Errorf("%s protocols = %+v, want one primary port", s.Engine, s.Protocols)
		}
		if !s.Capabilities.Query || !s.Capabilities.Upgrade || !s.Capabilities.Checkpoints {
			t.Errorf("%s capabilities = %+v", s.Engine, s.Capabilities)
		}
	}
	if !my.Capabilities.HistoryRecord || pg.Capabilities.HistoryRecord {
		t.Errorf("history record: mysql %v postgres %v, want true/false (today only MySQL records)", my.Capabilities.HistoryRecord, pg.Capabilities.HistoryRecord)
	}
}

func TestParseRuntimeRef_既定のレジストリに後から登録したエンジンも受理する(t *testing.T) {
	if err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "parsetest", VersionPattern: `^[0-9]{2}\.[0-9]+$`}); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.ParseRuntimeRef("parsetest@25.3"); err != nil {
		t.Errorf("registered engine refused: %v", err)
	}
	if _, err := domain.ParseRuntimeRef("parsetest@25.3.1"); !errors.Is(err, domain.ErrInvalidRuntime) {
		t.Errorf("version outside the engine's pattern: error = %v, want ErrInvalidRuntime", err)
	}
	var r domain.RuntimeRef
	if err := r.UnmarshalText([]byte("parsetest@25.3")); err != nil || r.Engine != "parsetest" {
		t.Errorf("UnmarshalText = %+v, %v", r, err)
	}
}

func TestCollationBound_登録されたエンジンの宣言に従い未登録は偽(t *testing.T) {
	if err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "collbound", CollationBound: true}); err != nil {
		t.Fatal(err)
	}
	if !domain.CollationBound("collbound") || domain.CollationBound("unregistered") {
		t.Error("CollationBound must follow the registered spec")
	}
}

func TestBranchDSN_エンジンの宣言したFormatDSNに委譲する(t *testing.T) {
	err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "dsntest", FormatDSN: func(host string, port int, user, password string) string {
		return "dsntest://" + user + ":" + password + "@" + host + ":" + strconv.Itoa(port) + "/db1"
	}})
	if err != nil {
		t.Fatal(err)
	}
	b := domain.Branch{Runtime: domain.RuntimeRef{Engine: "dsntest", Version: "1.0"}, User: "u", Password: "p", Port: 9}
	if got := b.DSN("h"); got != "dsntest://u:p@h:9/db1" {
		t.Errorf("DSN = %q", got)
	}
}

func TestBranchDSN_FormatDSNのないエンジンは標準の形になる(t *testing.T) {
	if err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "plaindsn"}); err != nil {
		t.Fatal(err)
	}
	b := domain.Branch{Runtime: domain.RuntimeRef{Engine: "plaindsn", Version: "1.0"}, User: "u", Password: "p", Port: 9}
	if got := b.DSN("h"); got != "plaindsn://u:p@h:9/" {
		t.Errorf("DSN = %q", got)
	}
}

func TestCheckUpgradePath_エンジンの宣言した検証に委譲する(t *testing.T) {
	veto := errors.New("veto")
	err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "uptest", CheckUpgradePath: func(from, to domain.RuntimeRef) error { return veto }})
	if err != nil {
		t.Fatal(err)
	}
	from, to := domain.RuntimeRef{Engine: "uptest", Version: "1.0"}, domain.RuntimeRef{Engine: "uptest", Version: "1.1"}
	if got := domain.CheckUpgradePath(from, to); !errors.Is(got, veto) {
		t.Errorf("CheckUpgradePath = %v, want the engine's verdict", got)
	}
	// engine-independent rules still come first
	if got := domain.CheckUpgradePath(to, from); !errors.Is(got, domain.ErrUpgradeRefused) {
		t.Errorf("downgrade = %v, want ErrUpgradeRefused", got)
	}
}

func TestCheckUpgradePath_検証を宣言しないエンジンは新しい版への移行を許す(t *testing.T) {
	if err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "nocheck"}); err != nil {
		t.Fatal(err)
	}
	from, to := domain.RuntimeRef{Engine: "nocheck", Version: "1.0"}, domain.RuntimeRef{Engine: "nocheck", Version: "2.0"}
	if err := domain.CheckUpgradePath(from, to); err != nil {
		t.Errorf("CheckUpgradePath = %v", err)
	}
}

func TestEngineSpec_DSNは宣言したFormatDSNかなければ標準の形で組み立てる(t *testing.T) {
	plain := domain.EngineSpec{Engine: "x"}
	if got := plain.DSN("::1", 5, "u", "p"); got != "x://u:p@[::1]:5/" {
		t.Errorf("DSN = %q", got)
	}
	custom := domain.EngineSpec{Engine: "x", FormatDSN: func(host string, port int, user, password string) string { return "custom" }}
	if got := custom.DSN("h", 1, "u", "p"); got != "custom" {
		t.Errorf("DSN = %q", got)
	}
}

func TestRuntimeRegistry_CheckUpgradePathは自分のレジストリの宣言で判定する(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	veto := errors.New("veto")
	if err := reg.Register(domain.EngineSpec{Engine: "x", CheckUpgradePath: func(from, to domain.RuntimeRef) error { return veto }}); err != nil {
		t.Fatal(err)
	}
	from, to := domain.RuntimeRef{Engine: "x", Version: "1.0"}, domain.RuntimeRef{Engine: "x", Version: "1.1"}
	if err := reg.CheckUpgradePath(from, to); !errors.Is(err, veto) {
		t.Errorf("CheckUpgradePath = %v, want the engine's verdict", err)
	}
	// the same refs are unknown to the default registry, which has no verdict on them
	if err := domain.CheckUpgradePath(from, to); err != nil {
		t.Errorf("default registry = %v", err)
	}
}
