// Package postgres runs PostgreSQL servers for bases and branches.
package postgres

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Runner runs external commands, optionally feeding stdin.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunInput(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error)
}

// Server runs postgres instances as db-branch-postgres@.service units.
// Runtimes are PGDG packages unpacked as is; the fixup links bin/ and lib/.
type Server struct {
	Run Runner
	// RuntimeDir returns the unpacked runtime for a version.
	RuntimeDir func(domain.RuntimeRef) string
	// StateRoot holds each instance's env file (outside the dataset).
	StateRoot string
	// RunRoot holds the sockets (RuntimeDirectory of the unit).
	RunRoot string
	// Bind is the address postgres listens on.
	Bind string
	// User owns data directories; initdb refuses to run as root.
	User string
	// Chown hands a path to User. Optional.
	Chown func(path string) error
	// CheckOwner fails with domain.ErrOwnership when path does not belong to
	// User. Optional.
	CheckOwner func(path string) error

	StartTimeout time.Duration
	PollInterval time.Duration
}

var _ domain.DBServer = (*Server)(nil)

// Superuser is the local admin initdb creates (trusted on the socket only).
const Superuser = "postgres"

func unit(s domain.Server) string                { return "db-branch-postgres@" + s.ID + ".service" }
func (p *Server) dir(s domain.Server) string     { return filepath.Join(p.StateRoot, s.ID) }
func (p *Server) sockDir(s domain.Server) string { return filepath.Join(p.RunRoot, s.ID) }
func (p *Server) lib(s domain.Server) string     { return filepath.Join(p.RuntimeDir(s.Runtime), "lib") }
func (p *Server) bin(s domain.Server, name string) string {
	return filepath.Join(p.RuntimeDir(s.Runtime), "bin", name)
}

// tool runs a runtime binary with its bundled libpq.
func (p *Server) tool(s domain.Server, name string, args ...string) (string, []string) {
	return "env", append([]string{"LD_LIBRARY_PATH=" + p.lib(s), p.bin(s, name)}, args...)
}

