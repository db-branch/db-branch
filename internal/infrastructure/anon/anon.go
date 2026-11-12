// Package anon is the anon masker module: masked copies of PostgreSQL bases
// with PostgreSQL Anonymizer (https://labs.dalibo.com/postgresql_anonymizer)
// static masking. The database is copied into the target first and
// anonymized there in place; the old row versions are then vacuumed away.
package anon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/db-branch/db-branch/pkg/ext"
)

// Module is the anon masker.
type Module struct {
	Run func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

	mu        sync.Mutex
	pkg, work string
}

func ja(label, help string) map[string]ext.SettingText {
	return map[string]ext.SettingText{"ja": {Label: label, Help: help}}
}

// Settings are shown under the module in the console.
func (m *Module) Settings() []ext.Setting {
	return []ext.Setting{
		{Key: "package", Label: "Anonymizer package", Type: "string",
			Help:         "A postgresql_anonymizer_<major> .deb from Dalibo (repo.dalibo.com) on each node. Unpacked into the PostgreSQL runtime when the extension is missing. Empty: the extension must already be there.",
			Translations: ja("Anonymizer のパッケージ", "Dalibo（repo.dalibo.com）の postgresql_anonymizer_<メジャー版> の .deb を各ノードに置いて指定します。拡張がランタイムに無いとき、ここから展開します。空なら、拡張がすでに入っている必要があります。")},
		{Key: "work_dir", Label: "Work directory", Type: "string", Default: "/var/lib/db-branch/anon",
			Help:         "Where the source dump is kept during a run (unmasked; removed after it).",
			Translations: ja("作業ディレクトリ", "実行中、元のダンプを置く場所です（マスク前のデータです。終われば消します）。")},
	}
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// Configure takes the settings.
func (m *Module) Configure(_ context.Context, v map[string]string) error {
	pkg, work := v["package"], or(v["work_dir"], "/var/lib/db-branch/anon")
	var errs []error
	if pkg != "" && !path.IsAbs(pkg) {
		errs = append(errs, fmt.Errorf("package %q: an absolute path", pkg))
	}
	if !path.IsAbs(work) {
		errs = append(errs, fmt.Errorf("work_dir %q: an absolute path", work))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	m.mu.Lock()
	m.pkg, m.work = pkg, work
	m.mu.Unlock()
	return nil
}

// WorkDir is where runs keep their files.
func (m *Module) WorkDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return or(m.work, "/var/lib/db-branch/anon")
}

// Rules list the databases to copy and, for each, the masking rules as
// PostgreSQL Anonymizer security labels.
type Rules struct {
	Databases []struct {
		Name  string `yaml:"name"`
		Rules string `yaml:"rules"`
	} `yaml:"databases"`
}

var dbName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// ParseRules reads and checks rules.
func ParseRules(s string) (Rules, error) {
	var r Rules
	dec := yaml.NewDecoder(strings.NewReader(s))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("anon rules: %w", err)
	}
	if len(r.Databases) == 0 {
		return r, errors.New("anon rules: list the databases to copy under databases: (each with its rules)")
	}
	for _, d := range r.Databases {
		if !dbName.MatchString(d.Name) {
			return r, fmt.Errorf("anon rules: database name %q: letters, digits and _", d.Name)
		}
		if !strings.Contains(strings.ToUpper(d.Rules), "SECURITY LABEL") {
			return r, fmt.Errorf("anon rules of %s: no SECURITY LABEL FOR anon ... statements; a database without masking needs no anonymizer", d.Name)
		}
	}
	return r, nil
}

// clearLabels removes the labels of the anon provider that dropping the
// extension leaves behind (it labels catalog objects too).
const clearLabels = "DELETE FROM pg_seclabel WHERE provider = 'anon';\n"

