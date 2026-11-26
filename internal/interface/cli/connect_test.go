package cli_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/interface/cli"
)

func setupConnectAPI(engine, dsn string) *fakeAPI {
	api := newFakeAPI()
	api.answers["GET /v1/branches/dev"] = map[string]any{
		"name": "dev", "status": "ready", "base": "prod", "runtime": engine + "@8.4.13",
		"connection": map[string]any{"host": "10.0.0.95", "port": 40001, "user": "branch"},
		"proxy":      map[string]any{"host": "10.0.0.95", "port": 6033, "user": "branch_dev"},
	}
	api.answers["GET /v1/branches/dev/dsn"] = map[string]any{"dsn": dsn}
	api.answers["GET /v1/branches/dev/dsn?via=proxy"] = map[string]any{"dsn": strings.Replace(dsn, ":40001/", ":6033/", 1)}
	return api
}

func runConnectCLI(t *testing.T, api *fakeAPI, execFn func(string, []string, []string, io.Reader, io.Writer, io.Writer) (int, error), args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := cli.App{
		Version:    "1.0.0",
		ClientOnly: true,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		Getenv: func(k string) string {
			return map[string]string{"DB_BRANCH_URL": "https://10.0.0.95:7080", "DB_BRANCH_TOKEN": "dbb_tok"}[k]
		},
		RemoteClient: func(cli.RemoteOptions) (cli.Client, error) { return api, nil },
		ExecCommand:  execFn,
	}
	code := app.Run(context.Background(), args)
	return code, out.String(), errOut.String()
}

