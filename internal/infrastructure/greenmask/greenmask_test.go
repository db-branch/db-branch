package greenmask_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/greenmask"
	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

type call struct {
	name   string
	args   []string
	env    []string
	config map[string]any
}

type runner struct {
	calls []call
	fail  string
	out   string
}

func (r *runner) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	c := call{name: name, args: args, env: env}
	for i, a := range args {
		if a == "--config" {
			b, _ := os.ReadFile(args[i+1])
			_ = json.Unmarshal(b, &c.config)
		}
	}
	r.calls = append(r.calls, c)
	joined := name + " " + strings.Join(args, " ")
	if r.fail != "" && strings.Contains(joined, r.fail) {
		return []byte("\x1b[31mERR\x1b[0m pg_dump: error: connection refused"), errors.New("exit status 1")
	}
	return []byte(r.out), nil
}

var src = ext.DBAccess{Engine: "postgres", Socket: "/run/pg/mask-dev", Port: 40011, User: "postgres", BinDir: "/rt/bin", LibDir: "/rt/lib"}
var dst = ext.DBAccess{Engine: "postgres", Socket: "/run/pg/base-dev", Port: 40012, User: "postgres", BinDir: "/rt/bin", LibDir: "/rt/lib"}

const rules = `
databases:
  - name: shop
    transformation:
      - schema: public
        name: users
        transformers:
          - name: RandomEmail
            params:
              column: email
  - name: logs
`

func module(t *testing.T) (*greenmask.Module, *runner) {
	t.Helper()
	r := &runner{}
	m := &greenmask.Module{Run: r.run}
	if err := m.Configure(ctx, map[string]string{"greenmask_path": "/opt/gm/greenmask", "work_dir": t.TempDir(), "jobs": "4"}); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func TestMask_データベースごとに変換しながらダンプしてマスク先へ戻す(t *testing.T) {
	m, r := module(t)
	if _, err := m.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules}); err != nil {
		t.Fatal(err)
	}
	// shop: make it on the target (when missing), dump with the transformation, restore.
	if len(r.calls) != 8 {
		t.Fatalf("calls %d: %+v", len(r.calls), r.calls)
	}
	if !strings.Contains(strings.Join(r.calls[1].args, " "), `CREATE DATABASE "shop"`) {
		t.Errorf("create %+v", r.calls[1])
	}
	create := r.calls[0]
	if create.name != "/rt/bin/psql" || !strings.Contains(strings.Join(create.args, " "), "-h /run/pg/base-dev -p 40012 -U postgres") || create.env[0] != "LD_LIBRARY_PATH=/rt/lib" {
		t.Errorf("create %+v", create)
	}
	dump := r.calls[2]
	if dump.name != "/opt/gm/greenmask" || dump.args[len(dump.args)-1] != "dump" {
		t.Fatalf("dump %+v", dump)
	}
	cfg := dump.config
	common := cfg["common"].(map[string]any)
	if common["pg_bin_path"] != "/rt/bin" {
		t.Errorf("common %v", common)
	}
	d := cfg["dump"].(map[string]any)
	if d["pg_dump_options"].(map[string]any)["dbname"] != "host=/run/pg/mask-dev port=40011 user=postgres dbname=shop" {
		t.Errorf("dump options %v", d["pg_dump_options"])
	}
	tr := d["transformation"].([]any)[0].(map[string]any)
	if tr["name"] != "users" || tr["transformers"].([]any)[0].(map[string]any)["name"] != "RandomEmail" {
		t.Errorf("transformation %v", tr)
	}
	ro := cfg["restore"].(map[string]any)["pg_restore_options"].(map[string]any)
	// Re-runs replace what an earlier run restored, and any error fails the run.
	if ro["dbname"] != "host=/run/pg/base-dev port=40012 user=postgres dbname=shop" || ro["jobs"] != 4.0 ||
		ro["clean"] != true || ro["if-exists"] != true || ro["exit-on-error"] != true {
		t.Errorf("restore options %v", ro)
	}
	if restore := r.calls[3]; strings.Join(restore.args[len(restore.args)-2:], " ") != "restore latest" {
		t.Errorf("restore %+v", restore)
	}
	// logs: copied without transformation.
	if logs := r.calls[6].config["dump"].(map[string]any); logs["transformation"] != nil && len(logs["transformation"].([]any)) != 0 {
		t.Errorf("logs %v", logs)
	}
	entries, _ := os.ReadDir(m.WorkDir())
	if len(entries) != 0 {
		t.Errorf("work files left: %v", entries)
	}
}

func TestMask_失敗や対応しないものを伝える(t *testing.T) {
	m, r := module(t)
	r.fail = "dump"
	if _, err := m.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules}); err == nil || !strings.Contains(err.Error(), "ERR pg_dump: error: connection refused") {
		t.Errorf("dump failure: %v", err)
	}
	for what, job := range map[string]ext.MaskJob{
		"mysql":          {Source: ext.DBAccess{Engine: "mysql"}, Target: ext.DBAccess{Engine: "mysql"}, Rules: rules},
		"no databases":   {Source: src, Target: dst, Rules: "databases: []"},
		"bad yaml":       {Source: src, Target: dst, Rules: "databases: ["},
		"bad name":       {Source: src, Target: dst, Rules: "databases:\n  - name: \"x; DROP\"\n"},
		"unknown key":    {Source: src, Target: dst, Rules: "dbs: []"},
		"engine mixture": {Source: src, Target: ext.DBAccess{Engine: "mysql"}, Rules: rules},
	} {
		if _, err := m.Mask(ctx, job); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	r.fail = "CREATE DATABASE"
	if _, err := m.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules}); err == nil {
		t.Error("create database failure")
	}
}

func TestConfigure_設定を確かめる(t *testing.T) {
	m := &greenmask.Module{}
	for _, v := range []map[string]string{{"jobs": "0"}, {"jobs": "x"}, {"work_dir": "relative"}, {"greenmask_path": "relative/gm"}} {
		if err := m.Configure(ctx, v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
	if err := m.Configure(ctx, map[string]string{}); err != nil || m.WorkDir() != "/var/lib/db-branch/greenmask" {
		t.Errorf("defaults: %v %s", err, m.WorkDir())
	}
	if s := m.Settings(); len(s) != 3 || s[0].Translations["ja"].Label == "" {
		t.Errorf("settings %v", s)
	}
	existing, r := module(t)
	r.out = "1"
	_, _ = existing.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules})
	if len(r.calls) != 6 {
		t.Errorf("databases already there are not created: %d calls", len(r.calls))
	}
	_ = filepath.Join
}

func TestMask_途中の失敗をそれぞれ伝える(t *testing.T) {
	for _, fail := range []string{"pg_database", "restore latest"} {
		m, r := module(t)
		r.fail = fail
		if _, err := m.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules}); err == nil {
			t.Errorf("%s: no error", fail)
		}
	}
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	for _, dir := range []string{filepath.Join(blocked, "sub"), blocked} {
		b := &greenmask.Module{Run: (&runner{}).run}
		_ = b.Configure(ctx, map[string]string{"work_dir": dir})
		if _, err := b.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules}); err == nil {
			t.Errorf("work dir %s: no error", dir)
		}
	}
}
