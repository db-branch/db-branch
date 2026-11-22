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
	envs       [][]string
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
	// onCall runs before executing a call (e.g. to cancel ctx mid-command).
	onCall func(call string)
}

func (f *fakeSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.RunInputEnv(ctx, nil, nil, name, args...)
}

func (f *fakeSystem) RunInput(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error) {
	return f.RunInputEnv(ctx, nil, in, name, args...)
}

func (f *fakeSystem) RunInputEnv(ctx context.Context, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
	f.calls = append(f.calls, call)
	f.envs = append(f.envs, append([]string(nil), env...))
	if f.onCall != nil {
		f.onCall(call)
		if ctx.Err() != nil {
			return nil, &execx.Error{Command: call, Err: errors.New("signal: killed")}
		}
	}
	var input string
	if in != nil {
		b, _ := io.ReadAll(in)
		input = string(b)
		f.stdin = append(f.stdin, input)
	}
	if f.failOn != "" && (strings.Contains(call, f.failOn) || strings.Contains(input, f.failOn)) {
		return nil, &execx.Error{Command: call, Stderr: "injected failure", Err: errors.New("exit 1")}
	}
	for k, v := range f.outputs {
		if strings.Contains(call, k) || strings.Contains(input, k) {
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
	f.sys.errorLog = filepath.Join(f.state, f.spec.ID, "log", "error.log")
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
		"log-error=" + filepath.Join(f.state, f.spec.ID, "log", "error.log"),
		"innodb_buffer_pool_size=128M",
		"innodb_doublewrite=OFF",
		"innodb_flush_neighbors=0",
		"mysqlx=OFF",
		// No file import or export through SQL, whatever a login may do.
		"secure_file_priv=NULL",
		"local_infile=OFF",
		"partial_revokes=ON",
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
	if !f.sys.called("mysql --socket="+filepath.Join(f.runDir, f.spec.ID, "mysqld.sock")+" -uroot --batch") ||
		!strings.Contains(strings.Join(f.sys.stdin, "\n"), "SET GLOBAL innodb_fast_shutdown = 0") {
		t.Errorf("calls = %q stdin = %q", f.sys.calls, f.sys.stdin)
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
	appendFile(f.sys.errorLog, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")

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
	if !f.sys.called("mysql --socket="+filepath.Join(f.runDir, f.spec.ID, "mysqld.sock")+" -uroot --batch") ||
		strings.Join(f.sys.stdin, "\n") != "CREATE DATABASE x" {
		t.Errorf("calls = %q stdin = %q", f.sys.calls, f.sys.stdin)
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

func TestInitialize_設定はrootの持ち物にしログとデータだけをサービスユーザーに渡す(t *testing.T) {
	f := newFixture(t)
	var owned, config []string
	f.srv.Chown = func(p string) error { owned = append(owned, p); return nil }
	f.srv.ChownConfig = func(p string) error { config = append(config, p); return nil }

	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.state, f.spec.ID)
	wantConfig := []string{dir, filepath.Join(dir, "my.cnf"), filepath.Join(dir, "env")}
	if strings.Join(config, ",") != strings.Join(wantConfig, ",") {
		t.Errorf("config chown %q, want %q", config, wantConfig)
	}
	wantOwned := []string{filepath.Join(dir, "log"), f.spec.DataDir}
	if strings.Join(owned, ",") != strings.Join(wantOwned, ",") {
		t.Errorf("service chown %q, want %q", owned, wantOwned)
	}
}

func TestInitialize_エラーログは設定ディレクトリの下のサービスユーザーのディレクトリに置く(t *testing.T) {
	f := newFixture(t)
	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.state, f.spec.ID)
	if cnf := read(t, filepath.Join(dir, "my.cnf")); !strings.Contains(cnf, "log-error="+filepath.Join(dir, "log", "error.log")+"\n") {
		t.Errorf("my.cnf = %s", cnf)
	}
	if info, err := os.Stat(filepath.Join(dir, "log")); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("log dir: %v %v", info, err)
	}
}

func TestInitialize_旧い場所のエラーログは新しい場所へ移す(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.state, f.spec.ID)
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "error.log"), []byte("Shutdown complete\n"), 0o640)

	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "log", "error.log")); got != "Shutdown complete\n" {
		t.Errorf("moved log = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "error.log")); err == nil {
		t.Error("old error.log remains")
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

func TestInitialize_設定をrootの持ち物にできなければ失敗する(t *testing.T) {
	f := newFixture(t)
	f.srv.ChownConfig = func(string) error { return errors.New("chown failed") }
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("want error")
	}
}

func TestInitialize_ログディレクトリを作れなければ失敗する(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.state, f.spec.ID)
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "log"), nil, 0o640)
	if err := f.srv.Initialize(ctx, f.spec); err == nil {
		t.Error("log is a file: want error")
	}
}

