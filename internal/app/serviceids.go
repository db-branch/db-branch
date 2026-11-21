package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strconv"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/execx"
	"github.com/db-branch/db-branch/internal/infrastructure/svcids"
	"github.com/db-branch/db-branch/internal/usecase/idalign"
)

// serviceIDHost is this machine's db-branch account and files.
func serviceIDHost(cfg config.Config) (*svcids.Host, error) {
	nd, err := loadNode(cfg)
	if err != nil {
		return nil, err
	}
	pool := ""
	if nd.Storage == "" {
		pool = nd.Pool
	}
	_, sysErr := os.Stat("/run/systemd/system")
	return &svcids.Host{
		Run: execx.Runner{}, Systemd: sysErr == nil, Pool: pool,
		Dirs: []string{cfg.DataDir, "/etc/db-branch"}, Globs: []string{"/run/db-branch*"},
	}, nil
}

// localServiceIDs returns this node's db-branch uid/gid (zero when absent).
func localServiceIDs() domain.ServiceIDs {
	ids, _ := (&svcids.Host{}).Local(context.Background())
	return ids
}

// serviceUID is the uid database servers run as, for the data directory check.
func serviceUID() (int, error) {
	u, err := user.Lookup(ServiceUser)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

// alignServiceIDsOn makes the node's db-branch ids the cluster's.
func (n Node) alignServiceIDsOn(ctx context.Context, host idalign.Host, ids domain.ServiceIDs) (idalign.Result, error) {
	res, err := idalign.Align(ctx, host, ids)
	if err == nil && res.Changed {
		fmt.Fprintf(n.Notes, "Changed the db-branch user and group from %s to %s (the cluster's) and re-owned their files.\n", res.From, res.To)
	}
	return res, err
}

// AlignIDs implements `db-branch cluster align-ids`: it aligns the ids and
// starts the agent again (it was stopped while the ids changed).
func (n Node) AlignIDs(ctx context.Context, cfg config.Config, ids domain.ServiceIDs) (idalign.Result, error) {
	host, err := serviceIDHost(cfg)
	if err != nil {
		return idalign.Result{}, err // nothing was stopped
	}
	res, err := n.alignServiceIDsOn(ctx, host, ids)
	if err != nil && errors.Is(err, idalign.ErrIDTaken) {
		return res, err // nothing was stopped
	}
	if rerr := n.Restart(ctx); rerr != nil {
		err = errors.Join(err, fmt.Errorf("restarting the agent: %w", rerr))
	}
	return res, err
}

// clusterServiceIDs is what a joining node is told: the recorded ids, or, on
// the leader of a cluster that has not recorded any yet, its own.
func (c *clusterRuntime) clusterServiceIDs() domain.ServiceIDs {
	if ids := c.store.ServiceIDs(); ids.Valid() {
		return ids
	}
	if c.store.IsLeader() {
		return localServiceIDs()
	}
	return domain.ServiceIDs{}
}

// checkServiceIDs returns this node's db-branch ids and how they differ from
// the cluster's. The leader of a cluster without ids records its own.
func (c *clusterRuntime) checkServiceIDs(ctx context.Context, log *slog.Logger) (domain.ServiceIDs, string) {
	local := localServiceIDs()
	cluster := c.store.ServiceIDs()
	if !cluster.Valid() && local.Valid() && c.store.IsLeader() {
		if err := c.store.InitServiceIDs(ctx, local); err != nil {
			log.Debug("recording the db-branch ids", "err", err) // e.g. not every node is upgraded yet
		} else {
			cluster = c.store.ServiceIDs()
		}
	}
	return local, domain.IDProblem(local, cluster)
}
