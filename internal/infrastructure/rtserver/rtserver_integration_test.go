//go:build integration

package rtserver_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/rtserver"
	"github.com/db-branch/db-branch/internal/infrastructure/rtunit"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimegrpc"
	"github.com/db-branch/db-branch/internal/infrastructure/srcdump"
)

// Runs the reference module test/fakeruntime (built for linux, path in
// DBB_FAKERUNTIME) through the real thing: its own OS user, the
// db-branch-rt@ template unit with a generated drop-in, systemd and the gRPC
// control plane. Needs root on a node with db-branch installed (the template
// unit); it leaves nothing behind. Run with sudo on the dev VM.
func TestServer_参照モジュールをsystemdで実際に動かす(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	src := os.Getenv("DBB_FAKERUNTIME")
	if src == "" {
		t.Skip("DBB_FAKERUNTIME names the linux build of test/fakeruntime")
	}
	const engine, name = "fakedb", "db-branch-rt-fakedb"
	run := func(cmd string, args ...string) {
		t.Helper()
		if out, err := exec.Command(cmd, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", cmd, args, err, out)
		}
	}
	run("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name)
	moduleDir := "/usr/lib/db-branch/runtimes/" + engine
	dataDir := "/srv/rtserver-it/data"
	id := "rtit-1"
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", rtunit.UnitName(id)).Run()
		_ = exec.Command("systemctl", "reset-failed", rtunit.UnitName(id)).Run()
		_ = os.RemoveAll("/etc/systemd/system/" + rtunit.UnitName(id) + ".d")
		_ = os.RemoveAll("/run/systemd/system/" + rtunit.UnitName(id) + ".d")
		_ = exec.Command("systemctl", "daemon-reload").Run()
		_ = os.RemoveAll(moduleDir)
		_ = os.RemoveAll("/srv/rtserver-it")
		_ = os.RemoveAll(rtunit.StateRoot + "/" + id)
		_ = os.RemoveAll(rtunit.RunRoot + "/" + engine)
		_ = exec.Command("userdel", name).Run()
	})
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := moduleDir + "/" + name
	run("install", "-m", "0755", "-o", "root", "-g", "root", src, bin)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := domain.RuntimeManifest{
		Engine: engine, DisplayName: "FakeDB", RunAsUser: name, Binary: bin,
		Protocols: []domain.ManifestPort{{Name: "sql", Primary: true}}, Capabilities: map[string]bool{"query": true, "upgrade": true, "source_dump": true},
	}
	cmd, err := runtimegrpc.Command(m, runtimegrpc.SystemUser)
	if err != nil {
		t.Fatal(err)
	}
	prov, stop, err := runtimegrpc.Start(cmd, m)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	srv := &rtserver.Server{
		Engine: m.Spec(), Provider: prov, Run: execx.Runner{}, Units: rtunit.Writer{Dir: "/run/systemd/system"}, User: name,
		Mkdir: func(p string) error { return os.MkdirAll(p, 0o750) },
		Chown: func(u, p string) error {
			usr, err := user.Lookup(u)
			if err != nil {
				return err
			}
			uid, _ := strconv.Atoi(usr.Uid)
			gid, _ := strconv.Atoi(usr.Gid)
			return os.Chown(p, uid, gid)
		},
		StateRoot: rtunit.StateRoot, RuntimeRoot: "/var/lib/db-branch/runtimes", Bind: "127.0.0.1",
		StartTimeout: 30 * time.Second, PollInterval: 200 * time.Millisecond,
	}
	ctx := context.Background()
	s := domain.Server{ID: id, Runtime: domain.RuntimeRef{Engine: engine, Version: "1.0"}, DataDir: dataDir, Port: 38999}

	if err := srv.Initialize(ctx, s); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := srv.CreateUser(ctx, s, "app", "pw"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.Start(ctx, s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ok, _ := srv.Running(ctx, s); !ok {
		t.Fatal("not running after Start")
	}
	// The server answers, as the engine's own user, in the unit's sandbox.
	if got := ask(t, "127.0.0.1:38999", "hello"); got != "OK hello" {
		t.Fatalf("answer = %q", got)
	}
	if out, _ := exec.Command("systemctl", "show", "--property=User", "--value", rtunit.UnitName(id)).Output(); string(out) != name+"\n" {
		t.Fatalf("unit user = %q", out)
	}
	if a := srv.Admin(s); a.Port != 38999 || a.Engine != engine {
		t.Fatalf("admin = %+v", a)
	}

	if err := srv.Stop(ctx, s, domain.StopClean); err != nil {
		t.Fatalf("clean stop: %v", err)
	}
	if ok, _ := srv.Running(ctx, s); ok {
		t.Fatal("running after Stop")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "clean")); err != nil {
		t.Fatal("the server left no clean marker")
	}

	// A killed server is not a clean shutdown.
	if err := srv.Start(ctx, s); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := srv.Stop(ctx, s, domain.StopImmediate); err != nil {
		t.Fatalf("immediate stop: %v", err)
	}
	if err := prov.VerifyCleanShutdown(ctx, srv.Instance(s)); !errors.Is(err, domain.ErrUncleanShutdown) {
		t.Fatalf("a killed server must be unclean, got %v", err)
	}

	if err := srv.Remove(ctx, s); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat("/run/systemd/system/" + rtunit.UnitName(id) + ".d"); err == nil {
		t.Fatal("the drop-in is still there")
	}
	t.Logf("fakedb ran under %s through %s", name, rtunit.UnitName(id))
}

