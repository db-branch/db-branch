package mysql

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Runner runs external commands, optionally feeding stdin and extra environment.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunInput(ctx context.Context, in io.Reader, name string, args ...string) ([]byte, error)
	RunInputEnv(ctx context.Context, env []string, in io.Reader, name string, args ...string) ([]byte, error)
}

// Server runs mysqld instances as instances of the db-branch-mysqld@.service
// systemd template, so they outlive agent restarts and log to journald.
type Server struct {
	Run Runner
	// RuntimeDir returns the unpacked runtime for a version.
	RuntimeDir func(domain.RuntimeRef) string
	// StateRoot holds per-instance my.cnf, env and error.log (outside the dataset,
	// so logs are not cloned into branches).
	StateRoot string
	// RunRoot holds per-instance sockets and pid files (RuntimeDirectory of the unit).
	RunRoot string
	// Bind is the address mysqld listens on.
	Bind string
	// Chown hands a path to the unprivileged service user. Optional.
	Chown func(path string) error
	// ChownConfig makes a path root's, readable by the service user's group
	// (root:db-branch). The instance directory, my.cnf and env go through it:
	// the service user must not be able to rewrite what its unit starts from.
	// Optional.
	ChownConfig func(path string) error
	// CheckOwner fails with domain.ErrOwnership when path does not belong to
	// the user the server runs as. Optional.
	CheckOwner func(path string) error

	StartTimeout time.Duration
	PollInterval time.Duration
	// UpgradeTimeout is how long a start may take when the server upgrades
	// its data dictionary (default 15 minutes).
	UpgradeTimeout time.Duration
}

var _ domain.DBServer = (*Server)(nil)

func (m *Server) dir(s domain.Server) string    { return filepath.Join(m.StateRoot, s.ID) }
func (m *Server) cnf(s domain.Server) string    { return filepath.Join(m.dir(s), "my.cnf") }
func (m *Server) logDir(s domain.Server) string { return filepath.Join(m.dir(s), "log") }
func (m *Server) errLog(s domain.Server) string { return filepath.Join(m.logDir(s), "error.log") }
func (m *Server) socket(s domain.Server) string {
	return filepath.Join(m.RunRoot, s.ID, "mysqld.sock")
}
func (m *Server) bin(s domain.Server, name string) string {
	return filepath.Join(m.RuntimeDir(s.Runtime), "bin", name)
}
func unit(s domain.Server) string { return "db-branch-mysqld@" + s.ID + ".service" }

