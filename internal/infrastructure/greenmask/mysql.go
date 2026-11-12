package greenmask

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/db-branch/db-branch/pkg/ext"
)

// MySQL is the greenmask-mysql masker: MySQL through Greenmask v1 (beta),
// which connects over TCP, so each run makes a temporary login on both
// servers and drops it at the end (before the target is snapshotted).
type MySQL struct {
	Run func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
	// Password makes the temporary logins' passwords (letters and digits).
	Password func() string

	mu         sync.Mutex
	path, work string
	jobs       int
}

const maskUser = "dbb_mask"

// RandomPassword is 24 random letters and digits.
func RandomPassword() string {
	const chars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 24)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

// Settings are shown under the module in the console.
func (m *MySQL) Settings() []ext.Setting {
	return []ext.Setting{
		{Key: "greenmask_path", Label: "Greenmask binary", Type: "string", Default: "/usr/local/bin/greenmask-mysql",
			Help:         "Greenmask v1 for MySQL (greenmask-mysql, beta), installed on every node from its official releases.",
			Translations: ja("Greenmask のパス", "MySQL 用の Greenmask v1（greenmask-mysql、ベータ版）です。公式のリリースから各ノードに入れてください。")},
		{Key: "work_dir", Label: "Work directory", Type: "string", Default: "/var/lib/db-branch/greenmask-mysql",
			Help:         "Where the masked dump is kept during a run (removed after it).",
			Translations: ja("作業ディレクトリ", "実行中、マスク済みのダンプを置く場所です（終われば消します）。")},
		{Key: "jobs", Label: "Dump jobs", Type: "int", Default: "2",
			Help:         "Parallel jobs when dumping the source (restores run in order).",
			Translations: ja("ダンプの並列数", "元のベースをダンプするときの並列数です（復元は順番に行います）。")},
	}
}

// Configure takes the settings.
func (m *MySQL) Configure(_ context.Context, v map[string]string) error {
	p, work, jobs := or(v["greenmask_path"], "/usr/local/bin/greenmask-mysql"), or(v["work_dir"], "/var/lib/db-branch/greenmask-mysql"), 2
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

// WorkDir is where runs keep their files.
func (m *MySQL) WorkDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return or(m.work, "/var/lib/db-branch/greenmask-mysql")
}

// Mask copies each database in the rules from the source into the target,
// transformed.
func (m *MySQL) Mask(ctx context.Context, job ext.MaskJob) (string, error) {
	if job.Source.Engine != "mysql" || job.Target.Engine != "mysql" {
		return "", fmt.Errorf("greenmask-mysql masks MySQL (this is %s); for PostgreSQL use the greenmask masker", job.Source.Engine)
	}
	rules, err := ParseRules(job.Rules)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	bin, jobs := or(m.path, "/usr/local/bin/greenmask-mysql"), max(m.jobs, 1)
	m.mu.Unlock()
	password := RandomPassword
	if m.Password != nil {
		password = m.Password
	}
	pw := password()
	var log bytes.Buffer
	env := []string{"LD_LIBRARY_PATH=" + job.Target.LibDir}
	do := func(name string, args ...string) (string, error) {
		raw, err := m.Run(ctx, env, name, args...)
		out := ansi.ReplaceAll(raw, nil)
		log.Write(out)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(name), args[len(args)-1], err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	sql := func(a ext.DBAccess, q string) (string, error) {
		return do(filepath.Join(a.BinDir, "mysql"), "--socket="+a.Socket, "-u"+a.User, "--batch", "-N", "-e", q)
	}
	login := func(a ext.DBAccess) string { return fmt.Sprintf("'%s'@'%s'", maskUser, a.Host) }

	// Clean up whatever was set up, whatever happens next.
	var undo []func() error
	defer func() {
		for i := len(undo) - 1; i >= 0; i-- {
			_ = undo[i]()
		}
	}()
	for _, a := range []ext.DBAccess{job.Source, job.Target} {
		a := a
		undo = append(undo, func() error { _, err := sql(a, "DROP USER IF EXISTS "+login(a)); return err })
		if _, err := sql(a, fmt.Sprintf("DROP USER IF EXISTS %[1]s; CREATE USER %[1]s IDENTIFIED BY '%[2]s'; GRANT ALL ON *.* TO %[1]s", login(a), pw)); err != nil {
			return log.String(), err
		}
	}
	// Greenmask restores with LOAD DATA LOCAL INFILE: allow it meanwhile.
	was, err := sql(job.Target, "SELECT @@GLOBAL.local_infile")
	if err != nil {
		return log.String(), err
	}
	if was != "1" {
		was = "0"
	}
	undo = append(undo, func() error { _, err := sql(job.Target, "SET GLOBAL local_infile = "+was); return err })
	if _, err := sql(job.Target, "SET GLOBAL local_infile = 1"); err != nil {
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
	conn := func(a ext.DBAccess, db string) map[string]any {
		return map[string]any{"user": maskUser, "password": pw, "host": a.Host, "port": a.Port, "connect-database": db}
	}
	for _, d := range rules.Databases {
		if _, err := sql(job.Target, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", d.Name)); err != nil {
			return log.String(), err
		}
		dir := filepath.Join(run, d.Name)
		dumpOpts := conn(job.Source, d.Name)
		dumpOpts["databases"], dumpOpts["add-drop-table"] = []string{d.Name}, true
		cfg := map[string]any{
			"common":  map[string]any{"bin_path": job.Source.BinDir, "tmp_dir": dir},
			"log":     map[string]any{"level": "info", "format": "text"},
			"engine":  "mysql",
			"storage": map[string]any{"type": "directory", "directory": map[string]any{"path": filepath.Join(dir, "storage")}},
			"dump":    map[string]any{"options": map[string]any{"jobs": jobs}, "mysql": map[string]any{"options": dumpOpts}, "transformation": d.Transformation},
			"restore": map[string]any{"options": map[string]any{"jobs": 1, "restore-in-order": true}, "mysql": map[string]any{"options": conn(job.Target, d.Name)}},
		}
		b, _ := json.Marshal(cfg)
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
