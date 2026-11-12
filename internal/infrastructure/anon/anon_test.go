package anon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/anon"
	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

type call struct {
	name string
	args []string
	sql  string // contents of a -f file
}

type runner struct {
	calls []call
	fail  string
	out   string
	// install makes dpkg-deb -x lay out the extension.
	install bool
}

func (r *runner) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	c := call{name: name, args: args}
	for i, a := range args {
		if a == "-f" {
			b, _ := os.ReadFile(args[i+1])
			c.sql = string(b)
		}
	}
	r.calls = append(r.calls, c)
	if strings.HasSuffix(name, "dpkg-deb") && r.install {
		dir := filepath.Join(args[2], "usr/share/postgresql/18/extension")
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "anon.control"), nil, 0o644)
	}
	if r.fail != "" && strings.Contains(name+" "+strings.Join(args, " ")+" "+c.sql, r.fail) {
		return []byte("ERROR: boom"), errors.New("exit status 1")
	}
	return []byte(r.out), nil
}

// runtime makes a PostgreSQL runtime tree with or without the extension.
func runtime(t *testing.T, withAnon bool) ext.DBAccess {
	t.Helper()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "usr/share/postgresql/18/extension"), 0o755)
	if withAnon {
		_ = os.WriteFile(filepath.Join(root, "usr/share/postgresql/18/extension/anon.control"), nil, 0o644)
	}
	return ext.DBAccess{Engine: "postgres", Socket: "/run/pg/x", Port: 1, User: "postgres", BinDir: filepath.Join(root, "bin"), LibDir: filepath.Join(root, "lib")}
}

const rules = `
databases:
  - name: shop
    rules: |
      SECURITY LABEL FOR anon ON COLUMN public.users.email IS 'MASKED WITH FUNCTION anon.fake_email()';
`

