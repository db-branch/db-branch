package postgres

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
)

// NewProvider adapts srv to domain.RuntimeProvider. From extra only
// PrepareRuntime, Query and DumpSource are used (see mysql.NewProvider); the
// PostgreSQL runtime fixup is the default PrepareRuntime.
func NewProvider(srv *Server, extra runtimeprovider.Hooks) *runtimeprovider.Provider {
	spec, ok := domain.DefaultRuntimes.Spec(domain.Postgres)
	if !ok {
		panic("postgres is not registered in domain.DefaultRuntimes")
	}
	prepare := extra.PrepareRuntime
	if prepare == nil {
		prepare = Fixup
	}
	return runtimeprovider.New(spec, srv, runtimeprovider.Hooks{
		PrepareRuntime: prepare,
		PrepareStart:   srv.prepareStart,
		Health:         srv.health,
		Quiesce:        func(context.Context, domain.Server, domain.StopMode) error { return nil },
		VerifyShutdown: srv.verifyShutdown,
		TailLog:        srv.tailLines,
		Query:          extra.Query,
		DumpSource:     extra.DumpSource,
	})
}

// prepareStart writes the env file and says how the unit launches postgres
// (what db-branch-postgres@.service runs, with its Type, KillSignal, KillMode
// and NotifyAccess).
func (p *Server) prepareStart(_ context.Context, s domain.Server) (domain.LaunchPlan, error) {
	if p.CheckOwner != nil {
		if err := p.CheckOwner(s.DataDir); err != nil {
			return domain.LaunchPlan{}, err
		}
	}
	if err := p.writeConfig(s); err != nil {
		return domain.LaunchPlan{}, err
	}
	return domain.LaunchPlan{
		Command: filepath.Join(p.RuntimeDir(s.Runtime), "bin", "postgres"),
		Args: []string{
			"-D", s.DataDir, "-p", fmt.Sprint(s.Port),
			"-c", "listen_addresses=" + p.listen(s),
			"-c", "unix_socket_directories=" + p.sockDir(s),
			"-c", fmt.Sprintf("shared_buffers=%dMB", s.MemoryMB),
			"-c", "jit=off",
		},
		Env:         map[string]string{"LD_LIBRARY_PATH": p.lib(s)},
		ServiceType: "notify",
		// SIGINT is PostgreSQL's fast shutdown: it writes a shutdown checkpoint.
		KillSignal:     "SIGINT",
		KillMode:       "mixed",
		NotifyAccess:   "main",
		TimeoutStopSec: 900,
	}, nil
}

func (p *Server) health(ctx context.Context, s domain.Server) (domain.HealthState, error) {
	out, err := p.Run.Run(ctx, "systemctl", "show", "--property=ActiveState", "--value", unit(s))
	if err != nil {
		return domain.HealthStopped, err
	}
	switch strings.TrimSpace(string(out)) {
	case "active":
		if !p.ready(ctx, s) {
			return domain.HealthStarting, nil
		}
		return domain.HealthReady, nil
	case "activating", "reloading":
		return domain.HealthStarting, nil
	default:
		return domain.HealthStopped, nil
	}
}

// verifyShutdown reads the control file: only "shut down" means the last stop
// wrote its checkpoint and a snapshot starts without recovery.
func (p *Server) verifyShutdown(ctx context.Context, s domain.Server) error {
	name, args := p.tool(s, "pg_controldata", s.DataDir)
	control, err := p.Run.Run(ctx, name, args...)
	state := controlState(string(control))
	if err != nil {
		return fmt.Errorf("%w: %s (cluster state=%q): %v", domain.ErrUncleanShutdown, s.ID, state, err)
	}
	if state != "shut down" {
		return fmt.Errorf("%w: %s (cluster state=%q)", domain.ErrUncleanShutdown, s.ID, state)
	}
	return nil
}

func controlState(control string) string {
	for _, line := range strings.Split(control, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Database cluster state" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (p *Server) tailLines(ctx context.Context, s domain.Server, n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	out, _ := p.Run.Run(ctx, "journalctl", "-u", unit(s), "-n", fmt.Sprint(n), "--no-pager", "-o", "cat")
	return strings.TrimSpace(string(out)), nil
}
