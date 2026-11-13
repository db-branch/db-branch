package mysql_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func mysqlRT(v string) domain.RuntimeRef { return domain.RuntimeRef{Engine: domain.MySQL, Version: v} }

func finding(fs []domain.Finding, code string) (domain.Finding, bool) {
	for _, f := range fs {
		if f.Code == code {
			return f, true
		}
	}
	return domain.Finding{}, false
}

func TestConfig_8_4でレガシー認証が必要なブランチにはnative_passwordを有効にする(t *testing.T) {
	f := newFixture(t)
	f.spec.LegacyAuth = true
	for version, want := range map[string]bool{"8.0.46": false, "8.4.13": true, "9.6.0": false} {
		f.spec.Runtime = mysqlRT(version)
		if err := f.srv.PrepareClone(ctx, f.spec); err != nil {
			t.Fatal(err)
		}
		cnf := read(t, filepath.Join(f.state, f.spec.ID, "my.cnf"))
		if got := strings.Contains(cnf, "mysql_native_password=ON\n"); got != want {
			t.Errorf("%s: native password option = %v, want %v", version, got, want)
		}
	}
}

func TestConfig_新しいバージョンで消えたオプションを落とすか書き換える(t *testing.T) {
	f := newFixture(t)
	f.spec.Extra = []string{"default-authentication-plugin=mysql_native_password", "expire_logs_days=2", "binlog_format=ROW", "innodb_log_file_size=48M", "master_info_repository=TABLE"}

	f.spec.Runtime = mysqlRT("8.0.46")
	_ = f.srv.PrepareClone(ctx, f.spec)
	old := read(t, filepath.Join(f.state, f.spec.ID, "my.cnf"))
	for _, want := range []string{"default-authentication-plugin=mysql_native_password", "expire_logs_days=2", "innodb_log_file_size=48M"} {
		if !strings.Contains(old, want) {
			t.Errorf("8.0 config lost %q:\n%s", want, old)
		}
	}

	f.spec.Runtime = mysqlRT("8.4.13")
	_ = f.srv.PrepareClone(ctx, f.spec)
	cnf := read(t, filepath.Join(f.state, f.spec.ID, "my.cnf"))
	for _, gone := range []string{"default-authentication-plugin", "expire_logs_days", "master_info_repository"} {
		if strings.Contains(cnf, gone) {
			t.Errorf("8.4 config still has %q:\n%s", gone, cnf)
		}
	}
	for _, want := range []string{"binlog_expire_logs_seconds=172800\n", "binlog_format=ROW\n", "innodb_log_file_size=48M\n"} {
		if !strings.Contains(cnf, want) {
			t.Errorf("8.4 config lacks %q:\n%s", want, cnf)
		}
	}

	f.spec.Runtime = mysqlRT("9.6.0")
	_ = f.srv.PrepareClone(ctx, f.spec)
	if cnf := read(t, filepath.Join(f.state, f.spec.ID, "my.cnf")); strings.Contains(cnf, "innodb_log_file_size") {
		t.Errorf("9.6 config still has innodb_log_file_size:\n%s", cnf)
	}
}

func TestConfig_binlog_expire_logs_secondsがあればexpire_logs_daysは足さない(t *testing.T) {
	f := newFixture(t)
	f.spec.Runtime = mysqlRT("8.4.13")
	f.spec.Extra = []string{"expire_logs_days=2", "binlog_expire_logs_seconds=60", "expire_logs_days=x"}
	_ = f.srv.PrepareClone(ctx, f.spec)
	cnf := read(t, filepath.Join(f.state, f.spec.ID, "my.cnf"))
	if strings.Count(cnf, "\nbinlog_expire_logs_seconds") != 1 || !strings.Contains(cnf, "binlog_expire_logs_seconds=60") {
		t.Errorf("config:\n%s", cnf)
	}
}

