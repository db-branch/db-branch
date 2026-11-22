package postgres_test

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/postgres"
)

var ctx = context.Background()

// fakeSystem plays systemd and the PostgreSQL tools.
type fakeSystem struct {
	calls      []string
	stdin      []string
	active     bool
	notReady   int
	stopResult string
	state      string // pg_controldata "Database cluster state"
	failOn     string
	crashes    bool
	initdb     func(dataDir string)
	// outputs answers a call whose text contains the key with the value on stdout.
	outputs map[string]string
	// onCall runs before executing a call (e.g. to cancel ctx mid-command).
	onCall func(call string)
}

func (f *fakeSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.RunInput(ctx, nil, name, args...)
}

func (f *fakeSystem) RunInput(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parts := []string{name}
	for _, a := range args {
		if strings.HasPrefix(a, "/") && strings.Contains(a, "/bin/") {
			a = filepath.Base(a) // runtime binaries by name
		}
		parts = append(parts, a)
	}
	call := strings.Join(parts, " ")
	f.calls = append(f.calls, call)
	if f.onCall != nil {
		f.onCall(call)
		if ctx.Err() != nil {
			return nil, &execx.Error{Command: call, Err: errors.New("signal: killed")}
		}
	}
	if in != nil {
		b, _ := io.ReadAll(in)
		f.stdin = append(f.stdin, string(b))
	}
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return nil, &execx.Error{Command: call, Stderr: "injected failure", Err: errors.New("exit 1")}
	}
	for k, v := range f.outputs {
		if strings.Contains(call, k) {
			return []byte(v), nil
		}
	}
	switch {
	case strings.Contains(call, " initdb "):
		if f.initdb != nil {
			f.initdb(args[len(args)-9])
		}
	case strings.HasPrefix(call, "systemctl start"):
		f.active = true
	case strings.HasPrefix(call, "systemctl stop"), strings.HasPrefix(call, "systemctl kill"):
		f.active = false
	case strings.HasPrefix(call, "systemctl is-active"):
		// Like the real runner: no output for a command that exits non-zero.
		if f.active && !f.crashes {
			return []byte("active\n"), nil
		}
		return nil, &execx.Error{Command: call, Err: errors.New("exit 3")}
	case strings.Contains(call, "--property=ActiveState"):
		switch {
		case f.crashes && f.active:
			return []byte("failed\n"), nil
		case f.active:
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), nil
	case strings.HasPrefix(call, "systemctl show"):
		return []byte(f.stopResult + "\n"), nil
	case strings.Contains(call, "pg_isready"):
		if f.notReady > 0 {
			f.notReady--
			return nil, &execx.Error{Command: call, Err: errors.New("exit 2")}
		}
	case strings.Contains(call, "pg_controldata"):
		return []byte("pg_control version number:            1800\nDatabase cluster state:               " + f.state + "\n"), nil
	case strings.HasPrefix(call, "journalctl"):
		if f.crashes {
			return []byte("FATAL: could not open file \"postgresql.conf\": Permission denied\n"), nil
		}
		return []byte("FATAL: something\n"), nil
	}
	return nil, nil
}

func (f *fakeSystem) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

type fixture struct {
	srv   *postgres.Server
	sys   *fakeSystem
	spec  domain.Server
	state string
	rt    string
}

var pg18 = domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{sys: &fakeSystem{stopResult: "success", state: "shut down"}, state: filepath.Join(root, "instances"), rt: filepath.Join(root, "rt")}
	data := filepath.Join(root, "data")
	_ = os.MkdirAll(data, 0o755)
	f.sys.initdb = func(string) {
		_ = os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte("local all all trust\n"), 0o600)
	}
	var chowned []string
	f.srv = &postgres.Server{
		Run: f.sys, RuntimeDir: func(domain.RuntimeRef) string { return f.rt },
		StateRoot: f.state, RunRoot: "/run/db-branch-postgres", Bind: "0.0.0.0", User: "db-branch",
		Chown:        func(p string) error { chowned = append(chowned, p); return nil },
		StartTimeout: 100 * time.Millisecond, PollInterval: time.Millisecond,
	}
	f.spec = domain.Server{ID: "base-prod", Runtime: pg18, DataDir: data, Port: 40001, MemoryMB: 128}
	return f
}

