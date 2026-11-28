// Package postgres runs PostgreSQL servers for bases and branches.
package postgres

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/db-branch/db-branch/internal/infrastructure/fsutil"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/retry"
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
	// ChownConfig makes a path root's, readable by User's group
	// (root:db-branch): the instance directory and env the unit starts from
	// must not be rewritable by the database server's own user. Optional.
	ChownConfig func(path string) error
	// CheckOwner fails with domain.ErrOwnership when path does not belong to
	// User. Optional.
	CheckOwner func(path string) error

	// Open opens a database connection for init SQL (default sql.Open).
	Open func(driver, dsn string) (*sql.DB, error)

	StartTimeout time.Duration
	PollInterval time.Duration
}

var (
	_ domain.DBServer         = (*Server)(nil)
	_ domain.CloneUserCleaner = (*Server)(nil)
)

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
		"PG_BIND=" + p.listen(s),
		fmt.Sprintf("PG_MEM=%d", s.MemoryMB),
		"LD_LIBRARY_PATH=" + p.lib(s),
		"",
	}, "\n")
	envFile := filepath.Join(p.dir(s), "env")
	// The directory first: from then on the service user cannot swap env for
	// a link while root writes it.
	if err := p.chownConfig(p.dir(s)); err != nil {
		return err
	}
	if err := fsutil.WriteFresh(envFile, []byte(env), 0o640); err != nil {
		return err
	}
	return p.chownConfig(envFile)
}

func (p *Server) chownConfig(path string) error {
	if p.ChownConfig == nil {
		return nil
	}
	return p.ChownConfig(path)
}

// listen is listen_addresses for the instance: none while it is in maintenance
// (a module works on the data over the socket; no one connects over the network).
func (p *Server) listen(s domain.Server) string {
	if s.Maintenance {
		return ""
	}
	return p.Bind
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
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	if p.CheckOwner != nil {
		if err := p.CheckOwner(s.DataDir); err != nil {
			return err
		}
	}
	if err := p.writeConfig(s); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, p.StartTimeout)
	defer cancel()
	if _, err := p.Run.Run(waitCtx, "systemctl", "start", unit(s)); err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return p.abortStart(ctx, s)
		}
		return fmt.Errorf("start %s: %w: %s", s.ID, err, p.journal(ctx, s))
	}
	for !p.ready(waitCtx, s) {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return p.abortStart(ctx, s)
		}
		if p.exited(waitCtx, s) {
			return domain.StartFailure(s.ID, p.journalFresh(ctx, s))
		}
		if err := retry.Sleep(waitCtx, p.PollInterval); err != nil {
			return p.abortStart(ctx, s)
		}
	}
	return nil
}

