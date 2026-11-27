package postgres_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/postgres"
)

func TestCheckUpgrade_同じメジャーなら情報だけ返す(t *testing.T) {
	for _, tc := range []struct {
		from      string
		pgVersion string
		to        string
	}{
		{"17.2", "17\n", "17.5"},
		{"19.0", "19\n", "19.1"},
	} {
		f := newFixture(t)
		f.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: tc.from}
		_ = os.WriteFile(filepath.Join(f.spec.DataDir, "PG_VERSION"), []byte(tc.pgVersion), 0o644)
		plan, err := f.srv.CheckUpgrade(ctx, f.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: tc.to}, domain.Login{})
		if err != nil || len(plan.Findings) != 1 || plan.Findings[0].Level != domain.LevelInfo {
			t.Errorf("%s -> %s: plan = %+v, err = %v", tc.pgVersion, tc.to, plan, err)
		}
	}
}

func TestCheckUpgrade_PG_VERSIONがランタイムと矛盾すればエラーにする(t *testing.T) {
	f := newFixture(t)
	f.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "17.12"}
	_ = os.WriteFile(filepath.Join(f.spec.DataDir, "PG_VERSION"), []byte("16\n"), 0o644)
	plan, _ := f.srv.CheckUpgrade(ctx, f.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "17.13"}, domain.Login{})
	if !domain.HasErrors(plan.Findings) || !strings.Contains(plan.Findings[0].Message, "PostgreSQL 16") {
		t.Errorf("plan = %+v", plan)
	}
}

func TestCheckUpgrade_PG_VERSIONが読めなければエラー(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.CheckUpgrade(ctx, f.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "17.5"}, domain.Login{}); err == nil {
		t.Error("want error")
	}
}

func TestStartUpgraded_起動してジャーナルを添える(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.StartUpgraded(ctx, f.spec, domain.Login{}); err != nil {
		t.Fatal(err)
	}
}

func setupPGMajorRuntimes(t *testing.T, f *fixture, oldVer, newVer string, newExts map[string]string) {
	t.Helper()
	oldRoot := filepath.Join(f.rt, oldVer)
	newRoot := filepath.Join(f.rt, newVer)
	oldMajor := strings.SplitN(oldVer, ".", 2)[0]
	newMajor := strings.SplitN(newVer, ".", 2)[0]
	for _, r := range []struct {
		dir, major string
	}{{oldRoot, oldMajor}, {newRoot, newMajor}} {
		_ = os.MkdirAll(filepath.Join(r.dir, "bin"), 0o755)
		_ = os.MkdirAll(filepath.Join(r.dir, "lib"), 0o755)
		_ = os.MkdirAll(filepath.Join(r.dir, "usr/share/postgresql", r.major, "extension"), 0o755)
	}
	for extName, ver := range newExts {
		p := filepath.Join(newRoot, "usr/share/postgresql", newMajor, "extension", extName+".control")
		_ = os.WriteFile(p, []byte("default_version = '"+ver+"'\n"), 0o644)
	}
	f.srv.RuntimeDir = func(rt domain.RuntimeRef) string {
		return filepath.Join(f.rt, rt.Version)
	}
}