func TestInitialize_サービスユーザーでinitdbしネットワークからはパスワードで入れるようにする(t *testing.T) {
	f := newFixture(t)
	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("runuser -u db-branch -- env LD_LIBRARY_PATH=" + filepath.Join(f.rt, "lib") + " initdb -D " + f.spec.DataDir + " -U postgres --auth-local=trust --auth-host=scram-sha-256") {
		t.Errorf("calls = %q", f.sys.calls)
	}
	hba, _ := os.ReadFile(filepath.Join(f.spec.DataDir, "pg_hba.conf"))
	if !strings.Contains(string(hba), "host all all 0.0.0.0/0 scram-sha-256") {
		t.Errorf("pg_hba = %s", hba)
	}
	if info, _ := os.Stat(f.spec.DataDir); info.Mode().Perm() != 0o700 {
		t.Errorf("data dir mode %v", info.Mode().Perm())
	}
	env, _ := os.ReadFile(filepath.Join(f.state, "base-prod", "env"))
	for _, want := range []string{"PG_BIN=" + filepath.Join(f.rt, "bin"), "PG_DATA=" + f.spec.DataDir, "PG_PORT=40001", "PG_BIND=0.0.0.0", "PG_MEM=128", "LD_LIBRARY_PATH=" + filepath.Join(f.rt, "lib")} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env lacks %s:\n%s", want, env)
		}
	}
}

func TestInitialize_envは設定ディレクトリごとrootの持ち物にする(t *testing.T) {
	f := newFixture(t)
	var config, service []string
	f.srv.ChownConfig = func(p string) error { config = append(config, p); return nil }
	f.srv.Chown = func(p string) error { service = append(service, p); return nil }

	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.state, "base-prod")
	if want := dir + "," + filepath.Join(dir, "env"); strings.Join(config, ",") != want {
		t.Errorf("config chown %q, want %q", config, want)
	}
	if len(service) != 1 || service[0] != f.spec.DataDir {
		t.Errorf("service chown %q, want only the data dir", service)
	}
}

func TestInitialize_設定をrootの持ち物にできなければ失敗する(t *testing.T) {
	f := newFixture(t)
	f.srv.ChownConfig = func(string) error { return errors.New("chown failed") }
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("want error")
	}
}

func TestInitialize_envの場所にあるリンクは辿らない(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.state, "base-prod")
	_ = os.MkdirAll(dir, 0o750)
	victim := filepath.Join(t.TempDir(), "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	_ = os.Symlink(victim, filepath.Join(dir, "env"))
	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Errorf("link target was written: %q", got)
	}
}

func TestInitialize_失敗はそのまま返す(t *testing.T) {
	f := newFixture(t)
	f.sys.failOn = "initdb"
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("initdb failure: want error")
	}
	f = newFixture(t)
	f.sys.initdb = nil // no pg_hba.conf written
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("missing pg_hba.conf: want error")
	}
	f = newFixture(t)
	f.srv.Chown = func(string) error { return errors.New("no such user") }
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("chown failure: want error")
	}
	f = newFixture(t)
	f.spec.DataDir = filepath.Join(t.TempDir(), "missing")
	f.srv.Chown = nil
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("missing data dir: want error")
	}
	f = newFixture(t)
	f.srv.StateRoot = "/proc/nope"
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("unwritable state root: want error")
	}
}