func (p *Server) abortStart(ctx context.Context, s domain.Server) error {
	if err := ctx.Err(); err != nil {
		p.stopAborted(ctx, s)
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	j := p.journalFresh(ctx, s)
	p.stopAborted(ctx, s)
	return fmt.Errorf("%s did not become ready within %s: %s", s.ID, p.StartTimeout, j)
}

func (p *Server) journalFresh(ctx context.Context, s domain.Server) string {
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.journal(logCtx, s)
}

func (p *Server) runTimeout(base context.Context, d time.Duration, name string, args ...string) error {
	c, cancel := context.WithTimeout(base, d)
	defer cancel()
	_, err := p.Run.Run(c, name, args...)
	return err
}

// stopAborted stops a unit whose start was cancelled or timed out, using a
// fresh context per command so one slow stop cannot starve SIGKILL/reset-failed.
func (p *Server) stopAborted(ctx context.Context, s domain.Server) {
	base := context.WithoutCancel(ctx)
	if err := p.runTimeout(base, 30*time.Second, "systemctl", "stop", unit(s)); err != nil {
		_ = p.runTimeout(base, 10*time.Second, "systemctl", "kill", "--signal=SIGKILL", unit(s))
		_ = p.runTimeout(base, 10*time.Second, "systemctl", "stop", unit(s))
	}
	_ = p.runTimeout(base, 10*time.Second, "systemctl", "reset-failed", unit(s))
}

func (p *Server) activeState(ctx context.Context, s domain.Server) (string, error) {
	out, err := p.Run.Run(ctx, "systemctl", "show", "--property=ActiveState", "--value", unit(s))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// exited reports whether the unit has stopped for good, so there is nothing
// left to wait for.
func (p *Server) exited(ctx context.Context, s domain.Server) bool {
	// is-active exits non-zero for a stopped unit, and the runner then
	// returns no output; show prints the state whatever it is.
	st, err := p.activeState(ctx, s)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := p.activeState(ctx, s)
	if err != nil {
		return fmt.Errorf("check %s: %w", s.ID, err)
	}
	if state == "inactive" || state == "failed" {
		if mode == domain.StopImmediate {
			return nil
		}
		return p.verifyStopped(ctx, s)
	}
	if mode == domain.StopImmediate {
		_, _ = p.Run.Run(ctx, "systemctl", "kill", "--signal=SIGKILL", unit(s))
		_, _ = p.Run.Run(ctx, "systemctl", "stop", unit(s))
		_, _ = p.Run.Run(ctx, "systemctl", "reset-failed", unit(s))
		return nil
	}
	if _, err := p.Run.Run(ctx, "systemctl", "stop", unit(s)); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("stop %s: %w", s.ID, ctx.Err())
		}
		return fmt.Errorf("stop %s: %w", s.ID, err)
	}
	return p.verifyStopped(ctx, s)
}