func TestServer_参照モジュールをsystemdでアップグレードできる(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	src := os.Getenv("DBB_FAKERUNTIME")
	if src == "" {
		t.Skip("DBB_FAKERUNTIME names the linux build of test/fakeruntime")
	}
	const engine, name = "fakedb", "db-branch-rt-fakedb"
	run := func(cmd string, args ...string) {
		t.Helper()
		if out, err := exec.Command(cmd, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", cmd, args, err, out)
		}
	}
	run("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name)
	moduleDir := "/usr/lib/db-branch/runtimes/" + engine
	dataDir := "/srv/rtserver-it-upgrade/data"
	id := "rtit-up-1"
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", rtunit.UnitName(id)).Run()
		_ = exec.Command("systemctl", "reset-failed", rtunit.UnitName(id)).Run()
		_ = os.RemoveAll("/etc/systemd/system/" + rtunit.UnitName(id) + ".d")
		_ = os.RemoveAll("/run/systemd/system/" + rtunit.UnitName(id) + ".d")
		_ = exec.Command("systemctl", "daemon-reload").Run()
		_ = os.RemoveAll(moduleDir)
		_ = os.RemoveAll("/srv/rtserver-it-upgrade")
		_ = os.RemoveAll(rtunit.StateRoot + "/" + id)
		_ = os.RemoveAll(rtunit.RunRoot + "/" + engine)
		_ = exec.Command("userdel", name).Run()
	})
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := moduleDir + "/" + name
	run("install", "-m", "0755", "-o", "root", "-g", "root", src, bin)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := domain.RuntimeManifest{
		Engine: engine, DisplayName: "FakeDB", RunAsUser: name, Binary: bin,
		Protocols: []domain.ManifestPort{{Name: "sql", Primary: true}}, Capabilities: map[string]bool{"query": true, "upgrade": true, "source_dump": true},
	}
	cmd, err := runtimegrpc.Command(m, runtimegrpc.SystemUser)
	if err != nil {
		t.Fatal(err)
	}
	prov, stop, err := runtimegrpc.Start(cmd, m)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	srv := &rtserver.Server{
		Engine: m.Spec(), Provider: prov, Run: execx.Runner{}, Units: rtunit.Writer{Dir: "/run/systemd/system"}, User: name,
		Mkdir: func(p string) error { return os.MkdirAll(p, 0o750) },
		Chown: func(u, p string) error {
			usr, err := user.Lookup(u)
			if err != nil {
				return err
			}
			uid, _ := strconv.Atoi(usr.Uid)
			gid, _ := strconv.Atoi(usr.Gid)
			return os.Chown(p, uid, gid)
		},
		StateRoot: rtunit.StateRoot, RuntimeRoot: "/var/lib/db-branch/runtimes", Bind: "127.0.0.1",
		StartTimeout: 30 * time.Second, PollInterval: 200 * time.Millisecond,
	}
	ctx := context.Background()
	login := domain.Login{User: "app", Password: "pw"}
	oldSrv := domain.Server{ID: id, Runtime: domain.RuntimeRef{Engine: engine, Version: "1.0"}, DataDir: dataDir, Port: 38998}
	newRuntime := domain.RuntimeRef{Engine: engine, Version: "2.0"}

	if err := srv.Initialize(ctx, oldSrv); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := srv.CreateUser(ctx, oldSrv, login.User, login.Password); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.Start(ctx, oldSrv); err != nil {
		t.Fatalf("Start old runtime: %v", err)
	}
	if got := ask(t, "127.0.0.1:38998", "before-upgrade"); got != "OK before-upgrade" {
		t.Fatalf("answer before upgrade = %q", got)
	}

	if err := prov.CheckUpgradePath(ctx, "1.0", "2.0"); err != nil {
		t.Fatalf("CheckUpgradePath 1.0->2.0: %v", err)
	}
	if err := prov.CheckUpgradePath(ctx, "2.0", "1.0"); err == nil {
		t.Fatal("CheckUpgradePath 2.0->1.0 must fail")
	}
	plan, err := srv.CheckUpgrade(ctx, oldSrv, newRuntime, login)
	if err != nil {
		t.Fatalf("CheckUpgrade: %v", err)
	}
	if domain.HasErrors(plan.Findings) {
		t.Fatalf("unexpected pre-check errors: %+v", plan.Findings)
	}

	if err := srv.Stop(ctx, oldSrv, domain.StopClean); err != nil {
		t.Fatalf("clean stop old runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "clean")); err != nil {
		t.Fatal("clean stop left no clean marker")
	}

	newSrv := oldSrv
	newSrv.Runtime = newRuntime
	upLog, err := srv.StartUpgraded(ctx, newSrv, login)
	if err != nil {
		t.Fatalf("StartUpgraded: %v (excerpt: %s)", err, upLog.Excerpt)
	}
	if ok, _ := srv.Running(ctx, newSrv); !ok {
		t.Fatal("not running after StartUpgraded")
	}
	if got := ask(t, "127.0.0.1:38998", "after-upgrade"); got != "OK after-upgrade" {
		t.Fatalf("answer after upgrade = %q", got)
	}
	conf, err := os.ReadFile(filepath.Join(rtunit.StateRoot, id, "fakedb.conf"))
	if err != nil {
		t.Fatalf("read fakedb.conf: %v", err)
	}
	wantDir := "runtime_dir=/var/lib/db-branch/runtimes/" + engine + "/2.0"
	if !strings.Contains(string(conf), wantDir) {
		t.Fatalf("fakedb.conf lacks %q:\n%s", wantDir, conf)
	}

	if err := srv.Stop(ctx, newSrv, domain.StopClean); err != nil {
		t.Fatalf("clean stop upgraded runtime: %v", err)
	}
	if err := srv.Remove(ctx, newSrv); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	t.Logf("fakedb upgraded from 1.0 to 2.0 through %s", rtunit.UnitName(id))
}