func TestCheckUpgrade_8_4へはnative_passwordのユーザーを警告してプラグインを残す(t *testing.T) {
	f := newFixture(t)
	f.sys.outputs = map[string]string{"FROM mysql.user": "app\t%\nbranch-x\t%\n"}

	plan, err := f.srv.CheckUpgrade(ctx, f.spec, mysqlRT("8.4.13"), domain.Login{User: "branch-x"})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := finding(plan.Findings, domain.CodeNativePassword)
	if !ok || w.Level != domain.LevelWarning || !strings.Contains(w.Message, "'app'@'%'") || !plan.LegacyAuth {
		t.Errorf("plan = %+v", plan)
	}
	if !f.sys.called("mysqld --defaults-file=") || !f.sys.called("mysqld --defaults-file="+filepath.Join(f.state, f.spec.ID, "my.cnf.check")+" --validate-config --user=db-branch") {
		t.Errorf("config was not validated: %q", f.sys.calls)
	}
}

func TestCheckUpgrade_9へはnative_passwordのユーザーをエラーにしブランチのユーザーは自動で直す(t *testing.T) {
	f := newFixture(t)
	f.sys.outputs = map[string]string{"FROM mysql.user": "app\t%\nbranch-x\t%\n"}

	plan, err := f.srv.CheckUpgrade(ctx, f.spec, mysqlRT("9.6.0"), domain.Login{User: "branch-x"})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := finding(plan.Findings, domain.CodeNativePasswordGone)
	if !ok || e.Level != domain.LevelError || !strings.Contains(e.Message, "'app'@'%'") || strings.Contains(e.Message, "branch-x") {
		t.Errorf("findings = %+v", plan.Findings)
	}
	if i, ok := finding(plan.Findings, domain.CodeBranchUser); !ok || i.Level != domain.LevelInfo || plan.LegacyAuth {
		t.Errorf("findings = %+v", plan.Findings)
	}
}

func TestCheckUpgrade_ブランチのユーザーだけなら9へもエラーにしない(t *testing.T) {
	f := newFixture(t)
	f.sys.outputs = map[string]string{"FROM mysql.user": "branch-x\t%\n"}
	plan, _ := f.srv.CheckUpgrade(ctx, f.spec, mysqlRT("9.6.0"), domain.Login{User: "branch-x"})
	if domain.HasErrors(plan.Findings) {
		t.Errorf("findings = %+v", plan.Findings)
	}
}

func TestCheckUpgrade_該当ユーザーがなければ何も言わない(t *testing.T) {
	f := newFixture(t)
	plan, err := f.srv.CheckUpgrade(ctx, f.spec, mysqlRT("8.4.13"), domain.Login{User: "branch-x"})
	if err != nil || len(plan.Findings) != 0 || plan.LegacyAuth {
		t.Errorf("plan = %+v, err = %v", plan, err)
	}
}

func TestCheckUpgrade_8_0から8_2のようなバージョンではユーザーを調べない(t *testing.T) {
	f := newFixture(t)
	f.sys.failOn = "FROM mysql.user"
	if _, err := f.srv.CheckUpgrade(ctx, f.spec, mysqlRT("8.2.0"), domain.Login{}); err != nil {
		t.Errorf("accounts were queried: %v", err)
	}
}

func TestCheckUpgrade_ユーザーを読めなければエラー(t *testing.T) {
	f := newFixture(t)
	f.sys.failOn = "FROM mysql.user"
	if _, err := f.srv.CheckUpgrade(ctx, f.spec, mysqlRT("8.4.13"), domain.Login{}); err == nil {
		t.Error("want error")
	}
}

func TestCheckUpgrade_新しいmysqldが設定を拒否したらエラーにする(t *testing.T) {
	f := newFixture(t)
	f.sys.failOn = "--validate-config"
	f.spec.Extra = []string{"default_authentication_plugin=mysql_native_password"}
	plan, err := f.srv.CheckUpgrade(ctx, f.spec, mysqlRT("8.4.13"), domain.Login{})
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := finding(plan.Findings, domain.CodeConfigInvalid); !ok || e.Level != domain.LevelError || !strings.Contains(e.Message, "injected failure") {
		t.Errorf("findings = %+v", plan.Findings)
	}
	if a, ok := finding(plan.Findings, domain.CodeConfigAdapted); !ok || !strings.Contains(a.Message, "default_authentication_plugin") {
		t.Errorf("findings = %+v", plan.Findings)
	}
}