// verifyStopped confirms a stopped unit finished a clean shutdown.
func (p *Server) verifyStopped(ctx context.Context, s domain.Server) error {
	result, err := p.Run.Run(ctx, "systemctl", "show", "--property=Result", "--value", unit(s))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("check %s shutdown result: %w", s.ID, err)
	}
	if err := p.verifyShutdown(ctx, s); err != nil {
		return err
	}
	if res := strings.TrimSpace(string(result)); res != "success" {
		return fmt.Errorf("%w: %s (result=%s)", domain.ErrUncleanShutdown, s.ID, res)
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

// quotePGLiteral quotes s as a PostgreSQL string literal (matching libpq's
// PQescapeLiteral: standard '...' when there are no backslashes or line breaks,
// or E'...' with escaped backslashes and line breaks so it is safe regardless
// of standard_conforming_strings and never emits raw newlines to psql).
func quotePGLiteral(s string) string {
	if strings.ContainsAny(s, "\\\r\n") {
		return "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`, "\n", `\n`, "\r", `\r`).Replace(s) + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// quotePGIdent quotes s as a PostgreSQL double-quoted identifier.
func quotePGIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// dollarTag picks a PostgreSQL dollar-quote delimiter that does not occur in body.
func dollarTag(body string) string {
	tag := "$$"
	for i := 1; strings.Contains(body, tag); i++ {
		tag = fmt.Sprintf("$dbb%d$", i)
	}
	return tag
}

func validateUser(user string) error {
	if user == "" {
		return errors.New("postgres login user cannot be empty")
	}
	if strings.ContainsAny(user, "\x00\r\n") {
		return errors.New("postgres login user cannot contain NUL bytes or newlines")
	}
	return nil
}

// CreateUser makes the login for clients, or puts it right and resets its
// password. It is no superuser (a superuser can run programs on the node with
// COPY ... TO PROGRAM and read any file the server can): it is a member of
// OwnerRole, which owns the objects in the databases.
func (p *Server) CreateUser(ctx context.Context, s domain.Server, user, password string) error {
	if err := validateUser(user); err != nil {
		return err
	}
	if strings.ContainsRune(password, 0) {
		return errors.New("postgres password cannot contain NUL bytes")
	}
	uLit, uIdent := quotePGLiteral(user), quotePGIdent(user)
	pwLit := quotePGLiteral(password)
	oLit, oIdent := quotePGLiteral(OwnerRole), quotePGIdent(OwnerRole)
	body := fmt.Sprintf(`BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %[4]s) THEN
    CREATE ROLE %[5]s NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = %[1]s) THEN
    ALTER ROLE %[2]s WITH LOGIN NOSUPERUSER CREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %[3]s;
  ELSE
    CREATE ROLE %[2]s WITH LOGIN NOSUPERUSER CREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %[3]s;
  END IF;
END`, uLit, uIdent, pwLit, oLit, oIdent)
	tag := dollarTag(body)
	err := p.Exec(ctx, s, fmt.Sprintf("DO %[1]s\n%[2]s\n%[1]s;\nGRANT %[3]s TO %[4]s;", tag, body, oIdent, uIdent))
	if err != nil {
		return err
	}
	return p.ownObjects(ctx, s)
}

// DropUser removes an inherited branch login role from a cloned server after
// transferring any remaining owned objects to OwnerRole and revoking grants, so
// the parent's credentials cannot reach a forked child.
func (p *Server) DropUser(ctx context.Context, s domain.Server, user string) error {
	if err := validateUser(user); err != nil {
		return err
	}
	if user == Superuser || user == OwnerRole {
		return fmt.Errorf("postgres cannot drop reserved role %q", user)
	}
	uLit, uIdent := quotePGLiteral(user), quotePGIdent(user)
	oLit, oIdent := quotePGLiteral(OwnerRole), quotePGIdent(OwnerRole)
	dbs, err := p.databases(ctx, s)
	if err != nil {
		return err
	}
	dbBody := fmt.Sprintf(`BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = %[1]s) THEN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = %[3]s) THEN
      REASSIGN OWNED BY %[2]s TO %[4]s;
    END IF;
    DROP OWNED BY %[2]s;
  END IF;
END`, uLit, uIdent, oLit, oIdent)
	dbSQL := fmt.Sprintf("DO %[1]s\n%[2]s\n%[1]s;", dollarTag(dbBody), dbBody)
	for _, db := range dbs {
		name, args := p.tool(s, "psql", "-h", p.sockDir(s), "-p", fmt.Sprint(s.Port), "-U", Superuser, "-d", db,
			"-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-")
		if _, err := p.Run.RunInput(ctx, strings.NewReader(dbSQL), name, args...); err != nil {
			return fmt.Errorf("database %s: %w", db, err)
		}
	}
	dropBody := fmt.Sprintf(`BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = %[1]s) THEN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = %[3]s) THEN
      REVOKE %[4]s FROM %[2]s;
    END IF;
    DROP ROLE %[2]s;
  END IF;
END`, uLit, uIdent, oLit, oIdent)
	return p.Exec(ctx, s, fmt.Sprintf("DO %[1]s\n%[2]s\n%[1]s;", dollarTag(dropBody), dropBody))
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

// UpdateStats runs VACUUM (FREEZE, ANALYZE) on every database: statistics
// for the planner, and rows marked visible and frozen so that branches do not
// rewrite every page they read to set hint bits.
func (p *Server) UpdateStats(ctx context.Context, s domain.Server) error {
	// The snapshot this is for carries the objects owned by OwnerRole, so a
	// branch's login finds them in order at once.
	if err := p.Exec(ctx, s, `DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'db_branch_owner') THEN
    CREATE ROLE db_branch_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
END $$;`); err != nil {
		return err
	}
	if err := p.ownObjects(ctx, s); err != nil {
		return err
	}
	name, args := p.tool(s, "vacuumdb", "-h", p.sockDir(s), "-p", fmt.Sprint(s.Port), "-U", Superuser, "--all", "--freeze", "--analyze", "--quiet")
	_, err := p.Run.Run(ctx, name, args...)
	return err
}
