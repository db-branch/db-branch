package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

type snapshotJSON struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Public    bool   `json:"public"`
	// Mask: how this snapshot was masked from another base.
	Mask *domain.MaskInfo `json:"mask,omitempty"`
}

type baseJSON struct {
	Name           string         `json:"name"`
	Runtime        string         `json:"runtime"`
	Status         string         `json:"status"`
	StatusReason   *string        `json:"status_reason"`
	Node           string         `json:"node"`
	Port           int            `json:"port"`
	CreatedAt      string         `json:"created_at"`
	LatestSnapshot *string        `json:"latest_snapshot"`
	Snapshots      []snapshotJSON `json:"snapshots"`
	// Replicas lists, by snapshot, the other nodes holding a copy.
	Replicas map[string][]string `json:"replicas"`
	// ReplicaNodes is the setting: null keeps a copy on every node, a list
	// names the other nodes that do (the base's own node is implied).
	ReplicaNodes *[]string `json:"replica_nodes"`
	// Holders says, per node, whether it holds the base ("held"), is pulling
	// it ("pulling"), still holds a copy it is giving up ("draining") or is
	// left out because of its operating system ("skipped", with reason).
	Holders     []holderJSON `json:"holders"`
	Operation   *opRef       `json:"operation"`
	Description string       `json:"description"`
	// Anonymous is what callers without a login may do with the base, on top
	// of the cluster-wide settings; null fields inherit those.
	Anonymous anonymousJSON `json:"anonymous"`
	// Can says which base actions the caller may do on this base (the
	// authorizer is asked with the base's name, so per-base rights show).
	// Absent in events, which carry no caller.
	Can map[string]bool `json:"can,omitempty"`
}

// anonymousJSON: max_branches null inherits (only the cluster-wide cap
// applies), 0 allows none; max_ttl null inherits (the cluster-wide TTL).
type anonymousJSON struct {
	MaxBranches *int    `json:"max_branches"`
	MaxTTL      *string `json:"max_ttl"`
}

func anonymousView(l domain.AnonymousLimits) anonymousJSON {
	out := anonymousJSON{MaxBranches: l.MaxBranches}
	if l.MaxTTL > 0 {
		ttl := l.MaxTTL.String()
		out.MaxTTL = &ttl
	}
	return out
}

type holderJSON struct {
	Node  string `json:"node"`
	State string `json:"state"`
	// Origin marks the node the base lives on.
	Origin bool `json:"origin"`
	// Reason says why a "skipped" node gets no copy (e.g. "other OS:
	// ubuntu-26.04", docs/mixed-os.md) and Distro is that node's distribution.
	Reason string `json:"reason,omitempty"`
	Distro string `json:"distro,omitempty"`
}

// Replica states of a node, as shown in holderJSON.State.
const (
	replicaHeld     = "held"
	replicaPulling  = "pulling"
	replicaDraining = "draining"
	// replicaSkipped: wanted by the setting but never copied there, because
	// a PostgreSQL copy is unusable on another distribution (domain.OriginDistro).
	replicaSkipped = "skipped"
)

// holdersOf lists every node that holds, pulls or is draining the base. nodes
// are all the cluster's nodes (a node that wants a copy but has none yet is
// pulling, unless its distribution rules the copy out: then it is skipped).
func holdersOf(b domain.Base, origin string, nodes []domain.Node) []holderJSON {
	latest, hasLatest := b.LatestSnapshot()
	distros := map[string]string{}
	for _, n := range nodes {
		distros[n.Name] = n.System.Distro
	}
	b.Node = origin
	want := domain.OriginDistro(b, latest, nodes)
	seen := map[string]bool{origin: true}
	out := []holderJSON{{Node: origin, State: replicaHeld, Origin: true}}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	if b.ReplicaNodes != nil {
		names = append(names, *b.ReplicaNodes...)
	}
	for _, ns := range b.Replicas {
		names = append(names, ns...)
	}
	sort.Strings(names)
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		switch {
		case !b.Wants(n):
			if b.HoldsAny(n) {
				out = append(out, holderJSON{Node: n, State: replicaDraining})
			}
		case hasLatest && b.HasReplica(latest.Name, n):
			out = append(out, holderJSON{Node: n, State: replicaHeld})
		case hasLatest && !domain.SameDistro(want, distros[n]):
			out = append(out, holderJSON{Node: n, State: replicaSkipped, Reason: "other OS: " + distros[n], Distro: distros[n]})
		case hasLatest:
			out = append(out, holderJSON{Node: n, State: replicaPulling})
		}
	}
	return out
}

