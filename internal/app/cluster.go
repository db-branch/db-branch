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
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/raft"

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
	// Before any data arrives: bases keep their numeric owners when copied
	// between nodes, so this node's db-branch ids must be the cluster's.
	if _, err := n.alignServiceIDs(ctx, cfg, info.ServiceIDs); err != nil {
		if !errors.Is(err, idalign.ErrIDTaken) {
			_ = n.Restart(ctx) // services may have been stopped
		}
		return err
	}
	ca, err := clusterpki.ParseCA(info.CACert, info.CAKey)
	if err != nil {
		return err
	}
	if err := ca.Save(filepath.Join(clusterDir(cfg), "ca")); err != nil {
		return err
	}
	cc, hosts := selfConfig(cfg)
	if _, err := ca.IssueNode(filepath.Join(clusterDir(cfg), "node"), cc.Node, hosts); err != nil {
		return err
	}
	cc.JoinVia = addr
	if err := saveClusterConfig(cfg, cc); err != nil {
		return err
	}
	return n.Restart(ctx)
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
	tr, err := raftstore.NewTLSTransport(fmt.Sprintf(":%d", RaftPort), cc.RaftAddr, cert.ServerTLS(true), cert.ClientTLS(), nil)
	if err != nil {
		return nil, err
	}
	client := nodeapi.NewClient(cert.ClientTLS())
	raftDir := filepath.Join(dir, "raft")
	fresh := !raftstore.HasState(raftDir)
	changes := make(chan raftstore.Command, 4096)
	onApply := func(cmd raftstore.Command) {
		select {
		case changes <- cmd:
		default: // a burst while nobody listens; subscribers resync on reconnect
		}
	}
	store, err := raftstore.Open(raftstore.Options{NodeID: cc.Node, APIAddr: cc.APIAddr, Dir: raftDir, Transport: tr, Forwarder: client, OnApply: onApply})
	if err != nil {
		return nil, err
	}
	c := &clusterRuntime{store: store, ca: ca, self: *cc, cfg: cfg, pool: pool, client: client, changes: changes, kick: make(chan struct{}, 1), replicate: make(chan struct{}, 1), syncRuntimes: make(chan struct{}, 1)}

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", NodeAPIPort))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("node API listen: %w", err)
	}
	c.server = &http.Server{Handler: (&nodeapi.Server{Store: store, Client: client, CA: ca, API: api, SendBase: send, Artifact: artifacts, ServiceIDs: c.clusterServiceIDs}).Handler(), TLSConfig: cert.ServerTLS(false), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = c.server.ServeTLS(l, "", "") }()

	if fresh && cc.Bootstrap {
		if err := store.Bootstrap([]raft.Server{{ID: raft.ServerID(cc.Node), Address: raft.ServerAddress(cc.RaftAddr)}}); err != nil {
			return nil, err
		}
		go c.importSingleNodeState(ctx, log)
	}
	if fresh && cc.JoinVia != "" {
		go c.joinAsMember(ctx, client, log)
	}
	go c.heartbeat(ctx, log)
	log.Info("cluster member started", "node", cc.Node, "api", cc.APIAddr, "raft", cc.RaftAddr)
	return c, nil
}

func (c *clusterRuntime) waitLeader(ctx context.Context) bool {
	for {
		if id, _ := c.store.Leader(); id != "" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// importSingleNodeState carries bases and branches from state.json into Raft
// when a single node becomes the first member of a cluster.
func (c *clusterRuntime) importSingleNodeState(ctx context.Context, log *slog.Logger) {
	if !c.waitLeader(ctx) {
		return
	}
	old := &filestore.Store{Path: filepath.Join(c.cfg.DataDir, "state.json")}
	bases, _ := old.ListBases(ctx)
	branches, _ := old.ListBranches(ctx)
	for _, b := range bases {
		if b.Node == "" {
			b.Node = c.self.Node
		}
		if err := c.store.PutBase(ctx, b); err != nil {
			log.Error("import base", "base", b.Name.String(), "err", err)
		}
	}
	for _, b := range branches {
		if b.Node == "" {
			b.Node = c.self.Node
		}
		if err := c.store.PutBranch(ctx, b); err != nil {
			log.Error("import branch", "branch", b.Name.String(), "err", err)
		}
	}
	log.Info("imported single-node state into the cluster", "bases", len(bases), "branches", len(branches))
}

func (c *clusterRuntime) joinAsMember(ctx context.Context, client *nodeapi.Client, log *slog.Logger) {
	for {
		err := client.AddMember(ctx, c.self.JoinVia, c.self.Node, c.self.RaftAddr)
		if err == nil {
			log.Info("joined the cluster; the leader promotes it to voter when one is needed", "via", c.self.JoinVia)
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
