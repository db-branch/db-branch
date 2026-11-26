package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/raft"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/authfile"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/interface/nodeapi"
	"github.com/db-branch/db-branch/internal/usecase/idalign"
)

// Cluster ports (design.md §8, docs/cluster-plan.md).
const (
	NodeAPIPort = 7070
	RaftPort    = 7071
	// NodeDownAfter is how long without a heartbeat marks a node down.
	NodeDownAfter  = 30 * time.Second
	heartbeatEvery = 10 * time.Second
)

// ClusterConfig is written by `cluster init` / `cluster join` and read by the agent.
type ClusterConfig struct {
	Node     string `json:"node"`
	APIAddr  string `json:"api_address"`
	RaftAddr string `json:"raft_address"`
	// Bootstrap: form a new one-node cluster on first start.
	Bootstrap bool `json:"bootstrap"`
	// JoinVia: ask this member to add us on first start.
	JoinVia string `json:"join_via,omitempty"`
}

func clusterDir(cfg config.Config) string { return filepath.Join(cfg.DataDir, "cluster") }

// LoadClusterConfig returns nil when the node is not part of a cluster.
func LoadClusterConfig(cfg config.Config) (*ClusterConfig, error) {
	b, err := os.ReadFile(filepath.Join(clusterDir(cfg), "cluster.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cc ClusterConfig
	return &cc, json.Unmarshal(b, &cc)
}

func saveClusterConfig(cfg config.Config, cc ClusterConfig) error {
	b, _ := json.MarshalIndent(cc, "", "  ")
	return os.WriteFile(filepath.Join(clusterDir(cfg), "cluster.json"), b, 0o600)
}

func selfConfig(cfg config.Config) (ClusterConfig, []string) {
	host := AdvertiseHost(cfg)
	name := hostname()
	return ClusterConfig{
		Node:     name,
		APIAddr:  net.JoinHostPort(host, strconv.Itoa(NodeAPIPort)),
		RaftAddr: net.JoinHostPort(host, strconv.Itoa(RaftPort)),
	}, []string{name, host}
}

// ClusterInit creates the cluster CA and this node's certificate; the agent
// bootstraps a one-node cluster (importing existing single-node state) on restart.
func (n Node) ClusterInit(ctx context.Context, cfg config.Config) error {
	if cc, err := LoadClusterConfig(cfg); err != nil || cc != nil {
		return errors.Join(errors.New("this node is already part of a cluster"), err)
	}
	ca, err := clusterpki.InitCA(filepath.Join(clusterDir(cfg), "ca"))
	if err != nil {
		return err
	}
	cc, hosts := selfConfig(cfg)
	if _, err := ca.IssueNode(filepath.Join(clusterDir(cfg), "node"), cc.Node, hosts); err != nil {
		return err
	}
	cc.Bootstrap = true
	if err := saveClusterConfig(cfg, cc); err != nil {
		return err
	}
	return n.Restart(ctx)
}

// ClusterJoin fetches the CA from a member using the token, issues this
// node's certificate and lets the agent join as a voter on restart.
func (n Node) ClusterJoin(ctx context.Context, cfg config.Config, addr, token string) error {
	if cc, err := LoadClusterConfig(cfg); err != nil || cc != nil {
		return errors.Join(errors.New("this node is already part of a cluster"), err)
	}
	st := &filestore.Store{Path: filepath.Join(cfg.DataDir, "state.json")}
	bases, _ := st.ListBases(ctx)
	branches, _ := st.ListBranches(ctx)
	if len(bases)+len(branches) > 0 {
		return fmt.Errorf("this node has %d bases and %d branches; delete them before joining (the cluster's state replaces them)", len(bases), len(branches))
	}
	tok, err := clusterpki.ParseToken(token)
	if err != nil {
		return err
	}
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, strconv.Itoa(NodeAPIPort))
	}
	info, err := nodeapi.Join(ctx, addr, tok)
	if err != nil {
		return err
	}
	cc, hosts := selfConfig(cfg)
	if slices.Contains(info.Nodes, cc.Node) {
		return fmt.Errorf("a node named %q is already in the cluster (change this machine's hostname before joining): %w", cc.Node, domain.ErrAlreadyExists)
	}
	host, err := serviceIDHost(cfg)
	if err != nil {
		return err // nothing was stopped
	}
	// Before any data arrives: bases keep their numeric owners when copied
	// between nodes, so this node's db-branch ids must be the cluster's.
	res, err := n.alignServiceIDsOn(ctx, host, info.ServiceIDs)
	if err != nil {
		if !errors.Is(err, idalign.ErrIDTaken) {
			_ = n.Restart(ctx) // services may have been stopped
		}
		return err
	}
	restarted := false
	defer func() {
		if res.Changed && !restarted {
			_ = n.Restart(ctx)
		}
	}()
	ca, err := clusterpki.ParseCA(info.CACert, info.CAKey)
	if err != nil {
		return err
	}
	if err := ca.Save(filepath.Join(clusterDir(cfg), "ca")); err != nil {
		return err
	}
	if _, err := ca.IssueNode(filepath.Join(clusterDir(cfg), "node"), cc.Node, hosts); err != nil {
		return err
	}
	cc.JoinVia = addr
	if err := saveClusterConfig(cfg, cc); err != nil {
		return err
	}
	restarted = true
	return n.Restart(ctx)
}