func TestPrepareClone_複製元のpidファイルを消す(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"postmaster.pid", "postmaster.opts", "PG_VERSION"} {
		_ = os.WriteFile(filepath.Join(f.spec.DataDir, n), []byte("x"), 0o600)
	}
	if err := f.srv.PrepareClone(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]bool{"postmaster.pid": false, "postmaster.opts": false, "PG_VERSION": true} {
		if _, err := os.Stat(filepath.Join(f.spec.DataDir, n)); (err == nil) != want {
			t.Errorf("%s exists = %v", n, err == nil)
		}
	}
	_ = os.WriteFile(filepath.Join(f.spec.DataDir, "postmaster.pid"), []byte("x"), 0o600)
	_ = os.Chmod(f.spec.DataDir, 0o500)
	defer os.Chmod(f.spec.DataDir, 0o755)
	if os.Geteuid() != 0 {
		if err := f.srv.PrepareClone(ctx, f.spec); err == nil {
			t.Error("undeletable pid file: want error")
		}
	}
}

func TestStart_ユニットを起動し受け付けるまで待つ(t *testing.T) {
	f := newFixture(t)
	f.sys.notReady = 2
	if err := f.srv.Start(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("systemctl start db-branch-postgres@base-prod.service") || !f.sys.called("pg_isready -q -h /run/db-branch-postgres/base-prod -p 40001") {
		t.Errorf("calls = %q", f.sys.calls)
	}
	if ok, _ := f.srv.Running(ctx, f.spec); !ok {
		t.Error("should be running")
	}
}

func TestStart_起動しなければログを添えて失敗する(t *testing.T) {
	f := newFixture(t)
	f.sys.notReady = 1 << 30
	if err := f.srv.Start(ctx, f.spec); err == nil || !strings.Contains(err.Error(), "FATAL: something") {
		t.Errorf("err = %v", err)
	}
	f = newFixture(t)
	f.sys.failOn = "systemctl start"
	if err := f.srv.Start(ctx, f.spec); err == nil {
		t.Error("want error")
	}
	f = newFixture(t)
	f.srv.StateRoot = "/proc/nope"
	if err := f.srv.Start(ctx, f.spec); err == nil {
		t.Error("config failure: want error")
	}
}

func TestStop_きれいに止まったことを制御ファイルで確かめる(t *testing.T) {
	f := newFixture(t)
	_ = f.srv.Start(ctx, f.spec)
	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("pg_controldata " + f.spec.DataDir) {
		t.Errorf("calls = %q", f.sys.calls)
	}
	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err != nil {
		t.Errorf("already stopped: %v", err)
	}
	for name, mutate := range map[string]func(*fakeSystem){
		"crashed":     func(s *fakeSystem) { s.state = "in production" },
		"result":      func(s *fakeSystem) { s.stopResult = "timeout" },
		"controldata": func(s *fakeSystem) { s.failOn = "pg_controldata" },
	} {
		f := newFixture(t)
		_ = f.srv.Start(ctx, f.spec)
		mutate(f.sys)
		if err := f.srv.Stop(ctx, f.spec, domain.StopClean); !errors.Is(err, domain.ErrUncleanShutdown) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f = newFixture(t)
	_ = f.srv.Start(ctx, f.spec)
	f.sys.failOn = "systemctl stop"
	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err == nil || errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("stop failure: %v", err)
	}
}

func TestStop_即時停止はシグナルで落とす(t *testing.T) {
	f := newFixture(t)
	_ = f.srv.Start(ctx, f.spec)
	if err := f.srv.Stop(ctx, f.spec, domain.StopImmediate); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("systemctl kill --signal=SIGKILL db-branch-postgres@base-prod.service") || f.sys.called("pg_controldata") {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestExecとCreateUser_ソケットからスーパーユーザーとして流し_ログインは持たせない(t *testing.T) {
	f := newFixture(t)
	if err := f.srv.Exec(ctx, f.spec, "CREATE TABLE t (id int)"); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("psql -h /run/db-branch-postgres/base-prod -p 40001 -U postgres -d postgres -X -q -v ON_ERROR_STOP=1 -f -") || f.sys.stdin[0] != "CREATE TABLE t (id int)" {
		t.Errorf("calls = %q stdin %q", f.sys.calls, f.sys.stdin)
	}
	if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err != nil {
		t.Fatal(err)
	}
	sql := f.sys.stdin[1]
	// The branch login is no superuser (COPY ... TO PROGRAM would run programs on the node).
	if !strings.Contains(sql, `CREATE ROLE "feature-x" WITH LOGIN NOSUPERUSER CREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD 'pw1'`) ||
		!strings.Contains(sql, `ALTER ROLE "feature-x" WITH LOGIN NOSUPERUSER`) || !strings.Contains(sql, `GRANT "db_branch_owner" TO "feature-x"`) {
		t.Errorf("sql = %s", sql)
	}
	if strings.Contains(strings.ReplaceAll(sql, "NOSUPERUSER", ""), "SUPERUSER") {
		t.Errorf("a superuser login is made: %s", sql)
	}
	// The objects in the databases go to the owner role.
	if !f.sys.called("-c SELECT datname FROM pg_database") {
		t.Errorf("databases are not listed: %q", f.sys.calls)
	}
}

func TestCreateUser_引用符やバックスラッシュやドル記号を含むパスワードとユーザー名が安全にエスケープされる(t *testing.T) {
	// Single quote without backslash: standard SQL literal with doubled single quotes.
	f := newFixture(t)
	if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "p'w"); err != nil {
		t.Fatal(err)
	}
	if sql := f.sys.stdin[0]; !strings.Contains(sql, `PASSWORD 'p''w'`) {
		t.Errorf("single-quote escape sql = %s", sql)
	}

	// Backslash-only (with trailing backslash) and newline/CR in password: E'...' without raw newlines.
	fBS := newFixture(t)
	if err := fBS.srv.CreateUser(ctx, fBS.spec, `bs\user`, "line1\n\\! id\r\\"); err != nil {
		t.Fatal(err)
	}
	sqlBS := fBS.sys.stdin[0]
	for _, want := range []string{
		`rolname = E'bs\\user'`,
		`CREATE ROLE "bs\user"`,
		`PASSWORD E'line1\n\\! id\r\\'`,
	} {
		if !strings.Contains(sqlBS, want) {
			t.Errorf("backslash/newline sql lacks %q:\n%s", want, sqlBS)
		}
	}
	if strings.Contains(sqlBS, "\n\\! id") {
		t.Errorf("password injected a raw newline line starting with psql meta-command:\n%s", sqlBS)
	}

	// Single quote, backslash, double quote, and $$ / $dbb1$ dollar-quote breakout attempts.
	f2 := newFixture(t)
	user := `u"s'r$$`
	pw := `p'w\$$$dbb1$; DROP TABLE x; --`
	if err := f2.srv.CreateUser(ctx, f2.spec, user, pw); err != nil {
		t.Fatal(err)
	}
	sql := f2.sys.stdin[0]
	if !strings.HasPrefix(sql, "DO $dbb2$\n") || !strings.Contains(sql, "\n$dbb2$;\n") {
		t.Errorf("DO block did not pick collision-free dollar tag:\n%s", sql)
	}
	wantIdent := `"u""s'r$$"`
	wantUserLit := `'u"s''r$$'`
	wantPWLit := `E'p''w\\$$$dbb1$; DROP TABLE x; --'`
	for _, want := range []string{
		`rolname = ` + wantUserLit,
		`ALTER ROLE ` + wantIdent + ` WITH LOGIN NOSUPERUSER CREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD ` + wantPWLit,
		`CREATE ROLE ` + wantIdent + ` WITH LOGIN NOSUPERUSER CREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD ` + wantPWLit,
		`GRANT "db_branch_owner" TO ` + wantIdent + `;`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql lacks %q:\n%s", want, sql)
		}
	}

	// Empty user, newlines in user, or NUL bytes in user/password must be rejected before running psql.
	for _, tc := range []struct{ u, p string }{
		{"", "pw"},
		{"bad\n\\! id", "pw"},
		{"bad\ruser", "pw"},
		{"bad\x00user", "pw"},
		{"user", "bad\x00pw"},
	} {
		f3 := newFixture(t)
		if err := f3.srv.CreateUser(ctx, f3.spec, tc.u, tc.p); err == nil {
			t.Errorf("CreateUser(%q, %q): want error", tc.u, tc.p)
		}
		if len(f3.sys.calls) != 0 {
			t.Errorf("CreateUser(%q, %q) ran external command: %q", tc.u, tc.p, f3.sys.calls)
		}
	}
}

func TestSplitScript_connect行だけを受け_psqlのコマンドは断る(t *testing.T) {
	parts, err := postgres.SplitScript("UPDATE a SET b = 1;\n\\c shop\nDELETE FROM c;\n\\connect \"my db\"\nSELECT 1;\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || parts[0].DB != "postgres" || parts[1].DB != "shop" || parts[2].DB != "my db" ||
		!strings.Contains(parts[0].SQL, "UPDATE a") || !strings.Contains(parts[1].SQL, "DELETE FROM c") || strings.Contains(parts[1].SQL, "connect") {
		t.Errorf("parts = %+v", parts)
	}
	for _, bad := range []string{"\\! id", "  \\! id > /tmp/x", "\\i /etc/passwd", "\\copy t to program 'id'", "\\c shop someone", "\\c", "\\\\"} {
		if _, err := postgres.SplitScript("SELECT 1;\n" + bad + "\nSELECT 2;"); err == nil {
			t.Errorf("%q is accepted", bad)
		}
	}
	// A backslash inside a line is for the server to read (and, in plain SQL, to refuse); psql never sees it.
	if _, err := postgres.SplitScript("SELECT 1 \\! id;"); err != nil {
		t.Errorf("a backslash after SQL is the server's business: %v", err)
	}
}

func TestExecAs_psqlに通さず_コマンドを書いたスクリプトは走らせない(t *testing.T) {
	f := newFixture(t)
	if err := f.srv.ExecAs(ctx, f.spec, "feature-x", "pw1", "\\! id > /tmp/x\nSELECT 1;"); err == nil || !strings.Contains(err.Error(), "psql command") {
		t.Errorf("err = %v", err)
	}
	for _, c := range f.sys.calls {
		if strings.Contains(c, "psql") {
			t.Errorf("psql ran the caller's text: %q", f.sys.calls)
		}
	}
	// Empty, NUL, or newline user must be rejected before opening a socket connection.
	for _, badUser := range []string{"", "bad\x00user", "bad\nuser", "bad\ruser"} {
		f2 := newFixture(t)
		opened := false
		f2.srv.Open = func(_, _ string) (*sql.DB, error) { opened = true; return nil, errors.New("should not open") }
		if err := f2.srv.ExecAs(ctx, f2.spec, badUser, "pw1", "SELECT 1;"); err == nil {
			t.Errorf("ExecAs(%q): want error", badUser)
		}
		if opened {
			t.Errorf("ExecAs(%q) opened DB connection", badUser)
		}
	}
}

func TestLoad_ダンプを流し込み圧縮も展開する(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "d.sql")
	_ = os.WriteFile(plain, []byte("CREATE TABLE a ();"), 0o644)
	gzPath := filepath.Join(dir, "d.sql.gz")
	gf, _ := os.Create(gzPath)
	zw := gzip.NewWriter(gf)
	_, _ = zw.Write([]byte("CREATE TABLE b ();"))
	_ = zw.Close()
	_ = gf.Close()
	if err := f.srv.Load(ctx, f.spec, plain); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Load(ctx, f.spec, gzPath); err != nil {
		t.Fatal(err)
	}
	if f.sys.stdin[0] != "CREATE TABLE a ();" || f.sys.stdin[1] != "CREATE TABLE b ();" {
		t.Errorf("stdin = %q", f.sys.stdin)
	}
	if err := f.srv.Load(ctx, f.spec, filepath.Join(dir, "missing.sql")); err == nil {
		t.Error("missing dump: want error")
	}
	_ = os.WriteFile(filepath.Join(dir, "bad.sql.gz"), []byte("not gzip"), 0o644)
	if err := f.srv.Load(ctx, f.spec, filepath.Join(dir, "bad.sql.gz")); err == nil {
		t.Error("broken gzip: want error")
	}
}

func TestRemove_ユニットを止めて設定を消す(t *testing.T) {
	f := newFixture(t)
	_ = f.srv.Start(ctx, f.spec)
	if err := f.srv.Remove(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.state, "base-prod")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("instance dir left: %v", err)
	}
}

