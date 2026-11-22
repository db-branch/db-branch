package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/membership"
	"github.com/db-branch/db-branch/pkg/ext"
)

// NodeStatus is one cluster member as the console draws it.
type NodeStatus struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Address string `json:"address"`
	Version string `json:"version"`
	Leader  bool   `json:"leader"`
	// Voter is whether the node votes in Raft; the others only replicate.
	Voter    bool      `json:"voter"`
	Down     bool      `json:"down"`
	LastSeen time.Time `json:"-"`
	PoolSize uint64    `json:"pool_size"`
	PoolFree uint64    `json:"pool_free"`
	// Runtimes are the engines installed on the node.
	Runtimes []string `json:"runtimes"`
	// Protocol is the newest cluster protocol the node speaks.
	Protocol int `json:"protocol"`
	// Modules are the modules the node runs ("name version").
	Modules []string `json:"modules"`
	// Metrics is the node's latest performance sample.
	Metrics *domain.Metrics `json:"metrics"`
	// ArcMaxMB is the ZFS ARC cap in effect on the node (0: unknown) and
	// ArcProblem why the zfs.arc_max setting could not be applied there.
	ArcMaxMB   uint64 `json:"arc_max_mb"`
	ArcProblem string `json:"arc_problem,omitempty"`
	// ModuleProblems are modules' configuration errors on the node.
	ModuleProblems map[string]string `json:"-"`
	// System is the node's OS, kernel and ZFS version; what an older node
	// does not report reads "unknown".
	System domain.NodeSystem `json:"system"`
	// ServiceIDs are the node's db-branch uid/gid and IDProblem how they
	// differ from the cluster's (empty: they match). Such a node takes no new
	// branches or replicas until fixed.
	ServiceIDs domain.ServiceIDs `json:"service_ids"`
	IDProblem  string            `json:"id_problem,omitempty"`
}

// ClusterStatus is the membership seen from this node.
type ClusterStatus struct {
	Mode   string       `json:"mode"` // "single" or "cluster"
	Leader string       `json:"leader"`
	Nodes  []NodeStatus `json:"nodes"`
	// Protocol is what every node speaks; features needing more wait for the
	// remaining nodes to be upgraded.
	Protocol int `json:"protocol"`
	// ServiceIDs are the db-branch uid/gid every node should use (zero until
	// the cluster has recorded them).
	ServiceIDs domain.ServiceIDs `json:"service_ids"`
}

// Cluster is present when the node runs in cluster mode.
type Cluster interface {
	Status(ctx context.Context) (ClusterStatus, error)
	// NewToken issues a join token (and invalidates the previous one).
	NewToken(ctx context.Context) (string, error)
	// Forward returns a handler that runs /v1 requests on node.
	Forward(node string) (http.Handler, error)
	// NodeHost is how clients reach node's database servers ("" if unknown).
	NodeHost(node string) string
	// BranchUsage is the space a branch on another node uses, as last reported.
	BranchUsage(node, branch string) (domain.Usage, bool)
	// NodeCheckpoints reports whether node, as last reported, supports checkpoints.
	NodeCheckpoints(node string) bool
	// AddRuntime asks every node to install ref; RemoveRuntime withdraws it.
	AddRuntime(ctx context.Context, ref domain.RuntimeRef) error
	RemoveRuntime(ctx context.Context, ref domain.RuntimeRef) error
	// RemoveNode takes a node out of the cluster (leader only); see
	// membership.Remover.
	RemoveNode(ctx context.Context, node string, force bool) (membership.RemoveReport, error)
}

// forwardedHeader marks a request one node passed to another, so that
// disagreeing views of the state cannot bounce it back and forth.
const forwardedHeader = "X-Db-Branch-Forwarded"

// remote reports whether work on something held by node must run elsewhere.
func (s *Server) remote(node string) bool {
	return s.Cluster != nil && node != "" && node != s.Node
}

// forward passes the request to node and reports whether it did (or failed
// trying); false means the caller should handle it here.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, node string) bool {
	if !s.remote(node) {
		return false
	}
	if forwardedFromNode(r) {
		fail(w, fmt.Errorf("%w: forwarded here, but this node sees it on %s", dbbranch.ErrOtherNode, node))
		return true
	}
	rec, err := s.forwardTo(r, node)
	if err != nil {
		fail(w, err)
		return true
	}
	if rec.status == http.StatusAccepted {
		// The operation was recorded through Raft on the other node; wait
		// until it reaches this node so polling it here works at once.
		var op struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(rec.body.Bytes(), &op) == nil && op.ID != "" {
			s.waitOperation(r.Context(), op.ID)
		}
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body.Bytes())
	return true
}

