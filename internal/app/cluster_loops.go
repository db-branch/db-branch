package app

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/infrastructure/zfs"
	"github.com/db-branch/db-branch/internal/infrastructure/zfsarc"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/membership"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/internal/usecase/runtime"
)

// runtimeLoop installs the runtimes the cluster asks for that this node
// lacks, each as an operation so every console shows the progress.
func (c *clusterRuntime) runtimeLoop(ctx context.Context, rt runtime.Service, ops *operation.Manager, log *slog.Logger) {
	if err := c.waitLeader(ctx); err != nil {
		return
	}
	// Engines installed before installs became cluster-wide (or by hand) are
	// made cluster-wide too, so every node gets them.
	wanted := map[string]bool{}
	for _, r := range c.store.Runtimes() {
		wanted[r.String()] = true
	}
	if mine, err := rt.List(ctx); err == nil {
		for _, r := range mine {
			if !wanted[r.String()] {
				if err := c.store.AddRuntime(ctx, r); err != nil {
					log.Warn("sharing an installed runtime with the cluster", "runtime", r.String(), "err", err)
				}
			}
		}
	}
	for {
		missing, err := rt.Missing(ctx, c.store.Runtimes())
		if err != nil {
			log.Warn("checking runtimes", "err", err)
		}
		for _, ref := range missing {
			if installingHere(ops, c.self.Node, ref) {
				continue
			}
			_, err := ops.Start("runtime.install", operation.Target{Resource: "runtime", Name: ref.String()},
				func(ctx context.Context, report func(string)) error {
					report("downloading")
					err := rt.Install(ctx, ref)
					c.beatNow()
					if errors.Is(err, domain.ErrRuntimeInstalled) {
						return nil
					}
					return err
				})
			if err != nil {
				log.Warn("starting runtime install", "runtime", ref.String(), "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		case <-c.syncRuntimes:
		}
	}
}

// replicationLoop keeps copies of other nodes' bases current.
// A failed pass is retried soon: the usual cause is the base's node not
// having applied the newest snapshot record yet.
func (c *clusterRuntime) replicationLoop(ctx context.Context, run func(context.Context) error, log *slog.Logger) {
	for {
		wait := 30 * time.Second
		if err := run(ctx); err != nil {
			log.Warn("replicating base snapshots", "err", err)
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-c.replicate:
		}
	}
}

func (c *clusterRuntime) heartbeat(ctx context.Context, log *slog.Logger) {
	beat := func() {
		size, free := poolStats(ctx, c.pool)
		n := domain.Node{
			Name: c.self.Node, Address: c.self.APIAddr, RaftAddress: c.self.RaftAddr, Host: AdvertiseHost(c.cfg),
			Version: version, LastSeen: time.Now().UTC().Truncate(time.Second), PoolSize: size, PoolFree: free,
			Protocol: raftstore.Protocol, Checkpoints: c.checkpoints,
		}
		if total, err := zfsarc.System.MemTotal(); err == nil {
			n.MemoryMB = total >> 20
		}
		if c.system != nil {
			n.System = c.system(ctx)
		}
		n.ServiceIDs, n.IDProblem = c.checkServiceIDs(ctx, log)
		if c.runtimes != nil {
			n.Runtimes = c.runtimes(ctx)
		}
		if c.metrics != nil {
			n.Metrics = c.metrics()
		}
		if c.problems != nil {
			if p := c.problems(); len(p) > 0 {
				n.ModuleProblems = p
			}
		}
		if c.arc != nil {
			n.ArcMaxMB, n.ArcProblem = c.arc()
		}
		if c.modules != nil {
			n.Modules = c.modules
		}
		if c.pool != "" {
			n.Usage = branchUsage(ctx, c.pool)
		}
		if err := c.store.PutNode(ctx, n); err != nil {
			log.Debug("heartbeat", "err", err)
		}
	}
	beat()
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		case <-c.kick:
			beat()
		}
	}
}

// version is set by the agent from its build version.
var version = "dev"

func poolStats(ctx context.Context, pool string) (size, free uint64) {
	if pool == "" {
		return 0, 0
	}
	out, err := execx.Runner{}.Run(ctx, "zfs", "get", "-Hp", "-o", "value", "used,available", pool)
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0, 0
	}
	used, _ := strconv.ParseUint(f[0], 10, 64)
	avail, _ := strconv.ParseUint(f[1], 10, 64)
	return used + avail, avail
}