func TestStartUpgraded_起動中のログから警告とアップグレードの進行を拾う(t *testing.T) {
	f := newFixture(t)
	f.sys.onStart = "2026-11-13T05:00:00.000000Z 0 [System] [MY-013381] [Server] Server upgrade from '80046' to '80413' started.\n" +
		"2026-11-13T05:00:01.000000Z 0 [Warning] [MY-010068] [Server] CA certificate ca.pem is self signed.\n" +
		"2026-11-13T05:00:01.000000Z 0 [Warning] [MY-010068] [Server] CA certificate ca.pem is self signed.\n" +
		"2026-11-13T05:00:02.000000Z 0 [Note] [MY-000000] [Server] unrelated note\n" +
		"2026-11-13T05:00:03.000000Z 0 [ERROR] [MY-012345] [InnoDB] something broke\n" +
		"not a log line\n"
	f.spec.Runtime = mysqlRT("8.4.13")

	got, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{User: "branch-x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 3 {
		t.Fatalf("findings = %+v", got.Findings)
	}
	want := []domain.FindingLevel{domain.LevelInfo, domain.LevelWarning, domain.LevelError}
	for i, w := range want {
		if got.Findings[i].Level != w {
			t.Errorf("finding %d = %+v, want level %s", i, got.Findings[i], w)
		}
	}
	if !strings.Contains(got.Excerpt, "something broke") {
		t.Errorf("excerpt = %q", got.Excerpt)
	}
}

func TestStartUpgraded_起動に失敗してもログの抜粋を返す(t *testing.T) {
	f := newFixture(t)
	f.sys.onStart = "2026-11-13T05:00:03.000000Z 0 [ERROR] [MY-013170] [InnoDB] Upgrade failed.\n"
	f.sys.crashes = true
	f.sys.pingFails = 1 << 30
	got, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{})
	if err == nil || !strings.Contains(got.Excerpt, "Upgrade failed") || len(got.Findings) != 1 {
		t.Errorf("log = %+v, err = %v", got, err)
	}
}

func TestStartUpgraded_待ち時間はUpgradeTimeoutまで(t *testing.T) {
	f := newFixture(t)
	f.sys.pingFails = 1 << 30
	f.srv.StartTimeout = time.Millisecond
	f.srv.UpgradeTimeout = 30 * time.Millisecond
	start := time.Now()
	_, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{})
	if err == nil || !strings.Contains(err.Error(), "did not become ready within 30ms") || time.Since(start) < 25*time.Millisecond {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
}

func TestStartUpgraded_9では起動後にブランチのユーザーをcaching_sha2へ直す(t *testing.T) {
	f := newFixture(t)
	f.spec.Runtime = mysqlRT("9.6.0")
	f.sys.outputs = map[string]string{"AND plugin = 'mysql_native_password'": "%\n"}
	if _, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{User: "branch-x", Password: "pw1"}); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("mysql --socket=") {
		t.Fatal("no client call")
	}
	found := false
	for _, c := range f.sys.calls {
		if strings.Contains(c, "ALTER USER 'branch-x'@'%' IDENTIFIED WITH caching_sha2_password BY 'pw1'") {
			found = true
		}
	}
	if !found {
		t.Errorf("calls = %q", f.sys.calls)
	}
}

func TestStartUpgraded_ユーザーを直せなければ失敗にする(t *testing.T) {
	f := newFixture(t)
	f.spec.Runtime = mysqlRT("9.6.0")
	f.sys.outputs = map[string]string{"AND plugin = 'mysql_native_password'": "%\n"}
	f.sys.failOn = "ALTER USER"
	if _, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{User: "branch-x"}); err == nil || !strings.Contains(err.Error(), "caching_sha2_password") {
		t.Errorf("err = %v", err)
	}
	f.sys.failOn = "AND plugin"
	if _, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{User: "branch-x"}); err == nil {
		t.Error("want error when the accounts cannot be read")
	}
}

func TestUpgradeFindings_多すぎる行は件数にまとめる(t *testing.T) {
	f := newFixture(t)
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("2026-11-13T05:00:00.000000Z 0 [Warning] [MY-0" + strings.Repeat("1", 1) + "] [Server] warning number " + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "\n")
	}
	f.sys.onStart = b.String()
	got, _ := f.srv.StartUpgraded(ctx, f.spec, domain.Login{})
	last := got.Findings[len(got.Findings)-1]
	if len(got.Findings) != 41 || !strings.Contains(last.Message, "20 more") {
		t.Errorf("%d findings, last = %+v", len(got.Findings), last)
	}
}