// forwardTo runs r on node and returns the buffered response.
func (s *Server) forwardTo(r *http.Request, node string) (*recorder, error) {
	if s.Node == "" {
		return nil, errors.New("cannot forward without a node name")
	}
	h, err := s.Cluster.Forward(node)
	if err != nil {
		return nil, err
	}
	out := r.Clone(r.Context())
	// Drop hop-by-hop Connection (so ReverseProxy cannot be tricked into stripping
	// our internal forwarding headers) and any non-canonical copies before setting them.
	delHeadersFold(out.Header, "Connection", forwardedHeader, principalHeader)
	out.Header.Set(forwardedHeader, s.Node)
	if b, err := json.Marshal(principalOf(r)); err == nil {
		out.Header.Set(principalHeader, string(b))
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	h.ServeHTTP(rec, out)
	return rec, nil
}

// ForwardWait bounds how long a forwarded request waits for its operation to
// replicate here.
var ForwardWait = 5 * time.Second

func (s *Server) waitOperation(ctx context.Context, id string) {
	deadline := time.Now().Add(ForwardWait)
	for time.Now().Before(deadline) {
		if _, err := s.Ops.Get(id); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// recorder buffers a (small) forwarded response.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.status = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

// hostOf is how clients reach the database servers on node.
func (s *Server) hostOf(node string) string {
	if s.remote(node) {
		if h := s.Cluster.NodeHost(node); h != "" {
			return h
		}
	}
	return s.Host
}

// nodeOf names the node holding something recorded with node.
func (s *Server) nodeOf(node string) string {
	if node == "" {
		return s.Node
	}
	return node
}

type nodeJSON struct {
	NodeStatus
	LastSeen *string `json:"last_seen"`
}

func (s *Server) cluster(w http.ResponseWriter, r *http.Request) {
	var st ClusterStatus
	if s.Cluster != nil {
		var err error
		if st, err = s.Cluster.Status(r.Context()); err != nil {
			fail(w, err)
			return
		}
	} else {
		st = s.single(r.Context())
		if s.Metrics != nil {
			if h := s.Metrics(); len(h) > 0 {
				st.Nodes[0].Metrics = &h[len(h)-1]
			}
		}
		refs, err := s.Runtimes.List(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		for _, ref := range refs {
			st.Nodes[0].Runtimes = append(st.Nodes[0].Runtimes, ref.String())
		}
	}
	nodes := make([]nodeJSON, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		v := nodeJSON{NodeStatus: n}
		v.System = n.System.OrUnknown()
		if v.Runtimes == nil {
			v.Runtimes = []string{}
		}
		if v.Modules == nil {
			v.Modules = []string{}
		}
		if !n.LastSeen.IsZero() {
			t := ts(n.LastSeen)
			v.LastSeen = &t
		}
		nodes = append(nodes, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": st.Mode, "self": s.Node, "leader": st.Leader, "protocol": st.Protocol, "service_ids": st.ServiceIDs, "nodes": nodes})
}

func (s *Server) clusterToken(local bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !local {
			writeError(w, http.StatusForbidden, "socket_only", "run `db-branch cluster token` on a node")
			return
		}
		if s.Cluster == nil {
			writeError(w, http.StatusConflict, "not_clustered", "run `db-branch cluster init` first")
			return
		}
		tok, err := s.Cluster.NewToken(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"token": tok})
	}
}

// clusterRemoveNode takes a failed or retired node out of the cluster. The
// leader does it; a follower passes the request on. With force=true the
// records of what the node held are forgotten too (see membership.Remover).
func (s *Server) clusterRemoveNode(w http.ResponseWriter, r *http.Request) {
	if s.Cluster == nil {
		writeError(w, http.StatusConflict, "not_clustered", "this node is not in a cluster")
		return
	}
	st, err := s.Cluster.Status(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if st.Leader == "" {
		fail(w, domain.ErrNoLeader)
		return
	}
	if s.forward(w, r, st.Leader) {
		return
	}
	rep, err := s.Cluster.RemoveNode(r.Context(), r.PathValue("name"), r.URL.Query().Get("force") == "true")
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// nodeMetrics returns a node's recent samples; each node keeps its own.
func (s *Server) nodeMetrics(w http.ResponseWriter, r *http.Request) {
	if s.forward(w, r, r.PathValue("name")) {
		return
	}
	if r.PathValue("name") != s.Node && s.Cluster == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such node")
		return
	}
	var list []domain.Metrics
	if s.Metrics != nil {
		list = s.Metrics()
	}
	writeJSON(w, http.StatusOK, items(list))
}

// nodeStorage is what a node's storage holds: the pool, and every dataset and
// snapshot under db-branch's root with its space use (the admin's Storage page).
func (s *Server) nodeStorage(w http.ResponseWriter, r *http.Request) {
	if s.forward(w, r, r.PathValue("name")) {
		return
	}
	if r.PathValue("name") != s.Node && s.Cluster == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such node")
		return
	}
	if s.Storage == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "this node does not report its storage")
		return
	}
	rep, err := s.Storage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	rep.Node = s.Node
	writeJSON(w, http.StatusOK, rep)
}

// refreshPrincipal acts for mask runs a refresh starts on other nodes.
var refreshPrincipal = ext.Principal{ID: "refresh", Name: "refresh", Roles: []string{"admin"}}

// StartMaskOn starts req as a base.mask operation on node, a cluster member
// other than this one, the way a forwarded POST /v1/bases/{target}/mask is
// (as "refresh"). It returns once the node has accepted it.
func (s *Server) StartMaskOn(ctx context.Context, node string, req dbbranch.MaskRequest) (err error) {
	defer func() {
		outcome, status := ext.OutcomeOK, http.StatusAccepted
		if err != nil {
			outcome, status = ext.OutcomeFailed, http.StatusBadGateway
		}
		_ = s.sink().Record(ctx, ext.Event{Time: time.Now().UTC(), Node: s.Node, Principal: refreshPrincipal, Action: "base.mask",
			Resource: ext.Resource{Kind: "base", Name: req.Target.String()}, Outcome: outcome, Status: status})
	}()
	if s.Cluster == nil {
		return errors.New("this node is not in a cluster")
	}
	body, _ := json.Marshal(map[string]string{"source": req.Source.String(), "snapshot": req.Snapshot, "masker": req.Masker})
	hr, err := http.NewRequestWithContext(withPrincipal(ctx, refreshPrincipal), http.MethodPost, "/v1/bases/"+req.Target.String()+"/mask", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	rec, err := s.forwardTo(hr, node)
	if err != nil {
		return err
	}
	if rec.status != http.StatusAccepted {
		return fmt.Errorf("node %s refused the mask of %s: %d %s", node, req.Target, rec.status, strings.TrimSpace(rec.body.String()))
	}
	return nil
}
