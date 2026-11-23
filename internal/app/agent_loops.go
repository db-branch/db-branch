package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/notify"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/routing"
)

func expireLoop(ctx context.Context, log *slog.Logger, every time.Duration, f func()) {
	if every == 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

func expireOnce(ctx context.Context, log *slog.Logger, a *App) {
	svc, _ := a.Branches()
	deleted, err := svc.Expire(ctx)
	for _, n := range deleted {
		log.Info("branch expired", "branch", n.String())
	}
	if err != nil {
		log.Error("expiring branches", "err", err)
	}
	if !pruneSnapshots.Load() {
		return
	}
	pruned, err := svc.PruneSnapshots(ctx)
	for _, s := range pruned {
		log.Info("old base snapshot deleted", "snapshot", s)
	}
	if err != nil {
		log.Error("pruning base snapshots", "err", err)
	}
}

// refreshDue starts, as operations (shown in events like base.snapshot), the
// scheduled refreshes of this node's bases that are due. Which are due is the
// use case's decision.
func refreshDue(ctx context.Context, log *slog.Logger, svc *dbbranch.Service, ops *operation.Manager, published func(domain.Name)) {
	due, err := svc.DueRefreshes(ctx)
	if err != nil {
		log.Error("finding due base refreshes", "err", err)
		return
	}
	for _, n := range due {
		if _, busy := ops.Active("base", n.String()); busy {
			continue
		}
		_, err := ops.Start("base.refresh", operation.Target{Resource: "base", Name: n.String()},
			func(ctx context.Context, report func(string)) error {
				err := svc.RefreshBase(ctx, n, report)
				if err != nil {
					log.Error("scheduled base refresh failed", "base", n.String(), "err", err)
				}
				published(n)
				return err
			})
		if err != nil {
			log.Error("starting a base refresh", "base", n.String(), "err", err)
		}
	}
}

// routingLoop keeps the proxy in line with the branches (cheap when nothing
// changed). It syncs as soon as kick fires (a branch changed) and otherwise
// every fallback, so a new branch is reachable through the proxy right after
// it is ready rather than up to one interval later.
func routingLoop(ctx context.Context, rt *routing.Service, kick <-chan struct{}, fallback time.Duration, log *slog.Logger) {
	failing := false
	for {
		changed, err := rt.Sync(ctx)
		switch {
		case err != nil && !failing:
			log.Warn("configuring the proxy", "err", err)
			failing = true
		case err == nil && failing:
			log.Info("proxy configured again")
			failing = false
		case changed:
			log.Debug("proxy configuration applied")
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-time.After(fallback):
		}
	}
}

// branchKicks signals (coalesced, never blocking the bus) whenever a branch
// event is published. A subscription the bus dropped is renewed with a kick,
// since events may have been missed.
func branchKicks(ctx context.Context, bus *operation.Bus) <-chan struct{} {
	kick := make(chan struct{}, 1)
	sub := bus.Subscribe(0)
	go pumpBranchKicks(ctx, bus, sub, kick)
	return kick
}

func pumpBranchKicks(ctx context.Context, bus *operation.Bus, sub *operation.Subscription, kick chan<- struct{}) {
	signal := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	for {
		for open := true; open; {
			select {
			case <-ctx.Done():
				sub.Close()
				return
			case ev, ok := <-sub.Events():
				if !ok {
					open = false
				} else if strings.HasPrefix(ev.Type, "branch.") {
					for drain := true; drain; {
						select {
						case ev, ok = <-sub.Events():
							if !ok {
								open = false
								drain = false
							}
						default:
							drain = false
						}
					}
					signal()
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		// Subscribe before signaling so events after the kick are never missed.
		sub = bus.Subscribe(0)
		signal()
	}
}

func every(ctx context.Context, d time.Duration, f func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
			f()
		}
	}
}

// notifyLoop checks for new notices on every tick while leading returns true
// (always true in single-node mode, and only on the Raft leader in cluster mode).
func notifyLoop(ctx context.Context, interval time.Duration, leading func() bool, n *notify.Service, log *slog.Logger) {
	if interval <= 0 {
		interval = heartbeatEvery
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if leading() {
			if err := n.Check(ctx); err != nil && ctx.Err() == nil {
				log.Warn("sending notices", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
