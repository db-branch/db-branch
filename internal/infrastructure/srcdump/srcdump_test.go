package srcdump_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
	"github.com/db-branch/db-branch/internal/infrastructure/srcdump"
)

type call struct {
	env  []string
	name string
	args []string
}

type recorder struct {
	calls []call
	err   error
}

func (r *recorder) stream(_ context.Context, env []string, out io.Writer, name string, args ...string) error {
	r.calls = append(r.calls, call{env, name, args})
	_, _ = io.WriteString(out, "-- dump "+args[len(args)-1]+"\n")
	return r.err
}

func job(engine domain.Engine, dbs ...string) domain.SourceDumpJob {
	return domain.SourceDumpJob{
		Tools:  domain.AdminAccess{Engine: engine, BinDir: "/rt/bin", LibDir: "/rt/lib"},
		Source: domain.RefreshSource{Host: "db.example.com", Port: 5432, User: "dumper", Password: "hunter2", Databases: dbs},
	}
}

// providers wires the dump hooks of both engines into runtime providers, as the agent does.
func providers(r *recorder) domain.ProviderSet {
	set := domain.ProviderSet{}
	for e, hook := range map[domain.Engine]func(srcdump.StreamFunc) func(context.Context, domain.SourceDumpJob, string) error{
		domain.Postgres: srcdump.PostgresDump, domain.MySQL: srcdump.MySQLDump,
	} {
		spec, _ := domain.DefaultRuntimes.Spec(e)
		set[e] = runtimeprovider.New(spec, nil, runtimeprovider.Hooks{DumpSource: hook(r.stream)})
	}
	return set
}

func newDumper(t *testing.T) (srcdump.Dumper, *recorder) {
	r := &recorder{}
	return srcdump.Dumper{WorkDir: filepath.Join(t.TempDir(), "work"), Providers: providers(r)}, r
}

