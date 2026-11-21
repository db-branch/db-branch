// Package rtserver runs the instances of an external engine for the core: a
// domain.DBServer made of the engine's runtime module (the Runtime gRPC
// service, which prepares and inspects) and the systemd template unit
// db-branch-rt@, which the core, not the module, starts and stops. The module
// never gets root or systemctl: it writes its own configuration as its own OS
// user, and hands the core a LaunchPlan the core checks before it writes it
// into the instance's drop-in.
package rtserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/rtunit"
)

// Runner runs a command on the node.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Server is the DBServer of one external engine.
type Server struct {
	Engine   domain.EngineSpec
	Provider domain.RuntimeProvider
	Run      Runner
	Units    rtunit.Writer
	// User is the engine's OS user (db-branch-rt-<engine>).
	User string
	// Chown hands a path to user.
	Chown func(user, path string) error
	// Mkdir makes a directory (and its parents). The module runs as its own
	// user and cannot make its state and run directories in root's.
	Mkdir func(path string) error
	// StateRoot holds the instances' state directories, RuntimeRoot the unpacked runtimes.
	StateRoot, RuntimeRoot string
	// Bind is the address instances listen on.
	Bind         string
	StartTimeout time.Duration
	PollInterval time.Duration
}

var (
	_ domain.DBServer = (*Server)(nil)
	_ domain.Upgrader = (*Server)(nil)
)

func unit(s domain.Server) string { return rtunit.UnitName(s.ID) }

func (v *Server) engine() string { return string(v.Engine.Engine) }

func (v *Server) stateDir(s domain.Server) string { return v.StateRoot + "/" + s.ID }
func (v *Server) runDir(s domain.Server) string {
	return rtunit.RunRoot + "/" + v.engine() + "/" + s.ID
}

// Instance is what the module is told about s.
func (v *Server) Instance(s domain.Server) domain.InstanceSpec { return v.instance(s) }

// instance is what the module is told about s.
func (v *Server) instance(s domain.Server) domain.InstanceSpec {
	ports := map[string]int{}
	for _, p := range v.Engine.Protocols {
		if p.Primary {
			ports[p.Name] = s.Port
		}
	}
	return domain.InstanceSpec{
		ID: s.ID, Runtime: s.Runtime, RuntimeDir: v.RuntimeRoot + "/" + v.engine() + "/" + s.Runtime.Version,
		DataDir: s.DataDir, StateDir: v.stateDir(s), RunDir: v.runDir(s), Bind: v.Bind, Ports: ports,
		Limits:      domain.ResourceLimits{MemoryMB: s.MemoryMB},
		Maintenance: s.Maintenance, LegacyAuth: s.LegacyAuth, Extra: s.Extra,
	}
}

// own makes the instance's state and run directories and hands them, with the
// data directory, to the engine's user: the module runs as that user and
// writes its configuration and data there.
func (v *Server) own(s domain.Server) error {
	steps := []struct {
		path string
		make bool
	}{{s.DataDir, false}, {v.stateDir(s), true}, {v.runDir(s), true}}
	for _, st := range steps {
		if st.make && v.Mkdir != nil {
			if err := v.Mkdir(st.path); err != nil {
				return fmt.Errorf("make %s: %w", st.path, err)
			}
		}
		if v.Chown != nil {
			if err := v.Chown(v.User, st.path); err != nil {
				return fmt.Errorf("give %s to %s: %w", st.path, v.User, err)
			}
		}
	}
	return nil
}

func (v *Server) Initialize(ctx context.Context, s domain.Server) error {
	if err := v.own(s); err != nil {
		return err
	}
	return v.Provider.Initialize(ctx, v.instance(s))
}

func (v *Server) PrepareClone(ctx context.Context, s domain.Server) error {
	if err := v.own(s); err != nil {
		return err
	}
	return v.Provider.PrepareClone(ctx, v.instance(s))
}

// Start has the module write its configuration, writes the unit's drop-in
// from the plan the module returned (checked first), starts the unit and
// waits until the module says the server is ready.
func (v *Server) Start(ctx context.Context, s domain.Server) error {
	if err := v.own(s); err != nil {
		return err
	}
	inst := v.instance(s)
	plan, err := v.Provider.PrepareStart(ctx, inst)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", s.ID, err)
	}
	_, err = v.launch(ctx, s, inst, plan)
	return err
}