// publish turns replicated changes into events for this node's subscribers.
func (c *clusterRuntime) publish(ctx context.Context, srv *httpapi.Server, bus *operation.Bus) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-c.changes:
			switch cmd.Op {
			case "add_runtime":
				select {
				case c.syncRuntimes <- struct{}{}:
				default:
				}
			case "put_node":
				bus.PublishTo(operation.Audience{Action: "cluster.read"}, "node.updated", map[string]string{"name": cmd.Node.Name})
			case "put_module":
				bus.PublishTo(operation.Audience{Action: "module.read"}, "module.updated", *cmd.Module)
				if c.moduleChanged != nil {
					c.moduleChanged()
				}
			case "delete_module":
				bus.PublishTo(operation.Audience{Action: "module.read"}, "module.updated", map[string]string{"name": cmd.Name})
			case "put_settings":
				bus.PublishTo(operation.Audience{Action: "setting.read"}, "setting.updated", map[string]string{"scope": cmd.Name})
			case "put_update_check", "start_rollout", "record_rollout":
				bus.PublishTo(operation.Audience{Action: "update.read"}, "update.updated", map[string]string{"what": cmd.Op})
			case "put_operation":
				bus.PublishTo(operation.Audience{Owner: cmd.Operation.Owner}, "operation.updated", *cmd.Operation)
			case "put_branch", "insert_branch":
				srv.PublishBranch(ctx, cmd.Branch.Name)
			case "migrate_branch_owner":
				if cmd.BranchOwner != nil {
					if n, err := domain.ParseName(cmd.BranchOwner.Name); err == nil {
						srv.PublishBranch(ctx, n)
					}
				}
			case "delete_branch":
				if n, err := domain.ParseName(cmd.Name); err == nil {
					srv.PublishBranch(ctx, n)
				}
			case "put_base", "insert_base":
				srv.PublishBase(ctx, cmd.Base.Name)
				c.replicateNow()
			case "add_replica":
				if n, err := domain.ParseName(cmd.Replica.Base); err == nil {
					srv.PublishBase(ctx, n)
				}
			case "delete_base":
				if n, err := domain.ParseName(cmd.Name); err == nil {
					srv.PublishBase(ctx, n)
				}
				c.replicateNow()
			}
		}
	}
}

func (c *clusterRuntime) flushPendingRevocations(log *slog.Logger) {
	if c.flushRevocations == nil {
		return
	}
	if err := c.flushRevocations(); err != nil && !errors.Is(err, domain.ErrClusterUpgrading) {
		log.Warn("flushing pending session revocations", "err", err)
	}
}

// healthLoop keeps branch status in line with node health: the leader marks
// branches of silent nodes lost; every node resumes its own lost branches and
// removes local copies of branches recreated elsewhere.
func (c *clusterRuntime) healthLoop(ctx context.Context, svc *dbbranch.Service, log *slog.Logger) {
	for {
		c.flushPendingRevocations(log)
		if c.store.IsLeader() {
			migrateBranchOwners(ctx, svc, log)
			lost, err := svc.MarkLost(ctx)
			for _, n := range lost {
				log.Warn("branch lost with its node", "branch", n.String())
			}
			if err != nil {
				log.Warn("marking lost branches", "err", err)
			}
		}
		if err := svc.Reconcile(ctx); err != nil {
			log.Warn("reconciling branches", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(heartbeatEvery):
		}
	}
}

// membershipLoop keeps an odd, capped number of voters while this node leads
// (see usecase/membership and docs/cluster-plan.md).
func (c *clusterRuntime) membershipLoop(ctx context.Context, log *slog.Logger) {
	svc := &membership.Service{
		Members: c.store, Self: c.self.Node, Nodes: c.store.ListNodes, Clock: time.Now, DownAfter: NodeDownAfter,
	}
	for {
		if c.store.IsLeader() {
			changed, err := svc.Reconcile(ctx)
			if err != nil {
				log.Warn("changing cluster membership", "err", err)
			} else if changed {
				voters := c.store.Voters()
				log.Info("cluster membership changed", "voters", voters)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(heartbeatEvery):
		}
	}
}

// branchUsage is the space each branch uses on this node: its live dataset
// plus the lines and checkpoints set aside (history).
func branchUsage(ctx context.Context, pool string) map[string]domain.Usage {
	z := zfs.New(execx.Runner{})
	usage, err := z.Usage(ctx, pool+"/db-branch/branches")
	if err != nil {
		return nil
	}
	if history, err := z.Usage(ctx, pool+"/db-branch/history"); err == nil {
		for name, h := range history {
			if u, ok := usage[name]; ok {
				u.Used += h.Used
				usage[name] = u
			}
		}
	}
	return usage
}

// svcCheckpoints is whether this node's branch service supports checkpoints
// (false before the node is initialized).
func svcCheckpoints(a *App) bool {
	svc, err := a.Branches()
	return err == nil && svc.CheckpointsAvailable()
}
