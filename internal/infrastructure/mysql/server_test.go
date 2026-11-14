package mysql_test

import (
	"bytes"
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
	"github.com/db-branch/db-branch/internal/infrastructure/mysql"
)

var ctx = context.Background()

// fakeSystem plays systemd, mysqld and the mysql client.
type fakeSystem struct {
	calls      []string
	stdin      []string
	active     bool
	pingFails  int
	stopResult string // systemctl show Result after stop
	shutdownOK bool   // whether stopping writes "Shutdown complete"
	errorLog   string
	failOn     string
	crashes    bool // mysqld exits right after starting
	// outputs answers a call whose text contains the key with the value on stdout.
	outputs map[string]string
	// onStart is appended to the error log by "systemctl start".
	onStart string
}

func (f *fakeSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.RunInput(ctx, nil, name, args...)
}

func (f *fakeSystem) RunInput(_ context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
	f.calls = append(f.calls, call)
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
	case strings.HasPrefix(call, "systemctl start"):
		f.active = true
		if f.onStart != "" {
			appendFile(f.errorLog, f.onStart)
		}
	case strings.HasPrefix(call, "systemctl stop"), strings.HasPrefix(call, "systemctl kill"):
		if f.active && strings.HasPrefix(call, "systemctl stop") {
			log := "[System] [MY-010910] [Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n"
			if !f.shutdownOK {
				log = "[ERROR] something went wrong\n"
			}
			appendFile(f.errorLog, log)
		}
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
	case strings.HasPrefix(call, "mysqladmin"):
		if f.pingFails > 0 {
			f.pingFails--
			return nil, &execx.Error{Command: call, Err: errors.New("exit 1")}
		}
		return []byte("mysqld is alive\n"), nil
	}
	return nil, nil
}

func (f *fakeSystem) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func appendFile(path, s string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer fh.Close()
	_, _ = fh.WriteString(s)
}