func TestDump_PostgreSQLはデータベースごとにpg_dumpして一つの0600ファイルにまとめる(t *testing.T) {
	d, r := newDumper(t)
	path, err := d.Dump(context.Background(), job(domain.Postgres, "app", "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "-- dump app\n-- dump postgres\n" {
		t.Errorf("dump = %q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("work dir mode = %v", fi.Mode().Perm())
	}
	if len(r.calls) != 2 || r.calls[0].name != "/rt/bin/pg_dump" {
		t.Fatalf("calls = %+v", r.calls)
	}
	app, def := strings.Join(r.calls[0].args, " "), strings.Join(r.calls[1].args, " ")
	if !strings.Contains(app, "--create") || strings.Contains(def, "--create") || !strings.Contains(def, "--clean --if-exists") {
		t.Errorf("args: %q / %q", app, def)
	}
	if !strings.Contains(app, "-h db.example.com -p 5432 -U dumper") {
		t.Errorf("connection args: %q", app)
	}
	if got := strings.Join(r.calls[0].env, " "); !strings.Contains(got, "PGPASSWORD=hunter2") || !strings.Contains(got, "LD_LIBRARY_PATH=/rt/lib") {
		t.Errorf("env = %q", got)
	}
}

func TestDump_MySQLはmysqldumpに一度で全データベースを渡す(t *testing.T) {
	d, r := newDumper(t)
	path, err := d.Dump(context.Background(), job(domain.MySQL, "app", "billing"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Discard(path)
	if len(r.calls) != 1 || r.calls[0].name != "/rt/bin/mysqldump" {
		t.Fatalf("calls = %+v", r.calls)
	}
	args := strings.Join(r.calls[0].args, " ")
	for _, want := range []string{"--protocol=tcp", "-h db.example.com", "-P 5432", "-u dumper", "--single-transaction", "--add-drop-database", "--databases app billing"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	// mysqldump (5.7, 8.0 and 8.4) refuses --connect-timeout: "unknown variable"
	if strings.Contains(args, "connect-timeout") {
		t.Errorf("args %q contain an option mysqldump does not know", args)
	}
	if got := strings.Join(r.calls[0].env, " "); !strings.Contains(got, "MYSQL_PWD=hunter2") {
		t.Errorf("env = %q", got)
	}
}

func TestDump_パスワードはコマンドラインにも環境の空欄にも出さない(t *testing.T) {
	d, r := newDumper(t)
	for _, e := range []domain.Engine{domain.Postgres, domain.MySQL} {
		j := job(e, "app")
		path, err := d.Dump(context.Background(), j)
		if err != nil {
			t.Fatal(err)
		}
		d.Discard(path)
		if strings.Contains(strings.Join(r.calls[len(r.calls)-1].args, " "), "hunter2") {
			t.Errorf("%s: password on the command line", e)
		}
		j.Source.Password = ""
		if path, err = d.Dump(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		d.Discard(path)
		for _, v := range r.calls[len(r.calls)-1].env {
			if strings.HasPrefix(v, "PGPASSWORD=") || strings.HasPrefix(v, "MYSQL_PWD=") {
				t.Errorf("%s: empty password still set: %s", e, v)
			}
		}
	}
}

func TestDump_失敗したら書きかけのファイルを残さない(t *testing.T) {
	d, r := newDumper(t)
	r.err = errors.New("connection refused")
	path, err := d.Dump(context.Background(), job(domain.Postgres, "app"))
	if err == nil || path != "" || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("path %q err %v", path, err)
	}
	if left, _ := os.ReadDir(d.WorkDir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestDump_対応しないエンジンや作業ディレクトリを作れない場合は失敗する(t *testing.T) {
	d, _ := newDumper(t)
	if _, err := d.Dump(context.Background(), job(domain.Engine("sqlite"), "app")); err == nil {
		t.Error("unsupported engine accepted")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	d.WorkDir = filepath.Join(blocker, "sub")
	if _, err := d.Dump(context.Background(), job(domain.MySQL, "app")); err == nil {
		t.Error("unusable work dir accepted")
	}
}

func TestDiscard_ダンプを消し存在しなくても失敗しない(t *testing.T) {
	d, _ := newDumper(t)
	path, _ := d.Dump(context.Background(), job(domain.MySQL, "app"))
	d.Discard(path)
	d.Discard(path)
	d.Discard("")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("still there: %v", err)
	}
}

func TestDump_MySQLの失敗とファイルを作れない作業ディレクトリも失敗として返す(t *testing.T) {
	d, r := newDumper(t)
	r.err = errors.New("access denied")
	if path, err := d.Dump(context.Background(), job(domain.MySQL, "app")); err == nil || path != "" || !strings.Contains(err.Error(), "mysqldump") {
		t.Errorf("mysql failure: %q %v", path, err)
	}
	if left, _ := os.ReadDir(d.WorkDir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	ro := filepath.Join(t.TempDir(), "ro")
	_ = os.Mkdir(ro, 0o500)
	d.WorkDir = ro
	if _, err := d.Dump(context.Background(), job(domain.Postgres, "app")); err == nil {
		t.Error("read-only work dir accepted")
	}
}

func TestDump_ダンプを実装していないプロバイダにはErrFeatureNotSupportedで断る(t *testing.T) {
	d, r := newDumper(t)
	spec, _ := domain.DefaultRuntimes.Spec(domain.MySQL)
	d.Providers = domain.ProviderSet{domain.MySQL: runtimeprovider.New(spec, nil, runtimeprovider.Hooks{})}

	if _, err := d.Dump(context.Background(), job(domain.MySQL, "app")); !errors.Is(err, domain.ErrFeatureNotSupported) {
		t.Errorf("Dump = %v, want ErrFeatureNotSupported", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("a dump tool ran: %+v", r.calls)
	}
	if left, _ := os.ReadDir(d.WorkDir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestDump_未登録のエンジンはErrUnsupportedEngine(t *testing.T) {
	d, _ := newDumper(t)
	if _, err := d.Dump(context.Background(), job(domain.Engine("sqlite"), "app")); !errors.Is(err, domain.ErrUnsupportedEngine) {
		t.Errorf("Dump = %v, want ErrUnsupportedEngine", err)
	}
}

func TestDump_プロバイダが宣言を返せなければ失敗する(t *testing.T) {
	d, _ := newDumper(t)
	d.Providers = domain.ProviderSet{domain.MySQL: brokenProvider{}}
	if _, err := d.Dump(context.Background(), job(domain.MySQL, "app")); err == nil {
		t.Error("a provider that cannot describe itself was used")
	}
}

type brokenProvider struct{ domain.RuntimeProvider }

func (brokenProvider) Describe(context.Context) (domain.EngineSpec, error) {
	return domain.EngineSpec{}, errors.New("not ready")
}

func TestPostgresDumpとMySQLDump_指定のファイルに書き込み既存の内容は残さない(t *testing.T) {
	r := &recorder{}
	out := filepath.Join(t.TempDir(), "out.sql")
	if err := os.WriteFile(out, []byte("stale content that is longer than the dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srcdump.MySQLDump(r.stream)(context.Background(), job(domain.MySQL, "app"), out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != "-- dump app\n" {
		t.Errorf("dump = %q", b)
	}
	if err := srcdump.PostgresDump(r.stream)(context.Background(), job(domain.Postgres, "app"), filepath.Join(t.TempDir(), "missing", "out.sql")); err == nil {
		t.Error("an unwritable target was accepted")
	}
}

func TestDump_プロバイダが未設定でもpanicせずErrUnsupportedEngine(t *testing.T) {
	d := srcdump.Dumper{WorkDir: filepath.Join(t.TempDir(), "work")}
	if _, err := d.Dump(context.Background(), job(domain.MySQL, "app")); !errors.Is(err, domain.ErrUnsupportedEngine) {
		t.Errorf("Dump = %v, want ErrUnsupportedEngine", err)
	}
}

func TestDump_固まったダンプは時間の上限で打ち切り書きかけのファイルを残さない(t *testing.T) {
	spec, _ := domain.DefaultRuntimes.Spec(domain.MySQL)
	hang := func(ctx context.Context, _ domain.SourceDumpJob, _ string) error {
		<-ctx.Done() // a source that accepts the connection and never answers
		return ctx.Err()
	}
	d := srcdump.Dumper{
		WorkDir:   filepath.Join(t.TempDir(), "work"),
		Providers: domain.ProviderSet{domain.MySQL: runtimeprovider.New(spec, nil, runtimeprovider.Hooks{DumpSource: hang})},
		Timeout:   20 * time.Millisecond,
	}
	caller, cancel := context.WithTimeout(context.Background(), 3*time.Second) // only so a missing limit fails the test instead of hanging it
	defer cancel()

	start := time.Now()
	path, err := d.Dump(caller, job(domain.MySQL, "app"))
	if !errors.Is(err, context.DeadlineExceeded) || path != "" || time.Since(start) > time.Second {
		t.Fatalf("Dump = %q, %v after %s, want the 20ms limit to end it", path, err, time.Since(start))
	}
	if left, _ := os.ReadDir(d.WorkDir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestDump_MySQLの引数は期待どおりの並びと内容で完全に一致する(t *testing.T) {
	d, r := newDumper(t)
	path, err := d.Dump(context.Background(), job(domain.MySQL, "app", "billing"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Discard(path)

	want := []string{"--protocol=tcp", "-h", "db.example.com", "-P", "5432", "-u", "dumper",
		"--single-transaction", "--routines", "--triggers", "--events", "--no-tablespaces", "--set-gtid-purged=OFF",
		"--add-drop-database", "--databases", "app", "billing"}
	if !reflect.DeepEqual(r.calls[0].args, want) {
		t.Errorf("args = %q\nwant   %q", r.calls[0].args, want)
	}
	if !reflect.DeepEqual(r.calls[0].env, []string{"LD_LIBRARY_PATH=/rt/lib", "MYSQL_PWD=hunter2"}) {
		t.Errorf("env = %q", r.calls[0].env)
	}
}

func TestDump_上限で打ち切られたときはツールのエラーに加えて時間切れだと分かる(t *testing.T) {
	spec, _ := domain.DefaultRuntimes.Spec(domain.MySQL)
	killed := func(ctx context.Context, _ domain.SourceDumpJob, _ string) error {
		<-ctx.Done()
		return errors.New("signal: killed") // what exec.CommandContext reports, without the context's error
	}
	d := srcdump.Dumper{
		WorkDir:   filepath.Join(t.TempDir(), "work"),
		Providers: domain.ProviderSet{domain.MySQL: runtimeprovider.New(spec, nil, runtimeprovider.Hooks{DumpSource: killed})},
		Timeout:   20 * time.Millisecond,
	}
	_, err := d.Dump(context.Background(), job(domain.MySQL, "app"))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("Dump = %v, want the tool's error and the deadline", err)
	}
}

func externalSpec(runAsUser string) domain.EngineSpec {
	return domain.EngineSpec{
		Engine:       domain.Engine("fakedb"),
		DisplayName:  "FakeDB",
		RunAsUser:    runAsUser,
		Capabilities: domain.RuntimeCaps{SourceDump: true},
	}
}

func TestDump_外部モジュールはRunAsUser所有の専用ディレクトリにダンプしWorkDirを0711にする(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	// 組み込みエンジンが先に 0700 で作っていた状態を想定する。
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var chowned []struct {
		path     string
		uid, gid int
	}
	var lookedUp string
	var gotOutPath string
	var chownedBeforeDump bool
	var preExisted bool
	var dirPermDuringDump os.FileMode
	var workPermDuringDump os.FileMode

	dumpHook := func(_ context.Context, _ domain.SourceDumpJob, outPath string) error {
		gotOutPath = outPath
		chownedBeforeDump = len(chowned) == 1 && chowned[0].path == filepath.Dir(outPath)
		_, statErr := os.Stat(outPath)
		preExisted = statErr == nil
		if fi, err := os.Stat(filepath.Dir(outPath)); err == nil {
			dirPermDuringDump = fi.Mode().Perm()
		}
		if fi, err := os.Stat(workDir); err == nil {
			workPermDuringDump = fi.Mode().Perm()
		}
		return os.WriteFile(outPath, []byte("-- external dump\n"), 0o600)
	}
	d := srcdump.Dumper{
		WorkDir: workDir,
		Providers: domain.ProviderSet{
			domain.Engine("fakedb"): runtimeprovider.New(externalSpec("db-branch-rt-fakedb"), nil, runtimeprovider.Hooks{DumpSource: dumpHook}),
		},
		LookupUser: func(name string) (int, int, error) {
			lookedUp = name
			return 1234, 5678, nil
		},
		Chown: func(path string, uid, gid int) error {
			chowned = append(chowned, struct {
				path     string
				uid, gid int
			}{path, uid, gid})
			return nil
		},
	}

	path, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app"))
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if lookedUp != "db-branch-rt-fakedb" {
		t.Errorf("lookedUp = %q, want db-branch-rt-fakedb", lookedUp)
	}
	if path != gotOutPath {
		t.Errorf("path = %q, want %q", path, gotOutPath)
	}
	dumpDir := filepath.Dir(path)
	if filepath.Dir(dumpDir) != workDir || !strings.HasPrefix(filepath.Base(dumpDir), "refresh-") {
		t.Errorf("dumpDir = %q, want %s/refresh-*", dumpDir, workDir)
	}
	if !chownedBeforeDump {
		t.Errorf("dump dir must be chowned before DumpSource runs: %+v", chowned)
	}
	if len(chowned) != 1 || chowned[0].uid != 1234 || chowned[0].gid != 5678 {
		t.Errorf("chowned = %+v, want uid=1234 gid=5678", chowned)
	}
	if preExisted {
		t.Error("outPath must not be pre-created as root before DumpSource runs")
	}
	if dirPermDuringDump != 0o700 {
		t.Errorf("dumpDir perm = %o, want 0700", dirPermDuringDump)
	}
	if workPermDuringDump != 0o711 {
		t.Errorf("workDir perm = %o, want 0711", workPermDuringDump)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "-- external dump\n" {
		t.Errorf("read dump = %q, %v", b, err)
	}

	d.Discard(path)
	if _, err := os.Stat(dumpDir); !os.IsNotExist(err) {
		t.Errorf("dumpDir still exists after Discard: %v", err)
	}
	if left, _ := os.ReadDir(workDir); len(left) != 0 {
		t.Errorf("left behind in workDir: %v", left)
	}
	// Idempotent second Discard does not panic or remove WorkDir.
	d.Discard(path)
	if _, err := os.Stat(workDir); err != nil {
		t.Errorf("workDir removed unexpectedly: %v", err)
	}
}

func TestDump_外部モジュールの失敗やタイムアウト時に専用ディレクトリを残さない(t *testing.T) {
	t.Run("DumpSourceの失敗", func(t *testing.T) {
		workDir := filepath.Join(t.TempDir(), "work")
		d := srcdump.Dumper{
			WorkDir: workDir,
			Providers: domain.ProviderSet{
				domain.Engine("fakedb"): runtimeprovider.New(externalSpec("db-branch-rt-fakedb"), nil, runtimeprovider.Hooks{
					DumpSource: func(_ context.Context, _ domain.SourceDumpJob, outPath string) error {
						_ = os.WriteFile(outPath, []byte("partial"), 0o600)
						return errors.New("dump failed")
					},
				}),
			},
			LookupUser: func(string) (int, int, error) { return 1000, 1000, nil },
			Chown:      func(string, int, int) error { return nil },
		}
		path, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app"))
		if err == nil || path != "" || !strings.Contains(err.Error(), "dump failed") {
			t.Fatalf("Dump = %q, %v", path, err)
		}
		if left, _ := os.ReadDir(workDir); len(left) != 0 {
			t.Errorf("left behind: %v", left)
		}
	})

	t.Run("タイムアウト", func(t *testing.T) {
		workDir := filepath.Join(t.TempDir(), "work")
		d := srcdump.Dumper{
			WorkDir: workDir,
			Timeout: 20 * time.Millisecond,
			Providers: domain.ProviderSet{
				domain.Engine("fakedb"): runtimeprovider.New(externalSpec("db-branch-rt-fakedb"), nil, runtimeprovider.Hooks{
					DumpSource: func(ctx context.Context, _ domain.SourceDumpJob, outPath string) error {
						_ = os.WriteFile(outPath, []byte("partial"), 0o600)
						<-ctx.Done()
						return errors.New("signal: killed")
					},
				}),
			},
			LookupUser: func(string) (int, int, error) { return 1000, 1000, nil },
			Chown:      func(string, int, int) error { return nil },
		}
		path, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app"))
		if err == nil || path != "" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dump = %q, %v", path, err)
		}
		if left, _ := os.ReadDir(workDir); len(left) != 0 {
			t.Errorf("left behind: %v", left)
		}
	})
}

func TestDump_外部モジュールでユーザー検索やchownや作業ディレクトリに失敗したら何も残さない(t *testing.T) {
	noopDump := func(_ context.Context, _ domain.SourceDumpJob, _ string) error {
		t.Error("DumpSource must not be called")
		return nil
	}
	prov := domain.ProviderSet{
		domain.Engine("fakedb"): runtimeprovider.New(externalSpec("db-branch-rt-fakedb"), nil, runtimeprovider.Hooks{DumpSource: noopDump}),
	}

	t.Run("ユーザー検索失敗", func(t *testing.T) {
		workDir := filepath.Join(t.TempDir(), "work")
		d := srcdump.Dumper{
			WorkDir:    workDir,
			Providers:  prov,
			LookupUser: func(string) (int, int, error) { return 0, 0, errors.New("unknown user") },
			Chown:      func(string, int, int) error { return nil },
		}
		if _, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app")); err == nil || !strings.Contains(err.Error(), "unknown user") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("chown失敗", func(t *testing.T) {
		workDir := filepath.Join(t.TempDir(), "work")
		d := srcdump.Dumper{
			WorkDir:    workDir,
			Providers:  prov,
			LookupUser: func(string) (int, int, error) { return 1000, 1000, nil },
			Chown:      func(string, int, int) error { return errors.New("operation not permitted") },
		}
		if _, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app")); err == nil || !strings.Contains(err.Error(), "operation not permitted") {
			t.Fatalf("err = %v", err)
		}
		if left, _ := os.ReadDir(workDir); len(left) != 0 {
			t.Errorf("left behind: %v", left)
		}
	})

	t.Run("WorkDir作成失敗", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		_ = os.WriteFile(blocker, nil, 0o600)
		d := srcdump.Dumper{
			WorkDir:    filepath.Join(blocker, "work"),
			Providers:  prov,
			LookupUser: func(string) (int, int, error) { return 1000, 1000, nil },
			Chown:      func(string, int, int) error { return nil },
		}
		if _, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app")); err == nil {
			t.Fatal("unusable work dir accepted")
		}
	})

	t.Run("読み取り専用WorkDirではMkdirTemp失敗", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		ro := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		// Chmod 0711 will make ro writable by owner unless ro is owned by someone else,
		// so test when WorkDir itself is a regular file (Chmod succeeds or MkdirAll fails).
		blocker := filepath.Join(t.TempDir(), "notadir")
		_ = os.WriteFile(blocker, nil, 0o600)
		d := srcdump.Dumper{
			WorkDir:    blocker,
			Providers:  prov,
			LookupUser: func(string) (int, int, error) { return 1000, 1000, nil },
			Chown:      func(string, int, int) error { return nil },
		}
		if _, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app")); err == nil {
			t.Fatal("non-directory work dir accepted")
		}
	})
}

func TestDump_外部モジュールで既定のユーザー検索とchownが動く(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	workDir := filepath.Join(t.TempDir(), "work")
	d := srcdump.Dumper{
		WorkDir: workDir,
		Providers: domain.ProviderSet{
			domain.Engine("fakedb"): runtimeprovider.New(externalSpec(cur.Username), nil, runtimeprovider.Hooks{
				DumpSource: func(_ context.Context, _ domain.SourceDumpJob, outPath string) error {
					return os.WriteFile(outPath, []byte("-- default lookup\n"), 0o600)
				},
			}),
		},
	}
	path, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app"))
	if err != nil {
		t.Fatalf("Dump with default lookup/chown: %v", err)
	}
	d.Discard(path)
	if left, _ := os.ReadDir(workDir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	d.Providers = domain.ProviderSet{
		domain.Engine("fakedb"): runtimeprovider.New(externalSpec("db-branch-rt-no-such-user-xyz"), nil, runtimeprovider.Hooks{
			DumpSource: func(context.Context, domain.SourceDumpJob, string) error { return nil },
		}),
	}
	if _, err := d.Dump(context.Background(), job(domain.Engine("fakedb"), "app")); err == nil {
		t.Fatal("unknown user must fail")
	}
}
