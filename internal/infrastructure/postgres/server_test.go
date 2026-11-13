package postgres_test

import (
	"compress/gzip"
	"context"
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
}

func (f *fakeSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.RunInput(ctx, nil, name, args...)
}

func (f *fakeSystem) RunInput(_ context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	parts := []string{name}
	for _, a := range args {
		if strings.HasPrefix(a, "/") && strings.Contains(a, "/bin/") {
			a = filepath.Base(a) // runtime binaries by name
		}
		parts = append(parts, a)
	}
	call := strings.Join(parts, " ")
	f.calls = append(f.calls, call)
	if in != nil {
		b, _ := io.ReadAll(in)
		f.stdin = append(f.stdin, string(b))
	}
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return nil, &execx.Error{Command: call, Stderr: "injected failure", Err: errors.New("exit 1")}
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

func TestExecとCreateUser_ソケットからスーパーユーザーとして流す(t *testing.T) {
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
	if !strings.Contains(sql, `CREATE ROLE "feature-x" WITH LOGIN SUPERUSER PASSWORD 'pw1'`) || !strings.Contains(sql, `ALTER ROLE "feature-x"`) {
		t.Errorf("sql = %s", sql)
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
