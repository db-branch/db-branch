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
		"正規表現が壊れている": {Engine: "x", VersionPattern: "^($"},
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
	swapDefaultRuntimes(t)
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
	swapDefaultRuntimes(t)
	if err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "collbound", CollationBound: true}); err != nil {
		t.Fatal(err)
	}
	if !domain.CollationBound("collbound") || domain.CollationBound("unregistered") {
		t.Error("CollationBound must follow the registered spec")
	}
}

func TestBranchDSN_エンジンの宣言したFormatDSNに委譲する(t *testing.T) {
	swapDefaultRuntimes(t)
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
	swapDefaultRuntimes(t)
	if err := domain.DefaultRuntimes.Register(domain.EngineSpec{Engine: "plaindsn"}); err != nil {
		t.Fatal(err)
	}
	b := domain.Branch{Runtime: domain.RuntimeRef{Engine: "plaindsn", Version: "1.0"}, User: "u", Password: "p", Port: 9}
	if got := b.DSN("h"); got != "plaindsn://u:p@h:9/" {
		t.Errorf("DSN = %q", got)
	}
}

func TestCheckUpgradePath_エンジンの宣言した検証に委譲する(t *testing.T) {
	swapDefaultRuntimes(t)
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
	swapDefaultRuntimes(t)
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

// swapDefaultRuntimes gives the test its own copy of the built-in registry, so
// engines it registers do not outlive it (go test -count=2 stays green).
func swapDefaultRuntimes(t *testing.T) {
	t.Helper()
	old := domain.DefaultRuntimes
	domain.DefaultRuntimes = domain.NewBuiltinRuntimes()
	t.Cleanup(func() { domain.DefaultRuntimes = old })
}

func TestNewBuiltinRuntimes_毎回独立したMySQLとPostgreSQLだけのレジストリを返す(t *testing.T) {
	a, b := domain.NewBuiltinRuntimes(), domain.NewBuiltinRuntimes()
	if err := a.Register(domain.EngineSpec{Engine: "extra"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Spec("extra"); ok {
		t.Error("registries share state")
	}
	if got := b.Engines(); len(got) != 2 || got[0] != domain.MySQL || got[1] != domain.Postgres {
		t.Errorf("Engines = %v", got)
	}
}

func TestRuntimeRegistry_VersionPatternは前後を固定した式だけを受理する(t *testing.T) {
	for _, pat := range []string{`[0-9]+`, `^[0-9]+`, `[0-9]+$`} {
		reg := domain.NewRuntimeRegistry()
		if err := reg.Register(domain.EngineSpec{Engine: "x", VersionPattern: pat}); !errors.Is(err, domain.ErrInvalidEngineSpec) {
			t.Errorf("pattern %q: Register = %v, want ErrInvalidEngineSpec", pat, err)
		}
	}
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: "x", VersionPattern: `^[0-9]+$`}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Parse("x@1/../../y"); !errors.Is(err, domain.ErrInvalidRuntime) {
		t.Errorf("Parse with a path in the version = %v", err)
	}
}

func TestRuntimeRegistry_登録したSpecは呼び出し側の変更から守られる(t *testing.T) {
	props := map[string]string{"recordsize": "128K"}
	ports := []domain.ProtocolPort{{Name: "http", Primary: true}}
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: "x", ZFSProps: props, Protocols: ports}); err != nil {
		t.Fatal(err)
	}
	props["recordsize"], ports[0].Name = "8K", "evil" // the registrant changes its own values later
	got, _ := reg.Spec("x")
	got.ZFSProps["recordsize"], got.Protocols[0].Name = "4K", "worse" // so does a reader
	again, _ := reg.Spec("x")
	if again.ZFSProps["recordsize"] != "128K" || again.Protocols[0].Name != "http" {
		t.Errorf("the registered spec changed: %+v", again)
	}
}