func TestCheckUpgrade_メジャー更新の事前確認で拡張とチェックサムと見積もりとpg_upgrade結果を返す(t *testing.T) {
	f := newFixture(t)
	setupPGMajorRuntimes(t, &f, "18.6", "19.0", map[string]string{
		"pg_trgm": "1.7",
		"pgcrypto": "1.4",
	})
	_ = os.WriteFile(filepath.Join(f.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f.sys.outputs = map[string]string{
		"pg_controldata":          "Data page checksum version:           0\nDatabase cluster state:               in production\n",
		"pg_encoding_to_char":     "UTF8|en_US.UTF-8|en_US.UTF-8\n",
		"datlocprovider":          "c|\n",
		"NOT datistemplate":       "postgres\nappdb\n",
		"pg_database_size":        "21474836480\n", // 20 GiB
		"FROM pg_class":           "12000\n",       // > threshold
		"pg_largeobject_metadata": "60000\n",       // > threshold
		"pg_extension":            "pg_trgm|1.6\nanon|3.3.2\n",
		"pg_replication_slots":    "1\n",
		"pg_subscription":         "1\n",
		"pg_upgrade --check":      "Performing Consistency Checks on Old Live Server\nChecking cluster versions                                     ok\n*Clusters are compatible*\n",
	}

	to := domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	plan, err := f.srv.CheckUpgrade(ctx, f.spec, to, domain.Login{User: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.PGMeta == nil || plan.PGMeta.Encoding != "UTF8" || plan.PGMeta.LCCollate != "en_US.UTF-8" {
		t.Errorf("PGMeta = %+v", plan.PGMeta)
	}
	if plan.Timeout <= 15*time.Minute {
		t.Errorf("expected scaled timeout > 15m, got %s", plan.Timeout)
	}
	// Check expected findings
	wantCodes := map[string]domain.FindingLevel{
		domain.CodeExtensionMissing:   domain.LevelError,
		domain.CodeExtensionUpdate:    domain.LevelInfo,
		domain.CodeLogicalReplication: domain.LevelWarning,
		domain.CodeManyRelations:      domain.LevelWarning,
		domain.CodeManyLargeObjects:   domain.LevelWarning,
		domain.CodeLargeDatabase:      domain.LevelWarning,
	}
	got := map[string]domain.FindingLevel{}
	for _, fd := range plan.Findings {
		got[fd.Code] = fd.Level
	}
	for code, lvl := range wantCodes {
		if got[code] != lvl {
			t.Errorf("finding %s = %q, want %q (all: %+v)", code, got[code], lvl, plan.Findings)
		}
	}
	// initdb for check must match --no-data-checksums and en_US.UTF-8
	if !f.sys.called("--no-data-checksums") || !f.sys.called("--lc-collate=en_US.UTF-8") {
		t.Errorf("initdb did not match old cluster settings: %v", f.sys.calls)
	}
}

func TestCheckUpgrade_pg_upgrade_checkのエラーをFindingに変換する(t *testing.T) {
	f := newFixture(t)
	setupPGMajorRuntimes(t, &f, "18.6", "19.0", map[string]string{})
	_ = os.WriteFile(filepath.Join(f.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f.sys.outputs = map[string]string{
		"pg_controldata":      "Data page checksum version:           1\n",
		"pg_encoding_to_char": "UTF8|C.UTF-8|C.UTF-8\n",
		"datlocprovider":      "b|C.UTF-8\n",
		"NOT datistemplate":   "postgres\n",
	}
	f.sys.failOn = "pg_upgrade --check"

	plan, err := f.srv.CheckUpgrade(ctx, f.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}, domain.Login{})
	if err != nil {
		t.Fatalf("pg_upgrade --check failure should become a finding, not err: %v", err)
	}
	if !domain.HasErrors(plan.Findings) {
		t.Errorf("expected error finding, got %+v", plan.Findings)
	}
	if !f.sys.called("--data-checksums") || !f.sys.called("--locale-provider=builtin") {
		t.Errorf("initdb args = %v", f.sys.calls)
	}
}

func TestStartMajorUpgraded_swapまたはlinkとチェックサムを合わせてアップグレードしNeedsStatsを返す(t *testing.T) {
	for _, tc := range []struct {
		from, to   string
		checksums  string
		wantFlag   string
		wantCheck  string
	}{
		{"18.6", "19.0", "1", "--swap", "--data-checksums"},
		{"17.12", "18.7", "0", "--swap", "--no-data-checksums"},
		{"16.4", "17.12", "0", "--link", "--no-data-checksums"},
	} {
		f := newFixture(t)
		setupPGMajorRuntimes(t, &f, tc.from, tc.to, nil)
		_ = os.WriteFile(filepath.Join(f.spec.DataDir, "PG_VERSION"), []byte(strings.SplitN(tc.from, ".", 2)[0]+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(f.spec.DataDir, "global_dummy"), []byte("old"), 0o600)
		f.sys.outputs = map[string]string{
			"pg_controldata": "Data page checksum version:           " + tc.checksums + "\n",
			"pg_upgrade ": "Performing Consistency Checks\nChecking cluster versions                                     ok\n" +
				"Performing Upgrade\nCreating dump of database schemas                             ok\n" +
				"Restoring database schemas in the new cluster                 ok\n" +
				"Swapping data directories                                     ok\n" +
				"Sync data directory to disk                                   ok\n" +
				"Upgrade Complete\n",
		}
		f.sys.initdb = func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, "pg_hba.conf"), []byte("local all all trust\n"), 0o600)
			_ = os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte(strings.SplitN(tc.to, ".", 2)[0]+"\n"), 0o644)
		}
		var phases []string
		f.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: tc.to}
		upCtx := domain.UpgradeContext{
			From:   domain.RuntimeRef{Engine: domain.Postgres, Version: tc.from},
			Plan:   domain.UpgradePlan{PGMeta: &domain.PGClusterMeta{Encoding: "UTF8", LCCollate: "C.UTF-8", LCCtype: "C.UTF-8", LocaleProvider: "c"}},
			Report: func(ph string) { phases = append(phases, ph) },
		}

		log, err := f.srv.StartMajorUpgraded(ctx, f.spec, domain.Login{}, upCtx)
		if err != nil {
			t.Fatalf("%s -> %s: %v", tc.from, tc.to, err)
		}
		if !log.NeedsStats {
			t.Errorf("%s -> %s: expected NeedsStats=true", tc.from, tc.to)
		}
		if !f.sys.called(tc.wantFlag) || !f.sys.called(tc.wantCheck) || f.sys.called("--copy ") {
			t.Errorf("%s -> %s: calls = %v", tc.from, tc.to, f.sys.calls)
		}
		hba, err := os.ReadFile(filepath.Join(f.spec.DataDir, "pg_hba.conf"))
		if err != nil || !strings.Contains(string(hba), "0.0.0.0/0 scram-sha-256") {
			t.Errorf("%s -> %s: pg_hba = %q (%v)", tc.from, tc.to, hba, err)
		}
		if ver, _ := os.ReadFile(filepath.Join(f.spec.DataDir, "PG_VERSION")); strings.TrimSpace(string(ver)) != strings.SplitN(tc.to, ".", 2)[0] {
			t.Errorf("%s -> %s: PG_VERSION = %q", tc.from, tc.to, ver)
		}
		if _, err := os.Stat(filepath.Join(f.spec.DataDir, ".pg_upgrade_new")); !os.IsNotExist(err) {
			t.Errorf("temp dir .pg_upgrade_new left behind")
		}
		if !slices.Contains(phases, "pg_upgrade: swapping") && !slices.Contains(phases, "starting") {
			t.Errorf("phases = %v", phases)
		}
		if len(log.Findings) == 0 {
			t.Errorf("expected pg_upgrade stage findings in UpgradeLog")
		}
	}
}

func TestUpdateMissingStats_版に応じてmissing_stats_onlyを使い分ける(t *testing.T) {
	f19 := newFixture(t)
	f19.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	if err := f19.srv.UpdateMissingStats(ctx, f19.spec); err != nil {
		t.Fatal(err)
	}
	if !f19.sys.called("vacuumdb") || !f19.sys.called("--analyze-in-stages") || !f19.sys.called("--missing-stats-only") {
		t.Errorf("PG19 vacuumdb calls = %v", f19.sys.calls)
	}

	f17 := newFixture(t)
	f17.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "17.12"}
	if err := f17.srv.UpdateMissingStats(ctx, f17.spec); err != nil {
		t.Fatal(err)
	}
	if !f17.sys.called("vacuumdb") || !f17.sys.called("--analyze-in-stages") || f17.sys.called("--missing-stats-only") {
		t.Errorf("PG17 vacuumdb calls = %v", f17.sys.calls)
	}
}

