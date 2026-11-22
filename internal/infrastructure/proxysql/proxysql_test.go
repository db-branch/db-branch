package proxysql_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/proxysql"
	"github.com/db-branch/db-branch/internal/usecase/routing"
)

var ctx = context.Background()

// fakeProxy records statements per login and accepts the credentials it holds.
type fakeProxy struct {
	creds  string // user:password currently accepted
	logins []string
	ran    []string
	failOn string
}

type conn struct{ p *fakeProxy }

func (c conn) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	c.p.ran = append(c.p.ran, fmt.Sprintf("%s %v", q, args))
	if c.p.failOn != "" && strings.Contains(q, c.p.failOn) {
		return nil, errors.New("proxysql said no")
	}
	if strings.Contains(q, "admin-admin_credentials") {
		c.p.creds = strings.SplitN(strings.SplitN(q, "'", 3)[1], "'", 2)[0]
	}
	return nil, nil
}
func (conn) Close() error { return nil }

func (p *fakeProxy) open(user, password, _ string) (proxysql.DB, error) {
	p.logins = append(p.logins, user)
	if user+":"+password != p.creds {
		return nil, errors.New("access denied")
	}
	return conn{p}, nil
}

func newAdmin(t *testing.T) (*proxysql.Admin, *fakeProxy) {
	p := &fakeProxy{creds: "admin:admin"}
	return &proxysql.Admin{Addr: "127.0.0.1:6032", CredFile: filepath.Join(t.TempDir(), "proxysql", "admin.json"), Open: p.open}, p
}

var cfg = routing.Config{
	MySQL: routing.Tables{
		Servers: []routing.Server{{Hostgroup: 1001, Host: "192.0.2.10", Port: 40001}},
		Users:   []routing.User{{Name: "feature-x", Password: "pw", Hostgroup: 1001}},
		Rules: []routing.Rule{
			{ID: 1, Pattern: "db_branch=", FlagOut: 100},
			{ID: 1001, FlagIn: 100, Pattern: "db_branch='feature-x'", Hostgroup: 1001, Apply: true},
			{ID: routing.CatchAllRule, FlagIn: 100, Pattern: "db_branch=", Error: routing.UnknownBranch, Apply: true},
		},
	},
	Postgres: routing.Tables{
		Servers: []routing.Server{{Hostgroup: 2002, Host: "192.0.2.11", Port: 40010}},
		Users:   []routing.User{{Name: "pg-x", Password: "pw2", Hostgroup: 2002}},
	},
}

func TestApply_初回は既定の資格情報を乗っ取ってから設定を入れ替える(t *testing.T) {
	a, p := newAdmin(t)
	if err := a.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(a.CredFile)
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ User, Password string }
	_ = json.Unmarshal(b, &c)
	if p.creds != c.User+":"+c.Password || c.User != "dbbranch" || len(c.Password) < 32 {
		t.Errorf("proxy creds %q, saved %+v", p.creds, c)
	}
	if info, _ := os.Stat(a.CredFile); info.Mode().Perm() != 0o600 {
		t.Errorf("cred file mode %v", info.Mode().Perm())
	}
	all := strings.Join(p.ran, "\n")
	for _, want := range []string{"127.0.0.1:6032", "127.0.0.1:6132", "mysql-monitor_enabled", "SAVE ADMIN VARIABLES TO DISK",
		"DELETE FROM mysql_servers", "[1001 192.0.2.10 40001]", "[feature-x pw 1001]",
		"[1 <nil> 0 100 db_branch= <nil> <nil> 0]", "[2000000000 <nil> 100 <nil> db_branch= <nil> " + routing.UnknownBranch + " 1]",
		"LOAD MYSQL QUERY RULES TO RUNTIME", "SAVE MYSQL USERS TO DISK",
		"pgsql-monitor_enabled", "INSERT INTO pgsql_servers (hostgroup_id, hostname, port) VALUES (?, ?, ?) [2002 192.0.2.11 40010]",
		"INSERT INTO pgsql_users", "[pg-x pw2 2002]", "LOAD PGSQL USERS TO RUNTIME", "SAVE PGSQL QUERY RULES TO DISK"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	// Next time it logs in with its own credentials.
	p.logins = nil
	if err := a.Apply(ctx, cfg); err != nil || len(p.logins) != 1 || p.logins[0] != "dbbranch" {
		t.Errorf("second apply: %v logins %v", err, p.logins)
	}
}