func TestServer_参照モジュールでDumpSourceとLoadの所有権が通る(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	src := os.Getenv("DBB_FAKERUNTIME")
	if src == "" {
		t.Skip("DBB_FAKERUNTIME names the linux build of test/fakeruntime")
	}
	const engine, name = "fakedb", "db-branch-rt-fakedb"
	run := func(cmd string, args ...string) {
		t.Helper()
		if out, err := exec.Command(cmd, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", cmd, args, err, out)
		}
	}
	run("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name)
	moduleDir := "/usr/lib/db-branch/runtimes/" + engine
	baseDir := "/srv/rtserver-it-dump"
	dataDir := filepath.Join(baseDir, "data")
	workDir := filepath.Join(baseDir, "refresh")
	uploadDir := filepath.Join(baseDir, "uploads")
	id := "rtit-dump-1"
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", rtunit.UnitName(id)).Run()
		_ = exec.Command("systemctl", "reset-failed", rtunit.UnitName(id)).Run()
		_ = os.RemoveAll("/etc/systemd/system/" + rtunit.UnitName(id) + ".d")
		_ = os.RemoveAll("/run/systemd/system/" + rtunit.UnitName(id) + ".d")
		_ = exec.Command("systemctl", "daemon-reload").Run()
		_ = os.RemoveAll(moduleDir)
		_ = os.RemoveAll(baseDir)
		_ = os.RemoveAll(rtunit.StateRoot + "/" + id)
		_ = os.RemoveAll(rtunit.RunRoot + "/" + engine)
		_ = exec.Command("userdel", name).Run()
	})
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := moduleDir + "/" + name
	run("install", "-m", "0755", "-o", "root", "-g", "root", src, bin)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := domain.RuntimeManifest{
		Engine: engine, DisplayName: "FakeDB", RunAsUser: name, Binary: bin,
		Protocols: []domain.ManifestPort{{Name: "sql", Primary: true}}, Capabilities: map[string]bool{"query": true, "upgrade": true, "source_dump": true},
	}
	cmd, err := runtimegrpc.Command(m, runtimegrpc.SystemUser)
	if err != nil {
		t.Fatal(err)
	}
	prov, stop, err := runtimegrpc.Start(cmd, m)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	modUser, err := user.Lookup(name)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	modUID, err := strconv.ParseUint(modUser.Uid, 10, 32)
	if err != nil {
		t.Fatalf("parse uid %q: %v", modUser.Uid, err)
	}

	srv := rtserver.NewSystem(m.Spec(), prov, name, "/var/lib/db-branch", "127.0.0.1")
	srv.StartTimeout = 30 * time.Second
	ctx := context.Background()
	s := domain.Server{ID: id, Runtime: domain.RuntimeRef{Engine: engine, Version: "1.0"}, DataDir: dataDir, Port: 38997}

	if err := srv.Initialize(ctx, s); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// 1. srcdump.Dumper(外部モジュール) -> fakeruntime.DumpSource が db-branch-rt-fakedb で書けること。
	dumper := srcdump.Dumper{
		WorkDir:   workDir,
		Providers: domain.ProviderSet{domain.Engine(engine): prov},
	}
	dumpPath, err := dumper.Dump(ctx, domain.SourceDumpJob{
		Tools:  domain.AdminAccess{Engine: domain.Engine(engine)},
		Source: domain.RefreshSource{Host: "127.0.0.1", Port: 38997, User: "dumper", Databases: []string{"app"}},
	})
	if err != nil {
		t.Fatalf("Dumper.Dump: %v", err)
	}
	defer dumper.Discard(dumpPath)

	if fi, err := os.Stat(workDir); err != nil || fi.Mode().Perm() != 0o711 {
		t.Fatalf("workDir stat = %v (perm %v), want 0711", err, fi.Mode().Perm())
	}
	dumpDir := filepath.Dir(dumpPath)
	if fi, err := os.Stat(dumpDir); err != nil {
		t.Fatalf("stat dumpDir: %v", err)
	} else if fi.Mode().Perm() != 0o700 || fi.Sys().(*syscall.Stat_t).Uid != uint32(modUID) {
		t.Fatalf("dumpDir perm=%o uid=%d, want 0700 uid=%d", fi.Mode().Perm(), fi.Sys().(*syscall.Stat_t).Uid, modUID)
	}
	dumpBody, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("root read dumpPath: %v", err)
	}
	for _, want := range []string{
		"-- fakedb dump",
		fmt.Sprintf("uid=%d", modUID),
		fmt.Sprintf("parent_uid=%d", modUID),
	} {
		if !strings.Contains(string(dumpBody), want) {
			t.Fatalf("dumpBody %q lacks %q", string(dumpBody), want)
		}
	}

	// 2. rtserver.Load -> fakeruntime.Load がダンプを読めること。
	if err := srv.Load(ctx, s, dumpPath); err != nil {
		t.Fatalf("srv.Load(dumpPath): %v", err)
	}
	stateDir := filepath.Join(rtunit.StateRoot, id)
	loaded, err := os.ReadFile(filepath.Join(stateDir, "loaded"))
	if err != nil {
		t.Fatalf("read loaded: %v", err)
	}
	if string(loaded) != string(dumpBody) {
		t.Fatalf("loaded = %q, want %q", loaded, dumpBody)
	}
	dumper.Discard(dumpPath)
	if _, err := os.Stat(dumpDir); !os.IsNotExist(err) {
		t.Fatalf("dumpDir still exists after Discard: %v", err)
	}

	// 3. root 所有 0700/0600 のアップロード相当ファイルも rtserver.Load 経由で読めること。
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	uploadPath := filepath.Join(uploadDir, "upload.sql")
	const uploadBody = "-- root-only upload\n"
	if err := os.WriteFile(uploadPath, []byte(uploadBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.Load(ctx, s, uploadPath); err != nil {
		t.Fatalf("srv.Load(uploadPath): %v", err)
	}
	loaded, err = os.ReadFile(filepath.Join(stateDir, "loaded"))
	if err != nil {
		t.Fatalf("read loaded after upload: %v", err)
	}
	if string(loaded) != uploadBody {
		t.Fatalf("loaded upload = %q, want %q", loaded, uploadBody)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), "load-") {
			t.Fatalf("staged dump left behind in stateDir: %s", ent.Name())
		}
	}

	if err := srv.Remove(ctx, s); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	t.Logf("fakedb DumpSource and Load succeeded under %s (uid %d)", name, modUID)
}

func ask(t *testing.T, addr, line string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, line+"\n"); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(trim(got))
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