func TestParsePGUpgradeCheck_reg型と拡張不足などの出力を構造化する(t *testing.T) {
	out := `Performing Consistency Checks on Old Live Server
------------------------------------------------
Checking cluster versions                                     ok
Checking data type usage                                      failed check: Checking for reg* data types in user tables

Data type checks failed:
Your installation contains one of the reg* data types in user tables.
`
	fs := postgres.ParsePGUpgradeOutput(out, true)
	if !domain.HasErrors(fs) || fs[0].Code != domain.CodePGCheckError {
		t.Errorf("got %+v", fs)
	}
	if got := postgres.UpgradeTimeout(0, 0); got != 15*time.Minute {
		t.Errorf("base timeout = %s", got)
	}
	if got := postgres.UpgradeTimeout(100_000, 50_000); got <= 15*time.Minute {
		t.Errorf("scaled timeout = %s", got)
	}
	if got := postgres.UpgradeTimeout(100_000_000, 100_000_000); got != 4*time.Hour {
		t.Errorf("capped timeout = %s, want 4h", got)
	}
	warnOut := "WARNING:  new data directory should not be inside the old data directory\n"
	wfs := postgres.ParsePGUpgradeOutput(warnOut, true)
	if len(wfs) != 1 || wfs[0].Level != domain.LevelWarning || wfs[0].Code != domain.CodePGCheckWarning {
		t.Errorf("warning findings = %+v", wfs)
	}
}