func listenAddr(addr string, defaultPort int) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if p, err := strconv.Atoi(port); err == nil && p > 0 && p <= 65535 {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return net.JoinHostPort(host, port)
			}
			return ":" + port
		}
	}
	return fmt.Sprintf(":%d", defaultPort)
}

func hasSingleNodeState(cfg config.Config) bool {
	_, err := os.Stat(filepath.Join(cfg.DataDir, "state.json"))
	return err == nil
}

// startCluster brings up Raft and the node API. It returns nil when the node
// is not clustered.
func startCluster(ctx context.Context, cfg config.Config, pool string, api http.Handler, send sendBase, artifacts openArtifact, log *slog.Logger) (*clusterRuntime, error) {
	cc, err := LoadClusterConfig(cfg)
	if err != nil || cc == nil {
		return nil, err
	}
	dir := clusterDir(cfg)
	ca, err := clusterpki.LoadCA(filepath.Join(dir, "ca"))
	if err != nil {
		return nil, err
	}
	cert, err := clusterpki.LoadNode(filepath.Join(dir, "node"), ca)
	if err != nil {
		return nil, err
	}
	tr, err := raftstore.NewTLSTransport(listenAddr(cc.RaftAddr, RaftPort), cc.RaftAddr, cert.ServerTLS(true), cert.ClientTLS(), nil)
	if err != nil {
		return nil, err
	}
	client := nodeapi.NewClient(cert.ClientTLS())
	raftDir := filepath.Join(dir, "raft")
	changes := make(chan raftstore.Command, 4096)
	onApply := func(cmd raftstore.Command) {
		select {
		case changes <- cmd:
		default: // a burst while nobody listens; subscribers resync on reconnect
		}
	}
	store, err := raftstore.Open(raftstore.Options{NodeID: cc.Node, APIAddr: cc.APIAddr, Dir: raftDir, Transport: tr, Forwarder: client, OnApply: onApply})
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	servers, err := store.Servers(ctx)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	unconfigured := len(servers) == 0
	c := &clusterRuntime{store: store, ca: ca, self: *cc, cfg: cfg, pool: pool, client: client, changes: changes, kick: make(chan struct{}, 1), replicate: make(chan struct{}, 1), syncRuntimes: make(chan struct{}, 1), joinFailed: make(chan struct{})}

	l, err := net.Listen("tcp", listenAddr(cc.APIAddr, NodeAPIPort))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("node API listen: %w", err)
	}
	c.server = &http.Server{Handler: (&nodeapi.Server{Store: store, Client: client, CA: ca, API: api, SendBase: send, Artifact: artifacts, ServiceIDs: c.clusterServiceIDs}).Handler(), TLSConfig: cert.ServerTLS(false), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = c.server.ServeTLS(l, "", "") }()

	if unconfigured && cc.Bootstrap {
		if err := store.Bootstrap([]raft.Server{{ID: raft.ServerID(cc.Node), Address: raft.ServerAddress(cc.RaftAddr)}}); err != nil {
			c.close()
			return nil, err
		}
	}
	if unconfigured && cc.JoinVia != "" {
		go c.joinAsMember(ctx, client, log)
	}
	log.Info("cluster member started", "node", cc.Node, "api", cc.APIAddr, "raft", cc.RaftAddr)
	return c, nil
}

