// Package greenmask is the greenmask masker module: it writes masked copies
// of PostgreSQL bases with Greenmask (https://greenmask.io, installed
// separately), which transforms the data while it dumps it, so only masked
// data is ever written to disk.
package greenmask

import (
	"bytes"
	"context"
	"encoding/json"
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

// Module is the greenmask masker.
type Module struct {
	// Run runs a command with extra environment and returns its output.
	Run func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

	mu         sync.Mutex
	path, work string
	jobs       int
}

func ja(label, help string) map[string]ext.SettingText {
	return map[string]ext.SettingText{"ja": {Label: label, Help: help}}
}

// Settings are shown under the module in the console.
func (m *Module) Settings() []ext.Setting {
	return []ext.Setting{
		{Key: "greenmask_path", Label: "Greenmask binary", Type: "string", Default: "/usr/local/bin/greenmask",
			Help:         "Greenmask v0.2 (for PostgreSQL), installed on every node from its official releases.",
			Translations: ja("Greenmask のパス", "PostgreSQL 用の Greenmask v0.2 です。公式のリリースから各ノードに入れてください。")},
		{Key: "work_dir", Label: "Work directory", Type: "string", Default: "/var/lib/db-branch/greenmask",
			Help:         "Where the masked dump is kept during a run (removed after it).",
			Translations: ja("作業ディレクトリ", "実行中、マスク済みのダンプを置く場所です（終われば消します）。")},
		{Key: "jobs", Label: "Restore jobs", Type: "int", Default: "2",
			Help:         "Parallel jobs when restoring into the target base.",
			Translations: ja("復元の並列数", "マスク先のベースへ戻すときの並列数です。")},
	}
}

// Configure takes the settings.
func (m *Module) Configure(_ context.Context, v map[string]string) error {
	p, work, jobs := or(v["greenmask_path"], "/usr/local/bin/greenmask"), or(v["work_dir"], "/var/lib/db-branch/greenmask"), 2
	var errs []error
	if !path.IsAbs(p) {
		errs = append(errs, fmt.Errorf("greenmask_path %q: an absolute path", p))
	}
	if !path.IsAbs(work) {
		errs = append(errs, fmt.Errorf("work_dir %q: an absolute path", work))
	}
	if s := v["jobs"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("jobs %q: 1 or more", s))
		}
		jobs = n
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	m.mu.Lock()
	m.path, m.work, m.jobs = p, work, jobs
	m.mu.Unlock()
	return nil
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// WorkDir is where runs keep their files.
func (m *Module) WorkDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return or(m.work, "/var/lib/db-branch/greenmask")
}

// Rules say which databases to copy and how to transform their tables
// (Greenmask's dump.transformation, as is).
type Rules struct {
	Databases []struct {
		Name           string `yaml:"name"`
		Transformation []any  `yaml:"transformation"`
	} `yaml:"databases"`
}

// ansi matches terminal colour codes in Greenmask's log.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

var dbName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// ParseRules reads and checks rules.
func ParseRules(s string) (Rules, error) {
	var r Rules
	dec := yaml.NewDecoder(strings.NewReader(s))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("greenmask rules: %w", err)
	}
	if len(r.Databases) == 0 {
		return r, errors.New("greenmask rules: list the databases to copy under databases: (each with its transformation)")
	}
	for _, d := range r.Databases {
		if !dbName.MatchString(d.Name) {
			return r, fmt.Errorf("greenmask rules: database name %q: letters, digits and _", d.Name)
		}
	}
	return r, nil
}

func conninfo(a ext.DBAccess, db string) string {
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s", a.Socket, a.Port, a.User, db)
}

// Mask copies each database in the rules from the source into the target,
// transformed.
func (m *Module) Mask(ctx context.Context, job ext.MaskJob) (string, error) {
	if job.Source.Engine != "postgres" || job.Target.Engine != "postgres" {
		return "", fmt.Errorf("greenmask masks PostgreSQL (this is %s); for MySQL use the greenmask-mysql masker", job.Source.Engine)
	}
	rules, err := ParseRules(job.Rules)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	bin, jobs := or(m.path, "/usr/local/bin/greenmask"), max(m.jobs, 1)
	m.mu.Unlock()
	if err := os.MkdirAll(m.WorkDir(), 0o700); err != nil {
		return "", err
	}
	run, err := os.MkdirTemp(m.WorkDir(), "run-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(run)
	env := []string{"LD_LIBRARY_PATH=" + job.Target.LibDir}
	var log bytes.Buffer
	do := func(name string, args ...string) (string, error) {
		raw, err := m.Run(ctx, env, name, args...)
		out := ansi.ReplaceAll(raw, nil)
		log.Write(out)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args[len(args)-1:], " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	psql := func(sql string) (string, error) {
		return do(filepath.Join(job.Target.BinDir, "psql"), "-h", job.Target.Socket, "-p", strconv.Itoa(job.Target.Port), "-U", job.Target.User,
			"-d", "postgres", "-X", "-q", "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql)
	}
	for _, d := range rules.Databases {
		exists, err := psql(fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname = '%s'", d.Name))
		if err != nil {
			return log.String(), err
		}
		if exists != "1" {
			if _, err := psql(fmt.Sprintf(`CREATE DATABASE "%s"`, d.Name)); err != nil {
				return log.String(), err
			}
		}
		dir := filepath.Join(run, d.Name)
		cfg := map[string]any{
			"common":  map[string]any{"pg_bin_path": job.Source.BinDir, "tmp_dir": dir},
			"storage": map[string]any{"type": "directory", "directory": map[string]any{"path": filepath.Join(dir, "storage")}},
			"dump": map[string]any{"pg_dump_options": map[string]any{"dbname": conninfo(job.Source, d.Name)},
				"transformation": d.Transformation},
			"restore": map[string]any{"pg_restore_options": map[string]any{
				"dbname": conninfo(job.Target, d.Name), "jobs": jobs,
				// Replace what an earlier run restored; fail on any error
				// (pg_restore's default is to carry on).
				"clean": true, "if-exists": true, "exit-on-error": true}},
		}
		b, _ := json.Marshal(cfg) // JSON is YAML too; decoded YAML always encodes
		if err := os.MkdirAll(filepath.Join(dir, "storage"), 0o700); err != nil {
			return log.String(), err
		}
		file := filepath.Join(dir, "config.json")
		if err := os.WriteFile(file, b, 0o600); err != nil {
			return log.String(), err
		}
		if _, err := do(bin, "--config", file, "dump"); err != nil {
			return log.String(), err
		}
		if _, err := do(bin, "--config", file, "restore", "latest"); err != nil {
			return log.String(), err
		}
	}
	return log.String(), nil
}