func (m *Server) writeConfig(s domain.Server) error {
	// The service user must traverse StateRoot to reach its own instance directory.
	if err := os.MkdirAll(m.StateRoot, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(m.StateRoot, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir(s), 0o750); err != nil {
		return err
	}
	cnf, _ := m.config(s)
	if err := os.WriteFile(m.cnf(s), []byte(cnf), 0o640); err != nil {
		return err
	}
	env := "MYSQLD=" + m.bin(s, "mysqld") + "\n"
	if err := os.WriteFile(filepath.Join(m.dir(s), "env"), []byte(env), 0o640); err != nil {
		return err
	}
	if err := m.chownConfig(m.dir(s), m.cnf(s), filepath.Join(m.dir(s), "env")); err != nil {
		return err
	}
	// The server writes its error log here; the directory above is not its own.
	if err := os.MkdirAll(m.logDir(s), 0o750); err != nil {
		return err
	}
	old := filepath.Join(m.dir(s), "error.log")
	if _, err := os.Stat(old); err == nil {
		if _, err := os.Stat(m.errLog(s)); err != nil {
			_ = os.Rename(old, m.errLog(s)) // best effort: an earlier layout kept it beside my.cnf
		}
		_ = os.Remove(old)
	}
	return m.chown(m.logDir(s))
}

func (m *Server) chownConfig(paths ...string) error {
	if m.ChownConfig == nil {
		return nil
	}
	for _, p := range paths {
		if err := m.ChownConfig(p); err != nil {
			return err
		}
	}
	return nil
}

// config renders the instance's my.cnf for the server's runtime. Options the
// runtime no longer knows are dropped or renamed (notes says which).
func (m *Server) config(s domain.Server) (cnf string, notes []string) {
	lines := []string{
		"# Written by db-branch. Changes are overwritten.",
		"[mysqld]",
		"basedir=" + m.RuntimeDir(s.Runtime),
		"datadir=" + s.DataDir,
		fmt.Sprintf("port=%d", s.Port),
		"bind-address=" + m.Bind,
		"socket=" + m.socket(s),
		"pid-file=" + filepath.Join(m.RunRoot, s.ID, "mysqld.pid"),
		"log-error=" + m.errLog(s),
		"skip-name-resolve",
		// No file import or export through SQL, whatever a login may do.
		"secure_file_priv=NULL",
		"local_infile=OFF",
		fmt.Sprintf("innodb_buffer_pool_size=%dM", s.MemoryMB),
		// ZFS is copy-on-write: torn pages cannot happen, so doublewrite only costs writes.
		"innodb_doublewrite=OFF",
		"innodb_flush_neighbors=0",
	}
	if s.Maintenance {
		// Work on the data only (a module replaying changes): no network.
		lines = append(lines, "skip-networking")
	}
	series := domain.MySQLSeries(s.Runtime.Version)
	if seriesAtLeast(series, "8.0") {
		// The X plugin exists from 8.0; 5.x does not know the option and refuses to start.
		lines = append(lines, "mysqlx=OFF")
	}
	if partialRevokes(s.Runtime.Version) {
		// Branch logins are kept out of the mysql schema (see CreateUser).
		lines = append(lines, "partial_revokes=ON")
	}
	if s.LegacyAuth && series == "8.4" {
		// 8.4 ships mysql_native_password disabled; 8.0 has it, 9.0 removed it.
		lines = append(lines, "mysql_native_password=ON")
	}
	extra, notes := adaptOptions(series, s.Extra)
	return strings.Join(append(lines, append(extra, "")...), "\n"), notes
}

func (m *Server) chown(paths ...string) error {
	if m.Chown == nil {
		return nil
	}
	for _, p := range paths {
		if err := m.Chown(p); err != nil {
			return err
		}
	}
	return nil
}

// Initialize creates an empty data directory with a passwordless root@localhost.
func (m *Server) Initialize(ctx context.Context, s domain.Server) error {
	if err := m.writeConfig(s); err != nil {
		return err
	}
	if err := m.chown(s.DataDir); err != nil {
		return err
	}
	if !seriesAtLeast(domain.MySQLSeries(s.Runtime.Version), "5.7") {
		// 5.6 has no --initialize-insecure: its script builds the data directory. Without
		// --skip-name-resolve it would also add accounts for the host name.
		rt := m.RuntimeDir(s.Runtime)
		_, err := m.Run.Run(ctx, filepath.Join(rt, "scripts", "mysql_install_db"), "--defaults-file="+m.cnf(s), "--basedir="+rt, "--datadir="+s.DataDir, "--user=db-branch", "--skip-name-resolve")
		return err
	}
	_, err := m.Run.Run(ctx, m.bin(s, "mysqld"), "--defaults-file="+m.cnf(s), "--initialize-insecure", "--user=db-branch")
	return err
}

// PrepareClone drops the cloned server identity so every branch gets its own
// server UUID, and removes any stale error.log from a previous dataset so
// verifyShutdown cannot mistake an older shutdown line for this dataset's state.
func (m *Server) PrepareClone(_ context.Context, s domain.Server) error {
	stale, _ := filepath.Glob(filepath.Join(s.DataDir, "*.pid"))
	for _, p := range append(stale, filepath.Join(s.DataDir, "auto.cnf"), m.errLog(s)) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return m.writeConfig(s)
}

// Start starts the unit and waits until mysqld answers on its socket.
func (m *Server) Start(ctx context.Context, s domain.Server) error {
	return m.start(ctx, s, m.StartTimeout)
}

func (m *Server) start(ctx context.Context, s domain.Server, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	if m.CheckOwner != nil {
		if err := m.CheckOwner(s.DataDir); err != nil {
			return err
		}
	}
	if err := m.writeConfig(s); err != nil {
		return err
	}
	offset := fileSize(m.errLog(s))
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := m.Run.Run(waitCtx, "systemctl", "start", unit(s)); err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return m.abortStart(ctx, s, offset, timeout)
		}
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	for {
		if _, err := m.Run.Run(waitCtx, m.bin(s, "mysqladmin"), "--socket="+m.socket(s), "-uroot", "ping"); err == nil {
			return nil
		}
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return m.abortStart(ctx, s, offset, timeout)
		}
		if m.exited(waitCtx, s) {
			logCtx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			out := m.startOutput(logCtx, s, offset, 2048)
			lcancel()
			return domain.StartFailure(s.ID, out)
		}
		if err := sleepContext(waitCtx, m.PollInterval); err != nil {
			return m.abortStart(ctx, s, offset, timeout)
		}
	}
}