func TestFixup_binとlibを版ごとの場所へつなぐ(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "usr/lib/postgresql/18/bin"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "usr/lib/x86_64-linux-gnu"), 0o755)
	if err := postgres.Fixup(dir); err != nil {
		t.Fatal(err)
	}
	for link, want := range map[string]string{"bin": "usr/lib/postgresql/18/bin", "lib": "usr/lib/x86_64-linux-gnu"} {
		if got, _ := os.Readlink(filepath.Join(dir, link)); got != want {
			t.Errorf("%s -> %s", link, got)
		}
	}
	if err := postgres.Fixup(dir); err == nil {
		t.Error("links exist already: want error")
	}
	if err := postgres.Fixup(t.TempDir()); err == nil {
		t.Error("not a postgres runtime: want error")
	}
}

func TestAdmin_ソケットの信頼された管理者で入る道を返す(t *testing.T) {
	f := newFixture(t)
	a := f.srv.Admin(f.spec)
	if a.Engine != domain.Postgres || a.User != "postgres" || a.Port != f.spec.Port || a.BinDir != filepath.Join(f.rt, "bin") || a.LibDir != filepath.Join(f.rt, "lib") || !strings.HasSuffix(a.Socket, f.spec.ID) || a.Host != "127.0.0.1" {
		t.Errorf("%+v", a)
	}
}

