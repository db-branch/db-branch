package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// requestCounter counts API requests by action and outcome on this node.
type requestCounter struct {
	mu sync.Mutex
	n  map[[2]string]uint64
}

func (c *requestCounter) add(action, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[[2]string]uint64{}
	}
	c.n[[2]string{action, outcome}]++
}

func (c *requestCounter) snapshot() map[[2]string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[[2]string]uint64, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

// status is the cluster as this node sees it (one node when single).
func (s *Server) status(ctx context.Context) (ClusterStatus, error) {
	if s.Cluster != nil {
		return s.Cluster.Status(ctx)
	}
	return s.single(ctx), nil
}

// single is the cluster of one node.
func (s *Server) single(ctx context.Context) ClusterStatus {
	n := NodeStatus{Name: s.Node, Host: s.Host, Version: s.Version, Leader: true, Voter: true, Modules: s.Modules}
	if s.System != nil {
		n.System = s.System(ctx)
	}
	if s.PoolStats != nil {
		n.PoolSize, n.PoolFree = s.PoolStats(ctx)
	}
	if s.ModuleProblems != nil {
		n.ModuleProblems = s.ModuleProblems()
	}
	return ClusterStatus{Mode: "single", Leader: s.Node, Nodes: []NodeStatus{n}}
}

// promWriter writes the Prometheus text exposition format.
type promWriter struct{ b strings.Builder }

func (p *promWriter) family(name, typ, help string) {
	fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (p *promWriter) sample(name string, labels map[string]string, v any) {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	if len(parts) > 0 {
		fmt.Fprintf(&p.b, "%s{%s} %v\n", name, strings.Join(parts, ","), v)
		return
	}
	fmt.Fprintf(&p.b, "%s %v\n", name, v)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// metrics serves the cluster's state for Prometheus: nodes, pools,
// branches, bases, replication, module problems and this node's API
// requests. Alerting is Prometheus's job (see docs/monitoring.md).
func (s *Server) metrics(w http.ResponseWriter, r *http.Request, svc Branches) {
	ctx := r.Context()
	st, err := s.status(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	branches, err := svc.ListBranches(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	bases, err := svc.ListBases(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	caller := principalOf(r)
	if len(ext.TokenBases(caller)) > 0 {
		allowedBranches := make([]domain.Branch, 0, len(branches))
		for _, b := range branches {
			if allowsTokenBase(caller, b.Base.String()) {
				allowedBranches = append(allowedBranches, b)
			}
		}
		branches = allowedBranches
		allowedBases := make([]domain.Base, 0, len(bases))
		for _, b := range bases {
			if allowsTokenBase(caller, b.Name.String()) {
				allowedBases = append(allowedBases, b)
			}
		}
		bases = allowedBases
	}
	var p promWriter
	p.family("dbbranch_build_info", "gauge", "The serving node and its version.")
	p.sample("dbbranch_build_info", map[string]string{"node": s.Node, "version": s.Version}, 1)

	p.family("dbbranch_node_up", "gauge", "1 when the node answers heartbeats.")
	up := map[string]bool{}
	for _, n := range st.Nodes {
		up[n.Name] = !n.Down
		p.sample("dbbranch_node_up", map[string]string{"node": n.Name}, b2i(!n.Down))
	}
	p.family("dbbranch_node_leader", "gauge", "1 for the Raft leader.")
	for _, n := range st.Nodes {
		p.sample("dbbranch_node_leader", map[string]string{"node": n.Name}, b2i(n.Leader || n.Name == st.Leader))
	}
	p.family("dbbranch_pool_size_bytes", "gauge", "Size of the node's ZFS pool.")
	for _, n := range st.Nodes {
		if n.PoolSize > 0 {
			p.sample("dbbranch_pool_size_bytes", map[string]string{"node": n.Name}, n.PoolSize)
		}
	}
	p.family("dbbranch_pool_free_bytes", "gauge", "Free space in the node's ZFS pool.")
	for _, n := range st.Nodes {
		if n.PoolSize > 0 {
			p.sample("dbbranch_pool_free_bytes", map[string]string{"node": n.Name}, n.PoolFree)
		}
	}

	p.family("dbbranch_branches", "gauge", "Branches by node and status.")
	byNode := map[[2]string]int{}
	for _, b := range branches {
		byNode[[2]string{s.nodeOf(b.Node), string(b.Status)}]++
	}
	for _, k := range sortedPairs(byNode) {
		p.sample("dbbranch_branches", map[string]string{"node": k[0], "status": k[1]}, byNode[k])
	}

	p.family("dbbranch_bases", "gauge", "Bases by status.")
	byStatus := map[string]int{}
	for _, b := range bases {
		byStatus[string(b.Status)]++
	}
	statuses := make([]string, 0, len(byStatus))
	for k := range byStatus {
		statuses = append(statuses, k)
	}
	sort.Strings(statuses)
	for _, k := range statuses {
		p.sample("dbbranch_bases", map[string]string{"status": k}, byStatus[k])
	}
	p.family("dbbranch_base_snapshots", "gauge", "Snapshots each base keeps.")
	for _, b := range bases {
		p.sample("dbbranch_base_snapshots", map[string]string{"base": b.Name.String()}, len(b.Snapshots))
	}
	nodes := statusNodes(st)
	p.family("dbbranch_base_replicas_missing", "gauge", "Up nodes that do not hold the base's latest snapshot yet (replication lag).")
	for _, b := range bases {
		latest, ok := b.LatestSnapshot()
		if !ok {
			continue
		}
		have := map[string]bool{}
		for _, n := range b.Holders(latest.Name) {
			have[s.nodeOf(n)] = true
		}
		// Only nodes that are to get a copy count: not those the replica
		// list leaves out, nor those of another distribution (see holdersOf).
		origin := b
		origin.Node = s.nodeOf(b.Node)
		want := domain.OriginDistro(origin, latest, nodes)
		missing := 0
		for _, n := range nodes {
			if up[n.Name] && !have[n.Name] && b.Wants(n.Name) && domain.SameDistro(want, n.System.Distro) {
				missing++
			}
		}
		p.sample("dbbranch_base_replicas_missing", map[string]string{"base": b.Name.String()}, missing)
	}

	p.family("dbbranch_module_problem", "gauge", "1 when a module reports a problem on a node (e.g. its license).")
	for _, n := range st.Nodes {
		names := make([]string, 0, len(n.ModuleProblems))
		for m := range n.ModuleProblems {
			names = append(names, m)
		}
		sort.Strings(names)
		for _, m := range names {
			p.sample("dbbranch_module_problem", map[string]string{"node": n.Name, "module": m}, 1)
		}
	}

	p.family("dbbranch_api_requests_total", "counter", "API requests this node answered, by action and outcome.")
	counts := s.requests.snapshot()
	keys := make([][2]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		p.sample("dbbranch_api_requests_total", map[string]string{"action": k[0], "outcome": k[1]}, counts[k])
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(p.b.String()))
}

func sortedPairs(m map[[2]string]int) [][2]string {
	keys := make([][2]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+"\x00"+keys[i][1] < keys[j][0]+"\x00"+keys[j][1] })
	return keys
}

// health answers load balancers and process supervisors without a login:
// /healthz when the agent serves, /readyz when it can also act (set up,
// every wanted module loaded, and the cluster has a leader).
func (s *Server) health(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case pathHealthz:
		_, _ = w.Write([]byte("ok\n"))
		return true
	case pathReadyz:
		if _, err := s.Branches(); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return true
		}
		if len(s.pending()) > 0 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return true
		}
		if s.Cluster != nil {
			st, err := s.Cluster.Status(r.Context())
			if err != nil || st.Leader == "" {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return true
			}
		} else if s.Node == "" {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return true
		}
		_, _ = w.Write([]byte("ready\n"))
		return true
	}
	return false
}