type fixture struct {
	srv    *mysql.Server
	sys    *fakeSystem
	spec   domain.Server
	state  string
	rtDir  string
	runDir string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		sys:    &fakeSystem{stopResult: "success", shutdownOK: true},
		state:  filepath.Join(root, "instances"),
		rtDir:  filepath.Join(root, "runtimes", "mysql", "8.4.13"),
		runDir: filepath.Join(root, "run"),
		spec: domain.Server{
			ID:       "branch-feature-x",
			Runtime:  domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"},
			DataDir:  filepath.Join(root, "data"),
			Port:     40001,
			MemoryMB: 128,
		},
	}
	f.sys.errorLog = filepath.Join(f.state, f.spec.ID, "error.log")
	_ = os.MkdirAll(f.spec.DataDir, 0o755)
	f.srv = &mysql.Server{
		Run:          f.sys,
		RuntimeDir:   func(domain.RuntimeRef) string { return f.rtDir },
		StateRoot:    f.state,
		RunRoot:      f.runDir,
		Bind:         "0.0.0.0",
		StartTimeout: time.Second,
		PollInterval: time.Millisecond,
	}
	return f
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInitialize_設定ファイルを書いてデータディレクトリを初期化する(t *testing.T) {
	f := newFixture(t)

	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	cnf := read(t, filepath.Join(f.state, f.spec.ID, "my.cnf"))
	for _, want := range []string{
		"datadir=" + f.spec.DataDir,
		"basedir=" + f.rtDir,
		"port=40001",
		"bind-address=0.0.0.0",
		"socket=" + filepath.Join(f.runDir, f.spec.ID, "mysqld.sock"),
		"log-error=" + filepath.Join(f.state, f.spec.ID, "error.log"),
		"innodb_buffer_pool_size=128M",
		"innodb_doublewrite=OFF",
		"innodb_flush_neighbors=0",
		"mysqlx=OFF",
	} {
		if !strings.Contains(cnf, want+"\n") {
			t.Errorf("my.cnf lacks %q:\n%s", want, cnf)
		}
	}
	env := read(t, filepath.Join(f.state, f.spec.ID, "env"))
	if env != "MYSQLD="+filepath.Join(f.rtDir, "bin", "mysqld")+"\n" {
		t.Errorf("env = %q", env)
	}
	if !f.sys.called("mysqld --defaults-file=" + filepath.Join(f.state, f.spec.ID, "my.cnf") + " --initialize-insecure") {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestPrepareClone_サーバーUUIDとPIDファイルを消す(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"auto.cnf", "host.pid", "ibdata1"} {
		_ = os.WriteFile(filepath.Join(f.spec.DataDir, name), nil, 0o644)
	}

	if err := f.srv.PrepareClone(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	for name, wantExists := range map[string]bool{"auto.cnf": false, "host.pid": false, "ibdata1": true} {
		_, err := os.Stat(filepath.Join(f.spec.DataDir, name))
		if (err == nil) != wantExists {
			t.Errorf("%s exists = %v, want %v", name, err == nil, wantExists)
		}
	}
	if _, err := os.Stat(filepath.Join(f.state, f.spec.ID, "my.cnf")); err != nil {
		t.Errorf("my.cnf should be written for the clone: %v", err)
	}
}

func TestStart_応答するまで待つ(t *testing.T) {
	f := newFixture(t)
	f.sys.pingFails = 3

	if err := f.srv.Start(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("systemctl start db-branch-mysqld@branch-feature-x.service") {
		t.Errorf("calls = %q", f.sys.calls)
	}
	if ok, _ := f.srv.Running(ctx, f.spec); !ok {
		t.Error("Running should be true")
	}
}

func TestStart_時間内に応答しなければエラーログを添えて失敗する(t *testing.T) {
	f := newFixture(t)
	f.sys.pingFails = 1 << 30
	f.srv.StartTimeout = 20 * time.Millisecond
	_ = os.MkdirAll(filepath.Dir(f.sys.errorLog), 0o755)
	f.sys.onStart = "[ERROR] [MY-010119] Aborting\n"

	err := f.srv.Start(ctx, f.spec)
	if err == nil || !strings.Contains(err.Error(), "Aborting") {
		t.Fatalf("error = %v", err)
	}
}

func TestStart_サービスが失敗したら即座に失敗する(t *testing.T) {
	f := newFixture(t)
	f.sys.failOn = "systemctl start"

	if err := f.srv.Start(ctx, f.spec); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestStop_正常停止ではパージを済ませてから止めて結果を確かめる(t *testing.T) {
	f := newFixture(t)
	_ = f.srv.Start(ctx, f.spec)
	appendFile(f.sys.errorLog, "old: Shutdown complete\n") // from an earlier run; must not count

	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("mysql --socket=" + filepath.Join(f.runDir, f.spec.ID, "mysqld.sock") + " -uroot --batch -e SET GLOBAL innodb_fast_shutdown = 0") {
		t.Errorf("calls = %q", f.sys.calls)
	}
	if !f.sys.called("systemctl stop db-branch-mysqld@branch-feature-x.service") {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestStop_正常停止を確認できなければUncleanShutdown(t *testing.T) {
	for label, mutate := range map[string]func(*fakeSystem){
		"no shutdown line": func(s *fakeSystem) { s.shutdownOK = false },
		"unit failed":      func(s *fakeSystem) { s.stopResult = "signal" },
	} {
		f := newFixture(t)
		_ = f.srv.Start(ctx, f.spec)
		appendFile(f.sys.errorLog, "old: Shutdown complete\n")
		mutate(f.sys)

		if err := f.srv.Stop(ctx, f.spec, domain.StopClean); !errors.Is(err, domain.ErrUncleanShutdown) {
			t.Errorf("%s: error = %v, want ErrUncleanShutdown", label, err)
		}
	}
}

func TestStop_止まっていれば何もしない(t *testing.T) {
	f := newFixture(t)

	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err != nil {
		t.Fatal(err)
	}
	if f.sys.called("systemctl stop") {
		t.Error("should not stop an inactive unit")
	}
}

func TestStop_即時停止はプロセスを強制終了する(t *testing.T) {
	f := newFixture(t)
	_ = f.srv.Start(ctx, f.spec)

	if err := f.srv.Stop(ctx, f.spec, domain.StopImmediate); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("systemctl kill --signal=SIGKILL db-branch-mysqld@branch-feature-x.service") {
		t.Errorf("calls = %q", f.sys.calls)
	}
	if f.sys.called("mysql ") {
		t.Error("immediate stop should not talk SQL to the server")
	}
	if ok, _ := f.srv.Running(ctx, f.spec); ok {
		t.Error("should not be running")
	}
}

func TestExec_ローカル管理者としてSQLを流す(t *testing.T) {
	f := newFixture(t)

	if err := f.srv.Exec(ctx, f.spec, "CREATE DATABASE x"); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("mysql --socket=" + filepath.Join(f.runDir, f.spec.ID, "mysqld.sock") + " -uroot --batch -e CREATE DATABASE x") {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestLoad_ダンプを標準入力から流し込み圧縮も展開する(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "dump.sql")
	_ = os.WriteFile(plain, []byte("CREATE TABLE t (id int);"), 0o644)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("INSERT INTO t VALUES (1);"))
	_ = w.Close()
	zipped := filepath.Join(dir, "dump.sql.gz")
	_ = os.WriteFile(zipped, gz.Bytes(), 0o644)

	for _, p := range []string{plain, zipped} {
		if err := f.srv.Load(ctx, f.spec, p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	want := []string{"CREATE TABLE t (id int);", "INSERT INTO t VALUES (1);"}
	if strings.Join(f.sys.stdin, "|") != strings.Join(want, "|") {
		t.Errorf("stdin = %q", f.sys.stdin)
	}
}

func TestLoad_読めないダンプや壊れたgzipは失敗する(t *testing.T) {
	f := newFixture(t)
	bad := filepath.Join(t.TempDir(), "bad.sql.gz")
	_ = os.WriteFile(bad, []byte("not gzip"), 0o644)

	for _, p := range []string{filepath.Join(t.TempDir(), "missing.sql"), bad} {
		if err := f.srv.Load(ctx, f.spec, p); err == nil {
			t.Errorf("%s: want error", p)
		}
	}
}

func TestRemove_設定とログを消す(t *testing.T) {
	f := newFixture(t)
	_ = f.srv.Initialize(ctx, f.spec)

	if err := f.srv.Remove(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.state, f.spec.ID)); !os.IsNotExist(err) {
		t.Errorf("instance dir should be gone: %v", err)
	}
}

func TestコマンドやSQLの失敗はエラーにする(t *testing.T) {
	cases := map[string]func(fixture) error{
		"initialize": func(f fixture) error { return f.srv.Initialize(ctx, f.spec) },
		"exec":       func(f fixture) error { return f.srv.Exec(ctx, f.spec, "SELECT 1") },
		"load": func(f fixture) error {
			p := filepath.Join(f.spec.DataDir, "d.sql")
			_ = os.WriteFile(p, []byte("x"), 0o644)
			return f.srv.Load(ctx, f.spec, p)
		},
		"stop clean": func(f fixture) error {
			_ = f.srv.Start(ctx, f.spec)
			return f.srv.Stop(ctx, f.spec, domain.StopClean)
		},
	}
	fail := map[string]string{"initialize": "--initialize", "exec": "mysql ", "load": "mysql ", "stop clean": "innodb_fast_shutdown"}
	for name, op := range cases {
		f := newFixture(t)
		f.sys.failOn = fail[name]
		if err := op(f); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestStateRootに書けなければ失敗する(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o644)
	f.srv.StateRoot = file

	for name, op := range map[string]func() error{
		"initialize": func() error { return f.srv.Initialize(ctx, f.spec) },
		"clone":      func() error { return f.srv.PrepareClone(ctx, f.spec) },
		"start":      func() error { return f.srv.Start(ctx, f.spec) },
	} {
		if err := op(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestInitialize_設定とデータディレクトリをサービスユーザーに渡す(t *testing.T) {
	f := newFixture(t)
	var owned []string
	f.srv.Chown = func(p string) error { owned = append(owned, p); return nil }

	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.state, f.spec.ID)
	want := []string{dir, filepath.Join(dir, "my.cnf"), filepath.Join(dir, "env"), f.spec.DataDir}
	if strings.Join(owned, ",") != strings.Join(want, ",") {
		t.Errorf("chown %q, want %q", owned, want)
	}
}

func TestInitialize_所有者を変えられなければ失敗する(t *testing.T) {
	for _, failing := range []string{"instances", "data"} {
		f := newFixture(t)
		f.srv.Chown = func(p string) error {
			if strings.Contains(p, failing) {
				return errors.New("chown failed")
			}
			return nil
		}
		if err := f.srv.Initialize(ctx, f.spec); err == nil {
			t.Errorf("%s: want error", failing)
		}
	}
}

func Test設定ファイルを書けなければ失敗する(t *testing.T) {
	for _, name := range []string{"my.cnf", "env"} {
		f := newFixture(t)
		_ = os.MkdirAll(filepath.Join(f.state, f.spec.ID, name, "x"), 0o755)
		if err := f.srv.Initialize(ctx, f.spec); err == nil {
			t.Errorf("%s is a directory: want error", name)
		}
	}
}

func TestPrepareClone_消せないファイルがあれば失敗する(t *testing.T) {
	f := newFixture(t)
	_ = os.MkdirAll(filepath.Join(f.spec.DataDir, "auto.cnf", "x"), 0o755)

	if err := f.srv.PrepareClone(ctx, f.spec); err == nil {
		t.Fatal("want error")
	}
}

func TestStop_ログがまだなくても正常停止を確認できる(t *testing.T) {
	f := newFixture(t)
	f.sys.active = true

	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err != nil {
		t.Fatal(err)
	}
}

func TestStop_停止コマンドが失敗したらエラーにする(t *testing.T) {
	f := newFixture(t)
	f.sys.active = true
	f.sys.failOn = "systemctl stop"

	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err == nil || errors.Is(err, domain.ErrUncleanShutdown) {
		t.Fatalf("error = %v, want a stop failure", err)
	}
}

func TestStart_失敗時のエラーログは末尾だけを添える(t *testing.T) {
	f := newFixture(t)
	f.sys.pingFails = 1 << 30
	f.srv.StartTimeout = 10 * time.Millisecond
	_ = os.MkdirAll(filepath.Dir(f.sys.errorLog), 0o755)
	f.sys.onStart = strings.Repeat("noise ", 1000) + "LAST LINE\n"

	err := f.srv.Start(ctx, f.spec)
	if err == nil || !strings.Contains(err.Error(), "LAST LINE") || len(err.Error()) > 2300 {
		t.Fatalf("error length %d: %.80q", len(err.Error()), err)
	}
}

func TestInitialize_サービスユーザーが辿れるよう親ディレクトリは誰でも通れるようにする(t *testing.T) {
	f := newFixture(t)

	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.state)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("state root mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestCreateUser_どこからでも全権限で入れるユーザーを作る(t *testing.T) {
	f := newFixture(t)
	if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err != nil {
		t.Fatal(err)
	}
	sql := f.sys.calls[len(f.sys.calls)-1]
	if !strings.Contains(sql, "CREATE USER IF NOT EXISTS 'feature-x'@'%' IDENTIFIED BY 'pw1'") || !strings.Contains(sql, "GRANT ALL PRIVILEGES ON *.* TO 'feature-x'@'%' WITH GRANT OPTION") {
		t.Errorf("sql = %s", sql)
	}
}

func TestAdmin_ソケットのrootで入る道を返す(t *testing.T) {
	f := newFixture(t)
	a := f.srv.Admin(f.spec)
	if a.Engine != domain.MySQL || a.User != "root" || a.Port != f.spec.Port || a.BinDir != filepath.Join(f.rtDir, "bin") || !strings.HasPrefix(a.Socket, f.runDir) || a.Host != "127.0.0.1" {
		t.Errorf("%+v", a)
	}
	f.srv.Bind = "192.0.2.10"
	if a := f.srv.Admin(f.spec); a.Host != "192.0.2.10" {
		t.Errorf("bound to one address: %+v", a)
	}
}

func TestPrepareClone_モジュールの追加設定と保守モードを設定ファイルに書く(t *testing.T) {
	f := newFixture(t)
	spec := f.spec
	spec.Extra = []string{"log_bin=binlog", "gtid_mode=ON"}

	if err := f.srv.PrepareClone(ctx, spec); err != nil {
		t.Fatal(err)
	}
	cnf := read(t, filepath.Join(f.state, spec.ID, "my.cnf"))
	if !strings.Contains(cnf, "log_bin=binlog\ngtid_mode=ON\n") || strings.Contains(cnf, "skip-networking") {
		t.Errorf("my.cnf = %s", cnf)
	}

	spec.Maintenance = true
	if err := f.srv.PrepareClone(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if cnf := read(t, filepath.Join(f.state, spec.ID, "my.cnf")); !strings.Contains(cnf, "skip-networking\n") {
		t.Errorf("maintenance my.cnf = %s", cnf)
	}
}

func TestStart_サーバーが権限エラーで落ちたら待たずにErrOwnershipで失敗する(t *testing.T) {
	f := newFixture(t)
	f.sys.pingFails = 1 << 30
	f.sys.crashes = true
	f.srv.StartTimeout = time.Minute
	f.sys.onStart = "[ERROR] File ./binlog.index not found (OS errno 13 - Permission denied)\n"

	begin := time.Now()
	err := f.srv.Start(ctx, f.spec)
	if !errors.Is(err, domain.ErrOwnership) || !strings.Contains(err.Error(), "binlog.index") {
		t.Fatalf("error = %v", err)
	}
	if time.Since(begin) > 5*time.Second {
		t.Error("waited for the timeout")
	}
}

func TestStart_落ちた理由が権限でなくても待たずに失敗する(t *testing.T) {
	f := newFixture(t)
	f.sys.pingFails = 1 << 30
	f.sys.crashes = true
	f.srv.StartTimeout = time.Minute
	f.sys.onStart = "[ERROR] bad option\n"
	err := f.srv.Start(ctx, f.spec)
	if err == nil || errors.Is(err, domain.ErrOwnership) || !strings.Contains(err.Error(), "bad option") {
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

func TestUpdateStats_全データベースをANALYZEする(t *testing.T) {
	f := newFixture(t)
	var _ domain.StatsUpdater = f.srv
	if err := f.srv.UpdateStats(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	last := f.sys.calls[len(f.sys.calls)-1]
	if !strings.HasPrefix(last, "mysqlcheck ") || !strings.Contains(last, "--analyze --all-databases") || !strings.Contains(last, "-uroot") {
		t.Errorf("call = %q", last)
	}
	f.sys.failOn = "mysqlcheck"
	if err := f.srv.UpdateStats(ctx, f.spec); err == nil {
		t.Error("failing mysqlcheck: want error")
	}
}

// with5 gives the fixture a runtime of an old series.
func (f fixture) with(version string) fixture {
	f.spec.Runtime.Version = version
	return f
}

func TestInitialize_MySQL5系の設定にはmysqlxを書かない_8系には書く(t *testing.T) {
	for _, tc := range []struct {
		version string
		x       bool
	}{{"5.6.51", false}, {"5.7.44", false}, {"8.0.46", true}, {"8.4.13", true}, {"9.6.0", true}} {
		f := newFixture(t).with(tc.version)
		if err := f.srv.Initialize(ctx, f.spec); err != nil {
			t.Fatal(err)
		}
		cnf := read(t, filepath.Join(f.state, f.spec.ID, "my.cnf"))
		if got := strings.Contains(cnf, "mysqlx=OFF\n"); got != tc.x {
			t.Errorf("%s: mysqlx=OFF present = %v, want %v\n%s", tc.version, got, tc.x, cnf)
		}
	}
}

func TestInitialize_5_7は_initialize_insecureで_5_6はmysql_install_dbで初期化する(t *testing.T) {
	f := newFixture(t).with("5.7.44")
	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("mysqld --defaults-file=" + filepath.Join(f.state, f.spec.ID, "my.cnf") + " --initialize-insecure") {
		t.Errorf("5.7: %q", f.sys.calls)
	}
	g := newFixture(t).with("5.6.51")
	if err := g.srv.Initialize(ctx, g.spec); err != nil {
		t.Fatal(err)
	}
	want := "mysql_install_db --defaults-file=" + filepath.Join(g.state, g.spec.ID, "my.cnf") + " --basedir=" + g.rtDir + " --datadir=" + g.spec.DataDir + " --user=db-branch --skip-name-resolve"
	if !g.sys.called(want) {
		t.Errorf("5.6 should run %q, calls = %q", want, g.sys.calls)
	}
	if g.sys.called("mysqld --defaults-file=" + filepath.Join(g.state, g.spec.ID, "my.cnf") + " --initialize-insecure") {
		t.Error("5.6 has no --initialize-insecure")
	}
}

func TestCreateUser_5_7は従来の文で_5_6はGRANTでユーザーを作り匿名と遠隔rootを消す(t *testing.T) {
	f := newFixture(t).with("5.7.44")
	if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err != nil {
		t.Fatal(err)
	}
	if sql := f.sys.calls[len(f.sys.calls)-1]; !strings.Contains(sql, "CREATE USER IF NOT EXISTS 'feature-x'@'%' IDENTIFIED BY 'pw1'") {
		t.Errorf("5.7: %s", sql)
	}
	g := newFixture(t).with("5.6.51")
	if err := g.srv.CreateUser(ctx, g.spec, "feature-x", "pw1"); err != nil {
		t.Fatal(err)
	}
	sql := g.sys.calls[len(g.sys.calls)-1]
	if strings.Contains(sql, "CREATE USER IF NOT EXISTS") {
		t.Errorf("5.6 cannot CREATE USER IF NOT EXISTS: %s", sql)
	}
	for _, want := range []string{"GRANT ALL PRIVILEGES ON *.* TO 'feature-x'@'%' IDENTIFIED BY 'pw1' WITH GRANT OPTION", "DELETE FROM mysql.user WHERE User = ''", "User = 'root' AND Host <> 'localhost'", "FLUSH PRIVILEGES"} {
		if !strings.Contains(sql, want) {
			t.Errorf("5.6 sql lacks %q: %s", want, sql)
		}
	}
}
