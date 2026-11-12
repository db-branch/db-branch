package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/interface/httpapi"
	"github.com/db-branch/db-branch/internal/interface/nodeapi"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

// clusterRuntime is the agent's view of the cluster.
type clusterRuntime struct {
	store  *raftstore.Store
	ca     *clusterpki.CA
	self   ClusterConfig
	server *http.Server
	cfg    config.Config
	pool   string
	client *nodeapi.Client
	// changes carries applied commands from Raft to publish.
	changes chan raftstore.Command
	// runtimes lists this node's engines for the heartbeat. Optional.
	runtimes func(ctx context.Context) []string
	// kick asks for a heartbeat now.
	kick chan struct{}
	// replicate asks for a replication pass now.
	replicate chan struct{}
	// syncRuntimes asks for a runtime check now.
	syncRuntimes chan struct{}
	// checkpoints is whether this node's storage supports checkpoints, for the heartbeat.
	checkpoints bool
	// modules are the modules this agent runs, for the heartbeat.
	modules []string
	// moduleChanged is called when the recorded modules change. Optional.
	moduleChanged func()
	// problems returns this node's module problems for the heartbeat. Optional.
	problems func() map[string]string
	// arc returns the ZFS ARC cap in effect (MB) and why the setting could
	// not be applied. Optional.
	arc func() (uint64, string)
	// metrics returns the latest performance sample for the heartbeat. Optional.
	metrics func() *domain.Metrics
	// system returns the node's OS, kernel and ZFS version for the heartbeat.
	// Optional.
	system func(context.Context) domain.NodeSystem
}

// fetchArtifact gets a runtime archive from the leader; the leader itself
// downloads from upstream.
func (c *clusterRuntime) fetchArtifact(ctx context.Context, a domain.Artifact) (io.ReadCloser, error) {
	if c.store.IsLeader() {
		return nil, artifact.ErrUseURL
	}
	id, _ := c.store.Leader()
	n, ok := c.store.State().Nodes[id]
	if id == "" || !ok || n.Address == "" {
		return nil, fmt.Errorf("fetch %s from the leader: %w", a.Ref, domain.ErrNoLeader)
	}
	return c.client.OpenArtifact(ctx, n.Address, a.Ref, a.Arch, a.Part)
}

// AddRuntime implements httpapi.Cluster.
func (c *clusterRuntime) AddRuntime(ctx context.Context, ref domain.RuntimeRef) error {
	return c.store.AddRuntime(ctx, ref)
}

// RemoveRuntime implements httpapi.Cluster.
func (c *clusterRuntime) RemoveRuntime(ctx context.Context, ref domain.RuntimeRef) error {
	return c.store.RemoveRuntime(ctx, ref)
}

func installingHere(ops *operation.Manager, node string, ref domain.RuntimeRef) bool {
	for _, op := range ops.List() {
		if op.State == operation.Running && op.Node == node && op.Kind == "runtime.install" && op.Target.Name == ref.String() {
			return true
		}
	}
	return false
}

type openArtifact = func(ctx context.Context, ref domain.RuntimeRef, arch string, part int) (io.ReadCloser, error)

type sendBase = func(ctx context.Context, base domain.Name, from, to string, w io.Writer) error

// Open implements domain.SnapshotSource over the node API.
func (c *clusterRuntime) Open(ctx context.Context, node string, base domain.Name, from, to string) (io.ReadCloser, error) {
	n, ok := c.store.State().Nodes[node]
	if !ok || n.Address == "" {
		return nil, fmt.Errorf("node %s is not a known cluster member", node)
	}
	return c.client.OpenStream(ctx, n.Address, base, from, to)
}

// replicateNow asks for a replication pass soon (e.g. after a base changed).
func (c *clusterRuntime) replicateNow() {
	select {
	case c.replicate <- struct{}{}:
	default:
	}
}

// beatNow sends a heartbeat soon (e.g. after the engines changed).
func (c *clusterRuntime) beatNow() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// adoptPassword makes the console password cluster-wide: the first node to
// offer one sets it (decided in the Raft log, so a node still catching up
// cannot overwrite it); a node whose own password lost drops its stale
// initial-password file.
func (c *clusterRuntime) adoptPassword(local auth.Store) error {
	mine, err := local.Load()
	if err != nil {
		return nil // nothing to offer; `admin reset-password` sets one
	}
	cur, err := raftstore.AdminStore{Store: c.store}.Init(mine)
	if err != nil {
		return err
	}
	if string(mine.Hash) != string(cur.Hash) {
		_ = os.Remove(InitialPasswordFile)
	}
	return nil
}

var _ httpapi.Cluster = (*clusterRuntime)(nil)

func (c *clusterRuntime) close() {
	_ = c.server.Close()
	_ = c.store.Close()
}

// Status implements httpapi.Cluster.
func (c *clusterRuntime) Status(ctx context.Context) (httpapi.ClusterStatus, error) {
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return httpapi.ClusterStatus{}, err
	}
	leader, _ := c.store.Leader()
	voters := map[string]bool{}
	if servers, err := c.store.Servers(ctx); err == nil {
		for _, sv := range servers {
			voters[sv.ID] = sv.Voter
		}
	}
	st := httpapi.ClusterStatus{Mode: "cluster", Leader: leader, Protocol: c.store.ClusterProtocol(), ServiceIDs: c.store.ServiceIDs()}
	now := time.Now()
	for _, n := range nodes {
		st.Nodes = append(st.Nodes, httpapi.NodeStatus{
			Name: n.Name, Host: n.Host, Address: n.Address, Version: n.Version, Leader: n.Name == leader, Voter: voters[n.Name],
			Down: n.Down(now, NodeDownAfter), LastSeen: n.LastSeen, PoolSize: n.PoolSize, PoolFree: n.PoolFree, Runtimes: n.Runtimes, Modules: n.Modules, Metrics: n.Metrics, ModuleProblems: n.ModuleProblems, ArcMaxMB: n.ArcMaxMB, ArcProblem: n.ArcProblem, System: n.System, ServiceIDs: n.ServiceIDs, IDProblem: n.IDProblem,
			Protocol: max(n.Protocol, 1),
		})
	}
	return st, nil
}

// NewToken implements httpapi.Cluster.
func (c *clusterRuntime) NewToken(ctx context.Context) (string, error) {
	secret := clusterpki.NewSecret()
	if err := c.store.SetJoinSecretHash(ctx, clusterpki.HashSecret(secret)); err != nil {
		return "", err
	}
	return clusterpki.Token{Fingerprint: c.ca.Fingerprint(), Secret: secret}.String(), nil
}

// Forward implements httpapi.Cluster.
func (c *clusterRuntime) Forward(node string) (http.Handler, error) {
	n, ok := c.store.State().Nodes[node]
	if !ok || n.Address == "" {
		return nil, fmt.Errorf("node %s is not a known cluster member", node)
	}
	return c.client.Proxy(n.Address), nil
}

// BranchUsage implements httpapi.Cluster.
func (c *clusterRuntime) BranchUsage(node, branch string) (domain.Usage, bool) {
	u, ok := c.store.State().Nodes[node].Usage[branch]
	return u, ok
}

// NodeCheckpoints implements httpapi.Cluster.
func (c *clusterRuntime) NodeCheckpoints(node string) bool {
	return c.store.State().Nodes[node].Checkpoints
}

// NodeHost implements httpapi.Cluster.
func (c *clusterRuntime) NodeHost(node string) string {
	return c.store.State().Nodes[node].Host
}