// clusterNodes lists the cluster's nodes with the system they report (this
// node alone when not clustered).
func (s *Server) clusterNodes(ctx context.Context) []domain.Node {
	if s.Cluster == nil {
		n := domain.Node{Name: s.Node}
		if s.System != nil {
			n.System = s.System(ctx)
		}
		return []domain.Node{n}
	}
	st, err := s.Cluster.Status(ctx)
	if err != nil {
		return nil
	}
	return statusNodes(st)
}

// statusNodes is the nodes of a cluster status as the domain sees them.
func statusNodes(st ClusterStatus) []domain.Node {
	out := make([]domain.Node, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		out = append(out, domain.Node{Name: n.Name, System: n.System})
	}
	return out
}

// baseActions are the per-base actions the console decides buttons by.
var baseActions = []string{"base.snapshot", "base.load", "base.delete", "base.describe", "base.publish", "base.sql",
	"base.start", "base.stop", "base.refresh", "base.refresh.read", "base.snapshot.delete", "base.snapshot_delete", "base.replicas"}

// baseViewFor is baseView with the caller's rights on the base.
func (s *Server) baseViewFor(r *http.Request, b domain.Base) baseJSON {
	v := s.baseView(r.Context(), b)
	p, az := principalOf(r), s.authorizer()
	v.Can = map[string]bool{}
	for _, a := range baseActions {
		v.Can[a] = az.Authorize(r.Context(), p, a, ext.Resource{Kind: "base", Name: b.Name.String()}) == nil
	}
	return v
}

func (s *Server) baseView(ctx context.Context, b domain.Base) baseJSON {
	v := baseJSON{
		Name: b.Name.String(), Runtime: b.Runtime.String(), Status: string(b.Status), StatusReason: optString(b.Error),
		Node: s.nodeOf(b.Node), Port: b.Port, CreatedAt: ts(b.CreatedAt), Snapshots: []snapshotJSON{},
		Replicas: map[string][]string{}, Description: b.Description, Anonymous: anonymousView(b.Anonymous),
	}
	for snap, nodes := range b.Replicas {
		v.Replicas[snap] = nodes
	}
	v.ReplicaNodes = b.ReplicaNodes
	v.Holders = holdersOf(b, v.Node, s.clusterNodes(ctx))
	for _, snap := range b.Snapshots {
		v.Snapshots = append(v.Snapshots, snapshotJSON{Name: snap.Name, CreatedAt: ts(snap.CreatedAt), Public: snap.Public, Mask: snap.Mask})
	}
	if latest, ok := b.LatestSnapshot(); ok {
		v.LatestSnapshot = &latest.Name
	}
	if op, ok := s.Ops.Active("base", b.Name.String()); ok {
		v.Operation = &opRef{ID: op.ID, Kind: op.Kind}
	}
	return v
}

func (s *Server) listBases(w http.ResponseWriter, r *http.Request, svc Branches) {
	list, err := svc.ListBases(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name.String() < list[j].Name.String() })
	p := principalOf(r)
	out := make([]baseJSON, 0, len(list))
	for _, b := range list {
		if !allowsTokenBase(p, b.Name.String()) {
			continue
		}
		out = append(out, s.baseViewFor(r, b))
	}
	writeJSON(w, http.StatusOK, items(out))
}

func (s *Server) getBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	b, err := svc.GetBase(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.baseViewFor(r, b))
}

// PublishBase sends the base's current state to event subscribers.
func (s *Server) PublishBase(ctx context.Context, n domain.Name) {
	if svc, err := s.Branches(); err == nil {
		s.publishBaseNow(ctx, svc, n)
	}
}

func (s *Server) publishBase(ctx context.Context, svc Branches, n domain.Name) {
	if s.Cluster == nil {
		s.publishBaseNow(ctx, svc, n)
	}
}

func (s *Server) publishBaseNow(ctx context.Context, svc Branches, n domain.Name) {
	b, err := svc.GetBase(ctx, n)
	if err != nil {
		s.Bus.PublishTo(operation.Audience{Action: "base.read", Base: n.String()}, "base.deleted", map[string]string{"name": n.String()})
		return
	}
	s.Bus.PublishTo(operation.Audience{Action: "base.read", Base: n.String()}, "base.updated", s.baseView(ctx, b))
}

