package greenmask_test

import (
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/greenmask"
	"github.com/db-branch/db-branch/pkg/ext"
)

var (
	msrc = ext.DBAccess{Engine: "mysql", Host: "127.0.0.1", Socket: "/run/my/mask-dev/mysqld.sock", Port: 40021, User: "root", BinDir: "/rt/bin", LibDir: "/rt/lib"}
	mdst = ext.DBAccess{Engine: "mysql", Host: "127.0.0.1", Socket: "/run/my/base-dev/mysqld.sock", Port: 40022, User: "root", BinDir: "/rt/bin", LibDir: "/rt/lib"}
)

const mysqlRules = `
databases:
  - name: shop
    transformation:
      - schema: shop
        name: users
        transformers:
          - name: RandomEmail
            params:
              column: email
`

func mysqlModule(t *testing.T) (*greenmask.MySQL, *runner) {
	t.Helper()
	r := &runner{out: "0"}
	m := &greenmask.MySQL{Run: r.run, Password: func() string { return "Pw123" }}
	if err := m.Configure(ctx, map[string]string{"greenmask_path": "/opt/gm/greenmask-mysql", "work_dir": t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func sqlOf(c call) string {
	for i, a := range c.args {
		if a == "-e" {
			return c.args[i+1]
		}
	}
	return ""
}

func TestMySQL_一時ログインで変換しながらダンプして戻し後片付けする(t *testing.T) {
	m, r := mysqlModule(t)
	if _, err := m.Mask(ctx, ext.MaskJob{Source: msrc, Target: mdst, Rules: mysqlRules}); err != nil {
		t.Fatal(err)
	}
	var sqls []string
	var dump, restore *call
	for i := range r.calls {
		c := &r.calls[i]
		switch {
		case c.name == "/rt/bin/mysql":
			if !strings.Contains(strings.Join(c.args, " "), "--socket=") || c.env[0] != "LD_LIBRARY_PATH=/rt/lib" {
				t.Errorf("mysql call %+v", c)
			}
			sqls = append(sqls, sqlOf(*c))
		case c.args[len(c.args)-1] == "dump":
			dump = c
		case c.args[len(c.args)-1] == "latest":
			restore = c
		}
	}
	all := strings.Join(sqls, "\n")
	for _, want := range []string{"CREATE USER 'dbb_mask'@'127.0.0.1' IDENTIFIED BY 'Pw123'", "SET GLOBAL local_infile = 1", "CREATE DATABASE IF NOT EXISTS `shop`",
		"DROP USER IF EXISTS 'dbb_mask'@'127.0.0.1'", "SET GLOBAL local_infile = 0"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if dump == nil || restore == nil {
		t.Fatalf("calls %+v", r.calls)
	}
	cfg := dump.config
	if cfg["engine"] != "mysql" || cfg["log"].(map[string]any)["format"] != "text" || cfg["common"].(map[string]any)["bin_path"] != "/rt/bin" {
		t.Errorf("config %v", cfg)
	}
	do := cfg["dump"].(map[string]any)["mysql"].(map[string]any)["options"].(map[string]any)
	if do["user"] != "dbb_mask" || do["password"] != "Pw123" || do["port"] != 40021.0 || do["add-drop-table"] != true || do["databases"].([]any)[0] != "shop" {
		t.Errorf("dump options %v", do)
	}
	ro := cfg["restore"].(map[string]any)["mysql"].(map[string]any)["options"].(map[string]any)
	if ro["port"] != 40022.0 || ro["connect-database"] != "shop" {
		t.Errorf("restore options %v", ro)
	}
	// Clean-up comes last, even after the restore.
	if !strings.HasPrefix(sqls[len(sqls)-1], "DROP USER") && !strings.HasPrefix(sqls[len(sqls)-1], "SET GLOBAL local_infile = 0") {
		t.Errorf("last SQL %q", sqls[len(sqls)-1])
	}
}

func TestMySQL_失敗しても一時ログインと設定を戻す(t *testing.T) {
	m, r := mysqlModule(t)
	r.fail = "dump"
	if _, err := m.Mask(ctx, ext.MaskJob{Source: msrc, Target: mdst, Rules: mysqlRules}); err == nil {
		t.Fatal("no error")
	}
	var all []string
	for _, c := range r.calls {
		all = append(all, sqlOf(c))
	}
	if j := strings.Join(all, "\n"); !strings.Contains(j, "DROP USER IF EXISTS") || !strings.Contains(j, "SET GLOBAL local_infile = 0") {
		t.Errorf("not cleaned up:\n%s", j)
	}
	for what, job := range map[string]ext.MaskJob{
		"postgres": {Source: src, Target: dst, Rules: mysqlRules},
		"rules":    {Source: msrc, Target: mdst, Rules: "databases: []"},
	} {
		if _, err := m.Mask(ctx, job); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	for _, f := range []string{"CREATE USER", "local_infile = 1", "CREATE DATABASE", "restore latest"} {
		m, r := mysqlModule(t)
		r.fail = f
		if _, err := m.Mask(ctx, ext.MaskJob{Source: msrc, Target: mdst, Rules: mysqlRules}); err == nil {
			t.Errorf("%s: no error", f)
		}
	}
}

func TestMySQL_設定を確かめる(t *testing.T) {
	m := &greenmask.MySQL{}
	for _, v := range []map[string]string{{"jobs": "0"}, {"work_dir": "x"}, {"greenmask_path": "x"}} {
		if err := m.Configure(ctx, v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
	if err := m.Configure(ctx, nil); err != nil || m.WorkDir() != "/var/lib/db-branch/greenmask-mysql" {
		t.Errorf("defaults %v %s", err, m.WorkDir())
	}
	if s := m.Settings(); len(s) != 3 || s[0].Default != "/usr/local/bin/greenmask-mysql" || s[0].Translations["ja"].Label == "" {
		t.Errorf("settings %v", s)
	}
	if greenmask.RandomPassword() == greenmask.RandomPassword() || len(greenmask.RandomPassword()) < 20 {
		t.Error("random passwords")
	}
}