func TestRuntimeRegistry_VersionPatternの選択肢やパス区切りを許す式を拒否する(t *testing.T) {
	for _, pat := range []string{`^[0-9]+\.[0-9]+|x$`, `^.+$`, `^[0-9./]+$`, `^\S+$`, `^(1|[^ ]+)$`, `^[0-9]*\.?[0-9]*$`, `^[^/\\ \n.]+$`, `^[0-9a-z%$;-]+$`, `^[a-z/]+$`, `^[a-z ]+$`, `^[a-z%]+$`, `^[a-z;]+$`, `^[a-z\t]+$`, `^[a-z\\]+$`} {
		reg := domain.NewRuntimeRegistry()
		if err := reg.Register(domain.EngineSpec{Engine: "x", VersionPattern: pat}); !errors.Is(err, domain.ErrInvalidEngineSpec) {
			t.Errorf("pattern %q: Register = %v, want ErrInvalidEngineSpec", pat, err)
		}
	}
	// an alternation inside the anchors is still fine
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: "x", VersionPattern: `^(25|26)\.[0-9]+$`}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Parse("x@25.3"); err != nil {
		t.Errorf("Parse = %v", err)
	}
	if _, err := reg.Parse("x@27.3"); !errors.Is(err, domain.ErrInvalidRuntime) {
		t.Errorf("Parse(27.3) = %v", err)
	}
}

func TestEngineSpec_Cloneは元と独立した複製を返す(t *testing.T) {
	a := domain.EngineSpec{Engine: "x", ZFSProps: map[string]string{"k": "v"}, Protocols: []domain.ProtocolPort{{Name: "p", Primary: true}}}
	b := a.Clone()
	b.ZFSProps["k"], b.Protocols[0].Name = "w", "q"
	if a.ZFSProps["k"] != "v" || a.Protocols[0].Name != "p" {
		t.Errorf("Clone shares state: %+v", a)
	}
}

func TestRuntimeRegistry_CheckUpgradePath_数字で始まらない版は文字列として順序づける(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: "rolling", VersionPattern: `^[a-z]+[0-9]+$`}); err != nil {
		t.Fatal(err)
	}
	ref := func(v string) domain.RuntimeRef { return domain.RuntimeRef{Engine: "rolling", Version: v} }
	cases := []struct {
		from, to string
		want     string // "" = allowed
	}{
		{"v1", "v2", ""},
		{"v2", "v10", ""}, // 10 is after 2, not before "2" as text
		{"v10", "v2", "downgrade"},
		{"v2", "v1", "downgrade"},
		{"v2", "v2", "already runs"},
		{"alpha1", "beta1", ""},
	}
	for _, c := range cases {
		err := reg.CheckUpgradePath(ref(c.from), ref(c.to))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s -> %s: %v", c.from, c.to, err)
		case c.want != "" && (!errors.Is(err, domain.ErrUpgradeRefused) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s -> %s: err = %v, want %q", c.from, c.to, err, c.want)
		}
	}
}

func TestRuntimeRegistry_CheckUpgradePath_段数が多い版や末尾が英字の版も順序づける(t *testing.T) {
	reg := domain.NewRuntimeRegistry()
	if err := reg.Register(domain.EngineSpec{Engine: "ch", VersionPattern: `^v?[0-9]+(\.[0-9]+)*[a-z]*$`}); err != nil {
		t.Fatal(err)
	}
	ref := func(v string) domain.RuntimeRef { return domain.RuntimeRef{Engine: "ch", Version: v} }
	cases := []struct {
		from, to string
		want     string // "" = allowed
	}{
		{"25.3.1.2703", "25.3.2.100", ""},
		{"25.3.2.100", "25.3.1.2703", "downgrade"},
		{"25.3.1", "25.3.2.100", ""},
		{"25.3.2.100", "25.3.1", "downgrade"},
		{"25.3.1.2703", "25.3.1.2703", "already runs"},
		{"25.3.1.9", "25.3.1.10", ""},
		{"2026a", "2027a", ""},
		{"2027a", "2026a", "downgrade"},
		{"v1", "v1a", ""},
		{"v1a", "v1", "downgrade"},
		{"v1a", "v1b", ""},
		{"v1b", "v1a", "downgrade"},
		{"v007", "v7", "already runs"},
	}
	for _, c := range cases {
		err := reg.CheckUpgradePath(ref(c.from), ref(c.to))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s -> %s: %v", c.from, c.to, err)
		case c.want != "" && (!errors.Is(err, domain.ErrUpgradeRefused) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s -> %s: err = %v, want %q", c.from, c.to, err, c.want)
		}
	}
}

func TestCheckUpgradePath_組み込みエンジンの版の接尾辞は従来どおり数に含めない(t *testing.T) {
	err := domain.CheckUpgradePath(rt("mysql@8.4.13"), rt("mysql@8.4.13-custom"))
	if !errors.Is(err, domain.ErrUpgradeRefused) || !strings.Contains(err.Error(), "already runs") {
		t.Errorf("err = %v", err)
	}
}