func (s *Server) createBase(w http.ResponseWriter, r *http.Request, svc Branches) {
	var in struct {
		Name    string `json:"name"`
		Runtime string `json:"runtime"`
		Dump    string `json:"dump"`
		// Description says what the base holds. Optional.
		Description string `json:"description,omitempty"`
		// Node to create the base on (the dump is a path there); default here.
		Node string `json:"node"`
		// ReplicaNodes: null or absent keeps a copy on every node, [] on the
		// base's node only, a list on those nodes besides it.
		ReplicaNodes *[]string `json:"replica_nodes"`
	}
	if !decode(w, r, &in) {
		return
	}
	if s.remote(in.Node) {
		body, _ := json.Marshal(in)
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		s.forward(w, r, in.Node)
		return
	}
	n, err := domain.ParseName(in.Name)
	if err != nil {
		fail(w, err)
		return
	}
	rt, err := domain.ParseRuntimeRef(in.Runtime)
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := svc.GetBase(r.Context(), n); err == nil {
		fail(w, fmt.Errorf("base %s: %w", n, domain.ErrAlreadyExists))
		return
	}
	dump, done, err := s.resolveDump(r, in.Dump)
	if err != nil {
		s.failDump(w, err)
		return
	}
	req := dbbranch.CreateBaseRequest{Name: n, Runtime: rt, Dump: dump, Description: in.Description, ReplicaNodes: in.ReplicaNodes}
	s.accepted(w, r, "base.create", operation.Target{Resource: "base", Name: n.String()},
		func(ctx context.Context, report func(string)) error {
			defer done()
			_, err := svc.CreateBase(ctx, req, report)
			s.publishBase(ctx, svc, n)
			return err
		})
}

// forwardBase passes work on a base held by another node to that node, or
// reports the lookup error; false means handle it here.
func (s *Server) forwardBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) bool {
	b, err := svc.GetBase(r.Context(), n)
	if err != nil {
		fail(w, err)
		return true
	}
	return s.forward(w, r, b.Node)
}

func (s *Server) snapshotBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if s.forwardBase(w, r, svc, n) {
		return
	}
	s.accepted(w, r, "base.snapshot", operation.Target{Resource: "base", Name: n.String()},
		func(ctx context.Context, report func(string)) error {
			_, err := svc.SnapshotBase(ctx, n, report)
			s.publishBase(ctx, svc, n)
			return err
		})
}

// maskBase writes a masked copy of another base into this one (a masker
// module does it) and snapshots it.
func (s *Server) maskBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if s.forwardBase(w, r, svc, n) {
		return
	}
	var in struct {
		Source   string `json:"source"`
		Snapshot string `json:"snapshot"`
		Masker   string `json:"masker"`
		Rules    string `json:"rules"`
	}
	if !decode(w, r, &in) {
		return
	}
	source, err := domain.ParseName(in.Source)
	if err != nil {
		fail(w, err)
		return
	}
	if in.Masker == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "masker is required (a masker module, e.g. greenmask)")
		return
	}
	req := dbbranch.MaskRequest{Target: n, Source: source, Snapshot: in.Snapshot, Masker: in.Masker, Rules: in.Rules, By: ownerDisplayName(principalOf(r))}
	s.accepted(w, r, "base.mask", operation.Target{Resource: "base", Name: n.String()},
		func(ctx context.Context, report func(string)) error {
			_, err := svc.MaskBase(ctx, req, report)
			s.publishBase(ctx, svc, n)
			return err
		})
}

// baseCommand runs a quick, synchronous base operation and answers 204.
func (s *Server) baseCommand(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name, f func(Branches, context.Context, domain.Name) error) {
	if s.forwardBase(w, r, svc, n) {
		return
	}
	if err := f(svc, r.Context(), n); err != nil {
		fail(w, err)
		return
	}
	s.publishBase(r.Context(), svc, n)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	s.baseCommand(w, r, svc, n, Branches.StartBase)
}

func (s *Server) stopBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	s.baseCommand(w, r, svc, n, Branches.StopBase)
}

func (s *Server) deleteBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	s.baseCommand(w, r, svc, n, Branches.DeleteBase)
}