func TestConnect_接続情報をマスク表示とRevealとJSONで出力する(t *testing.T) {
	api := setupConnectAPI("mysql", "mysql://u1:s3cr3t!@10.0.0.95:40001/shop")

	// Default table masks password.
	code, out, errOut := runConnectCLI(t, api, nil, "connect", "dev")
	if code != 0 {
		t.Fatalf("connect exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "s3cr3t!") || !strings.Contains(out, "10.0.0.95:40001") || !strings.Contains(out, "ProxySQL:") {
		t.Errorf("masked output unexpected: %q", out)
	}

	// --reveal shows password and --proxy adds --proxy to Command hint.
	code, out, _ = runConnectCLI(t, api, nil, "connect", "dev", "--reveal", "--proxy")
	if code != 0 || !strings.Contains(out, "s3cr3t!") || !strings.Contains(out, "6033") || !strings.Contains(out, "--proxy --exec mysql") {
		t.Errorf("reveal proxy output = (%d, %q)", code, out)
	}

	// -o json outputs structured fields.
	code, out, _ = runConnectCLI(t, api, nil, "connect", "dev", "-o", "json")
	if code != 0 || !strings.Contains(out, `"password": "s3cr3t!"`) || !strings.Contains(out, `"database": "shop"`) {
		t.Errorf("json output = (%d, %q)", code, out)
	}

	// Non-ready branch fails.
	api.answers["GET /v1/branches/paused"] = map[string]any{"name": "paused", "status": "paused", "runtime": "mysql@8.4.13"}
	if code, _, errOut := runConnectCLI(t, api, nil, "connect", "paused"); code == 0 || !strings.Contains(errOut, "paused") {
		t.Errorf("paused connect = (%d, %q)", code, errOut)
	}
}

func TestConnectExec_一時ファイル経由でmysqlとpsqlを起動し終了後に削除する(t *testing.T) {
	// MySQL with special chars in password, empty database, and MYSQL_PWD stripped.
	t.Setenv("MYSQL_PWD", "stale-secret")
	apiMy := setupConnectAPI("mysql", `mysql://u1:p%22w%5Cx@10.0.0.95:40001/`)
	var capturedBin string
	var capturedArgs []string
	var capturedCNF string
	var tmpPath string

	code, _, errOut := runConnectCLI(t, apiMy, func(name string, args []string, extraEnv []string, _ io.Reader, _, _ io.Writer) (int, error) {
		capturedBin = name
		capturedArgs = append([]string{}, args...)
		for _, e := range extraEnv {
			if strings.HasPrefix(e, "MYSQL_PWD=") {
				t.Errorf("MYSQL_PWD must be stripped from mysql env: %s", e)
			}
		}
		for _, a := range args {
			if strings.HasPrefix(a, "--defaults-extra-file=") {
				tmpPath = strings.TrimPrefix(a, "--defaults-extra-file=")
				b, err := os.ReadFile(tmpPath)
				if err != nil {
					t.Fatalf("read defaults-extra-file: %v", err)
				}
				info, _ := os.Stat(tmpPath)
				if info.Mode().Perm() != 0o600 {
					t.Errorf("my.cnf perm = %o, want 0600", info.Mode().Perm())
				}
				capturedCNF = string(b)
			}
		}
		return 0, nil
	}, "connect", "dev", "--exec", "auto", "--", "-e", "SELECT 1")
	if code != 0 {
		t.Fatalf("mysql exec exit %d: %s", code, errOut)
	}
	if capturedBin != "mysql" {
		t.Errorf("bin = %q, want mysql", capturedBin)
	}
	if !strings.Contains(capturedCNF, `password="p\"w\\x"`) {
		t.Errorf("escaped password missing in %q", capturedCNF)
	}
	// Empty database must NOT pass an empty "" arg to mysql.
	for _, a := range capturedArgs {
		if a == "" {
			t.Errorf("empty arg passed to mysql: %v", capturedArgs)
		}
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp defaults-extra-file should be removed after exit")
	}

	// PostgreSQL PGPASSFILE escaping & child exit code propagation.
	apiPG := setupConnectAPI("postgres", `postgres://pguser:p%3Aw%5Cx@10.0.0.95:40001/mydb`)
	var capturedPGPass string
	var pgPassPath string
	code, _, _ = runConnectCLI(t, apiPG, func(name string, args []string, extraEnv []string, _ io.Reader, _, _ io.Writer) (int, error) {
		capturedBin = name
		capturedArgs = args
		for _, e := range extraEnv {
			if strings.HasPrefix(e, "PGPASSFILE=") {
				pgPassPath = strings.TrimPrefix(e, "PGPASSFILE=")
				b, err := os.ReadFile(pgPassPath)
				if err != nil {
					t.Fatal(err)
				}
				capturedPGPass = string(b)
			}
		}
		return 3, errors.New("exit status 3")
	}, "connect", "dev", "--exec", "psql")
	if code != 3 || capturedBin != "psql" {
		t.Errorf("psql code=%d bin=%q", code, capturedBin)
	}
	if capturedPGPass != `10.0.0.95:40001:mydb:pguser:p\:w\\x`+"\n" {
		t.Errorf("PGPASSFILE = %q", capturedPGPass)
	}
	if _, err := os.Stat(pgPassPath); !os.IsNotExist(err) {
		t.Errorf("PGPASSFILE should be removed after exit")
	}

	// Mismatched client / invalid flags.
	if c, _, _ := runConnectCLI(t, apiPG, nil, "connect", "dev", "--exec", "mysql"); c == 0 {
		t.Error("mysql on postgres branch should fail")
	}
	if c, _, _ := runConnectCLI(t, apiPG, nil, "connect", "dev", "--exec", "psql", "--format", "pgadmin"); c == 0 {
		t.Error("--exec + --format together should fail")
	}
}

func TestConnectFormat_F11互換クライアント接続ファイルを生成する(t *testing.T) {
	apiMy := setupConnectAPI("mysql", "mysql://u1:pw123@10.0.0.95:40001/shop")
	dir := t.TempDir()

	// 1. DBeaver .dbp with --include-password -> valid ZIP + AES-128-CBC decryptable credentials-config.json
	dbpPath := filepath.Join(dir, "dev.dbp")
	code, _, errOut := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "dbeaver", "--include-password", "--out", dbpPath)
	if code != 0 || !strings.Contains(errOut, "Warning:") {
		t.Fatalf("dbeaver format = (%d, %q)", code, errOut)
	}
	rawZip, err := os.ReadFile(dbpPath)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(rawZip), int64(len(rawZip)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		files[f.Name], _ = io.ReadAll(rc)
		_ = rc.Close()
	}
	ds := string(files["projects/db-branch-dev/.dbeaver/data-sources.json"])
	if !strings.Contains(ds, `"save-password": true`) || strings.Contains(ds, "pw123") {
		t.Errorf("data-sources.json unexpected: %s", ds)
	}
	enc := files["projects/db-branch-dev/.dbeaver/credentials-config.json"]
	key := []byte{0xba, 0xbb, 0x4a, 0x9f, 0x77, 0x4a, 0xb8, 0x53, 0xc9, 0x6c, 0x2d, 0x65, 0x3d, 0xfe, 0x54, 0x4a}
	blk, _ := aes.NewCipher(key)
	dec := make([]byte, len(enc)-16)
	cipher.NewCBCDecrypter(blk, enc[:16]).CryptBlocks(dec, enc[16:])
	pad := int(dec[len(dec)-1])
	var creds map[string]map[string]map[string]string
	if err := json.Unmarshal(dec[:len(dec)-pad], &creds); err != nil {
		t.Fatalf("decrypt creds = %v", err)
	}
	if creds["db-branch-dev"]["#connection"]["password"] != "pw123" {
		t.Errorf("decrypted creds = %+v", creds)
	}

	// Refuse overwrite without --force, allow with --force.
	if c, _, _ := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "dbeaver", "--out", dbpPath); c == 0 {
		t.Error("overwrite without --force should fail")
	}
	if c, _, eOut := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "dbeaver", "--out", dbpPath, "--force"); c != 0 {
		t.Errorf("overwrite with --force failed: %s", eOut)
	}
	// Binary .dbp refuses --out -
	if c, _, _ := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "dbeaver", "--out", "-"); c == 0 {
		t.Error(".dbp to stdout should fail")
	}

	// 2. DataGrip XML to --out - keeps stdout pure XML and prints JDBC URL to stderr.
	code, out, errOut := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "datagrip", "--out", "-")
	if code != 0 || !strings.HasPrefix(out, "<?xml") || !strings.Contains(errOut, "jdbc:mysql://10.0.0.95:40001/shop") {
		t.Errorf("datagrip stdout/stderr = (%d, %q, %q)", code, out, errOut)
	}

	// 3. TablePlus without --out writes URL to stdout (masked by default unless --include-password).
	code, out, _ = runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "tableplus")
	if code != 0 || strings.Contains(out, "pw123") || !strings.Contains(out, "mysql://u1@10.0.0.95:40001/shop") {
		t.Errorf("tableplus without password = (%d, %q)", code, out)
	}
	code, out, _ = runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "tableplus", "--include-password")
	if code != 0 || !strings.Contains(out, "mysql://u1:pw123@10.0.0.95:40001/shop") {
		t.Errorf("tableplus with password = (%d, %q)", code, out)
	}

	// 4. Chat2DB, Navicat, Workbench, pgAdmin (postgres).
	for _, f := range []struct{ fmt, outPath, want string }{
		{"chat2db", filepath.Join(dir, "c.json"), `"type": "MYSQL"`},
		{"navicat", filepath.Join(dir, "n.ncx"), `SSH="false" SavePassword="false"`},
		{"workbench", filepath.Join(dir, "w.xml"), `db.mgmt.Connection`},
	} {
		if c, _, eOut := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", f.fmt, "--out", f.outPath); c != 0 {
			t.Fatalf("%s failed: %s", f.fmt, eOut)
		}
		b, _ := os.ReadFile(f.outPath)
		if !strings.Contains(string(b), f.want) {
			t.Errorf("%s content missing %q: %s", f.fmt, f.want, b)
		}
	}

	apiPG := setupConnectAPI("postgres", "postgres://u1:pw@10.0.0.95:40001/app")
	pgOut := filepath.Join(dir, "pg.json")
	if c, _, eOut := runConnectCLI(t, apiPG, nil, "connect", "dev", "--format", "pgadmin", "--out", pgOut); c != 0 {
		t.Fatalf("pgadmin failed: %s", eOut)
	}
	b, _ := os.ReadFile(pgOut)
	if !strings.Contains(string(b), `"MaintenanceDB": "app"`) {
		t.Errorf("pgadmin = %s", b)
	}
	// Unsupported engine for pgadmin / workbench fails.
	if c, _, _ := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "pgadmin", "--out", filepath.Join(dir, "x.json")); c == 0 {
		t.Error("pgadmin on mysql should fail")
	}

	// Proxy mode, default --out filename in working dir, datagrip --include-password, and engine/format edge cases.
	t.Chdir(dir)
	if c, _, eOut := runConnectCLI(t, apiMy, nil, "connect", "dev", "--proxy", "--format", "datagrip", "--include-password"); c != 0 || !strings.Contains(eOut, "password=pw123") {
		t.Errorf("datagrip --proxy --include-password = (%d, %q)", c, eOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "dataSources-dev-proxysql.xml")); err != nil {
		t.Errorf("expected default output file dataSources-dev-proxysql.xml: %v", err)
	}

	apiPGSQL := setupConnectAPI("postgresql", "postgresql://u1:pw@10.0.0.95:40001/")
	if c, out, eOut := runConnectCLI(t, apiPGSQL, nil, "connect", "alice/dev"); c != 0 || !strings.Contains(out, "postgres") {
		t.Errorf("postgresql alice/dev = (%d, %q, %q)", c, out, eOut)
	}
	if c, out, _ := runConnectCLI(t, apiPGSQL, nil, "connect", "dev", "--format", "datagrip", "--out", "-"); c != 0 || !strings.Contains(out, "jdbc:postgresql://10.0.0.95:40001/") {
		t.Errorf("postgres datagrip = (%d, %q)", c, out)
	}
	if c, _, _ := runConnectCLI(t, apiMy, nil, "connect", "dev", "--exec", "psql"); c == 0 {
		t.Error("psql on mysql should fail")
	}
	if c, _, _ := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "datagrip", "--out", filepath.Join(dir, "missing-dir", "f.xml")); c == 0 {
		t.Error("missing parent dir without --force should fail")
	}
	if c, _, _ := runConnectCLI(t, apiMy, nil, "connect", "dev", "--format", "datagrip", "--out", dir, "--force"); c == 0 {
		t.Error("writing over directory with --force should fail")
	}
	if c, _, eOut := runConnectCLI(t, apiPGSQL, func(name string, args []string, extraEnv []string, _ io.Reader, _, _ io.Writer) (int, error) {
		if name != "psql" {
			t.Errorf("expected psql, got %q", name)
		}
		for i, a := range args {
			if a == "-d" {
				t.Errorf("empty database must omit -d flag: %v", args[i:])
			}
		}
		for _, e := range extraEnv {
			if strings.HasPrefix(e, "PGPASSFILE=") {
				b, _ := os.ReadFile(strings.TrimPrefix(e, "PGPASSFILE="))
				if !strings.Contains(string(b), ":40001:*:u1:pw") {
					t.Errorf("empty database PGPASSFILE should use *: %q", string(b))
				}
			}
		}
		return 0, nil
	}, "connect", "dev", "--exec", "auto"); c != 0 {
		t.Errorf("psql --exec auto failed: %s", eOut)
	}

	// DBeaver without --include-password + mariadb + empty db JDBC + TablePlus %20 + dangling symlink rejection.
	apiMaria := setupConnectAPI("mariadb", "mariadb://user%201:pw%202@10.0.0.95:40001/")
	dbpNoPW := filepath.Join(dir, "maria.dbp")
	if c, _, eOut := runConnectCLI(t, apiMaria, nil, "connect", "dev", "--format", "dbeaver", "--out", dbpNoPW); c != 0 {
		t.Fatalf("mariadb dbeaver without pw failed: %s", eOut)
	}
	if c, out, _ := runConnectCLI(t, apiMaria, nil, "connect", "dev", "--format", "datagrip", "--out", "-"); c != 0 || !strings.Contains(out, "jdbc:mariadb://10.0.0.95:40001/") {
		t.Errorf("mariadb datagrip = (%d, %q)", c, out)
	}
	if c, out, _ := runConnectCLI(t, apiMaria, nil, "connect", "dev", "--format", "tableplus", "--include-password"); c != 0 || !strings.Contains(out, "mariadb://user%201:pw%202@") {
		t.Errorf("tableplus percent encoding = (%d, %q)", c, out)
	}
	dangling := filepath.Join(dir, "dangling.xml")
	if err := os.Symlink(filepath.Join(dir, "nonexistent-target.xml"), dangling); err == nil {
		if c, _, _ := runConnectCLI(t, apiMaria, nil, "connect", "dev", "--format", "datagrip", "--out", dangling); c == 0 {
			t.Error("dangling symlink without --force must fail")
		}
	}

	apiSQLite := setupConnectAPI("sqlite", "sqlite://u:pw@10.0.0.95:40001/db")
	for _, badArgs := range [][]string{
		{"connect", "bad!name"},
		{"connect", "missing"},
		{"connect", "dev", "--exec", "unknown"},
	} {
		if c, _, _ := runConnectCLI(t, apiMy, nil, badArgs...); c == 0 {
			t.Errorf("expected error for %v", badArgs)
		}
	}
	for _, badArgs := range [][]string{
		{"connect", "dev", "--exec", "auto"},
		{"connect", "dev", "--format", "dbeaver"},
		{"connect", "dev", "--format", "datagrip"},
		{"connect", "dev", "--format", "navicat"},
		{"connect", "dev", "--format", "workbench"},
		{"connect", "dev", "--format", "unknownfmt"},
	} {
		if c, _, _ := runConnectCLI(t, apiSQLite, nil, badArgs...); c == 0 {
			t.Errorf("expected sqlite error for %v", badArgs)
		}
	}
	apiOther := setupConnectAPI("redis", "redis://u:pw@10.0.0.95:40001/0")
	if c, _, _ := runConnectCLI(t, apiOther, nil, "connect", "dev", "--format", "chat2db"); c == 0 {
		t.Error("chat2db on redis should fail")
	}

	// Control character in password rejected by exec, and bad DSN URL rejected.
	apiCtrl := setupConnectAPI("mysql", "mysql://u:pw%0Aline@10.0.0.95:40001/db")
	if c, _, _ := runConnectCLI(t, apiCtrl, nil, "connect", "dev", "--exec", "mysql"); c == 0 {
		t.Error("newline in password should fail --exec")
	}
	apiBadDSN := setupConnectAPI("mysql", "://bad-dsn")
	if c, _, _ := runConnectCLI(t, apiBadDSN, nil, "connect", "dev"); c == 0 {
		t.Error("invalid DSN should fail")
	}
	apiNoDSN := setupConnectAPI("mysql", "mysql://u:p@10.0.0.95:40001/db")
	delete(apiNoDSN.answers, "GET /v1/branches/dev/dsn")
	if c, _, _ := runConnectCLI(t, apiNoDSN, nil, "connect", "dev"); c == 0 {
		t.Error("missing DSN response should fail")
	}
	// Missing auth/client config in connect fails cleanly.
	var outBuf, errBuf bytes.Buffer
	unconf := cli.App{ClientOnly: true, Stdout: &outBuf, Stderr: &errBuf}
	if c := unconf.Run(context.Background(), []string{"connect", "dev"}); c == 0 {
		t.Error("unconfigured connect should fail")
	}
}