// ensureExtension unpacks the package into the runtime when it lacks the
// extension.
func (m *Module) ensureExtension(ctx context.Context, a ext.DBAccess, do func(string, ...string) (string, error)) error {
	root := filepath.Dir(a.BinDir)
	have := func() bool {
		found, _ := filepath.Glob(filepath.Join(root, "usr/share/postgresql/*/extension/anon.control"))
		return len(found) > 0
	}
	if have() {
		return nil
	}
	m.mu.Lock()
	pkg := m.pkg
	m.mu.Unlock()
	if pkg == "" {
		return errors.New("the PostgreSQL runtime has no anon extension: put Dalibo's postgresql_anonymizer_<major> .deb on the node and set it as the anon module's package")
	}
	if _, err := do("dpkg-deb", "-x", pkg, root); err != nil {
		return err
	}
	if !have() {
		return fmt.Errorf("%s did not contain the anon extension for this PostgreSQL", pkg)
	}
	return nil
}

// Mask copies each database into the target and anonymizes it there.
func (m *Module) Mask(ctx context.Context, job ext.MaskJob) (string, error) {
	if job.Source.Engine != "postgres" || job.Target.Engine != "postgres" {
		return "", fmt.Errorf("anon masks PostgreSQL (this is %s)", job.Source.Engine)
	}
	rules, err := ParseRules(job.Rules)
	if err != nil {
		return "", err
	}
	var log bytes.Buffer
	env := []string{"LD_LIBRARY_PATH=" + job.Target.LibDir}
	do := func(name string, args ...string) (string, error) {
		out, err := m.Run(ctx, env, name, args...)
		log.Write(out)
		if err != nil {
			return "", fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if err := m.ensureExtension(ctx, job.Target, do); err != nil {
		return log.String(), err
	}
	if err := os.MkdirAll(m.WorkDir(), 0o700); err != nil {
		return log.String(), err
	}
	run, err := os.MkdirTemp(m.WorkDir(), "run-")
	if err != nil {
		return log.String(), err
	}
	defer os.RemoveAll(run)
	conn := func(a ext.DBAccess) []string {
		return []string{"-h", a.Socket, "-p", strconv.Itoa(a.Port), "-U", a.User}
	}
	tool := func(a ext.DBAccess, name string) string { return filepath.Join(a.BinDir, name) }
	psql := func(db string, args ...string) (string, error) {
		return do(tool(job.Target, "psql"), append(append(conn(job.Target), "-d", db, "-X", "-q", "-tA", "-v", "ON_ERROR_STOP=1"), args...)...)
	}
	for _, d := range rules.Databases {
		exists, err := psql("postgres", "-c", fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname = '%s'", d.Name))
		if err != nil {
			return log.String(), err
		}
		if exists != "1" {
			if _, err := psql("postgres", "-c", fmt.Sprintf(`CREATE DATABASE "%s"`, d.Name)); err != nil {
				return log.String(), err
			}
		}
		dump := filepath.Join(run, d.Name+".dump")
		if _, err := do(tool(job.Source, "pg_dump"), append(conn(job.Source), "-d", d.Name, "-Fc", "-f", dump)...); err != nil {
			return log.String(), err
		}
		if _, err := do(tool(job.Target, "pg_restore"), append(conn(job.Target), "-d", d.Name, "--clean", "--if-exists", "--exit-on-error", "--no-owner", dump)...); err != nil {
			return log.String(), err
		}
		_ = os.Remove(dump) // unmasked data: gone as soon as it is in the target
		script := filepath.Join(run, d.Name+".sql")
		// The local admin (a superuser) does static masking only, in this
		// session: anon.nosuperuser guards dynamic masking.
		sql := "LOAD 'anon';\nSET anon.nosuperuser = off;\nCREATE EXTENSION IF NOT EXISTS anon CASCADE;\nSELECT anon.init();\n" + d.Rules +
			"\nSELECT anon.anonymize_database();\nSELECT anon.remove_masks_for_all_columns();\nDROP EXTENSION anon CASCADE;\n" + clearLabels +
			"-- Old row versions still hold the original values: rewrite every table.\nVACUUM FULL;\n"
		if err := os.WriteFile(script, []byte(sql), 0o600); err != nil {
			return log.String(), err
		}
		if _, err := psql(d.Name, "-f", script); err != nil {
			return log.String(), err
		}
	}
	return log.String(), nil
}