func TestInitialize_設定ファイルの場所にあるリンクは辿らない(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.state, f.spec.ID)
	_ = os.MkdirAll(dir, 0o750)
	victim := filepath.Join(t.TempDir(), "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	for _, name := range []string{"my.cnf", "env"} {
		_ = os.Symlink(victim, filepath.Join(dir, name))
	}
	if err := f.srv.Initialize(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Errorf("link target was written: %q", got)
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

func TestCreateUser_どこからでも入れるがサーバーのファイルや管理の権限は持たない(t *testing.T) {
	f := newFixture(t)
	if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err != nil {
		t.Fatal(err)
	}
	sql := strings.Join(f.sys.stdin, "\n")
	if !strings.Contains(sql, "CREATE USER IF NOT EXISTS 'feature-x'@'%' IDENTIFIED BY 'pw1'") || !strings.Contains(sql, "TO 'feature-x'@'%'") {
		t.Errorf("sql = %s", sql)
	}
	// 8.0.16 and later: nothing in the mysql schema (INSERT there is enough to CREATE USER).
	if !strings.Contains(sql, "SET GLOBAL partial_revokes = ON") || !strings.Contains(sql, " ON mysql.* FROM 'feature-x'@'%'") {
		t.Errorf("the login can reach the mysql schema: %s", sql)
	}
	// What an older branch had is taken back first, and the grant names no server-level privilege.
	if !strings.Contains(sql, "REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'feature-x'@'%'") {
		t.Errorf("an old account keeps its privileges: %s", sql)
	}
	grant := sql[strings.Index(sql, "GRANT SELECT"):strings.Index(sql, "SET GLOBAL")]
	for _, bad := range []string{"ALL PRIVILEGES", "FILE", "SUPER", "SHUTDOWN", "RELOAD", "PROCESS", "CREATE USER", "REPLICATION", "GRANT OPTION"} {
		if strings.Contains(grant, bad) {
			t.Errorf("the grant has %s: %s", bad, grant)
		}
	}
}

func TestMySQL_ExecAsはパスワードをコマンドライン引数に渡さず環境変数とstdinでSQLを実行する(t *testing.T) {
	f := newFixture(t)
	pw := "s3cr3t=pw 'with' space"
	sql := "DELIMITER //\r\nUPDATE t SET a = 1//\r\nDELIMITER ;\r\nSELECT 1"
	if err := f.srv.ExecAs(ctx, f.spec, "feature-x", pw, sql); err != nil {
		t.Fatal(err)
	}
	call := f.sys.calls[len(f.sys.calls)-1]
	if !strings.HasPrefix(call, "mysql ") || !strings.Contains(call, "--binary-mode") || !strings.Contains(call, "--user=feature-x") || strings.Contains(call, "-uroot") {
		t.Errorf("call = %q", call)
	}
	if strings.Contains(call, "env ") || strings.Contains(call, "MYSQL_PWD") || strings.Contains(call, pw) {
		t.Errorf("password must not appear in argv: %q", call)
	}
	if strings.Contains(call, "-e") || strings.Contains(call, "UPDATE t") {
		t.Errorf("SQL must be passed on stdin, not -e argv: %q", call)
	}
	env := f.sys.envs[len(f.sys.envs)-1]
	if len(env) != 1 || env[0] != "MYSQL_PWD="+pw {
		t.Errorf("env = %q, want [MYSQL_PWD=%s]", env, pw)
	}
	wantStdin := "DELIMITER //\nUPDATE t SET a = 1//\nDELIMITER ;\nSELECT 1"
	if len(f.sys.stdin) == 0 || f.sys.stdin[len(f.sys.stdin)-1] != wantStdin {
		t.Errorf("stdin = %q, want %q", f.sys.stdin, wantStdin)
	}

	// On error, the password must not leak into the returned error.
	f.sys.failOn = "mysql "
	err := f.srv.ExecAs(ctx, f.spec, "feature-x", pw, sql)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("error leaked password: %q", err.Error())
	}

	// Empty user or NUL byte in user/password must be rejected before running mysql.
	for _, tc := range []struct{ u, p string }{{"", "pw"}, {"bad\x00u", "pw"}, {"u", "bad\x00pw"}} {
		f2 := newFixture(t)
		if err := f2.srv.ExecAs(ctx, f2.spec, tc.u, tc.p, "SELECT 1"); err == nil {
			t.Errorf("ExecAs(%q, %q): want error", tc.u, tc.p)
		}
		if len(f2.sys.calls) != 0 {
			t.Errorf("ExecAs(%q, %q) ran command: %q", tc.u, tc.p, f2.sys.calls)
		}
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
	if sql := f.sys.stdin[len(f.sys.stdin)-1]; !strings.Contains(sql, "CREATE USER IF NOT EXISTS 'feature-x'@'%' IDENTIFIED BY 'pw1'") {
		t.Errorf("5.7: %s", sql)
	}
	g := newFixture(t).with("5.6.51")
	if err := g.srv.CreateUser(ctx, g.spec, "feature-x", "pw1"); err != nil {
		t.Fatal(err)
	}
	sql := g.sys.stdin[len(g.sys.stdin)-1]
	if strings.Contains(sql, "CREATE USER IF NOT EXISTS") {
		t.Errorf("5.6 cannot CREATE USER IF NOT EXISTS: %s", sql)
	}
	for _, want := range []string{"GRANT USAGE ON *.* TO 'feature-x'@'%' IDENTIFIED BY 'pw1'", "REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'feature-x'@'%'", "DELETE FROM mysql.user WHERE User = ''", "User = 'root' AND Host <> 'localhost'", "FLUSH PRIVILEGES"} {
		if !strings.Contains(sql, want) {
			t.Errorf("5.6 sql lacks %q: %s", want, sql)
		}
	}
}

func TestCreateUser_mysqlスキーマの権限を外すのは8_0_16以降だけ(t *testing.T) {
	for _, tc := range []struct {
		version string
		revoke  bool
	}{{"5.6.51", false}, {"5.7.44", false}, {"8.0", false}, {"8.0.x", false}, {"8.0.15", false}, {"8.0.16", true}, {"8.0.46", true}, {"8.4.13", true}, {"9.6.0", true}} {
		f := newFixture(t).with(tc.version)
		if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err != nil {
			t.Fatal(err)
		}
		sql := strings.Join(f.sys.stdin, "\n")
		if got := strings.Contains(sql, " ON mysql.* FROM 'feature-x'@'%'") && strings.Contains(sql, "SET GLOBAL partial_revokes = ON"); got != tc.revoke {
			t.Errorf("%s: revoke on the mysql schema = %v, want %v", tc.version, got, tc.revoke)
		}
		// The option is written to my.cnf only where the runtime knows it.
		if err := f.srv.Initialize(ctx, f.spec); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(read(t, filepath.Join(f.state, f.spec.ID, "my.cnf")), "partial_revokes=ON\n"); got != tc.revoke {
			t.Errorf("%s: partial_revokes in my.cnf = %v, want %v", tc.version, got, tc.revoke)
		}
	}
}

func TestMySQL_partialRevokesは8_0_36_custom等のサフィックス付きバージョンでもtrueを返す(t *testing.T) {
	for _, tc := range []struct {
		version string
		revoke  bool
	}{
		{"8.0.15-custom", false},
		{"8.0.16-rc1", true},
		{"8.0.36-custom", true},
		{"8.0.36-0ubuntu0.24.04.1", true},
		{"8.0-custom", false},
		{"8.0.x-custom", false},
		{"8.4.0-custom", true},
		{"9.6.0-rc1", true},
	} {
		f := newFixture(t).with(tc.version)
		if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err != nil {
			t.Fatal(err)
		}
		sql := strings.Join(f.sys.stdin, "\n")
		if got := strings.Contains(sql, " ON mysql.* FROM 'feature-x'@'%'") && strings.Contains(sql, "SET GLOBAL partial_revokes = ON"); got != tc.revoke {
			t.Errorf("%s: revoke on the mysql schema = %v, want %v", tc.version, got, tc.revoke)
		}
		if err := f.srv.Initialize(ctx, f.spec); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(read(t, filepath.Join(f.state, f.spec.ID, "my.cnf")), "partial_revokes=ON\n"); got != tc.revoke {
			t.Errorf("%s: partial_revokes in my.cnf = %v, want %v", tc.version, got, tc.revoke)
		}
	}
}

func TestCreateUser_引用符やバックスラッシュを含むパスワードが安全にエスケープされMySQL5_7以降もパスワード同期される(t *testing.T) {
	pw := `p'w\1` + "\n\r\x1a" + `end\`
	wantPW := `'p''w\\1\n\r\Zend\\'`
	user := `u's\r`
	wantUser := `'u''s\\r'`
	wantModeReset := "SET SESSION sql_mode = REPLACE(@@SESSION.sql_mode, 'NO_BACKSLASH_ESCAPES', '');"

	// MySQL 8.4 (LegacyAuth=false): clears NO_BACKSLASH_ESCAPES on every session
	// that embeds quoted literals, escapes user & password, uses IDENTIFIED WITH
	// caching_sha2_password BY to avoid Error 1524 on legacy accounts, revokes
	// mysql.*, and keeps password off argv.
	f := newFixture(t).with("8.4.13")
	if err := f.srv.CreateUser(ctx, f.spec, user, pw); err != nil {
		t.Fatal(err)
	}
	if len(f.sys.stdin) != 3 {
		t.Fatalf("8.4 stdin calls = %d, want 3: %q", len(f.sys.stdin), f.sys.stdin)
	}
	if !strings.HasPrefix(f.sys.stdin[0], wantModeReset) {
		t.Errorf("8.4 createUser stdin[0] does not start with sql_mode reset:\n%s", f.sys.stdin[0])
	}
	if !strings.HasPrefix(f.sys.stdin[2], wantModeReset) || !strings.Contains(f.sys.stdin[2], " ON mysql.* FROM "+wantUser+"@'%'") {
		t.Errorf("8.4 revoke stdin[2] lacks leading sql_mode reset or escaped user:\n%s", f.sys.stdin[2])
	}
	for _, want := range []string{
		"CREATE USER IF NOT EXISTS " + wantUser + "@'%' IDENTIFIED BY " + wantPW,
		"ALTER USER " + wantUser + "@'%' IDENTIFIED WITH caching_sha2_password BY " + wantPW,
		"REVOKE ALL PRIVILEGES, GRANT OPTION FROM " + wantUser + "@'%'",
	} {
		if !strings.Contains(f.sys.stdin[0], want) {
			t.Errorf("8.4 stdin[0] lacks %q:\n%s", want, f.sys.stdin[0])
		}
	}
	if calls := strings.Join(f.sys.calls, "\n"); strings.Contains(calls, pw) || strings.Contains(calls, wantPW) {
		t.Errorf("password leaked to argv: %s", calls)
	}

	// MySQL 8.4 (LegacyAuth=true) and 8.0: keep IDENTIFIED BY without forcing plugin switch.
	for _, tc := range []struct {
		version string
		legacy  bool
	}{{"8.4.13", true}, {"8.0.46", false}} {
		fx := newFixture(t).with(tc.version)
		fx.spec.LegacyAuth = tc.legacy
		if err := fx.srv.CreateUser(ctx, fx.spec, user, pw); err != nil {
			t.Fatal(err)
		}
		if s := fx.sys.stdin[0]; !strings.Contains(s, "ALTER USER "+wantUser+"@'%' IDENTIFIED BY "+wantPW) {
			t.Errorf("%s (legacy=%v) stdin[0] lacks ALTER USER IDENTIFIED BY:\n%s", tc.version, tc.legacy, s)
		}
	}

	// MySQL 9.x: always uses IDENTIFIED WITH caching_sha2_password BY even if LegacyAuth=true.
	f9Legacy := newFixture(t).with("9.6.0")
	f9Legacy.spec.LegacyAuth = true
	if err := f9Legacy.srv.CreateUser(ctx, f9Legacy.spec, user, pw); err != nil {
		t.Fatal(err)
	}
	if s := f9Legacy.sys.stdin[0]; !strings.Contains(s, "ALTER USER "+wantUser+"@'%' IDENTIFIED WITH caching_sha2_password BY "+wantPW) {
		t.Errorf("9.6 (legacy=true) stdin[0] lacks caching_sha2_password:\n%s", s)
	}

	// MySQL 5.7: also runs ALTER USER to sync password on existing accounts.
	f57 := newFixture(t).with("5.7.44")
	if err := f57.srv.CreateUser(ctx, f57.spec, user, pw); err != nil {
		t.Fatal(err)
	}
	if s := f57.sys.stdin[0]; !strings.HasPrefix(s, wantModeReset) || !strings.Contains(s, "ALTER USER "+wantUser+"@'%' IDENTIFIED BY "+wantPW) {
		t.Errorf("5.7 stdin[0] lacks leading sql_mode reset or ALTER USER:\n%s", s)
	}

	// MySQL 5.6: escapes user & password in GRANT ... IDENTIFIED BY and never emits ALTER USER.
	f56 := newFixture(t).with("5.6.51")
	if err := f56.srv.CreateUser(ctx, f56.spec, user, pw); err != nil {
		t.Fatal(err)
	}
	s56 := f56.sys.stdin[0]
	if !strings.HasPrefix(s56, wantModeReset) || !strings.Contains(s56, "GRANT USAGE ON *.* TO "+wantUser+"@'%' IDENTIFIED BY "+wantPW) {
		t.Errorf("5.6 stdin[0] lacks leading sql_mode reset or escaped GRANT:\n%s", s56)
	}
	if strings.Contains(s56, "ALTER USER") {
		t.Errorf("5.6 must not emit ALTER USER:\n%s", s56)
	}

	// MySQL 9.6 upgrade convertOwn: unescapes mysql --batch TSV output (\\ -> \) before quoting,
	// sends query via stdin (not -e), clears NO_BACKSLASH_ESCAPES on both SELECT and ALTER USER,
	// and keeps password off argv.
	f96 := newFixture(t).with("9.6.0")
	// Real mysql --batch escapes "\" as "\\" and "\t" as "\t" in stdout.
	f96.sys.outputs = map[string]string{"AND plugin = 'mysql_native_password'": "h'o\\\\st\\t1\\n2\\0x\n"}
	if _, err := f96.srv.StartUpgraded(ctx, f96.spec, domain.Login{User: user, Password: pw}); err != nil {
		t.Fatal(err)
	}
	if len(f96.sys.stdin) != 2 {
		t.Fatalf("9.6 convertOwn stdin calls = %d, want 2: %q", len(f96.sys.stdin), f96.sys.stdin)
	}
	if !strings.HasPrefix(f96.sys.stdin[0], wantModeReset) || !strings.Contains(f96.sys.stdin[0], "WHERE user = "+wantUser+" AND plugin = 'mysql_native_password'") {
		t.Errorf("9.6 query stdin[0] lacks leading sql_mode reset or escaped user:\n%s", f96.sys.stdin[0])
	}
	wantAlter := "ALTER USER " + wantUser + "@'h''o\\\\st\t1\\n2\\0x' IDENTIFIED WITH caching_sha2_password BY " + wantPW
	if !strings.HasPrefix(f96.sys.stdin[1], wantModeReset) || !strings.Contains(f96.sys.stdin[1], wantAlter) {
		t.Errorf("9.6 convertOwn stdin[1] lacks leading sql_mode reset or %q:\n%s", wantAlter, f96.sys.stdin[1])
	}
	if calls := strings.Join(f96.sys.calls, "\n"); strings.Contains(calls, pw) || strings.Contains(calls, wantPW) || strings.Contains(calls, "SELECT host") {
		t.Errorf("9.6 convertOwn leaked password or query to argv: %s", calls)
	}

	// Empty user or NUL bytes in user/password must be rejected before running mysql.
	for _, tc := range []struct{ u, p string }{{"", "pw"}, {"bad\x00u", "pw"}, {"u", "bad\x00pw"}} {
		fBad := newFixture(t)
		if err := fBad.srv.CreateUser(ctx, fBad.spec, tc.u, tc.p); err == nil {
			t.Errorf("CreateUser(%q, %q): want error", tc.u, tc.p)
		}
		if len(fBad.sys.calls) != 0 {
			t.Errorf("CreateUser(%q, %q) ran command: %q", tc.u, tc.p, fBad.sys.calls)
		}
	}
	// StartUpgraded convertOwn: empty user skips conversion without touching anonymous accounts;
	// NUL in user or password fails without running mysql.
	fEmpty96 := newFixture(t).with("9.6.0")
	fEmpty96.sys.outputs = map[string]string{"AND plugin = 'mysql_native_password'": "localhost\n"}
	if _, err := fEmpty96.srv.StartUpgraded(ctx, fEmpty96.spec, domain.Login{User: "", Password: "pw"}); err != nil {
		t.Fatalf("StartUpgraded with empty login: %v", err)
	}
	if len(fEmpty96.sys.stdin) != 0 {
		t.Errorf("StartUpgraded with empty user must not query/alter accounts: %q", fEmpty96.sys.stdin)
	}
	for _, tc := range []domain.Login{{User: "bad\x00u", Password: "pw"}, {User: "u", Password: "bad\x00pw"}} {
		fBad96 := newFixture(t).with("9.6.0")
		if _, err := fBad96.srv.StartUpgraded(ctx, fBad96.spec, tc); err == nil {
			t.Errorf("StartUpgraded convertOwn(%+v): want error", tc)
		}
		if len(fBad96.sys.stdin) != 0 {
			t.Errorf("StartUpgraded convertOwn(%+v) ran mysql: %q", tc, fBad96.sys.stdin)
		}
	}
}

func TestCreateUser_権限を外す文の失敗は返す(t *testing.T) {
	for name, fail := range map[string]string{"set": "SET GLOBAL partial_revokes", "revoke": " ON mysql.* FROM"} {
		f := newFixture(t)
		f.sys.failOn = fail
		if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestStopClean_既に異常停止済みのサーバーに対してverifyShutdownを実行しErrUncleanShutdownを返す(t *testing.T) {
	for label, setup := range map[string]func(*fixture){
		"no error log": func(*fixture) {},
		"crashed after previous clean stop": func(f *fixture) {
			appendFile(f.sys.errorLog, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n[Server] /mysqld (mysqld 8.4.13) starting as process 42\n[ERROR] crashed\n")
		},
		"unit failed despite old shutdown line": func(f *fixture) {
			appendFile(f.sys.errorLog, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
			f.sys.stopResult = "signal"
		},
		"mysql 5.7 crashed during InnoDB init after previous clean stop": func(f *fixture) {
			appendFile(f.sys.errorLog, "[Note] /mysqld: ready for connections.\n[Note] /mysqld: Shutdown complete\n[Note] InnoDB: Initializing buffer pool, total size = 128M\n")
		},
	} {
		f := newFixture(t)
		setup(&f)
		err := f.srv.Stop(ctx, f.spec, domain.StopClean)
		if !errors.Is(err, domain.ErrUncleanShutdown) {
			t.Errorf("%s: Stop(StopClean) = %v, want ErrUncleanShutdown", label, err)
		}
		if f.sys.called("systemctl stop") {
			t.Errorf("%s: should not call systemctl stop on inactive unit: %q", label, f.sys.calls)
		}
	}

	// PrepareClone removes stale error.log so a restored/cloned dataset cannot reuse an old shutdown line.
	fp := newFixture(t)
	appendFile(fp.sys.errorLog, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
	if err := fp.srv.PrepareClone(ctx, fp.spec); err != nil {
		t.Fatal(err)
	}
	if err := fp.srv.Stop(ctx, fp.spec, domain.StopClean); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Errorf("after PrepareClone without Start: StopClean = %v, want ErrUncleanShutdown", err)
	}

	// activating / deactivating states must NOT be treated as stopped.
	for _, st := range []string{"activating", "deactivating"} {
		fa := newFixture(t)
		appendFile(fa.sys.errorLog, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
		fa.sys.outputs = map[string]string{"--property=ActiveState": st + "\n"}
		if err := fa.srv.Stop(ctx, fa.spec, domain.StopImmediate); err != nil {
			t.Fatalf("%s StopImmediate: %v", st, err)
		}
		if !fa.sys.called("systemctl kill --signal=SIGKILL") {
			t.Errorf("%s StopImmediate did not kill unit: %q", st, fa.sys.calls)
		}

		fc := newFixture(t)
		appendFile(fc.sys.errorLog, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
		fc.sys.outputs = map[string]string{"--property=ActiveState": st + "\n"}
		fc.sys.failOn = "innodb_fast_shutdown"
		if err := fc.srv.Stop(ctx, fc.spec, domain.StopClean); err == nil || errors.Is(err, domain.ErrUncleanShutdown) {
			t.Errorf("%s StopClean: err = %v, want stop/exec failure (not nil or ErrUncleanShutdown)", st, err)
		}
	}

	// Cancelled ctx and systemctl show errors must NOT be turned into nil or ErrUncleanShutdown.
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

	// Cleanly stopped previously: succeeds without calling systemctl stop.
	clean := newFixture(t)
	appendFile(clean.sys.errorLog, "[Server] /mysqld: ready for connections.\n[Server] /mysqld: Shutdown complete (mysqld 8.4.13)\n")
	if err := clean.srv.Stop(ctx, clean.spec, domain.StopClean); err != nil {
		t.Fatalf("cleanly stopped: %v", err)
	}
	if clean.sys.called("systemctl stop") {
		t.Errorf("cleanly stopped: called systemctl stop: %q", clean.sys.calls)
	}

	// StopImmediate on a stopped server skips shutdown verification.
	imm := newFixture(t)
	imm.sys.stopResult = "signal"
	if err := imm.srv.Stop(ctx, imm.spec, domain.StopImmediate); err != nil {
		t.Fatalf("StopImmediate on stopped: %v", err)
	}
}

func TestStart_ctxキャンセル時に即座に待機中断し起動途中ユニットを停止する(t *testing.T) {
	// Cancelled while waiting for mysqld to become ready: stops polling right away and stops the unit.
	f := newFixture(t)
	f.sys.pingFails = 1 << 30
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
	if !f.sys.called("systemctl stop db-branch-mysqld@branch-feature-x.service") {
		t.Errorf("in-flight unit was not stopped: %q", f.sys.calls)
	}
	if f.sys.active {
		t.Error("unit should no longer be active after cancel")
	}

	// If graceful stop fails while aborting, falls back to SIGKILL.
	fk := newFixture(t)
	fk.sys.pingFails = 1 << 30
	fk.sys.failOn = "systemctl stop"
	fk.srv.StartTimeout = time.Minute
	fk.srv.PollInterval = 5 * time.Second
	kctx, kcancel := context.WithCancel(ctx)
	ktimer := time.AfterFunc(20*time.Millisecond, kcancel)
	defer ktimer.Stop()
	if err := fk.srv.Start(kctx, fk.spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("fallback err = %v, want context.Canceled", err)
	}
	if !fk.sys.called("systemctl kill --signal=SIGKILL db-branch-mysqld@branch-feature-x.service") {
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
	ft.sys.pingFails = 1 << 30
	ft.srv.StartTimeout = 10 * time.Millisecond
	if err := ft.srv.Start(ctx, ft.spec); err == nil {
		t.Fatal("timeout: want error")
	}
	if !ft.sys.called("systemctl stop db-branch-mysqld@branch-feature-x.service") || ft.sys.active {
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
	if !fMid.sys.called("systemctl stop db-branch-mysqld@branch-feature-x.service") || fMid.sys.active {
		t.Errorf("mid-start cancelled unit left running: active=%v calls=%q", fMid.sys.active, fMid.sys.calls)
	}

	// StartUpgraded cancelled: preserves context.Canceled, stops unit, and still collects journal excerpt.
	fUp := newFixture(t)
	fUp.sys.pingFails = 1 << 30
	fUp.sys.outputs = map[string]string{"journalctl": "upgrade interrupted log\n"}
	fUp.srv.UpgradeTimeout = time.Minute
	fUp.srv.PollInterval = 5 * time.Second
	upCtx, upCancel := context.WithCancel(ctx)
	upTimer := time.AfterFunc(20*time.Millisecond, upCancel)
	defer upTimer.Stop()
	upLog, err := fUp.srv.StartUpgraded(upCtx, fUp.spec, domain.Login{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartUpgraded cancel err = %v, want context.Canceled", err)
	}
	if !strings.Contains(upLog.Excerpt, "Shutdown complete") {
		t.Errorf("StartUpgraded cancel excerpt = %q, want the error log of the aborted start", upLog.Excerpt)
	}
	if fUp.sys.active {
		t.Error("StartUpgraded unit left active after cancel")
	}
}

func TestStop_状態や結果を確かめられなければエラーにしキャンセルはそのまま返す(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	f := newFixture(t)
	if err := f.srv.Stop(cancelled, f.spec, domain.StopClean); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Stop = %v, want context.Canceled", err)
	}

	for name, fail := range map[string]string{
		"状態を読めない":     "ActiveState",
		"停止後の結果を読めない": "Result",
	} {
		f := newFixture(t)
		f.sys.active = true
		f.sys.shutdownOK = true
		f.sys.failOn = fail
		if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err == nil || errors.Is(err, domain.ErrUncleanShutdown) {
			t.Errorf("%s: error = %v, want a check failure", name, err)
		}
	}

	// A stopped unit whose result cannot be read is not assumed clean.
	f = newFixture(t)
	f.sys.failOn = "Result"
	if err := f.srv.Stop(ctx, f.spec, domain.StopClean); err == nil {
		t.Error("already-stopped unit with unreadable result passed")
	}
}

func TestStop_キャンセルされたら各段階でcontextエラーを返す(t *testing.T) {
	for _, step := range []string{"mysql --socket", "systemctl stop", "Result"} {
		f := newFixture(t)
		f.sys.active = true
		f.sys.shutdownOK = true
		c, cancel := context.WithCancel(ctx)
		f.sys.onCall = func(call string) {
			if strings.Contains(call, step) {
				cancel()
			}
		}
		f.sys.failOn = step
		if err := f.srv.Stop(c, f.spec, domain.StopClean); !errors.Is(err, context.Canceled) {
			t.Errorf("cancel at %q: error = %v, want context.Canceled", step, err)
		}
	}
}
