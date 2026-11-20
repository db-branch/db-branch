package conformance

import (
	"context"
	"errors"
	"fmt"

	"github.com/db-branch/db-branch/internal/domain"
)

type lifecycleStep struct {
	name string
	run  func(ctx context.Context, srv domain.DBServer, s domain.Server) error
}

func mustRun(ctx context.Context, srv domain.DBServer, s domain.Server, want bool, after string) error {
	ok, err := srv.Running(ctx, s)
	if err != nil {
		return err
	}
	if ok != want {
		if want {
			return fmt.Errorf("not running after %s", after)
		}
		return fmt.Errorf("still running after %s", after)
	}
	return nil
}

func start(after string) func(context.Context, domain.DBServer, domain.Server) error {
	return func(ctx context.Context, srv domain.DBServer, s domain.Server) error {
		if err := srv.Start(ctx, s); err != nil {
			return err
		}
		return mustRun(ctx, srv, s, true, after)
	}
}

var lifecycleSteps = []lifecycleStep{
	{"lifecycle initialize", func(ctx context.Context, srv domain.DBServer, s domain.Server) error {
		return srv.Initialize(ctx, s)
	}},
	// The core repeats CreateUser on start, reset, restore and recovery.
	{"lifecycle create user twice", func(ctx context.Context, srv domain.DBServer, s domain.Server) error {
		for range 2 {
			if err := srv.CreateUser(ctx, s, "conformance", "Conformance0Pw"); err != nil {
				return err
			}
		}
		return nil
	}},
	{"lifecycle start", start("Start")},
	{"lifecycle clean stop", func(ctx context.Context, srv domain.DBServer, s domain.Server) error {
		if err := srv.Stop(ctx, s, domain.StopClean); err != nil {
			return err
		}
		return mustRun(ctx, srv, s, false, "a clean stop")
	}},
	{"lifecycle restart", start("restart")},
	// A clone is a copy of a cleanly stopped instance with its identity removed.
	{"lifecycle clone", func(ctx context.Context, srv domain.DBServer, s domain.Server) error {
		if err := srv.Stop(ctx, s, domain.StopClean); err != nil {
			return err
		}
		if err := srv.PrepareClone(ctx, s); err != nil {
			return err
		}
		return start("clone start")(ctx, srv, s)
	}},
	// Crash consistency: what a snapshot of a running server looks like.
	{"lifecycle crash recovery", func(ctx context.Context, srv domain.DBServer, s domain.Server) error {
		if err := srv.Stop(ctx, s, domain.StopImmediate); err != nil {
			return err
		}
		return start("recovery from a killed server")(ctx, srv, s)
	}},
	{"lifecycle remove", func(ctx context.Context, srv domain.DBServer, s domain.Server) error {
		if err := srv.Stop(ctx, s, domain.StopImmediate); err != nil {
			return err
		}
		return srv.Remove(ctx, s)
	}},
}

// RunLifecycle drives one instance through its life on srv: initialise, create
// the login twice, start, stop cleanly, restart, clone, recover from a kill,
// remove. A step that fails ends the run: the later steps are reported as not
// run, and the instance is stopped and removed regardless.
func RunLifecycle(ctx context.Context, srv domain.DBServer, s domain.Server) []Result {
	var rs []Result
	failed := ""
	for _, st := range lifecycleSteps {
		r := Result{Name: st.name}
		if failed != "" {
			r.Err = errors.New("not run: " + failed + " failed")
		} else if err := st.run(ctx, srv, s); err != nil {
			r.Err = err
			failed = st.name
		}
		rs = append(rs, r)
	}
	if failed != "" {
		_ = srv.Stop(ctx, s, domain.StopImmediate)
		_ = srv.Remove(ctx, s)
	}
	return rs
}
