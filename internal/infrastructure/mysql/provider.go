package mysql

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
)

// NewProvider adapts srv to domain.RuntimeProvider. From extra only
// PrepareRuntime (the runtime fixup), Query and DumpSource are used: they come
// from packages this one does not depend on, and a feature without its hook is
// declared unsupported.
func NewProvider(srv *Server, extra runtimeprovider.Hooks) *runtimeprovider.Provider {
	spec, ok := domain.DefaultRuntimes.Spec(domain.MySQL)
	if !ok {
		panic("mysql is not registered in domain.DefaultRuntimes")
	}
	return runtimeprovider.New(spec, srv, runtimeprovider.Hooks{
		PrepareRuntime: extra.PrepareRuntime,
		PrepareStart:   srv.prepareStart,
		Health:         srv.health,
		Quiesce:        srv.quiesce,
		VerifyShutdown: srv.verifyShutdown,
		TailLog:        srv.tailLines,
		Query:          extra.Query,
		DumpSource:     extra.DumpSource,
	})
}

// prepareStart writes my.cnf and env and says how the unit launches mysqld
// (what db-branch-mysqld@.service runs).
func (m *Server) prepareStart(_ context.Context, s domain.Server) (domain.LaunchPlan, error) {
	if m.CheckOwner != nil {
		if err := m.CheckOwner(s.DataDir); err != nil {
			return domain.LaunchPlan{}, err
		}
	}
	if err := m.writeConfig(s); err != nil {
		return domain.LaunchPlan{}, err
	}
	return domain.LaunchPlan{
		Command:        m.bin(s, "mysqld"),
		Args:           []string{"--defaults-file=" + m.cnf(s)},
		ServiceType:    "simple",
		KillSignal:     "SIGTERM",
		TimeoutStopSec: 900,
	}, nil
}

func (m *Server) health(ctx context.Context, s domain.Server) (domain.HealthState, error) {
	if ok, _ := m.Running(ctx, s); !ok {
		return domain.HealthStopped, nil
	}
	if _, err := m.Run.Run(ctx, m.bin(s, "mysqladmin"), "--socket="+m.socket(s), "-uroot", "ping"); err != nil {
		return domain.HealthStarting, nil
	}
	return domain.HealthReady, nil
}

// quiesce makes InnoDB finish purge and change-buffer merges before a clean
// stop, so a snapshot afterwards needs no crash recovery.
func (m *Server) quiesce(ctx context.Context, s domain.Server, mode domain.StopMode) error {
	if mode != domain.StopClean {
		return nil
	}
	if ok, _ := m.Running(ctx, s); !ok {
		return nil
	}
	return m.Exec(ctx, s, "SET GLOBAL innodb_fast_shutdown = 0")
}

// verifyShutdown checks the error log says the last stop completed: a
// "Shutdown complete" after the last time the server started or came up.
func (m *Server) verifyShutdown(_ context.Context, s domain.Server) error {
	b, _ := os.ReadFile(m.errLog(s))
	log := string(b)
	up := max(strings.LastIndex(log, "ready for connections"), strings.LastIndex(log, "starting as process"))
	down := strings.LastIndex(log, "Shutdown complete")
	if down < 0 || down < up {
		return fmt.Errorf("%w: %s: the error log does not end with a completed shutdown", domain.ErrUncleanShutdown, s.ID)
	}
	return nil
}

func (m *Server) tailLines(_ context.Context, s domain.Server, n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	b, _ := os.ReadFile(m.errLog(s))
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}
