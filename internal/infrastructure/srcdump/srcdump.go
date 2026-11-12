// Package srcdump dumps production databases with the base runtime's own
// client tools (pg_dump, mysqldump), for scheduled base refreshes.
//
// A dump holds unmasked production data: it is written 0600 into a 0700 work
// directory and deleted by the caller right after it is loaded. The password
// reaches the tool by environment only.
package srcdump

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/db-branch/db-branch/internal/domain"
)

// StreamFunc runs name with extra environment variables and writes its
// standard output to out (execx.Runner.StreamEnv).
type StreamFunc func(ctx context.Context, env []string, out io.Writer, name string, args ...string) error

// Dumper implements domain.SourceDumper.
type Dumper struct {
	// WorkDir holds dumps while they exist.
	WorkDir string
	Stream  StreamFunc
}

var _ domain.SourceDumper = Dumper{}

// Dump writes the source databases into one new SQL script and returns its path.
func (d Dumper) Dump(ctx context.Context, job domain.SourceDumpJob) (string, error) {
	var run func(context.Context, io.Writer, domain.SourceDumpJob) error
	switch job.Tools.Engine {
	case domain.Postgres:
		run = d.postgres
	case domain.MySQL:
		run = d.mysql
	default:
		return "", fmt.Errorf("%w: %s", domain.ErrUnsupportedEngine, job.Tools.Engine)
	}
	if err := os.MkdirAll(d.WorkDir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(d.WorkDir, "refresh-*.sql") // 0600
	if err != nil {
		return "", err
	}
	err = run(ctx, f, job)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// Discard deletes a dump.
func (Dumper) Discard(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

func withPassword(env []string, name, password string) []string {
	if password == "" {
		return env
	}
	return append(env, name+"="+password)
}

// postgres dumps each database with its own CREATE DATABASE (the base is
// refreshed in place, so the old one is dropped first). The maintenance
// database "postgres" cannot be dropped while connected: it is cleaned only.
func (d Dumper) postgres(ctx context.Context, out io.Writer, job domain.SourceDumpJob) error {
	s := job.Source
	env := withPassword([]string{"LD_LIBRARY_PATH=" + job.Tools.LibDir, "PGCONNECT_TIMEOUT=30"}, "PGPASSWORD", s.Password)
	for _, db := range s.Databases {
		args := []string{"-h", s.Host, "-p", strconv.Itoa(s.Port), "-U", s.User, "--no-password",
			"--clean", "--if-exists", "--no-owner", "--no-privileges"}
		if db != "postgres" {
			args = append(args, "--create")
		}
		if err := d.Stream(ctx, env, out, filepath.Join(job.Tools.BinDir, "pg_dump"), append(args, db)...); err != nil {
			return fmt.Errorf("pg_dump %s: %w", db, err)
		}
	}
	return nil
}

func (d Dumper) mysql(ctx context.Context, out io.Writer, job domain.SourceDumpJob) error {
	s := job.Source
	env := withPassword([]string{"LD_LIBRARY_PATH=" + job.Tools.LibDir}, "MYSQL_PWD", s.Password)
	args := []string{"--protocol=tcp", "-h", s.Host, "-P", strconv.Itoa(s.Port), "-u", s.User, "--connect-timeout=30",
		"--single-transaction", "--routines", "--triggers", "--events", "--no-tablespaces", "--set-gtid-purged=OFF",
		"--add-drop-database", "--databases"}
	if err := d.Stream(ctx, env, out, filepath.Join(job.Tools.BinDir, "mysqldump"), append(args, s.Databases...)...); err != nil {
		return fmt.Errorf("mysqldump: %w", err)
	}
	return nil
}
