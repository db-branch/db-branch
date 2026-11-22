// Package srcdump dumps production databases with the base runtime's own
// client tools (pg_dump, mysqldump), for scheduled base refreshes.
//
// A dump holds unmasked production data: it is written 0600 into a 0700 work
// directory and deleted by the caller right after it is loaded. The password
// reaches the tool by environment only.
package srcdump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// StreamFunc runs name with extra environment variables and writes its
// standard output to out (execx.Runner.StreamEnv).
type StreamFunc func(ctx context.Context, env []string, out io.Writer, name string, args ...string) error

// Dumper implements domain.SourceDumper for any engine whose runtime provider
// can dump sources (its DumpSource): the engine specific part is the provider's.
type Dumper struct {
	// WorkDir holds dumps while they exist.
	WorkDir   string
	Providers domain.RuntimeProviders
	// Timeout bounds one dump (0: 6 hours), so a source that accepts the
	// connection and never answers cannot hold the base's refresh forever.
	// (mysqldump has no connect timeout of its own.)
	Timeout time.Duration
	// LookupUser finds the numeric uid and gid of an OS user for an external
	// runtime module's dump directory. Nil uses os/user.Lookup.
	LookupUser func(name string) (uid, gid int, err error)
	// Chown hands an external runtime module's dump directory to its OS user.
	// Nil uses os.Chown.
	Chown func(path string, uid, gid int) error

	// MaxBytes caps the dump's size (0 or nil: no cap). A dump that grows
	// past it is stopped and removed, so a huge source cannot fill the disk.
	MaxBytes func() int64
	// Free reports the free bytes of the file system holding dir, and
	// Reserve is what must stay free: a dump that eats into it is stopped.
	// Nil Free: not checked.
	Free    func(dir string) (uint64, error)
	Reserve uint64
	// Poll is how often the dump's size and the free space are checked
	// (0: 1 second).
	Poll time.Duration
}

const defaultDumpTimeout = 6 * time.Hour

var _ domain.SourceDumper = Dumper{}