func (m *Server) abortStart(ctx context.Context, s domain.Server, offset int64, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		m.stopAborted(ctx, s)
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	logCtx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	out := m.startOutput(logCtx, s, offset, 2048)
	lcancel()
	m.stopAborted(ctx, s)
	return fmt.Errorf("%s did not become ready within %s: %s", s.ID, timeout, out)
}

func (m *Server) runTimeout(base context.Context, d time.Duration, name string, args ...string) error {
	c, cancel := context.WithTimeout(base, d)
	defer cancel()
	_, err := m.Run.Run(c, name, args...)
	return err
}

// stopAborted stops a unit whose start was cancelled or timed out, using a
// fresh context per command so one slow stop cannot starve SIGKILL/reset-failed.
func (m *Server) stopAborted(ctx context.Context, s domain.Server) {
	base := context.WithoutCancel(ctx)
	if err := m.runTimeout(base, 30*time.Second, "systemctl", "stop", unit(s)); err != nil {
		_ = m.runTimeout(base, 10*time.Second, "systemctl", "kill", "--signal=SIGKILL", unit(s))
		_ = m.runTimeout(base, 10*time.Second, "systemctl", "stop", unit(s))
	}
	_ = m.runTimeout(base, 10*time.Second, "systemctl", "reset-failed", unit(s))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (m *Server) activeState(ctx context.Context, s domain.Server) (string, error) {
	out, err := m.Run.Run(ctx, "systemctl", "show", "--property=ActiveState", "--value", unit(s))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// exited reports whether the unit has stopped for good (mysqld gave up), so
// there is nothing left to wait for.
func (m *Server) exited(ctx context.Context, s domain.Server) bool {
	// is-active exits non-zero for a stopped unit, and the runner then
	// returns no output; show prints the state whatever it is.
	st, err := m.activeState(ctx, s)
	return err == nil && (st == "failed" || st == "inactive")
}

// Running reports whether the unit is active.
func (m *Server) Running(ctx context.Context, s domain.Server) (bool, error) {
	out, _ := m.Run.Run(ctx, "systemctl", "is-active", unit(s))
	return strings.TrimSpace(string(out)) == "active", nil
}

// Stop stops the server. StopClean makes InnoDB finish purge and change-buffer
// merges first and then checks the shutdown really completed, so a snapshot
// taken afterwards needs no crash recovery when a branch starts from it.
func (m *Server) Stop(ctx context.Context, s domain.Server, mode domain.StopMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := m.activeState(ctx, s)
	if err != nil {
		return fmt.Errorf("check %s: %w", s.ID, err)
	}
	if state == "inactive" || state == "failed" {
		if mode == domain.StopImmediate {
			return nil
		}
		return m.verifyStopped(ctx, s)
	}
	if mode == domain.StopImmediate {
		_, _ = m.Run.Run(ctx, "systemctl", "kill", "--signal=SIGKILL", unit(s))
		_, _ = m.Run.Run(ctx, "systemctl", "stop", unit(s))
		_, _ = m.Run.Run(ctx, "systemctl", "reset-failed", unit(s))
		return nil
	}
	if err := m.Exec(ctx, s, "SET GLOBAL innodb_fast_shutdown = 0"); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	offset := fileSize(m.errLog(s))
	if _, err := m.Run.Run(ctx, "systemctl", "stop", unit(s)); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("stop %s: %w", s.ID, ctx.Err())
		}
		return fmt.Errorf("stop %s: %w", s.ID, err)
	}
	res, err := m.unitResult(ctx, s)
	if err != nil {
		return err
	}
	logged := m.tailLog(s, offset)
	if res != "success" || !strings.Contains(logged, "Shutdown complete") {
		return fmt.Errorf("%w: %s (result=%s): %s", domain.ErrUncleanShutdown, s.ID, res, logged)
	}
	return nil
}

func (m *Server) unitResult(ctx context.Context, s domain.Server) (string, error) {
	result, err := m.Run.Run(ctx, "systemctl", "show", "--property=Result", "--value", unit(s))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("check %s shutdown result: %w", s.ID, err)
	}
	return strings.TrimSpace(string(result)), nil
}

// verifyStopped confirms an already-stopped unit finished a clean shutdown.
func (m *Server) verifyStopped(ctx context.Context, s domain.Server) error {
	res, err := m.unitResult(ctx, s)
	if err != nil {
		return err
	}
	if res != "success" {
		return fmt.Errorf("%w: %s (result=%s): %s", domain.ErrUncleanShutdown, s.ID, res, m.tailLog(s, 0))
	}
	return m.verifyShutdown(ctx, s)
}