// CheckUpgrade delegates the pre-check to the runtime module.
func (v *Server) CheckUpgrade(ctx context.Context, s domain.Server, to domain.RuntimeRef, own domain.Login) (domain.UpgradePlan, error) {
	return v.Provider.CheckUpgrade(ctx, v.instance(s), to.Version, own)
}

// StartUpgraded asks the runtime module for the upgrade launch plan on s's new
// runtime, launches the systemd unit the same way Start does, and returns the
// module's upgrade log (falling back to the unit's journal on launch failure).
func (v *Server) StartUpgraded(ctx context.Context, s domain.Server, login domain.Login) (domain.UpgradeLog, error) {
	if err := v.own(s); err != nil {
		return domain.UpgradeLog{}, err
	}
	inst := v.instance(s)
	plan, log, err := v.Provider.StartUpgraded(ctx, inst, login)
	if err != nil {
		return log, err
	}
	if excerpt, err := v.launch(ctx, s, inst, plan); err != nil {
		if log.Excerpt == "" {
			log.Excerpt = excerpt
		}
		return log, err
	}
	return log, nil
}

func (v *Server) launch(ctx context.Context, s domain.Server, inst domain.InstanceSpec, plan domain.LaunchPlan) (string, error) {
	if _, err := v.Units.Write(rtunit.Unit{
		ID: s.ID, Engine: v.engine(), User: v.User, Plan: plan, DataDir: s.DataDir,
		StateDir: inst.StateDir, RunDir: inst.RunDir, Limits: inst.Limits,
		StateRoot: v.StateRoot, RuntimeRoot: v.RuntimeRoot,
	}); err != nil {
		return "", fmt.Errorf("unit of %s: %w", s.ID, err)
	}
	if _, err := v.Run.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return "", fmt.Errorf("daemon-reload: %w", err)
	}
	if _, err := v.Run.Run(ctx, "systemctl", "start", unit(s)); err != nil {
		j := v.journal(ctx, s)
		return j, fmt.Errorf("start %s: %w: %s", s.ID, err, j)
	}
	deadline := time.Now().Add(v.StartTimeout)
	for {
		if h, err := v.Provider.Health(ctx, inst); err == nil && h == domain.HealthReady {
			return "", nil
		}
		if v.exited(ctx, s) {
			j := v.journal(ctx, s)
			return j, domain.StartFailure(s.ID, j)
		}
		if time.Now().After(deadline) {
			j := v.journal(ctx, s)
			return j, fmt.Errorf("%s did not become ready within %s: %s", s.ID, v.StartTimeout, j)
		}
		time.Sleep(v.PollInterval)
	}
}

func (v *Server) exited(ctx context.Context, s domain.Server) bool {
	out, err := v.Run.Run(ctx, "systemctl", "show", "--property=ActiveState", "--value", unit(s))
	st := strings.TrimSpace(string(out))
	return err == nil && (st == "failed" || st == "inactive")
}

func (v *Server) journal(ctx context.Context, s domain.Server) string {
	out, _ := v.Run.Run(ctx, "journalctl", "-u", unit(s), "-n", "20", "--no-pager", "-o", "cat")
	return strings.TrimSpace(string(out))
}

func (v *Server) Running(ctx context.Context, s domain.Server) (bool, error) {
	out, _ := v.Run.Run(ctx, "systemctl", "is-active", unit(s))
	return strings.TrimSpace(string(out)) == "active", nil
}

// Stop stops the unit. StopClean has the module ready the server first
// (flush), then confirms with the module that the shutdown was clean.
func (v *Server) Stop(ctx context.Context, s domain.Server, mode domain.StopMode) error {
	if ok, _ := v.Running(ctx, s); !ok {
		return nil
	}
	if mode == domain.StopImmediate {
		v.kill(ctx, s)
		return nil
	}
	inst := v.instance(s)
	if err := v.Provider.Quiesce(ctx, inst, mode); err != nil {
		return fmt.Errorf("quiesce %s: %w", s.ID, err)
	}
	if _, err := v.Run.Run(ctx, "systemctl", "stop", unit(s)); err != nil {
		return fmt.Errorf("stop %s: %w", s.ID, err)
	}
	return v.Provider.VerifyCleanShutdown(ctx, inst)
}

