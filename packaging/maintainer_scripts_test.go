package packaging_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type scriptEnv struct {
	t       *testing.T
	root    string
	binDir  string
	logFile string
	env     []string
}

func newScriptEnv(t *testing.T, systemdRunning bool) *scriptEnv {
	t.Helper()
	root := t.TempDir()
	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "calls.log")

	if systemdRunning {
		if err := os.MkdirAll(filepath.Join(root, "run", "systemd", "system"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	s := &scriptEnv{
		t:       t,
		root:    root,
		binDir:  binDir,
		logFile: logFile,
		env: []string{
			"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
			"DB_BRANCH_PKG_ROOT=" + root,
			"CALL_LOG=" + logFile,
		},
	}
	s.writeStub("systemctl", `#!/bin/sh
echo "systemctl $*" >> "$CALL_LOG"
case "$1" in
is-enabled)
	[ "${STUB_ENABLED:-1}" = "1" ]
	exit $?
	;;
esac
exit 0
`)
	s.writeStub("getent", `#!/bin/sh
echo "getent $*" >> "$CALL_LOG"
case "$1:$2" in
group:db-branch)
	if [ "${STUB_GROUP_DB_BRANCH:-0}" = "1" ]; then
		echo "db-branch:x:999:${STUB_GROUP_MEMBERS:-}"
		exit 0
	fi
	exit 2
	;;
passwd:db-branch)
	[ "${STUB_USER_DB_BRANCH:-0}" = "1" ] && exit 0
	exit 2
	;;
group:db-branch-admin)
	[ "${STUB_GROUP_ADMIN:-0}" = "1" ] && exit 0
	exit 2
	;;
esac
exit 2
`)
	for _, cmd := range []string{"addgroup", "adduser", "chgrp"} {
		s.writeStub(cmd, "#!/bin/sh\necho \""+cmd+" $*\" >> \"$CALL_LOG\"\nexit 0\n")
	}
	return s
}

func (s *scriptEnv) writeStub(name, content string) {
	s.t.Helper()
	p := filepath.Join(s.binDir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *scriptEnv) installPolicyRCD(code int) {
	s.t.Helper()
	p := filepath.Join(s.root, "usr", "sbin", "policy-rc.d")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	body := "#!/bin/sh\necho \"policy-rc.d $*\" >> \"$CALL_LOG\"\nexit " + string(rune('0'+code/100)) + string(rune('0'+(code/10)%10)) + string(rune('0'+code%10)) + "\n"
	if code == 0 {
		body = "#!/bin/sh\necho \"policy-rc.d $*\" >> \"$CALL_LOG\"\nexit 0\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *scriptEnv) touch(relPath, content string) string {
	s.t.Helper()
	p := filepath.Join(s.root, relPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
	return p
}

func (s *scriptEnv) run(script string, extraEnv []string, args ...string) (string, error) {
	s.t.Helper()
	cmdArgs := append([]string{script}, args...)
	cmd := exec.Command("sh", cmdArgs...)
	cmd.Env = append(slices.Clone(s.env), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *scriptEnv) calls() []string {
	s.t.Helper()
	b, err := os.ReadFile(s.logFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestPrerm_upgrade時はサービスを止めずremove時のみエージェントを停止する(t *testing.T) {
	t.Run("upgrade時は何もしない", func(t *testing.T) {
		s := newScriptEnv(t, true)
		out, err := s.run("scripts/prerm", nil, "upgrade", "1.0.0")
		if err != nil {
			t.Fatalf("prerm upgrade failed: %v\n%s", err, out)
		}
		if len(s.calls()) != 0 {
			t.Errorf("prerm upgrade should not invoke systemctl, got: %v", s.calls())
		}
	})

	t.Run("remove時はdb-branch.serviceのみ停止し稼働中DBユニットは止めない", func(t *testing.T) {
		s := newScriptEnv(t, true)
		out, err := s.run("scripts/prerm", nil, "remove")
		if err != nil {
			t.Fatalf("prerm remove failed: %v\n%s", err, out)
		}
		calls := s.calls()
		if !slices.Contains(calls, "systemctl stop db-branch.service") {
			t.Errorf("expected systemctl stop db-branch.service, got: %v", calls)
		}
		for _, c := range calls {
			if strings.Contains(c, "mysqld") || strings.Contains(c, "postgres") || strings.Contains(c, "db-branch-rt") || strings.Contains(c, "disable") {
				t.Errorf("unexpected call during prerm remove: %q", c)
			}
		}
		// Idempotent second remove.
		if out2, err := s.run("scripts/prerm", nil, "remove"); err != nil {
			t.Fatalf("second prerm remove failed: %v\n%s", err, out2)
		}
	})

	t.Run("policy-rc.dが101を返したらremove時もstopしない", func(t *testing.T) {
		s := newScriptEnv(t, true)
		s.installPolicyRCD(101)
		if out, err := s.run("scripts/prerm", nil, "remove"); err != nil {
			t.Fatalf("prerm remove with policy-rc.d=101 failed: %v\n%s", err, out)
		}
		if slices.Contains(s.calls(), "systemctl stop db-branch.service") {
			t.Errorf("policy-rc.d 101 must prevent stop: %v", s.calls())
		}
	})

	t.Run("deconfigureやfailed-upgradeは無操作で成功する", func(t *testing.T) {
		for _, arg := range []string{"deconfigure", "failed-upgrade"} {
			s := newScriptEnv(t, true)
			if out, err := s.run("scripts/prerm", nil, arg); err != nil {
				t.Fatalf("prerm %s failed: %v\n%s", arg, err, out)
			}
			if len(s.calls()) != 0 {
				t.Errorf("prerm %s should not call systemctl: %v", arg, s.calls())
			}
		}
	})

	t.Run("未知の引数は非0終了する", func(t *testing.T) {
		s := newScriptEnv(t, true)
		if _, err := s.run("scripts/prerm", nil, "bogus"); err == nil {
			t.Fatal("expected non-zero exit for unknown argument")
		}
	})
}

func TestPostinst_configure分岐とconfig不在耐性とupgrade時の再起動制御(t *testing.T) {
	t.Run("初回configureはユーザー作成しconfig不在でも失敗せず有効化と起動を行う", func(t *testing.T) {
		s := newScriptEnv(t, true)
		out, err := s.run("scripts/postinst", nil, "configure")
		if err != nil {
			t.Fatalf("postinst configure (no config.toml) failed: %v\n%s", err, out)
		}
		calls := s.calls()
		wantSubs := []string{
			"addgroup --system db-branch",
			"addgroup --system db-branch-admin",
			"systemctl daemon-reload",
			"systemctl enable db-branch.service",
			"systemctl restart db-branch.service",
		}
		for _, want := range wantSubs {
			if !slices.Contains(calls, want) {
				t.Errorf("missing call %q in %v", want, calls)
			}
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "chgrp ") {
				t.Errorf("chgrp should not run when config.toml is absent: %q", c)
			}
		}
	})

	t.Run("configが存在すればchgrpを実行し既存db-branchメンバーをdb-branch-adminへ移行する", func(t *testing.T) {
		s := newScriptEnv(t, true)
		cfgPath := s.touch("etc/db-branch/config.toml", "# config\n")
		out, err := s.run("scripts/postinst", []string{
			"STUB_GROUP_DB_BRANCH=1",
			"STUB_USER_DB_BRANCH=1",
			"STUB_GROUP_MEMBERS=alice,db-branch,bob",
		}, "configure")
		if err != nil {
			t.Fatalf("postinst configure failed: %v\n%s", err, out)
		}
		calls := s.calls()
		if !slices.Contains(calls, "chgrp db-branch-admin "+cfgPath) {
			t.Errorf("expected chgrp on %s, got: %v", cfgPath, calls)
		}
		if !slices.Contains(calls, "adduser alice db-branch-admin") || !slices.Contains(calls, "adduser bob db-branch-admin") {
			t.Errorf("expected member migration to db-branch-admin, got: %v", calls)
		}
		if slices.Contains(calls, "adduser db-branch db-branch-admin") {
			t.Errorf("db-branch service user must not be added to db-branch-admin: %v", calls)
		}
	})

	t.Run("upgrade時は既存アカウントを再作成せず有効化済みならrestartし無効化やmask時は起動しない", func(t *testing.T) {
		existingEnv := []string{
			"STUB_GROUP_DB_BRANCH=1",
			"STUB_USER_DB_BRANCH=1",
			"STUB_GROUP_ADMIN=1",
		}

		sEnabled := newScriptEnv(t, true)
		out, err := sEnabled.run("scripts/postinst", append(slices.Clone(existingEnv), "STUB_ENABLED=1"), "configure", "0.3.1")
		if err != nil {
			t.Fatalf("postinst upgrade (enabled) failed: %v\n%s", err, out)
		}
		calls := sEnabled.calls()
		for _, c := range calls {
			if strings.HasPrefix(c, "addgroup ") || strings.HasPrefix(c, "adduser ") || strings.Contains(c, "unmask") || strings.Contains(c, "systemctl enable") {
				t.Errorf("upgrade must not recreate users, unmask, or force enable: %q in %v", c, calls)
			}
		}
		if !slices.Contains(calls, "systemctl restart db-branch.service") {
			t.Errorf("upgrade of enabled service must restart: %v", calls)
		}
		// Idempotent repeated configure on upgrade.
		if out2, err := sEnabled.run("scripts/postinst", append(slices.Clone(existingEnv), "STUB_ENABLED=1"), "configure", "0.3.1"); err != nil {
			t.Fatalf("second postinst upgrade failed: %v\n%s", err, out2)
		}

		sDisabled := newScriptEnv(t, true)
		out, err = sDisabled.run("scripts/postinst", append(slices.Clone(existingEnv), "STUB_ENABLED=0"), "configure", "0.3.1")
		if err != nil {
			t.Fatalf("postinst upgrade (disabled/masked) failed: %v\n%s", err, out)
		}
		calls = sDisabled.calls()
		if slices.Contains(calls, "systemctl enable db-branch.service") ||
			slices.Contains(calls, "systemctl restart db-branch.service") ||
			slices.Contains(calls, "systemctl unmask db-branch.service") {
			t.Errorf("upgrade of disabled/masked service must not unmask, enable, or restart: %v", calls)
		}
		if !slices.Contains(calls, "systemctl try-restart db-branch.service") {
			t.Errorf("expected try-restart for disabled service on upgrade: %v", calls)
		}
	})

	t.Run("remove後の再インストール時はwantsが保持され有効化済み扱いで再起動する", func(t *testing.T) {
		s := newScriptEnv(t, true)
		s.touch("etc/db-branch/config.toml", "pool = \"tank\"\n")
		if out, err := s.run("scripts/prerm", nil, "remove"); err != nil {
			t.Fatalf("prerm remove failed: %v\n%s", err, out)
		}
		if out, err := s.run("scripts/postrm", nil, "remove"); err != nil {
			t.Fatalf("postrm remove failed: %v\n%s", err, out)
		}
		out, err := s.run("scripts/postinst", []string{
			"STUB_GROUP_DB_BRANCH=1",
			"STUB_USER_DB_BRANCH=1",
			"STUB_GROUP_ADMIN=1",
			"STUB_ENABLED=1",
		}, "configure", "1.0.0")
		if err != nil {
			t.Fatalf("reinstall after remove failed: %v\n%s", err, out)
		}
		if !slices.Contains(s.calls(), "systemctl restart db-branch.service") {
			t.Errorf("expected restart on reinstall after remove: %v", s.calls())
		}
	})

	t.Run("policy-rc.dが101を返したらconfigure時もrestartしない", func(t *testing.T) {
		s := newScriptEnv(t, true)
		s.installPolicyRCD(101)
		if out, err := s.run("scripts/postinst", nil, "configure"); err != nil {
			t.Fatalf("postinst configure with policy-rc.d=101 failed: %v\n%s", err, out)
		}
		calls := s.calls()
		if !slices.Contains(calls, "systemctl enable db-branch.service") {
			t.Errorf("enable should still run under policy-rc.d=101: %v", calls)
		}
		if slices.Contains(calls, "systemctl restart db-branch.service") {
			t.Errorf("policy-rc.d 101 must prevent restart: %v", calls)
		}
	})

	t.Run("abort系は何もせず成功し未知の引数は非0終了する", func(t *testing.T) {
		for _, arg := range []string{"abort-upgrade", "abort-remove", "abort-deconfigure"} {
			s := newScriptEnv(t, true)
			if out, err := s.run("scripts/postinst", nil, arg); err != nil {
				t.Fatalf("postinst %s failed: %v\n%s", arg, err, out)
			}
			if len(s.calls()) != 0 {
				t.Errorf("postinst %s should be no-op, got: %v", arg, s.calls())
			}
		}
		s := newScriptEnv(t, true)
		if _, err := s.run("scripts/postinst", nil, "bogus"); err == nil {
			t.Fatal("expected non-zero exit for unknown argument")
		}
	})
}

func TestPostrm_removeは設定とデータを残しpurgeは設定のみ削除しZFSやデータは消さない(t *testing.T) {
	t.Run("remove時はdaemon-reloadのみ行い設定とデータを残す", func(t *testing.T) {
		s := newScriptEnv(t, true)
		cfg := s.touch("etc/db-branch/config.toml", "pool = \"tank\"\n")
		pw := s.touch("etc/db-branch/initial-admin-password", "secret\n")
		mp := s.touch("etc/modprobe.d/db-branch-zfs.conf", "options zfs zfs_arc_max=1024\n")
		data := s.touch("var/lib/db-branch/node.json", "{}\n")

		out, err := s.run("scripts/postrm", nil, "remove")
		if err != nil {
			t.Fatalf("postrm remove failed: %v\n%s", err, out)
		}
		for _, keep := range []string{cfg, pw, mp, data} {
			if _, err := os.Stat(keep); err != nil {
				t.Errorf("postrm remove must keep %s: %v", keep, err)
			}
		}
		calls := s.calls()
		if !slices.Contains(calls, "systemctl daemon-reload") {
			t.Errorf("expected daemon-reload on remove, got: %v", calls)
		}
		for _, c := range calls {
			if strings.Contains(c, "mask") || strings.Contains(c, "disable") {
				t.Errorf("postrm remove must not mask or disable unit: %q", c)
			}
		}
	})

	t.Run("purge時はetc設定とwantsリンクを削除しvarLibデータは残す", func(t *testing.T) {
		s := newScriptEnv(t, true)
		s.touch("etc/db-branch/config.toml", "pool = \"tank\"\n")
		s.touch("etc/db-branch/initial-admin-password", "secret\n")
		s.touch("etc/db-branch/trusted-keys/k1.pub", "key\n")
		mp := s.touch("etc/modprobe.d/db-branch-zfs.conf", "options zfs zfs_arc_max=1024\n")
		wants := s.touch("etc/systemd/system/multi-user.target.wants/db-branch.service", "symlink\n")
		data := s.touch("var/lib/db-branch/node.json", "{}\n")

		out, err := s.run("scripts/postrm", nil, "purge")
		if err != nil {
			t.Fatalf("postrm purge failed: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(s.root, "etc/db-branch")); !os.IsNotExist(err) {
			t.Errorf("/etc/db-branch should be removed on purge, stat err=%v", err)
		}
		if _, err := os.Stat(mp); !os.IsNotExist(err) {
			t.Errorf("/etc/modprobe.d/db-branch-zfs.conf should be removed on purge, stat err=%v", err)
		}
		if _, err := os.Stat(wants); !os.IsNotExist(err) {
			t.Errorf("wants symlink should be removed on purge, stat err=%v", err)
		}
		if _, err := os.Stat(data); err != nil {
			t.Errorf("/var/lib/db-branch data must be preserved on purge: %v", err)
		}
		if !slices.Contains(s.calls(), "systemctl disable db-branch.service") {
			t.Errorf("expected systemctl disable on purge, got: %v", s.calls())
		}
		// Idempotent second purge.
		if out2, err := s.run("scripts/postrm", nil, "purge"); err != nil {
			t.Fatalf("second postrm purge failed: %v\n%s", err, out2)
		}
	})

	t.Run("upgradeやabort系は無操作で成功し未知の引数は非0終了する", func(t *testing.T) {
		for _, arg := range []string{"upgrade", "failed-upgrade", "abort-install", "abort-upgrade", "disappear"} {
			s := newScriptEnv(t, true)
			cfg := s.touch("etc/db-branch/config.toml", "pool = \"tank\"\n")
			if out, err := s.run("scripts/postrm", nil, arg); err != nil {
				t.Fatalf("postrm %s failed: %v\n%s", arg, err, out)
			}
			if len(s.calls()) != 0 {
				t.Errorf("postrm %s should be no-op, got: %v", arg, s.calls())
			}
			if _, err := os.Stat(cfg); err != nil {
				t.Errorf("postrm %s touched config: %v", arg, err)
			}
		}
		s := newScriptEnv(t, true)
		if _, err := s.run("scripts/postrm", nil, "bogus"); err == nil {
			t.Fatal("expected non-zero exit for unknown argument")
		}
	})
}

func TestProxySQLScripts_引数分岐に従ってsystemd操作を行う(t *testing.T) {
	t.Run("proxysql postinst 初回configureはproxysqlを有効化起動しagentをtry-restartする", func(t *testing.T) {
		s := newScriptEnv(t, true)
		out, err := s.run("proxysql/postinst", nil, "configure")
		if err != nil {
			t.Fatalf("proxysql/postinst configure failed: %v\n%s", err, out)
		}
		calls := s.calls()
		if !slices.Contains(calls, "systemctl enable --now proxysql.service") ||
			!slices.Contains(calls, "systemctl try-restart db-branch.service") {
			t.Errorf("unexpected calls: %v", calls)
		}
	})

	t.Run("proxysql postinst upgradeはproxysqlをenableせずagentのみtry-restartする", func(t *testing.T) {
		s := newScriptEnv(t, true)
		out, err := s.run("proxysql/postinst", nil, "configure", "0.3.1")
		if err != nil {
			t.Fatalf("proxysql/postinst upgrade failed: %v\n%s", err, out)
		}
		calls := s.calls()
		if slices.Contains(calls, "systemctl enable --now proxysql.service") {
			t.Errorf("proxysql/postinst upgrade must not re-enable proxysql.service: %v", calls)
		}
		if !slices.Contains(calls, "systemctl try-restart db-branch.service") {
			t.Errorf("expected try-restart db-branch.service, got: %v", calls)
		}
	})

	t.Run("proxysql postinst policy-rc.dが101なら起動や再起動を行わない", func(t *testing.T) {
		s := newScriptEnv(t, true)
		s.installPolicyRCD(101)
		if out, err := s.run("proxysql/postinst", nil, "configure"); err != nil {
			t.Fatalf("proxysql/postinst with policy-rc.d=101 failed: %v\n%s", err, out)
		}
		calls := s.calls()
		if !slices.Contains(calls, "systemctl enable proxysql.service") {
			t.Errorf("expected enable without --now when policy-rc.d=101: %v", calls)
		}
		if slices.Contains(calls, "systemctl enable --now proxysql.service") ||
			slices.Contains(calls, "systemctl try-restart db-branch.service") {
			t.Errorf("policy-rc.d 101 must prevent start/restart: %v", calls)
		}
	})

	t.Run("proxysql postinst abort系は無操作で未知の引数は非0終了する", func(t *testing.T) {
		for _, arg := range []string{"abort-upgrade", "abort-remove", "abort-deconfigure"} {
			s := newScriptEnv(t, true)
			if out, err := s.run("proxysql/postinst", nil, arg); err != nil {
				t.Fatalf("proxysql/postinst %s failed: %v\n%s", arg, err, out)
			}
			if len(s.calls()) != 0 {
				t.Errorf("expected no-op, got: %v", s.calls())
			}
		}
		s := newScriptEnv(t, true)
		if _, err := s.run("proxysql/postinst", nil, "bogus"); err == nil {
			t.Fatal("expected non-zero exit for unknown argument")
		}
	})

	t.Run("proxysql restart-agentはremoveとpurgeでtry-restartしupgradeでは無操作", func(t *testing.T) {
		for _, arg := range []string{"remove", "purge"} {
			s := newScriptEnv(t, true)
			if out, err := s.run("proxysql/restart-agent", nil, arg); err != nil {
				t.Fatalf("proxysql/restart-agent %s failed: %v\n%s", arg, err, out)
			}
			if !slices.Contains(s.calls(), "systemctl try-restart db-branch.service") {
				t.Errorf("restart-agent %s expected try-restart, got: %v", arg, s.calls())
			}
		}
		for _, arg := range []string{"upgrade", "failed-upgrade", "abort-install", "abort-upgrade", "disappear"} {
			s := newScriptEnv(t, true)
			if out, err := s.run("proxysql/restart-agent", nil, arg); err != nil {
				t.Fatalf("proxysql/restart-agent %s failed: %v\n%s", arg, err, out)
			}
			if len(s.calls()) != 0 {
				t.Errorf("restart-agent %s expected no-op, got: %v", arg, s.calls())
			}
		}
		s := newScriptEnv(t, true)
		if _, err := s.run("proxysql/restart-agent", nil, "bogus"); err == nil {
			t.Fatal("expected non-zero exit for unknown argument")
		}
	})

	t.Run("proxysql restart-agentはpolicy-rc.dが101なら再起動を行わない", func(t *testing.T) {
		s := newScriptEnv(t, true)
		s.installPolicyRCD(101)
		if out, err := s.run("proxysql/restart-agent", nil, "remove"); err != nil {
			t.Fatalf("proxysql/restart-agent with policy-rc.d=101 failed: %v\n%s", err, out)
		}
		if slices.Contains(s.calls(), "systemctl try-restart db-branch.service") {
			t.Errorf("policy-rc.d 101 must prevent try-restart: %v", s.calls())
		}
	})

	t.Run("systemd非稼働環境では全スクリプトがsystemctlを呼ばず成功する", func(t *testing.T) {
		for _, tc := range []struct {
			script string
			args   []string
		}{
			{"scripts/prerm", []string{"remove"}},
			{"scripts/postinst", []string{"configure"}},
			{"scripts/postrm", []string{"remove"}},
			{"scripts/postrm", []string{"purge"}},
			{"proxysql/postinst", []string{"configure"}},
			{"proxysql/restart-agent", []string{"remove"}},
		} {
			s := newScriptEnv(t, false)
			if out, err := s.run(tc.script, nil, tc.args...); err != nil {
				t.Fatalf("%s %v without systemd failed: %v\n%s", tc.script, tc.args, err, out)
			}
			for _, c := range s.calls() {
				if strings.HasPrefix(c, "systemctl ") {
					t.Errorf("%s %v called systemctl without /run/systemd/system: %q", tc.script, tc.args, c)
				}
			}
		}
	})
}