func (c *clusterRuntime) failJoin(err error) {
	c.joinFailOnce.Do(func() {
		c.joinErrVal = err
		close(c.joinFailed)
	})
}

func (c *clusterRuntime) waitLeader(ctx context.Context) error {
	for {
		if id, _ := c.store.Leader(); id != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.joinFailed:
			return c.joinErrVal
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// importSingleNodeState carries all single-node state from state.json into
// Raft when a single node becomes the first member of a cluster, then moves
// state.json aside as state.json.imported so restarts do not re-import it.
// Writes are non-destructive to entries already present in Raft, so retrying
// after a partial failure never rolls back newer cluster state.
func (c *clusterRuntime) importSingleNodeState(ctx context.Context, log *slog.Logger) error {
	statePath := filepath.Join(c.cfg.DataDir, "state.json")
	if _, err := os.Stat(statePath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	snap, err := (&filestore.Store{Path: statePath}).Snapshot()
	if err != nil {
		return err
	}
	existing := c.store.State()
	var errs []error
	record := func(what string, err error) {
		if err != nil {
			log.Error("import single-node state", "item", what, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}
	for _, b := range snap.Bases {
		if b.Node == "" {
			b.Node = c.self.Node
		}
		cur, exists := existing.Bases[b.Name.String()]
		if !exists {
			record("base "+b.Name.String(), c.store.PutBase(ctx, b))
		}
		if !b.Anonymous.Inherits() && (!exists || cur.Anonymous.Inherits()) {
			record("base anonymous "+b.Name.String(), c.store.SetBaseAnonymous(ctx, b.Name, b.Anonymous))
		}
		if b.ReplicaNodes != nil && (!exists || cur.ReplicaNodes == nil) {
			record("base replica_nodes "+b.Name.String(), c.store.SetBaseReplicaNodes(ctx, b.Name, b.ReplicaNodes))
		}
		snapNames := make([]string, 0, len(b.Replicas))
		for sn := range b.Replicas {
			snapNames = append(snapNames, sn)
		}
		sort.Strings(snapNames)
		for _, snapName := range snapNames {
			for _, n := range b.Replicas[snapName] {
				if n == "" || n == b.Node {
					continue
				}
				if !exists || !cur.HasReplica(snapName, n) {
					record("base replica "+b.Name.String()+"@"+snapName, c.store.AddReplica(ctx, b.Name, snapName, n))
				}
			}
		}
	}
	for _, b := range snap.Branches {
		if b.Node == "" {
			b.Node = c.self.Node
		}
		if _, exists := existing.Branches[b.Name.String()]; !exists {
			record("branch "+b.Name.String(), c.store.PutBranch(ctx, b))
		}
	}
	for _, u := range snap.RouteUsers {
		if _, exists := existing.RouteUsers[u.Name]; !exists {
			record("route_user "+u.Name, c.store.PutRouteUser(ctx, u))
		}
	}
	for _, m := range snap.Modules {
		if _, exists := existing.Modules[m.Name]; !exists {
			record("module "+m.Name, c.store.PutModule(ctx, m))
		}
	}
	scopes := make([]string, 0, len(snap.Settings))
	for scope := range snap.Settings {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	for _, scope := range scopes {
		vals := snap.Settings[scope]
		if scope == domain.TrialScope {
			if started := domain.TrialStartedIn(vals); !started.IsZero() {
				_, err := c.store.StartTrial(ctx, started)
				record("trial", err)
			}
			continue
		}
		cur := existing.Settings[scope]
		merged := make(map[string]string, len(vals)+len(cur))
		changed := false
		for k, v := range vals {
			merged[k] = v
			if _, ok := cur[k]; !ok {
				changed = true
			}
		}
		for k, v := range cur {
			merged[k] = v
		}
		if changed {
			record("settings "+scope, c.store.PutSettings(ctx, scope, merged))
		}
	}
	for _, t := range snap.Tokens {
		if _, exists := existing.Tokens[t.ID]; !exists {
			record("token "+t.ID, c.store.PutToken(ctx, t))
		}
	}
	cred, credErr := authfile.Store{Path: filepath.Join(c.cfg.DataDir, "auth.json")}.Load()
	usersToImport := mergeSingleNodeUsers(snap.Users, cred, credErr == nil)
	for _, u := range usersToImport {
		if _, exists := existing.Users[u.ID]; !exists {
			record("user "+u.ID, c.store.InsertUser(ctx, u))
		}
	}
	mods := make([]string, 0, len(snap.ModuleData))
	for mod := range snap.ModuleData {
		mods = append(mods, mod)
	}
	sort.Strings(mods)
	for _, mod := range mods {
		keys := make([]string, 0, len(snap.ModuleData[mod]))
		for k := range snap.ModuleData[mod] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, exists := existing.ModuleData[mod][k]; !exists {
				record("module_data "+mod+"/"+k, c.store.PutModuleData(ctx, mod, k, snap.ModuleData[mod][k]))
			}
		}
	}
	for _, r := range snap.BaseRefreshes {
		cur, exists := existing.BaseRefreshes[r.Base.String()]
		if !exists {
			record("base_refresh "+r.Base.String(), c.store.SetBaseRefresh(ctx, r))
		}
		if r.LastRun != nil && (!exists || cur.LastRun == nil) {
			record("base_refresh_run "+r.Base.String(), c.store.RecordBaseRefreshRun(ctx, r.Base, *r.LastRun))
		}
	}
	if existing.Update == nil && snap.Update != nil && !snap.Update.At.IsZero() {
		record("update_check", c.store.PutUpdateCheck(ctx, *snap.Update))
	}
	if existing.Rollout == nil && snap.Rollout != nil {
		record("rollout", c.store.StartRollout(ctx, *snap.Rollout))
	}
	// Keep the single node's anonymous cluster id (sent with licence
	// renewals), so that becoming a cluster does not look like a new one.
	if snap.ClusterID != "" {
		_, err := c.store.InitClusterID(ctx, snap.ClusterID)
		record("cluster_id", err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := os.Rename(statePath, statePath+".imported"); err != nil {
		return fmt.Errorf("moving %s aside: %w", statePath, err)
	}
	log.Info("imported single-node state into the cluster",
		"bases", len(snap.Bases), "branches", len(snap.Branches),
		"settings_scopes", len(snap.Settings), "tokens", len(snap.Tokens),
		"users", len(usersToImport),
		"modules", len(snap.Modules), "refreshes", len(snap.BaseRefreshes))
	return nil
}

func (c *clusterRuntime) joinAsMember(ctx context.Context, client *nodeapi.Client, log *slog.Logger) {
	for {
		err := client.AddMember(ctx, c.self.JoinVia, c.self.Node, c.self.RaftAddr)
		if err == nil {
			log.Info("joined the cluster; the leader promotes it to voter when one is needed", "via", c.self.JoinVia)
			return
		}
		if errors.Is(err, domain.ErrAlreadyExists) {
			log.Error("cannot join the cluster: node name already in use", "node", c.self.Node, "via", c.self.JoinVia, "err", err)
			c.failJoin(err)
			return
		}
		log.Warn("waiting to join the cluster", "via", c.self.JoinVia, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