// localHost is how this node reaches a server listening on bind.
func localHost(bind string) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		return "127.0.0.1"
	}
	return bind
}

// Admin is root over the instance's socket.
func (m *Server) Admin(s domain.Server) domain.AdminAccess {
	rt := m.RuntimeDir(s.Runtime)
	return domain.AdminAccess{Engine: domain.MySQL, Host: localHost(m.Bind), Socket: m.socket(s), Port: s.Port, User: "root",
		BinDir: filepath.Join(rt, "bin"), LibDir: filepath.Join(rt, "lib")}
}

// Exec runs sql as root over the local socket, passing the SQL on stdin so
// passwords or large statements never appear on the command line or in errors.
func (m *Server) Exec(ctx context.Context, s domain.Server, sql string) error {
	_, err := m.Run.RunInput(ctx, strings.NewReader(sql), m.bin(s, "mysql"), "--socket="+m.socket(s), "-uroot", "--batch")
	return err
}

// allowBackslashEscapes resets NO_BACKSLASH_ESCAPES on the session before
// running SQL built with quoteMySQL, so server sql_mode overrides in my.cnf
// cannot corrupt escaped literals.
const allowBackslashEscapes = "SET SESSION sql_mode = REPLACE(@@SESSION.sql_mode, 'NO_BACKSLASH_ESCAPES', ''); "

var mysqlLiteralReplacer = strings.NewReplacer(
	`\`, `\\`,
	`'`, `''`,
	"\x00", `\0`,
	"\n", `\n`,
	"\r", `\r`,
	"\x1a", `\Z`,
)

// quoteMySQL quotes s as a MySQL single-quoted string literal.
func quoteMySQL(s string) string {
	return "'" + mysqlLiteralReplacer.Replace(s) + "'"
}

func validateLogin(user, password string) error {
	if user == "" {
		return errors.New("mysql login user cannot be empty")
	}
	if strings.ContainsRune(user, 0) || strings.ContainsRune(password, 0) {
		return errors.New("mysql login and password cannot contain NUL bytes")
	}
	return nil
}

// clientPrivileges is what a branch login may do: read and change the data
// and the objects in it. Not FILE (read and write files as the server's OS
// user), SUPER, SHUTDOWN, RELOAD, PROCESS, CREATE USER, the replication
// privileges, or GRANT OPTION.
const clientPrivileges = "SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, REFERENCES, INDEX, ALTER, " +
	"CREATE TEMPORARY TABLES, LOCK TABLES, EXECUTE, CREATE VIEW, SHOW VIEW, CREATE ROUTINE, ALTER ROUTINE, EVENT, TRIGGER"

// CreateUser makes a client account that may connect from anywhere with
// clientPrivileges, syncs its password, and takes back any extra privileges
// an older branch account may have had.
func (m *Server) CreateUser(ctx context.Context, s domain.Server, user, password string) error {
	if err := validateLogin(user, password); err != nil {
		return err
	}
	if err := m.createUser(ctx, s, user, password); err != nil {
		return err
	}
	if !partialRevokes(s.Runtime.Version) {
		return nil
	}
	// INSERT on the mysql schema is enough to CREATE USER, and with UPDATE on it a login
	// could rewrite the grant tables: the login gets nothing in that schema.
	if err := m.Exec(ctx, s, "SET GLOBAL partial_revokes = ON"); err != nil {
		return err
	}
	return m.Exec(ctx, s, fmt.Sprintf(allowBackslashEscapes+"REVOKE %s ON mysql.* FROM %s@'%%'", clientPrivileges, quoteMySQL(user)))
}

// partialRevokes says whether the runtime can take a privilege away on one
// schema while it is granted on all (MySQL 8.0.16 and later).
func partialRevokes(version string) bool {
	series := domain.MySQLSeries(version)
	if !seriesAtLeast(series, "8.0") {
		return false
	}
	if series != "8.0" {
		return true
	}
	base, _, _ := strings.Cut(version, "-")
	parts := strings.Split(base, ".")
	if len(parts) < 3 {
		return false
	}
	patch, err := strconv.Atoi(parts[2])
	return err == nil && patch >= 16
}