func module(t *testing.T, r *runner, pkg string) *anon.Module {
	t.Helper()
	m := &anon.Module{Run: r.run}
	if err := m.Configure(ctx, map[string]string{"package": pkg, "work_dir": t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMask_コピーしてから匿名化しラベルと拡張を外してVACUUMする(t *testing.T) {
	r := &runner{}
	m := module(t, r, "")
	src, dst := runtime(t, true), runtime(t, true)
	src.Socket, dst.Socket, src.Port, dst.Port = "/run/pg/mask-dev", "/run/pg/base-dev", 40011, 40012
	if _, err := m.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules}); err != nil {
		t.Fatal(err)
	}
	var names []string
	var script string
	for _, c := range r.calls {
		names = append(names, filepath.Base(c.name))
		if c.sql != "" {
			script = c.sql
		}
	}
	if got := strings.Join(names, " "); got != "psql psql pg_dump pg_restore psql" {
		t.Errorf("calls %s", got)
	}
	dump, restore := strings.Join(r.calls[2].args, " "), strings.Join(r.calls[3].args, " ")
	if !strings.Contains(dump, "-h /run/pg/mask-dev -p 40011") || !strings.Contains(restore, "-h /run/pg/base-dev -p 40012") || !strings.Contains(restore, "--clean --if-exists --exit-on-error") {
		t.Errorf("dump %s\nrestore %s", dump, restore)
	}
	order := []string{"LOAD 'anon'", "SET anon.nosuperuser = off", "CREATE EXTENSION IF NOT EXISTS anon CASCADE", "SELECT anon.init()", "SECURITY LABEL FOR anon ON COLUMN public.users.email",
		"SELECT anon.anonymize_database()", "SELECT anon.remove_masks_for_all_columns()", "DROP EXTENSION anon CASCADE", "DELETE FROM pg_seclabel WHERE provider = 'anon'", "VACUUM FULL"}
	last := -1
	for _, s := range order {
		i := strings.Index(script, s)
		if i <= last {
			t.Errorf("%q out of order in\n%s", s, script)
		}
		last = i
	}
	entries, _ := os.ReadDir(m.WorkDir())
	if len(entries) != 0 {
		t.Errorf("the raw dump was left behind: %v", entries)
	}
}

func TestMask_拡張がなければ指定のパッケージから入れる(t *testing.T) {
	r := &runner{install: true}
	m := module(t, r, "/srv/postgresql_anonymizer_18_3.3.2_amd64.deb")
	src, dst := runtime(t, false), runtime(t, false)
	if _, err := m.Mask(ctx, ext.MaskJob{Source: src, Target: dst, Rules: rules}); err != nil {
		t.Fatal(err)
	}
	if c := r.calls[0]; !strings.HasSuffix(c.name, "dpkg-deb") || strings.Join(c.args, " ") != "-x /srv/postgresql_anonymizer_18_3.3.2_amd64.deb "+filepath.Dir(dst.BinDir) {
		t.Errorf("install %+v", c)
	}
	none := module(t, &runner{}, "")
	if _, err := none.Mask(ctx, ext.MaskJob{Source: runtime(t, false), Target: runtime(t, false), Rules: rules}); err == nil || !strings.Contains(err.Error(), "postgresql_anonymizer") {
		t.Errorf("no extension and no package: %v", err)
	}
	broken := module(t, &runner{}, "/srv/not-anon.deb") // installs nothing
	if _, err := broken.Mask(ctx, ext.MaskJob{Source: runtime(t, false), Target: runtime(t, false), Rules: rules}); err == nil {
		t.Error("a package without the extension")
	}
}

func TestMask_失敗やおかしな依頼を伝える(t *testing.T) {
	for _, f := range []string{"pg_database", "CREATE DATABASE", "pg_dump", "pg_restore", "anonymize_database", "dpkg-deb"} {
		r := &runner{fail: f}
		m := module(t, r, "/srv/anon.deb")
		dst := runtime(t, f != "dpkg-deb")
		if _, err := m.Mask(ctx, ext.MaskJob{Source: runtime(t, true), Target: dst, Rules: rules}); err == nil {
			t.Errorf("%s: no error", f)
		}
	}
	m := module(t, &runner{}, "")
	for what, job := range map[string]ext.MaskJob{
		"mysql":     {Source: ext.DBAccess{Engine: "mysql"}, Target: ext.DBAccess{Engine: "mysql"}, Rules: rules},
		"no rules":  {Source: runtime(t, true), Target: runtime(t, true), Rules: "databases: []"},
		"bad yaml":  {Source: runtime(t, true), Target: runtime(t, true), Rules: "databases: ["},
		"bad name":  {Source: runtime(t, true), Target: runtime(t, true), Rules: "databases:\n  - name: \"a b\"\n"},
		"bad key":   {Source: runtime(t, true), Target: runtime(t, true), Rules: "dbs: 1"},
		"no labels": {Source: runtime(t, true), Target: runtime(t, true), Rules: "databases:\n  - name: shop\n"},
	} {
		if _, err := m.Mask(ctx, job); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}

func TestConfigure_設定を確かめる(t *testing.T) {
	m := &anon.Module{}
	for _, v := range []map[string]string{{"work_dir": "x"}, {"package": "relative.deb"}} {
		if err := m.Configure(ctx, v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
	if err := m.Configure(ctx, nil); err != nil || m.WorkDir() != "/var/lib/db-branch/anon" {
		t.Errorf("defaults %v %s", err, m.WorkDir())
	}
	if s := m.Settings(); len(s) != 2 || s[0].Translations["ja"].Label == "" {
		t.Errorf("settings %v", s)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	b := &anon.Module{Run: (&runner{}).run}
	_ = b.Configure(ctx, map[string]string{"work_dir": blocked})
	if _, err := b.Mask(ctx, ext.MaskJob{Source: runtime(t, true), Target: runtime(t, true), Rules: rules}); err == nil {
		t.Error("an unusable work dir")
	}
}