type streamFake struct {
	*fakeSystem
	onStream func(dir string, out io.Writer, args []string)
}

func (s *streamFake) StreamDir(ctx context.Context, dir string, out io.Writer, name string, args ...string) error {
	b, err := s.Run(ctx, name, args...)
	if len(b) > 0 {
		_, _ = out.Write(b)
	}
	if s.onStream != nil {
		s.onStream(dir, out, args)
	}
	return err
}

func TestStartMajorUpgraded_エラー処理とストリームとICUロケール(t *testing.T) {
	// 1. Minor upgrade delegates to StartUpgraded (NeedsStats == false).
	f := newFixture(t)
	f.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "18.7"}
	log, err := f.srv.StartMajorUpgraded(ctx, f.spec, domain.Login{}, domain.UpgradeContext{
		From: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"},
	})
	if err != nil || log.NeedsStats {
		t.Fatalf("minor delegation = %+v, %v", log, err)
	}

	// 2. CheckOwner and PG_VERSION checks fail cleanly.
	f2 := newFixture(t)
	f2.srv.CheckOwner = func(string) error { return os.ErrPermission }
	f2.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	if _, err := f2.srv.StartMajorUpgraded(ctx, f2.spec, domain.Login{}, domain.UpgradeContext{From: pg18}); err == nil {
		t.Error("want CheckOwner error")
	}
	f2.srv.CheckOwner = nil
	if _, err := f2.srv.StartMajorUpgraded(ctx, f2.spec, domain.Login{}, domain.UpgradeContext{From: pg18}); err == nil {
		t.Error("want missing PG_VERSION error")
	}
	_ = os.WriteFile(filepath.Join(f2.spec.DataDir, "PG_VERSION"), []byte("17\n"), 0o644)
	if _, err := f2.srv.StartMajorUpgraded(ctx, f2.spec, domain.Login{}, domain.UpgradeContext{From: pg18}); err == nil {
		t.Error("want mismatched PG_VERSION error")
	}

	// 3. StreamDir + pg_upgrade failure preserves excerpt; ICU locale and default locale work.
	f3 := newFixture(t)
	sf := &streamFake{fakeSystem: f3.sys}
	f3.srv.Run = sf
	setupPGMajorRuntimes(t, &f3, "18.6", "19.0", nil)
	_ = os.WriteFile(filepath.Join(f3.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f3.sys.outputs = map[string]string{
		"pg_controldata": "Data page checksum version:           1\n",
	}
	f3.sys.failOn = "pg_upgrade --swap"
	f3.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	upLog, err := f3.srv.StartMajorUpgraded(ctx, f3.spec, domain.Login{}, domain.UpgradeContext{
		From: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"},
		Plan: domain.UpgradePlan{PGMeta: &domain.PGClusterMeta{Encoding: "UTF8", LocaleProvider: "i", Locale: "en-US-x-icu"}},
	})
	if err == nil || upLog.Excerpt == "" || !f3.sys.called("--locale-provider=icu") {
		t.Fatalf("expected pg_upgrade failure with ICU and excerpt: log=%+v err=%v calls=%v", upLog, err, f3.sys.calls)
	}

	// 4. Default locale fallback when PGMeta is nil and checkMajorUpgrade errors.
	f4 := newFixture(t)
	setupPGMajorRuntimes(t, &f4, "18.6", "19.0", nil)
	_ = os.WriteFile(filepath.Join(f4.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f4.sys.failOn = "pg_controldata"
	if _, err := f4.srv.CheckUpgrade(ctx, f4.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}, domain.Login{}); err == nil {
		t.Error("want pg_controldata error")
	}
	f4.sys.failOn = "template0"
	f4.sys.outputs = map[string]string{"pg_controldata": "Data page checksum version: 1\n"}
	if _, err := f4.srv.CheckUpgrade(ctx, f4.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}, domain.Login{}); err == nil {
		t.Error("want template0 query error")
	}

	// 5. StreamDir success covers lineWriter streaming, default locale, update_extensions.sql, and existing hbaRemote.
	f5 := newFixture(t)
	sf5 := &streamFake{fakeSystem: f5.sys}
	f5.srv.Run = sf5
	setupPGMajorRuntimes(t, &f5, "18.6", "19.0", map[string]string{"commented": "1.0"})
	ctrl := filepath.Join(f5.rt, "19.0/usr/share/postgresql/19/extension/commented.control")
	_ = os.WriteFile(ctrl, []byte("# comment\ncomment = 'x'\n"), 0o644)
	_ = os.WriteFile(filepath.Join(f5.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f5.sys.outputs = map[string]string{
		"pg_controldata": "no checksum line\n",
		"pg_upgrade ": "Performing Consistency Checks\nCreating dump of database schemas\n" +
			"Restoring database schemas in the new cluster\nSwapping data directories ok\nSync data directory to disk ok\nUpgrade Complete",
	}
	f5.sys.initdb = func(dir string) {
		_ = os.WriteFile(filepath.Join(dir, "pg_hba.conf"), []byte("host all all 0.0.0.0/0 scram-sha-256\n"), 0o600)
	}
	sf5.onStream = func(dir string, out io.Writer, args []string) {
		if slices.Contains(args, "--swap") {
			_ = os.WriteFile(filepath.Join(dir, "update_extensions.sql"), []byte("ALTER EXTENSION x UPDATE;\n"), 0o644)
			_, _ = out.Write([]byte("partial line without newline"))
		}
	}
	f5.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	upLog5, err := f5.srv.StartMajorUpgraded(ctx, f5.spec, domain.Login{}, domain.UpgradeContext{
		From: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"},
		Plan: domain.UpgradePlan{PGMeta: &domain.PGClusterMeta{Encoding: "UTF8"}},
	})
	if err != nil || !upLog5.NeedsStats || !f5.sys.called("--locale=C.UTF-8") {
		t.Fatalf("f5 = %+v, %v", upLog5, err)
	}
	foundExtUpdate := false
	for _, fd := range upLog5.Findings {
		if fd.Code == domain.CodeExtensionUpdate {
			foundExtUpdate = true
		}
	}
	if !foundExtUpdate {
		t.Errorf("expected CodeExtensionUpdate from update_extensions.sql: %+v", upLog5.Findings)
	}

	// 6. CheckUpgrade reads pg_upgrade_output.d/*.txt on check failure and handles initdb failure.
	f6 := newFixture(t)
	sf6 := &streamFake{fakeSystem: f6.sys}
	f6.srv.Run = sf6
	setupPGMajorRuntimes(t, &f6, "18.6", "19.0", map[string]string{"bad": "1.0"})
	_ = os.WriteFile(filepath.Join(f6.rt, "19.0/usr/share/postgresql/19/extension/bad.control"), []byte("# only comment\n"), 0o644)
	_ = os.WriteFile(filepath.Join(f6.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f6.sys.outputs = map[string]string{
		"pg_controldata":      "Data page checksum version: 1\n",
		"pg_encoding_to_char": "UTF8||\n",
		"NOT datistemplate":   "postgres\n",
		"pg_extension":        "bad|1.0\n",
	}
	sf6.onStream = func(_ string, _ io.Writer, args []string) {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-D" && strings.Contains(args[i+1], "pgcheck") {
				outDir := filepath.Join(args[i+1], "pg_upgrade_output.d", "ts")
				_ = os.MkdirAll(outDir, 0o755)
				_ = os.WriteFile(filepath.Join(outDir, "tables_using_reg.txt"), []byte(strings.Repeat("a", 1100)), 0o644)
			}
		}
	}
	f6.sys.failOn = "pg_upgrade --check"
	plan6, err := f6.srv.CheckUpgrade(ctx, f6.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}, domain.Login{})
	if err != nil || !domain.HasErrors(plan6.Findings) {
		t.Fatalf("f6 plan = %+v, err = %v", plan6, err)
	}
	f6.sys.failOn = "initdb"
	if _, err := f6.srv.CheckUpgrade(ctx, f6.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}, domain.Login{}); err == nil {
		t.Error("want initdb error")
	}

	// 7. Error paths in StartMajorUpgraded (pg_controldata, Chown, initdb) and inspectCluster.
	f7 := newFixture(t)
	setupPGMajorRuntimes(t, &f7, "18.6", "19.0", nil)
	_ = os.WriteFile(filepath.Join(f7.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f7.sys.failOn = "pg_controldata"
	f7.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	if _, err := f7.srv.StartMajorUpgraded(ctx, f7.spec, domain.Login{}, domain.UpgradeContext{From: pg18}); err == nil {
		t.Error("want pg_controldata error")
	}
	f7.sys.failOn = ""
	f7.sys.outputs = map[string]string{"pg_controldata": "Data page checksum version: 1\n"}
	f7.srv.Chown = func(string) error { return os.ErrPermission }
	if _, err := f7.srv.StartMajorUpgraded(ctx, f7.spec, domain.Login{}, domain.UpgradeContext{From: pg18}); err == nil {
		t.Error("want Chown error in stageUpgradeDirs")
	}
	f7.srv.Chown = nil
	f7.sys.failOn = "initdb"
	if _, err := f7.srv.StartMajorUpgraded(ctx, f7.spec, domain.Login{}, domain.UpgradeContext{
		From: pg18, Plan: domain.UpgradePlan{PGMeta: &domain.PGClusterMeta{}},
	}); err == nil {
		t.Error("want initdb error in StartMajorUpgraded")
	}
	f7.sys.failOn = "NOT datistemplate"
	if _, err := f7.srv.CheckUpgrade(ctx, f7.spec, domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}, domain.Login{}); err == nil {
		t.Error("want error when initdb also fails or databases fail")
	}
	// Trigger Chown error on final promote step and pg_upgrade failure with >4096 bytes output.
	f8 := newFixture(t)
	setupPGMajorRuntimes(t, &f8, "18.6", "19.0", nil)
	_ = os.WriteFile(filepath.Join(f8.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f8.sys.outputs = map[string]string{
		"pg_controldata": "Data page checksum version: 1\n",
		"pg_upgrade ":    strings.Repeat("x", 5000),
	}
	f8.sys.initdb = func(dir string) {
		_ = os.WriteFile(filepath.Join(dir, "pg_hba.conf"), []byte("local all all trust\n"), 0o600)
	}
	chownCalls := 0
	f8.srv.Chown = func(string) error {
		chownCalls++
		if chownCalls >= 4 {
			return os.ErrPermission
		}
		return nil
	}
	f8.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	if _, err := f8.srv.StartMajorUpgraded(ctx, f8.spec, domain.Login{}, domain.UpgradeContext{From: pg18}); err == nil {
		t.Error("want final chown error")
	}
	f9 := newFixture(t)
	setupPGMajorRuntimes(t, &f9, "18.6", "19.0", nil)
	_ = os.WriteFile(filepath.Join(f9.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f9.sys.outputs = map[string]string{"pg_controldata": "Data page checksum version: 1\n"}
	f9.sys.failOn = "pg_upgrade --swap"
	f9.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	upLog9, err := f9.srv.StartMajorUpgraded(ctx, f9.spec, domain.Login{}, domain.UpgradeContext{From: pg18})
	if err == nil || upLog9.Excerpt == "" {
		t.Errorf("expected excerpt on pg_upgrade failure, got %q err=%v", upLog9.Excerpt, err)
	}
	f9b := newFixture(t)
	sf9b := &streamFake{fakeSystem: f9b.sys, onStream: func(_ string, out io.Writer, _ []string) {
		_, _ = out.Write([]byte(strings.Repeat("x", 5000)))
	}}
	f9b.srv.Run = sf9b
	setupPGMajorRuntimes(t, &f9b, "18.6", "19.0", nil)
	_ = os.WriteFile(filepath.Join(f9b.spec.DataDir, "PG_VERSION"), []byte("18\n"), 0o644)
	f9b.sys.outputs = map[string]string{"pg_controldata": "Data page checksum version: 1\n"}
	f9b.sys.failOn = "initdb"
	f9b.spec.Runtime = domain.RuntimeRef{Engine: domain.Postgres, Version: "19.0"}
	upLog9b, err := f9b.srv.StartMajorUpgraded(ctx, f9b.spec, domain.Login{}, domain.UpgradeContext{From: pg18})
	if err == nil || len(upLog9b.Excerpt) != 4096 {
		t.Errorf("expected 4096-byte truncated excerpt, got len=%d err=%v", len(upLog9b.Excerpt), err)
	}
}