func (m *Server) createUser(ctx context.Context, s domain.Server, user, password string) error {
	u, pw := quoteMySQL(user), quoteMySQL(password)
	series := domain.MySQLSeries(s.Runtime.Version)
	if !seriesAtLeast(series, "5.7") {
		// 5.6: no CREATE USER IF NOT EXISTS (GRANT ... IDENTIFIED BY makes the account), and the
		// install script left anonymous accounts and root for other hosts without a password.
		return m.Exec(ctx, s, fmt.Sprintf(allowBackslashEscapes+
			"DELETE FROM mysql.user WHERE User = '' OR (User = 'root' AND Host <> 'localhost'); FLUSH PRIVILEGES; "+
			"GRANT USAGE ON *.* TO %[1]s@'%%' IDENTIFIED BY %[2]s; "+
			"REVOKE ALL PRIVILEGES, GRANT OPTION FROM %[1]s@'%%'; "+
			"GRANT %[3]s ON *.* TO %[1]s@'%%'", u, pw, clientPrivileges))
	}
	// On 8.4 without mysql_native_password and on 9.x, ALTER USER without a
	// plugin fails (Error 1524) if an existing account still uses the unloaded
	// plugin; switching to caching_sha2_password avoids that.
	alterAuth := "IDENTIFIED BY " + pw
	if seriesAtLeast(series, "9.0") || (series == "8.4" && !s.LegacyAuth) {
		alterAuth = "IDENTIFIED WITH caching_sha2_password BY " + pw
	}
	return m.Exec(ctx, s, fmt.Sprintf(allowBackslashEscapes+
		"CREATE USER IF NOT EXISTS %[1]s@'%%' IDENTIFIED BY %[2]s; "+
		"ALTER USER %[1]s@'%%' %[3]s; "+
		"REVOKE ALL PRIVILEGES, GRANT OPTION FROM %[1]s@'%%'; "+
		"GRANT %[4]s ON *.* TO %[1]s@'%%'", u, pw, alterAuth, clientPrivileges))
}

// ExecAs runs sql as the branch login over the local socket. The client runs
// with --binary-mode, which switches off its own commands (system, \!,
// source): they would run on the node as the agent. DELIMITER still works.
// The password goes in the process environment and the SQL on stdin, so
// neither appears on the command line or in error messages.
func (m *Server) ExecAs(ctx context.Context, s domain.Server, user, password, sql string) error {
	if err := validateLogin(user, password); err != nil {
		return err
	}
	// --binary-mode disables \r\n -> \n normalization in the mysql client,
	// which would leave a trailing \r inside DELIMITER directives.
	normalized := strings.ReplaceAll(sql, "\r\n", "\n")
	_, err := m.Run.RunInputEnv(ctx, []string{"MYSQL_PWD=" + password}, strings.NewReader(normalized),
		m.bin(s, "mysql"), "--socket="+m.socket(s), "--binary-mode", "--batch", "--user="+user)
	return err
}

// Load streams a SQL dump (plain or .gz) into the mysql client.
func (m *Server) Load(ctx context.Context, s domain.Server, dumpPath string) error {
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
	_, err = m.Run.RunInput(ctx, in, m.bin(s, "mysql"), "--socket="+m.socket(s), "-uroot", "--batch")
	return err
}

// Remove stops the unit and deletes the instance configuration and logs.
func (m *Server) Remove(ctx context.Context, s domain.Server) error {
	_, _ = m.Run.Run(ctx, "systemctl", "stop", unit(s))
	_, _ = m.Run.Run(ctx, "systemctl", "reset-failed", unit(s))
	return os.RemoveAll(m.dir(s))
}

// startOutput is what the server wrote while starting: its error log since
// offset, or, when mysqld never got as far as writing to it (the binary is
// missing or not executable), the unit's own output from the journal.
func (m *Server) startOutput(ctx context.Context, s domain.Server, offset int64, max int) string {
	if logged := m.tailLogN(s, offset, max); logged != "" {
		return logged
	}
	out, _ := m.Run.Run(ctx, "journalctl", "-u", unit(s), "-n", "10", "--no-pager", "-o", "cat")
	return strings.TrimSpace(string(out))
}

// tailLog returns the error log written after offset (last 2 KiB at most).
func (m *Server) tailLog(s domain.Server, offset int64) string { return m.tailLogN(s, offset, 2048) }

// tailLogN is tailLog with a limit of max bytes.
func (m *Server) tailLogN(s domain.Server, offset int64, max int) string {
	b, _ := os.ReadFile(m.errLog(s))
	if offset < int64(len(b)) {
		b = b[offset:]
	} else {
		b = nil
	}
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return strings.TrimSpace(string(b))
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// UpdateStats runs ANALYZE TABLE on every table (mysqlcheck --analyze).
func (m *Server) UpdateStats(ctx context.Context, s domain.Server) error {
	_, err := m.Run.Run(ctx, m.bin(s, "mysqlcheck"), "--socket="+m.socket(s), "-uroot", "--analyze", "--all-databases", "--silent")
	return err
}