func TestStart_サーバーが権限エラーで落ちたら待たずにErrOwnershipで失敗する(t *testing.T) {
	f := newFixture(t)
	f.sys.notReady = 1 << 30
	f.sys.crashes = true
	f.srv.StartTimeout = time.Minute
	err := f.srv.Start(ctx, f.spec)
	if !errors.Is(err, domain.ErrOwnership) {
		t.Fatalf("error = %v", err)
	}
}

func TestStart_データディレクトリの所有者が違えば起動せずに失敗する(t *testing.T) {
	f := newFixture(t)
	f.srv.CheckOwner = func(string) error { return domain.ErrOwnership }
	if err := f.srv.Start(ctx, f.spec); !errors.Is(err, domain.ErrOwnership) {
		t.Fatalf("error = %v", err)
	}
	if f.sys.called("systemctl start") {
		t.Error("started anyway")
	}
}

func TestUpdateStats_全データベースをVACUUM_FREEZE_ANALYZEする(t *testing.T) {
	f := newFixture(t)
	var _ domain.StatsUpdater = f.srv
	if err := f.srv.UpdateStats(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	last := f.sys.calls[len(f.sys.calls)-1]
	if !strings.Contains(last, " vacuumdb ") || !strings.Contains(last, "--all --freeze --analyze") || !strings.Contains(last, "LD_LIBRARY_PATH=") {
		t.Errorf("call = %q", last)
	}
	f.sys.failOn = "vacuumdb"
	if err := f.srv.UpdateStats(ctx, f.spec); err == nil {
		t.Error("failing vacuumdb: want error")
	}
}

func TestStopClean_既に異常停止済みのサーバーに対してverifyShutdownを実行しErrUncleanShutdownを返す(t *testing.T) {
	for label, mutate := range map[string]func(*fakeSystem){
		"crashed":     func(s *fakeSystem) { s.state = "in production" },
		"unit failed": func(s *fakeSystem) { s.stopResult = "signal" },
		"controldata": func(s *fakeSystem) { s.failOn = "pg_controldata" },
	} {
		f := newFixture(t)
		mutate(f.sys)
		err := f.srv.Stop(ctx, f.spec, domain.StopClean)
		if !errors.Is(err, domain.ErrUncleanShutdown) {
			t.Errorf("%s: Stop(StopClean) = %v, want ErrUncleanShutdown", label, err)
		}
		if f.sys.called("systemctl stop") {
			t.Errorf("%s: should not call systemctl stop on inactive unit: %q", label, f.sys.calls)
		}
	}

	// Cleanly stopped previously: succeeds without calling systemctl stop.
	clean := newFixture(t)
	if err := clean.srv.Stop(ctx, clean.spec, domain.StopClean); err != nil {
		t.Fatalf("cleanly stopped: %v", err)
	}
	if clean.sys.called("systemctl stop") || !clean.sys.called("pg_controldata") {
		t.Errorf("cleanly stopped calls = %q", clean.sys.calls)
	}

	// StopImmediate on a stopped server skips shutdown verification.
	imm := newFixture(t)
	imm.sys.state = "in production"
	imm.sys.stopResult = "signal"
	if err := imm.srv.Stop(ctx, imm.spec, domain.StopImmediate); err != nil {
		t.Fatalf("StopImmediate on stopped: %v", err)
	}
	if imm.sys.called("pg_controldata") {
		t.Errorf("StopImmediate should not call pg_controldata: %q", imm.sys.calls)
	}

	// activating / deactivating states must NOT be treated as stopped.
	for _, st := range []string{"activating", "deactivating"} {
		fa := newFixture(t)
		fa.sys.outputs = map[string]string{"--property=ActiveState": st + "\n"}
		if err := fa.srv.Stop(ctx, fa.spec, domain.StopImmediate); err != nil {
			t.Fatalf("%s StopImmediate: %v", st, err)
		}
		if !fa.sys.called("systemctl kill --signal=SIGKILL") {
			t.Errorf("%s StopImmediate did not kill unit: %q", st, fa.sys.calls)
		}

		fc := newFixture(t)
		fc.sys.outputs = map[string]string{"--property=ActiveState": st + "\n"}
		if err := fc.srv.Stop(ctx, fc.spec, domain.StopClean); err != nil {
			t.Fatalf("%s StopClean: %v", st, err)
		}
		if !fc.sys.called("systemctl stop") {
			t.Errorf("%s StopClean did not stop unit: %q", st, fc.sys.calls)
		}
	}

	// Cancelled ctx and systemctl show errors must NOT become nil or ErrUncleanShutdown.
	fCan := newFixture(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := fCan.srv.Stop(cctx, fCan.spec, domain.StopClean); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled StopClean = %v, want context.Canceled", err)
	}
	fShow := newFixture(t)
	fShow.sys.failOn = "--property=ActiveState"
	if err := fShow.srv.Stop(ctx, fShow.spec, domain.StopClean); err == nil || errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("ActiveState error = %v, want non-UncleanShutdown error", err)
	}
	fRes := newFixture(t)
	fRes.sys.failOn = "--property=Result"
	if err := fRes.srv.Stop(ctx, fRes.spec, domain.StopClean); err == nil || errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("Result error = %v, want non-UncleanShutdown error", err)
	}
	// Cancelled mid-pg_controldata returns context.Canceled, not ErrUncleanShutdown.
	fCtrl := newFixture(t)
	ctrlCtx, ctrlCancel := context.WithCancel(ctx)
	fCtrl.sys.onCall = func(call string) {
		if strings.Contains(call, "pg_controldata") {
			ctrlCancel()
		}
	}
	if err := fCtrl.srv.Stop(ctrlCtx, fCtrl.spec, domain.StopClean); !errors.Is(err, context.Canceled) {
		t.Errorf("mid-controldata cancel = %v, want context.Canceled", err)
	}
}

