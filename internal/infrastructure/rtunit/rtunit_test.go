package rtunit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/rtunit"
)

func unit() rtunit.Unit {
	return rtunit.Unit{
		ID: "branch-x", Engine: "clickhouse", User: "db-branch-rt-clickhouse",
		Plan: domain.LaunchPlan{
			Command:     "/var/lib/db-branch/runtimes/clickhouse/25.3.1/bin/clickhouse",
			Args:        []string{"server", "--config-file=/var/lib/db-branch/instances/branch-x/config.xml", "a b"},
			Env:         map[string]string{"LD_LIBRARY_PATH": "/opt/lib", "A": `x"y`},
			ServiceType: "notify", KillSignal: "SIGINT", KillMode: "mixed", NotifyAccess: "main", TimeoutStopSec: 300,
		},
		DataDir:  "/tank/db-branch/branches/x",
		StateDir: "/var/lib/db-branch/instances/branch-x",
		RunDir:   "/run/db-branch-rt/clickhouse/branch-x",
		Limits:   domain.ResourceLimits{MemoryMB: 512, CPUQuota: 200},
	}
}

const golden = `# Written by db-branch. Changes are overwritten.
[Service]
User=db-branch-rt-clickhouse
Group=db-branch-rt-clickhouse
Slice=db-branch.slice
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
InaccessiblePaths=/run/db-branch /etc/db-branch
ReadWritePaths=/tank/db-branch/branches/x /var/lib/db-branch/instances/branch-x /run/db-branch-rt/clickhouse/branch-x
Type=notify
NotifyAccess=main
KillSignal=SIGINT
KillMode=mixed
TimeoutStopSec=300
LimitNOFILE=65535
MemoryMax=1536M
CPUQuota=200%
Environment="A=x\"y"
Environment="LD_LIBRARY_PATH=/opt/lib"
ExecStart=
ExecStart=/var/lib/db-branch/runtimes/clickhouse/25.3.1/bin/clickhouse "server" "--config-file=/var/lib/db-branch/instances/branch-x/config.xml" "a b"
`

func TestDropIn_ユニットの設定を決まった形で書き出す(t *testing.T) {
	got, err := rtunit.DropIn(unit())
	if err != nil {
		t.Fatal(err)
	}
	if got != golden {
		t.Errorf("drop-in differs:\n--- got\n%s--- want\n%s", got, golden)
	}
}