// Dump writes the source databases into one new SQL script and returns its path.
func (d Dumper) Dump(ctx context.Context, job domain.SourceDumpJob) (string, error) {
	var p domain.RuntimeProvider
	ok := false
	if d.Providers != nil {
		p, ok = d.Providers.Provider(job.Tools.Engine)
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", domain.ErrUnsupportedEngine, job.Tools.Engine)
	}
	spec, err := p.Describe(ctx)
	if err != nil {
		return "", err
	}
	if err := spec.Capabilities.Require("source_dump"); err != nil {
		return "", err
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultDumpTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, cut := context.WithCancelCause(ctx)
	defer cut(nil)
	if spec.RunAsUser != "" {
		return d.dumpExternal(ctx, cut, p, job, spec.RunAsUser)
	}
	if err := os.MkdirAll(d.WorkDir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(d.WorkDir, "refresh-*.sql") // 0600
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := d.run(ctx, cut, p, job, name); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// run dumps into path while watching its size and the disk, and returns the
// reason when the dump was stopped for either.
func (d Dumper) run(ctx context.Context, cut context.CancelCauseFunc, p domain.RuntimeProvider, job domain.SourceDumpJob, path string) error {
	stop := d.watch(ctx, cut, path)
	err := p.DumpSource(ctx, job, path)
	stop()
	if cause := context.Cause(ctx); errors.Is(cause, domain.ErrTooLarge) || errors.Is(cause, domain.ErrNoSpace) {
		return cause
	}
	return wrapContextErr(ctx, err)
}

// watch stops the dump (cancelling its context with the reason) when the
// file passes MaxBytes or the disk falls below Reserve. The returned func
// ends the watching.
func (d Dumper) watch(ctx context.Context, cut context.CancelCauseFunc, path string) (stop func()) {
	var max int64
	if d.MaxBytes != nil {
		max = d.MaxBytes()
	}
	if max <= 0 && d.Free == nil {
		return func() {}
	}
	poll := d.Poll
	if poll <= 0 {
		poll = time.Second
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if fi, err := os.Stat(path); err == nil && max > 0 && fi.Size() > max {
				cut(fmt.Errorf("%w: the dump passed %d bytes (the limit of this refresh)", domain.ErrTooLarge, max))
				return
			}
			if d.Free != nil {
				if free, err := d.Free(filepath.Dir(path)); err == nil && free < d.Reserve {
					cut(fmt.Errorf("%w: only %d bytes left in %s while dumping (keeping %d free)", domain.ErrNoSpace, free, filepath.Dir(path), d.Reserve))
					return
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}

// Sweep deletes every dump left in the work directory, for the start of the
// agent: a crash during a refresh leaves unmasked production data behind. It
// returns how many entries it removed.
func (d Dumper) Sweep() int {
	if d.WorkDir == "" {
		return 0
	}
	entries, err := os.ReadDir(d.WorkDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "refresh-") {
			continue
		}
		if os.RemoveAll(filepath.Join(d.WorkDir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// dumpExternal gives an external runtime module its own 0700 directory under a
// 0711 WorkDir (traverse-only for others, not listable) so the module's OS user
// can create and write the dump file while root can still read and remove it.
func (d Dumper) dumpExternal(ctx context.Context, cut context.CancelCauseFunc, p domain.RuntimeProvider, job domain.SourceDumpJob, runAsUser string) (string, error) {
	uid, gid, err := d.lookupUser(runAsUser)
	if err != nil {
		return "", fmt.Errorf("lookup user %s: %w", runAsUser, err)
	}
	if err := os.MkdirAll(d.WorkDir, 0o711); err != nil {
		return "", err
	}
	if err := os.Chmod(d.WorkDir, 0o711); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(d.WorkDir, "refresh-*") // 0700
	if err != nil {
		return "", err
	}
	if err := d.chown(dir, uid, gid); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("give %s to %s: %w", dir, runAsUser, err)
	}
	name := filepath.Join(dir, "dump.sql")
	if err := d.run(ctx, cut, p, job, name); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return name, nil
}

func (d Dumper) lookupUser(name string) (int, int, error) {
	if d.LookupUser != nil {
		return d.LookupUser(name)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uid, gid, nil
}

func (d Dumper) chown(path string, uid, gid int) error {
	if d.Chown != nil {
		return d.Chown(path, uid, gid)
	}
	return os.Chown(path, uid, gid)
}

func wrapContextErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		// A tool killed with its context only says "signal: killed".
		return fmt.Errorf("%w (%w)", err, cerr)
	}
	return err
}

// Discard deletes a dump (and its per-dump directory for an external module).
func (d Dumper) Discard(path string) {
	if path == "" {
		return
	}
	if d.WorkDir != "" {
		workDir := filepath.Clean(d.WorkDir)
		dir := filepath.Dir(filepath.Clean(path))
		if dir != workDir && filepath.Dir(dir) == workDir && strings.HasPrefix(filepath.Base(dir), "refresh-") {
			_ = os.RemoveAll(dir)
			return
		}
	}
	_ = os.Remove(path)
}

// PostgresDump is the DumpSource of the PostgreSQL provider: it writes the
// dump into outPath (an existing file, which keeps its mode).
func PostgresDump(stream StreamFunc) func(context.Context, domain.SourceDumpJob, string) error {
	return dumpTo(stream, postgresDump)
}

// MySQLDump is the DumpSource of the MySQL provider.
func MySQLDump(stream StreamFunc) func(context.Context, domain.SourceDumpJob, string) error {
	return dumpTo(stream, mysqlDump)
}

type engineDump func(ctx context.Context, stream StreamFunc, out io.Writer, job domain.SourceDumpJob) error

func dumpTo(stream StreamFunc, run engineDump) func(context.Context, domain.SourceDumpJob, string) error {
	return func(ctx context.Context, job domain.SourceDumpJob, outPath string) error {
		f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		err = run(ctx, stream, f, job)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
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
func postgresDump(ctx context.Context, stream StreamFunc, out io.Writer, job domain.SourceDumpJob) error {
	s := job.Source
	env := withPassword([]string{"LD_LIBRARY_PATH=" + job.Tools.LibDir, "PGCONNECT_TIMEOUT=30"}, "PGPASSWORD", s.Password)
	for _, db := range s.Databases {
		args := []string{"-h", s.Host, "-p", strconv.Itoa(s.Port), "-U", s.User, "--no-password",
			"--clean", "--if-exists", "--no-owner", "--no-privileges"}
		if db != "postgres" {
			args = append(args, "--create")
		}
		if err := stream(ctx, env, out, filepath.Join(job.Tools.BinDir, "pg_dump"), append(args, db)...); err != nil {
			return fmt.Errorf("pg_dump %s: %w", db, err)
		}
	}
	return nil
}

func mysqlDump(ctx context.Context, stream StreamFunc, out io.Writer, job domain.SourceDumpJob) error {
	s := job.Source
	env := withPassword([]string{"LD_LIBRARY_PATH=" + job.Tools.LibDir}, "MYSQL_PWD", s.Password)
	args := []string{"--protocol=tcp", "-h", s.Host, "-P", strconv.Itoa(s.Port), "-u", s.User,
		"--single-transaction", "--routines", "--triggers", "--events", "--no-tablespaces", "--set-gtid-purged=OFF",
		"--add-drop-database", "--databases"}
	if err := stream(ctx, env, out, filepath.Join(job.Tools.BinDir, "mysqldump"), append(args, s.Databases...)...); err != nil {
		return fmt.Errorf("mysqldump: %w", err)
	}
	return nil
}