func TestStart_ctxキャンセル時に即座に待機中断し起動途中ユニットを停止する(t *testing.T) {
	// Cancelled while waiting for postgres to become ready: stops polling right away and stops the unit.
	f := newFixture(t)
	f.sys.notReady = 1 << 30
	f.srv.StartTimeout = time.Minute
	f.srv.PollInterval = 5 * time.Second
	cctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()

	begin := time.Now()
	err := f.srv.Start(cctx, f.spec)
	elapsed := time.Since(begin)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("Start waited %s after cancellation", elapsed)
	}
	if !f.sys.called("systemctl stop db-branch-postgres@base-prod.service") {
		t.Errorf("in-flight unit was not stopped: %q", f.sys.calls)
	}
	if f.sys.active {
		t.Error("unit should no longer be active after cancel")
	}

	// If graceful stop fails while aborting, falls back to SIGKILL.
	fk := newFixture(t)
	fk.sys.notReady = 1 << 30
	fk.sys.failOn = "systemctl stop"
	fk.srv.StartTimeout = time.Minute
	fk.srv.PollInterval = 5 * time.Second
	kctx, kcancel := context.WithCancel(ctx)
	ktimer := time.AfterFunc(20*time.Millisecond, kcancel)
	defer ktimer.Stop()
	if err := fk.srv.Start(kctx, fk.spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("fallback err = %v, want context.Canceled", err)
	}
	if !fk.sys.called("systemctl kill --signal=SIGKILL db-branch-postgres@base-prod.service") {
		t.Errorf("expected SIGKILL fallback when stop fails: %q", fk.sys.calls)
	}

	// Already cancelled before Start: does not start the unit.
	fPre := newFixture(t)
	preCtx, preCancel := context.WithCancel(ctx)
	preCancel()
	if err := fPre.srv.Start(preCtx, fPre.spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled err = %v, want context.Canceled", err)
	}
	if fPre.sys.called("systemctl start") {
		t.Errorf("started unit despite cancelled ctx: %q", fPre.sys.calls)
	}

	// Timeout also stops the stuck unit.
	ft := newFixture(t)
	ft.sys.notReady = 1 << 30
	ft.srv.StartTimeout = 10 * time.Millisecond
	if err := ft.srv.Start(ctx, ft.spec); err == nil {
		t.Fatal("timeout: want error")
	}
	if !ft.sys.called("systemctl stop db-branch-postgres@base-prod.service") || ft.sys.active {
		t.Errorf("timed-out unit was left running: active=%v calls=%q", ft.sys.active, ft.sys.calls)
	}

	// Cancelled mid-"systemctl start": preserves context.Canceled and stops the unit.
	fMid := newFixture(t)
	midCtx, midCancel := context.WithCancel(ctx)
	fMid.sys.onCall = func(call string) {
		if strings.HasPrefix(call, "systemctl start") {
			fMid.sys.active = true
			midCancel()
		}
	}
	if err := fMid.srv.Start(midCtx, fMid.spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-start cancel err = %v, want context.Canceled", err)
	}
	if !fMid.sys.called("systemctl stop db-branch-postgres@base-prod.service") || fMid.sys.active {
		t.Errorf("mid-start cancelled unit left running: active=%v calls=%q", fMid.sys.active, fMid.sys.calls)
	}

	// StartUpgraded cancelled: preserves context.Canceled, stops unit, and still collects journal excerpt.
	fUp := newFixture(t)
	fUp.sys.notReady = 1 << 30
	fUp.srv.StartTimeout = time.Minute
	fUp.srv.PollInterval = 5 * time.Second
	upCtx, upCancel := context.WithCancel(ctx)
	upTimer := time.AfterFunc(20*time.Millisecond, upCancel)
	defer upTimer.Stop()
	upLog, err := fUp.srv.StartUpgraded(upCtx, fUp.spec, domain.Login{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartUpgraded cancel err = %v, want context.Canceled", err)
	}
	if !strings.Contains(upLog.Excerpt, "FATAL: something") {
		t.Errorf("StartUpgraded cancel excerpt = %q, want journal output", upLog.Excerpt)
	}
	if fUp.sys.active {
		t.Error("StartUpgraded unit left active after cancel")
	}
}
