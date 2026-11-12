package domain

import (
	"context"
	"errors"
	"time"
)

// Node is one machine in the cluster.
type Node struct {
	Name string `json:"name"`
	// Address is the node-to-node API (host:7070).
	Address string `json:"address"`
	// RaftAddress is the Raft transport (host:7071).
	RaftAddress string `json:"raft_address"`
	// Host is what clients use to reach the node's database servers.
	Host     string    `json:"host"`
	Version  string    `json:"version"`
	LastSeen time.Time `json:"last_seen"`
	// PoolSize and PoolFree describe the ZFS pool in bytes.
	PoolSize uint64 `json:"pool_size"`
	PoolFree uint64 `json:"pool_free"`
	MemoryMB uint64 `json:"memory_mb"`
	// ArcMaxMB is the ZFS ARC cap in effect on the node (0: not reported, as
	// by older versions), and ArcProblem why the cap set for the cluster could
	// not be applied there.
	ArcMaxMB   uint64 `json:"arc_max_mb,omitempty"`
	ArcProblem string `json:"arc_problem,omitempty"`
	// Usage is the space each branch on the node uses, by branch name, so
	// every node can show it.
	Usage map[string]Usage `json:"usage,omitempty"`
	// Protocol is the newest cluster protocol the node's binary speaks
	// (0 for binaries from before protocols were reported, meaning 1).
	Protocol int `json:"protocol,omitempty"`
	// Runtimes are the engine versions installed on the node.
	Runtimes []string `json:"runtimes,omitempty"`
	// Modules are the modules the node's agent runs, as "name version".
	Modules []string `json:"modules,omitempty"`
	// Metrics is the node's latest performance sample.
	Metrics *Metrics `json:"metrics,omitempty"`
	// ModuleProblems are modules' last configuration errors (e.g. licence).
	ModuleProblems map[string]string `json:"module_problems,omitempty"`
}

// Usage is a dataset's space use in bytes (see DatasetInfo).
type Usage struct {
	Used       uint64 `json:"used"`
	Referenced uint64 `json:"referenced"`
}

// Down reports whether the node has not been heard from within timeout.
func (n Node) Down(now time.Time, timeout time.Duration) bool {
	return now.Sub(n.LastSeen) > timeout
}

// NodeStore records cluster members.
type NodeStore interface {
	PutNode(ctx context.Context, n Node) error
	ListNodes(ctx context.Context) ([]Node, error)
	DeleteNode(ctx context.Context, name string) error
}

// ErrClusterUpgrading is returned for a change some cluster members cannot
// apply yet; it becomes possible once every node runs a newer version.
var ErrClusterUpgrading = errors.New("not every cluster node supports this yet; upgrade all nodes")

// ClusterProtocol is the protocol every member speaks: the lowest one.
func ClusterProtocol(nodes []Node, own int) int {
	if len(nodes) == 0 {
		return own
	}
	min := 0
	for _, n := range nodes {
		p := n.Protocol
		if p == 0 {
			p = 1
		}
		if min == 0 || p < min {
			min = p
		}
	}
	return min
}

// ErrNoLeader is returned while the cluster has no leader to accept writes.
var ErrNoLeader = errors.New("no cluster leader")

// Metrics is one performance sample of a node.
type Metrics struct {
	Time time.Time `json:"time"`
	// CPU is the share of CPU time in use since the previous sample, 0-100.
	CPU   float64 `json:"cpu"`
	Load1 float64 `json:"load1"`
	// MemUsed is total minus available, in bytes.
	MemTotal uint64 `json:"mem_total"`
	MemUsed  uint64 `json:"mem_used"`
	// NetRx and NetTx are bytes per second over every interface but loopback.
	NetRx float64 `json:"net_rx"`
	NetTx float64 `json:"net_tx"`
	// ARC is the ZFS cache size in bytes (0 without ZFS).
	ARC uint64 `json:"arc"`
}