// baseSQL runs SQL on the base's server (started if stopped), to change
// its data before the next snapshot.
func (s *Server) baseSQL(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if s.forwardBase(w, r, svc, n) {
		return
	}
	var in struct {
		SQL string `json:"sql"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.SQL) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "sql is empty")
		return
	}
	if err := svc.ExecBase(r.Context(), n, in.SQL); err != nil {
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
			fail(w, err)
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "query_failed", err.Error())
		return
	}
	s.publishBase(r.Context(), svc, n)
	w.WriteHeader(http.StatusNoContent)
}

// patchBase changes what can change about a base: its description.
func (s *Server) patchBase(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in struct {
		Description *string `json:"description"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Description == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "nothing to change: send description")
		return
	}
	b, err := svc.DescribeBase(r.Context(), n, *in.Description)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		fail(w, err)
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.publishBase(r.Context(), svc, n)
	writeJSON(w, http.StatusOK, s.baseViewFor(r, b))
}

func (s *Server) patchSnapshot(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if s.forwardBase(w, r, svc, n) {
		return
	}
	var in struct {
		Public *bool `json:"public"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Public == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "nothing to change: send public")
		return
	}
	b, err := svc.SetSnapshotPublic(r.Context(), n, r.PathValue("snapshot"), *in.Public)
	if err != nil {
		fail(w, err)
		return
	}
	s.publishBase(r.Context(), svc, n)
	writeJSON(w, http.StatusOK, s.baseViewFor(r, b))
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if s.forwardBase(w, r, svc, n) {
		return
	}
	if err := svc.DeleteSnapshot(r.Context(), n, r.PathValue("snapshot")); err != nil {
		fail(w, err)
		return
	}
	s.publishBase(r.Context(), svc, n)
	w.WriteHeader(http.StatusNoContent)
}

// putReplicas chooses the nodes that keep a copy of the base. The body must
// carry replica_nodes: null for every node, [] for the base's node only.
func (s *Server) putReplicas(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in struct {
		ReplicaNodes json.RawMessage `json:"replica_nodes"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.ReplicaNodes) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "send replica_nodes: null for every node, [] for the base's node only, or a list of nodes")
		return
	}
	var nodes *[]string
	if err := json.Unmarshal(in.ReplicaNodes, &nodes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "replica_nodes must be null or a list of node names")
		return
	}
	b, err := svc.SetReplicaNodes(r.Context(), n, nodes)
	if err != nil {
		fail(w, err)
		return
	}
	s.publishBase(r.Context(), svc, n)
	writeJSON(w, http.StatusOK, s.baseViewFor(r, b))
}

// patchAnonymous changes what callers without a login may do with the base:
// max_branches (a number, 0 for none, null to inherit the cluster-wide cap)
// and max_ttl (a duration such as 4h, null to inherit). Keys left out stay as
// they are. It does not make a snapshot public (see patchSnapshot).
func (s *Server) patchAnonymous(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in struct {
		MaxBranches json.RawMessage `json:"max_branches"`
		MaxTTL      json.RawMessage `json:"max_ttl"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.MaxBranches) == 0 && len(in.MaxTTL) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "nothing to change: send max_branches and/or max_ttl (null: inherit the cluster-wide setting)")
		return
	}
	b, err := svc.GetBase(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	l := b.Anonymous.Clone()
	if len(in.MaxBranches) > 0 {
		l.MaxBranches = nil
		if string(in.MaxBranches) != "null" {
			var v int
			if err := json.Unmarshal(in.MaxBranches, &v); err != nil {
				fail(w, fmt.Errorf("%w: max_branches must be a whole number, or null to inherit", domain.ErrInvalidAnonymous))
				return
			}
			l.MaxBranches = &v
		}
	}
	if len(in.MaxTTL) > 0 {
		l.MaxTTL = 0
		if string(in.MaxTTL) != "null" {
			var v string
			d, err := time.ParseDuration("x")
			if json.Unmarshal(in.MaxTTL, &v) == nil {
				d, err = time.ParseDuration(v)
			}
			if err != nil || d <= 0 {
				fail(w, fmt.Errorf("%w: max_ttl must be a positive duration such as 4h, or null to inherit", domain.ErrInvalidAnonymous))
				return
			}
			l.MaxTTL = d
		}
	}
	b, err = svc.SetAnonymous(r.Context(), n, l)
	if err != nil {
		fail(w, err)
		return
	}
	s.publishBase(r.Context(), svc, n)
	writeJSON(w, http.StatusOK, s.baseViewFor(r, b))
}