func TestDropIn_省略できる項目は書かない(t *testing.T) {
	u := unit()
	u.Plan = domain.LaunchPlan{Command: "/var/lib/db-branch/runtimes/clickhouse/25.3.1/bin/clickhouse"}
	u.Limits = domain.ResourceLimits{NoFile: 4096}
	got, err := rtunit.DropIn(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"Type=", "NotifyAccess=", "KillSignal=", "KillMode=", "TimeoutStopSec=", "MemoryMax=", "CPUQuota=", "Environment="} {
		if strings.Contains(got, absent) {
			t.Errorf("drop-in has %q although nothing asked for it:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, "LimitNOFILE=4096\n") || !strings.HasSuffix(got, "ExecStart=\nExecStart=/var/lib/db-branch/runtimes/clickhouse/25.3.1/bin/clickhouse\n") {
		t.Errorf("drop-in = %s", got)
	}
	// the sandbox is always there
	for _, must := range []string{"NoNewPrivileges=yes\n", "ProtectSystem=strict\n", "InaccessiblePaths=/run/db-branch /etc/db-branch\n", "User=db-branch-rt-clickhouse\n"} {
		if !strings.Contains(got, must) {
			t.Errorf("drop-in lacks %q", must)
		}
	}
}

func TestDropIn_引数と環境変数の特殊文字をsystemdの規則でエスケープする(t *testing.T) {
	u := unit()
	u.Plan.Args = []string{`a"b\c%d$e`, "plain"}
	u.Plan.Env = map[string]string{"K": `a"b\c%d$e`}
	got, err := rtunit.DropIn(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `ExecStart=/var/lib/db-branch/runtimes/clickhouse/25.3.1/bin/clickhouse "a\"b\\c%%d$$e" "plain"`+"\n") {
		t.Errorf("ExecStart not escaped:\n%s", got)
	}
	if !strings.Contains(got, `Environment="K=a\"b\\c%%d$e"`+"\n") {
		t.Errorf("Environment not escaped:\n%s", got)
	}
}

func TestDropIn_ディレクティブを注入できる入力を拒否する(t *testing.T) {
	cases := map[string]func(*rtunit.Unit){
		"IDに../":                 func(u *rtunit.Unit) { u.ID = "../x" },
		"IDに空白":                  func(u *rtunit.Unit) { u.ID = "a b" },
		"IDが空":                   func(u *rtunit.Unit) { u.ID = "" },
		"IDが大文字":                 func(u *rtunit.Unit) { u.ID = "Branch" },
		"IDに改行":                  func(u *rtunit.Unit) { u.ID = "x\nUser=root" },
		"エンジン名が不正":               func(u *rtunit.Unit) { u.Engine = "Click House" },
		"組み込みのmysql":             func(u *rtunit.Unit) { u.Engine = "mysql"; u.User = "db-branch-rt-mysql" },
		"組み込みのpostgres":          func(u *rtunit.Unit) { u.Engine = "postgres"; u.User = "db-branch-rt-postgres" },
		"rootで動かす":               func(u *rtunit.Unit) { u.User = "root" },
		"db-branchで動かす":          func(u *rtunit.Unit) { u.User = "db-branch" },
		"他のエンジンのユーザー":            func(u *rtunit.Unit) { u.User = "db-branch-rt-other" },
		"ユーザー名に改行":               func(u *rtunit.Unit) { u.User = "db-branch-rt-clickhouse\nUser=root" },
		"計画に改行(Validate)":        func(u *rtunit.Unit) { u.Plan.Args = []string{"a\nExecStartPre=/bin/evil"} },
		"コマンドが相対パス":              func(u *rtunit.Unit) { u.Plan.Command = "bin/clickhouse" },
		"コマンドに空白":                func(u *rtunit.Unit) { u.Plan.Command = "/bin/a b" },
		"コマンドに%":                 func(u *rtunit.Unit) { u.Plan.Command = "/bin/%n" },
		"コマンドに$":                 func(u *rtunit.Unit) { u.Plan.Command = "/bin/$X" },
		"コマンドに..":                func(u *rtunit.Unit) { u.Plan.Command = "/var/lib/../../bin/sh" },
		"環境変数名が不正":               func(u *rtunit.Unit) { u.Plan.Env = map[string]string{"A B": "x"} },
		"DataDirが相対":             func(u *rtunit.Unit) { u.DataDir = "tank/x" },
		"DataDirに空白":             func(u *rtunit.Unit) { u.DataDir = "/tank/a b" },
		"DataDirに引用符":            func(u *rtunit.Unit) { u.DataDir = `/tank/a"b` },
		"DataDirに%":              func(u *rtunit.Unit) { u.DataDir = "/tank/%n" },
		"DataDirに$":              func(u *rtunit.Unit) { u.DataDir = "/tank/$x" },
		"DataDirに改行":             func(u *rtunit.Unit) { u.DataDir = "/tank/x\nUser=root" },
		"DataDirが/":              func(u *rtunit.Unit) { u.DataDir = "/" },
		"DataDirが/etc":           func(u *rtunit.Unit) { u.DataDir = "/etc" },
		"DataDirが/run":           func(u *rtunit.Unit) { u.DataDir = "/run" },
		"DataDirが設定の中":           func(u *rtunit.Unit) { u.DataDir = "/etc/db-branch/x" },
		"DataDirがソケットの中":         func(u *rtunit.Unit) { u.DataDir = "/run/db-branch/agent.sock/x" },
		"DataDirが/etc/db-branch": func(u *rtunit.Unit) { u.DataDir = "/etc/db-branch" },
		"DataDirに..":             func(u *rtunit.Unit) { u.DataDir = "/tank/../etc/db-branch" },
		"StateDirが別のID":          func(u *rtunit.Unit) { u.StateDir = "/var/lib/db-branch/instances/branch-y" },
		"StateDirが他の場所":          func(u *rtunit.Unit) { u.StateDir = "/var/lib/db-branch" },
		"RunDirが別のエンジン":          func(u *rtunit.Unit) { u.RunDir = "/run/db-branch-rt/other/branch-x" },
		"RunDirが/run/db-branch":  func(u *rtunit.Unit) { u.RunDir = "/run/db-branch" },
		"負のメモリ":                  func(u *rtunit.Unit) { u.Limits.MemoryMB = -1 },
		"負のCPU":                  func(u *rtunit.Unit) { u.Limits.CPUQuota = -5 },
		"CPUが大きすぎる":              func(u *rtunit.Unit) { u.Limits.CPUQuota = 1000001 },
		"NoFileが小さすぎる":           func(u *rtunit.Unit) { u.Limits.NoFile = 3 },
		"NoFileが大きすぎる":           func(u *rtunit.Unit) { u.Limits.NoFile = 1 << 30 },
	}
	for name, mutate := range cases {
		u := unit()
		mutate(&u)
		if _, err := rtunit.DropIn(u); !errors.Is(err, rtunit.ErrInvalidUnit) {
			t.Errorf("%s: err = %v, want ErrInvalidUnit", name, err)
		}
	}
}

func TestUnitName_テンプレートのインスタンス名(t *testing.T) {
	if got := rtunit.UnitName("branch-x"); got != "db-branch-rt@branch-x.service" {
		t.Errorf("UnitName = %q", got)
	}
}

func TestWriter_dropinをsystemdの置き場に書き置き換えと削除ができる(t *testing.T) {
	dir := t.TempDir()
	w := rtunit.Writer{Dir: dir}

	path, err := w.Write(unit())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "db-branch-rt@branch-x.service.d", "override.conf")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	b, _ := os.ReadFile(path)
	if string(b) != golden {
		t.Errorf("content differs:\n%s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}

	u := unit()
	u.Limits.MemoryMB = 1024
	if _, err := w.Write(u); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "MemoryMax=2560M\n") {
		t.Errorf("not replaced:\n%s", b)
	}
	if left, _ := os.ReadDir(filepath.Dir(path)); len(left) != 1 {
		t.Errorf("temporary files left: %v", left)
	}

	if err := w.Remove("branch-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("drop-in directory still there: %v", err)
	}
	if err := w.Remove("branch-x"); err != nil {
		t.Errorf("removing what is gone = %v", err)
	}
}

func TestWriter_不正なユニットは何も書かず不正なIDは削除もしない(t *testing.T) {
	dir := t.TempDir()
	w := rtunit.Writer{Dir: dir}
	bad := unit()
	bad.User = "root"
	if _, err := w.Write(bad); !errors.Is(err, rtunit.ErrInvalidUnit) {
		t.Errorf("Write = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("something was written: %v", entries)
	}
	keep := filepath.Join(dir, "keep")
	_ = os.Mkdir(keep, 0o755)
	for _, id := range []string{"..", "../keep", "", "a/b"} {
		if err := w.Remove(id); !errors.Is(err, rtunit.ErrInvalidUnit) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidUnit", id, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a directory outside the drop-ins was touched: %v", err)
	}
}

func TestWriter_書き込めなければ失敗を返す(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	if _, err := (rtunit.Writer{Dir: filepath.Join(blocker, "sub")}).Write(unit()); err == nil {
		t.Error("Write succeeded where a directory cannot be made")
	}
}

func TestWriter_既定の置き場は_run_systemd_system(t *testing.T) {
	if rtunit.DefaultUnitDir != "/run/systemd/system" || rtunit.StateRoot != "/var/lib/db-branch/instances" || rtunit.RunRoot != "/run/db-branch-rt" {
		t.Errorf("defaults = %q %q %q", rtunit.DefaultUnitDir, rtunit.StateRoot, rtunit.RunRoot)
	}
}