func (v *Server) kill(ctx context.Context, s domain.Server) {
	_, _ = v.Run.Run(ctx, "systemctl", "kill", "--signal=SIGKILL", unit(s))
	_, _ = v.Run.Run(ctx, "systemctl", "stop", unit(s))
	_, _ = v.Run.Run(ctx, "systemctl", "reset-failed", unit(s))
}

// Exec: the core's own SQL as an admin has no counterpart in the module
// contract, which offers logins and the module's own commands only.
func (v *Server) Exec(context.Context, domain.Server, string) error {
	return domain.FeatureNotSupported("admin_exec")
}

func (v *Server) ExecAs(ctx context.Context, s domain.Server, user, password, sql string) error {
	return v.Provider.ExecAs(ctx, v.instance(s), user, password, sql)
}

func (v *Server) CreateUser(ctx context.Context, s domain.Server, user, password string) error {
	return v.Provider.CreateUser(ctx, v.instance(s), user, password)
}

// Load stages dumpPath into the instance's state directory as a 0600 copy owned
// by the module's OS user, hands the copy to the module, and removes it on return.
func (v *Server) Load(ctx context.Context, s domain.Server, dumpPath string) error {
	staged, err := v.stageDump(ctx, s, dumpPath)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	return v.Provider.Load(ctx, v.instance(s), staged)
}

func (v *Server) stageDump(ctx context.Context, s domain.Server, dumpPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	src, err := os.Open(dumpPath)
	if err != nil {
		return "", fmt.Errorf("open dump %s: %w", dumpPath, err)
	}
	defer src.Close()

	dir := v.stateDir(s)
	if err := v.prepareStageDir(dir); err != nil {
		return "", err
	}
	dst, err := os.CreateTemp(dir, loadTempPattern(dumpPath)) // 0600
	if err != nil {
		return "", fmt.Errorf("create staged dump in %s (check directory permissions and disk space): %w", dir, err)
	}
	staged := dst.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(staged)
		}
	}()

	_, err = io.Copy(dst, ctxReader{ctx: ctx, r: src})
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return "", stageWriteError(dir, err)
	}
	if v.Chown != nil {
		if err := v.Chown(v.User, staged); err != nil {
			return "", fmt.Errorf("give staged dump %s to %s: %w", staged, v.User, err)
		}
	}
	ok = true
	return staged, nil
}

func (v *Server) prepareStageDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("prepare staging directory %s: %w", dir, err)
	}
	if v.Mkdir != nil {
		if err := v.Mkdir(dir); err != nil {
			return fmt.Errorf("prepare staging directory %s: %w", dir, err)
		}
	} else if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("prepare staging directory %s: %w", dir, err)
	}
	if v.Chown != nil {
		if err := v.Chown(v.User, dir); err != nil {
			return fmt.Errorf("give staging directory %s to %s: %w", dir, v.User, err)
		}
	}
	return nil
}

func stageWriteError(dir string, err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("stage dump into %s: not enough disk space (loading an external engine needs room for a temporary copy of the dump): %w", dir, err)
	}
	return fmt.Errorf("stage dump into %s: %w", dir, err)
}

func loadTempPattern(dumpPath string) string {
	base := filepath.Base(dumpPath)
	if strings.HasSuffix(strings.ToLower(base), ".sql.gz") {
		return "load-*.sql.gz"
	}
	if ext := filepath.Ext(base); len(ext) > 1 && !strings.ContainsAny(ext, `*\/`) {
		return "load-*" + ext
	}
	return "load-*"
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Remove stops and forgets the unit, then has the module delete its configuration.
func (v *Server) Remove(ctx context.Context, s domain.Server) error {
	v.kill(ctx, s)
	if err := v.Units.Remove(s.ID); err != nil {
		return err
	}
	_, _ = v.Run.Run(ctx, "systemctl", "daemon-reload")
	return v.Provider.RemoveInstance(ctx, v.instance(s))
}

// Admin is how local tools reach the server; an unreachable module gives none.
func (v *Server) Admin(s domain.Server) domain.AdminAccess {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := v.Provider.AdminAccess(ctx, v.instance(s))
	if err != nil {
		return domain.AdminAccess{}
	}
	return a
}