func TestApply_失敗は理由を返し乗っ取りに失敗したら資格情報を残さない(t *testing.T) {
	a, p := newAdmin(t)
	p.failOn = "admin-mysql_ifaces"
	if err := a.Apply(ctx, cfg); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(a.CredFile); !errors.Is(err, os.ErrNotExist) {
		t.Error("credentials of a failed take-over must not be kept")
	}
	a, p = newAdmin(t)
	p.failOn = "INSERT INTO mysql_users"
	if err := a.Apply(ctx, cfg); err == nil || !strings.Contains(err.Error(), "mysql_users") {
		t.Errorf("apply failure: %v", err)
	}
	a, p = newAdmin(t)
	p.creds = "someone:else"
	if err := a.Apply(ctx, cfg); err == nil {
		t.Error("unknown credentials: want error")
	}
	a, _ = newAdmin(t)
	_ = os.MkdirAll(filepath.Dir(a.CredFile), 0o755)
	_ = os.WriteFile(a.CredFile, []byte("{"), 0o600)
	if err := a.Apply(ctx, cfg); err == nil {
		t.Error("broken credential file: want error")
	}
	a, _ = newAdmin(t)
	a.CredFile = "/proc/nope/admin.json"
	if err := a.Apply(ctx, cfg); err == nil {
		t.Error("unwritable credential file: want error")
	}
}

func TestApply_既定のドライバーで届かなければエラー(t *testing.T) {
	a := &proxysql.Admin{Addr: "127.0.0.1:1", CredFile: filepath.Join(t.TempDir(), "admin.json")}
	if err := a.Apply(ctx, cfg); err == nil {
		t.Error("want error")
	}
	_ = os.WriteFile(a.CredFile, []byte(`{"user":"dbbranch","password":"x"}`), 0o600)
	if err := a.Apply(ctx, cfg); err == nil {
		t.Error("want error")
	}
}

func TestRoute_モジュールとしても同じ設定を入れる(t *testing.T) {
	a, p := newAdmin(t)
	if err := a.Route(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.ran, "\n"), "[feature-x pw 1001]") {
		t.Errorf("ran %v", p.ran)
	}
}

func TestStatements_設定でコメントの規則を外し版を変える(t *testing.T) {
	all := ""
	for _, s := range proxysql.Statements(cfg, proxysql.Options{NoCommentRouting: true, ServerVersion: "8.0.36"}) {
		all += fmt.Sprintf("%s %v\n", s.Query(), s.Args())
	}
	if strings.Contains(all, "INSERT INTO mysql_query_rules") || !strings.Contains(all, "DELETE FROM mysql_query_rules") || !strings.Contains(all, "[8.0.36]") {
		t.Errorf("statements:\n%s", all)
	}
}

func TestModule_設定を変えると最後の振り分けを入れ直す(t *testing.T) {
	a, p := newAdmin(t)
	m := proxysql.NewModule(a)
	if err := m.Configure(ctx, map[string]string{"comment_routing": "false"}); err != nil {
		t.Fatal(err)
	}
	if len(p.ran) != 0 {
		t.Errorf("nothing to re-apply yet: %v", p.ran)
	}
	if err := m.Route(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(p.ran, "\n"), "INSERT INTO mysql_query_rules") {
		t.Error("comment rules applied although switched off")
	}
	p.ran = nil
	if err := m.Configure(ctx, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.ran, "\n"), "INSERT INTO mysql_query_rules") {
		t.Errorf("defaults re-applied: %v", p.ran)
	}
	if err := m.Configure(ctx, map[string]string{"comment_routing": "perhaps"}); err == nil {
		t.Error("bad bool: want error")
	}
	if len(proxysql.Settings) != 2 {
		t.Error("settings")
	}
}

func TestSettings_すべての項目に日本語がある(t *testing.T) {
	for _, s := range proxysql.Settings {
		ja := s.Translations["ja"]
		if ja.Label == "" || (s.Help != "" && ja.Help == "") {
			t.Errorf("%s has no Japanese: %+v", s.Key, ja)
		}
	}
}

func TestStatements_ルールのユーザー名をusernameカラムに入れる(t *testing.T) {
	c := cfg
	c.MySQL.Rules = []routing.Rule{{ID: 1, Username: "app", Pattern: "p", FlagOut: 100}}
	got := ""
	for _, s := range proxysql.Statements(c, proxysql.Options{}) {
		if strings.Contains(s.Query(), "INSERT INTO mysql_query_rules") {
			got = s.Query() + fmt.Sprint(s.Args())
		}
	}
	if !strings.Contains(got, "username") || !strings.Contains(got, "app") {
		t.Errorf("rule insert: %s", got)
	}
}
