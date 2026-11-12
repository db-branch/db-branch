package mysql

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

	StartTimeout time.Duration
	PollInterval time.Duration
}

var _ domain.DBServer = (*Server)(nil)

func (m *Server) dir(s domain.Server) string    { return filepath.Join(m.StateRoot, s.ID) }
func (m *Server) cnf(s domain.Server) string    { return filepath.Join(m.dir(s), "my.cnf") }
func (m *Server) errLog(s domain.Server) string { return filepath.Join(m.dir(s), "error.log") }
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
	cnf := strings.Join([]string{
		"# Written by db-branch. Changes are overwritten.",
		"[mysqld]",
		"basedir=" + m.RuntimeDir(s.Runtime),
		"datadir=" + s.DataDir,
		fmt.Sprintf("port=%d", s.Port),
		"bind-address=" + m.Bind,
		"socket=" + m.socket(s),
		"pid-file=" + filepath.Join(m.RunRoot, s.ID, "mysqld.pid"),
		"log-error=" + m.errLog(s),
		"mysqlx=OFF",
		"skip-name-resolve",
		fmt.Sprintf("innodb_buffer_pool_size=%dM", s.MemoryMB),
		// ZFS is copy-on-write: torn pages cannot happen, so doublewrite only costs writes.
		"innodb_doublewrite=OFF",
		"innodb_flush_neighbors=0",
		"",
	}, "\n")
	if err := os.WriteFile(m.cnf(s), []byte(cnf), 0o640); err != nil {
		return err
	}
	env := "MYSQLD=" + m.bin(s, "mysqld") + "\n"
	if err := os.WriteFile(filepath.Join(m.dir(s), "env"), []byte(env), 0o640); err != nil {
		return err
	}
	return m.chown(m.dir(s), m.cnf(s), filepath.Join(m.dir(s), "env"))
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
	_, err := m.Run.Run(ctx, m.bin(s, "mysqld"), "--defaults-file="+m.cnf(s), "--initialize-insecure", "--user=db-branch")
	return err
}

// PrepareClone drops the cloned server identity so every branch gets its own server UUID.
func (m *Server) PrepareClone(_ context.Context, s domain.Server) error {
	stale, _ := filepath.Glob(filepath.Join(s.DataDir, "*.pid"))
	for _, p := range append(stale, filepath.Join(s.DataDir, "auto.cnf")) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return m.writeConfig(s)
}

// Start starts the unit and waits until mysqld answers on its socket.
func (m *Server) Start(ctx context.Context, s domain.Server) error {
	if err := m.writeConfig(s); err != nil {
		return err
	}
	if _, err := m.Run.Run(ctx, "systemctl", "start", unit(s)); err != nil {
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	deadline := time.Now().Add(m.StartTimeout)
	for {
		if _, err := m.Run.Run(ctx, m.bin(s, "mysqladmin"), "--socket="+m.socket(s), "-uroot", "ping"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become ready within %s: %s", s.ID, m.StartTimeout, m.tailLog(s, 0))
		}
		time.Sleep(m.PollInterval)
	}
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
	if ok, _ := m.Running(ctx, s); !ok {
		return nil
	}
	if mode == domain.StopImmediate {
		_, _ = m.Run.Run(ctx, "systemctl", "kill", "--signal=SIGKILL", unit(s))
		_, _ = m.Run.Run(ctx, "systemctl", "stop", unit(s))
		_, _ = m.Run.Run(ctx, "systemctl", "reset-failed", unit(s))
		return nil
	}
	if err := m.Exec(ctx, s, "SET GLOBAL innodb_fast_shutdown = 0"); err != nil {
		return err
	}
	offset := fileSize(m.errLog(s))
	if _, err := m.Run.Run(ctx, "systemctl", "stop", unit(s)); err != nil {
		return fmt.Errorf("stop %s: %w", s.ID, err)
	}
	result, _ := m.Run.Run(ctx, "systemctl", "show", "--property=Result", "--value", unit(s))
	logged := m.tailLog(s, offset)
	if strings.TrimSpace(string(result)) != "success" || !strings.Contains(logged, "Shutdown complete") {
		return fmt.Errorf("%w: %s (result=%s): %s", domain.ErrUncleanShutdown, s.ID, strings.TrimSpace(string(result)), logged)
	}
	return nil
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

// Exec runs sql as root over the local socket.
func (m *Server) Exec(ctx context.Context, s domain.Server, sql string) error {
	_, err := m.Run.Run(ctx, m.bin(s, "mysql"), "--socket="+m.socket(s), "-uroot", "--batch", "-e", sql)
	return err
}

// CreateUser makes a client account that may connect from anywhere with
// every privilege. Names are branch names and passwords letters and digits,
// so neither needs escaping inside the quotes.
func (m *Server) CreateUser(ctx context.Context, s domain.Server, user, password string) error {
	return m.Exec(ctx, s, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL PRIVILEGES ON *.* TO '%s'@'%%' WITH GRANT OPTION",
		user, password, user))
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

// tailLog returns the error log written after offset (last 2 KiB at most).
func (m *Server) tailLog(s domain.Server, offset int64) string {
	b, _ := os.ReadFile(m.errLog(s))
	if offset < int64(len(b)) {
		b = b[offset:]
	} else {
		b = nil
	}
	if len(b) > 2048 {
		b = b[len(b)-2048:]
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