func (p *Server) writeConfig(s domain.Server) error {
	if err := os.MkdirAll(p.StateRoot, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(p.StateRoot, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(p.dir(s), 0o750); err != nil {
		return err
	}
	env := strings.Join([]string{
		"PG_BIN=" + filepath.Join(p.RuntimeDir(s.Runtime), "bin"),
		"PG_DATA=" + s.DataDir,
		fmt.Sprintf("PG_PORT=%d", s.Port),
		"PG_BIND=" + p.Bind,
		fmt.Sprintf("PG_MEM=%d", s.MemoryMB),
		"LD_LIBRARY_PATH=" + p.lib(s),
		"",
	}, "\n")
	envFile := filepath.Join(p.dir(s), "env")
	if err := os.WriteFile(envFile, []byte(env), 0o640); err != nil {
		return err
	}
	return p.chown(p.dir(s), envFile)
}

func (p *Server) chown(paths ...string) error {
	if p.Chown == nil {
		return nil
	}
	for _, path := range paths {
		if err := p.Chown(path); err != nil {
			return err
		}
	}
	return nil
}

// hbaRemote lets clients in from anywhere with a password; the local socket
// stays trusted for the agent.
const hbaRemote = "\n# Added by db-branch: clients connect over the network with a password.\nhost all all 0.0.0.0/0 scram-sha-256\nhost all all ::/0 scram-sha-256\n"

// Initialize creates a cluster with a trusted local superuser.
func (p *Server) Initialize(ctx context.Context, s domain.Server) error {
	if err := p.writeConfig(s); err != nil {
		return err
	}
	if err := p.chown(s.DataDir); err != nil {
		return err
	}
	if err := os.Chmod(s.DataDir, 0o700); err != nil {
		return err
	}
	name, args := p.tool(s, "initdb", "-D", s.DataDir, "-U", Superuser, "--auth-local=trust", "--auth-host=scram-sha-256",
		"-E", "UTF8", "--locale=C.UTF-8")
	if _, err := p.Run.Run(ctx, "runuser", append([]string{"-u", p.User, "--", name}, args...)...); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.DataDir, "pg_hba.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(hbaRemote)
	return err
}

// PrepareClone removes the cloned postmaster's pid file.
func (p *Server) PrepareClone(_ context.Context, s domain.Server) error {
	for _, f := range []string{"postmaster.pid", "postmaster.opts"} {
		if err := os.Remove(filepath.Join(s.DataDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return p.writeConfig(s)
}

func (p *Server) ready(ctx context.Context, s domain.Server) bool {
	name, args := p.tool(s, "pg_isready", "-q", "-h", p.sockDir(s), "-p", fmt.Sprint(s.Port))
	_, err := p.Run.Run(ctx, name, args...)
	return err == nil
}

// Start starts the unit and waits until postgres accepts connections.
func (p *Server) Start(ctx context.Context, s domain.Server) error {
	if p.CheckOwner != nil {
		if err := p.CheckOwner(s.DataDir); err != nil {
			return err
		}
	}
	if err := p.writeConfig(s); err != nil {
		return err
	}
	if _, err := p.Run.Run(ctx, "systemctl", "start", unit(s)); err != nil {
		return fmt.Errorf("start %s: %w: %s", s.ID, err, p.journal(ctx, s))
	}
	deadline := time.Now().Add(p.StartTimeout)
	for !p.ready(ctx, s) {
		if p.exited(ctx, s) {
			return domain.StartFailure(s.ID, p.journal(ctx, s))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become ready within %s: %s", s.ID, p.StartTimeout, p.journal(ctx, s))
		}
		time.Sleep(p.PollInterval)
	}
	return nil
}

// exited reports whether the unit has stopped for good, so there is nothing
// left to wait for.
func (p *Server) exited(ctx context.Context, s domain.Server) bool {
	// is-active exits non-zero for a stopped unit, and the runner then
	// returns no output; show prints the state whatever it is.
	out, err := p.Run.Run(ctx, "systemctl", "show", "--property=ActiveState", "--value", unit(s))
	st := strings.TrimSpace(string(out))
	return err == nil && (st == "failed" || st == "inactive")
}

func (p *Server) journal(ctx context.Context, s domain.Server) string {
	out, _ := p.Run.Run(ctx, "journalctl", "-u", unit(s), "-n", "20", "--no-pager", "-o", "cat")
	return strings.TrimSpace(string(out))
}

// Running reports whether the unit is active.
func (p *Server) Running(ctx context.Context, s domain.Server) (bool, error) {
	out, _ := p.Run.Run(ctx, "systemctl", "is-active", unit(s))
	return strings.TrimSpace(string(out)) == "active", nil
}

// Stop stops the server. StopClean is PostgreSQL's fast shutdown (the unit's
// KillSignal), confirmed by the control file saying "shut down", so a
// snapshot afterwards starts without recovery.
func (p *Server) Stop(ctx context.Context, s domain.Server, mode domain.StopMode) error {
	if ok, _ := p.Running(ctx, s); !ok {
		return nil
	}
	if mode == domain.StopImmediate {
		_, _ = p.Run.Run(ctx, "systemctl", "kill", "--signal=SIGKILL", unit(s))
		_, _ = p.Run.Run(ctx, "systemctl", "stop", unit(s))
		_, _ = p.Run.Run(ctx, "systemctl", "reset-failed", unit(s))
		return nil
	}
	if _, err := p.Run.Run(ctx, "systemctl", "stop", unit(s)); err != nil {
		return fmt.Errorf("stop %s: %w", s.ID, err)
	}
	result, _ := p.Run.Run(ctx, "systemctl", "show", "--property=Result", "--value", unit(s))
	name, args := p.tool(s, "pg_controldata", s.DataDir)
	control, err := p.Run.Run(ctx, name, args...)
	state := ""
	for _, line := range strings.Split(string(control), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Database cluster state" {
			state = strings.TrimSpace(v)
		}
	}
	if err != nil || strings.TrimSpace(string(result)) != "success" || state != "shut down" {
		return fmt.Errorf("%w: %s (result=%s, cluster state=%q): %v", domain.ErrUncleanShutdown, s.ID, strings.TrimSpace(string(result)), state, err)
	}
	return nil
}

func (p *Server) psql(s domain.Server) (string, []string) {
	return p.tool(s, "psql", "-h", p.sockDir(s), "-p", fmt.Sprint(s.Port), "-U", Superuser, "-d", "postgres",
		"-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-")
}

// Admin is the trusted local superuser over the instance's socket.
func (p *Server) Admin(s domain.Server) domain.AdminAccess {
	host := p.Bind
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return domain.AdminAccess{Engine: domain.Postgres, Host: host, Socket: p.sockDir(s), Port: s.Port, User: Superuser,
		BinDir: filepath.Join(p.RuntimeDir(s.Runtime), "bin"), LibDir: p.lib(s)}
}

// Exec runs sql as the local superuser.
func (p *Server) Exec(ctx context.Context, s domain.Server, sql string) error {
	name, args := p.psql(s)
	_, err := p.Run.RunInput(ctx, strings.NewReader(sql), name, args...)
	return err
}

// CreateUser makes a superuser login for clients, or resets its password.
// Names are branch names and passwords letters and digits: the quoting below
// is enough.
func (p *Server) CreateUser(ctx context.Context, s domain.Server, user, password string) error {
	return p.Exec(ctx, s, fmt.Sprintf(`DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
    ALTER ROLE "%[1]s" WITH LOGIN SUPERUSER PASSWORD '%[2]s';
  ELSE
    CREATE ROLE "%[1]s" WITH LOGIN SUPERUSER PASSWORD '%[2]s';
  END IF;
END
$$;`, user, password))
}

// Load streams a SQL dump (plain or .gz) into psql.
func (p *Server) Load(ctx context.Context, s domain.Server, dumpPath string) error {
	f, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var in io.Reader = f
	if strings.HasSuffix(dumpPath, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("%s: %w", dumpPath, err)
		}
		defer gz.Close()
		in = gz
	}
	name, args := p.psql(s)
	_, err = p.Run.RunInput(ctx, in, name, args...)
	return err
}

// Remove stops the unit and deletes the instance configuration.
func (p *Server) Remove(ctx context.Context, s domain.Server) error {
	_, _ = p.Run.Run(ctx, "systemctl", "stop", unit(s))
	_, _ = p.Run.Run(ctx, "systemctl", "reset-failed", unit(s))
	return os.RemoveAll(p.dir(s))
}
