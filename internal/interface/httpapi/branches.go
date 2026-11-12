package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

type storageJSON struct {
	Used       uint64 `json:"used"`
	Referenced uint64 `json:"referenced"`
}

type connectionJSON struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
}

type opRef struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type branchJSON struct {
	Name         string         `json:"name"`
	Status       string         `json:"status"`
	StatusReason *string        `json:"status_reason"`
	Base         string         `json:"base"`
	Snapshot     string         `json:"snapshot"`
	Runtime      string         `json:"runtime"`
	Node         string         `json:"node"`
	CreatedAt    string         `json:"created_at"`
	ExpiresAt    *string        `json:"expires_at"`
	ResetAt      *string        `json:"reset_at"`
	Storage      *storageJSON   `json:"storage"`
	Connection   connectionJSON `json:"connection"`
	// Proxy is how to reach the branch through this node's proxy, by user name.
	Proxy     *connectionJSON `json:"proxy"`
	Operation *opRef          `json:"operation"`
	// Owner manages the branch; ID is "owner/name" ("name" without an owner).
	Owner  *string `json:"owner"`
	ID     string  `json:"id"`
	Access string  `json:"access"`
	// Record: recording was asked for at creation (the history module records).
	Record bool `json:"record"`
	// Checkpoints is whether the node holding the branch can take checkpoints.
	Checkpoints bool `json:"checkpoints"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optTS(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Server) branchView(ctx context.Context, svc Branches, b domain.Branch) branchJSON {
	v := branchJSON{
		Name: b.Name.String(), Status: string(b.Status), StatusReason: optString(b.Error),
		Base: b.Base.String(), Snapshot: b.Snapshot, Runtime: b.Runtime.String(), Node: s.nodeOf(b.Node),
		CreatedAt: ts(b.CreatedAt), ExpiresAt: optTS(b.ExpiresAt), ResetAt: optTS(b.ResetAt),
		Connection: connectionJSON{Host: s.hostOf(b.Node), Port: b.Port, User: b.User},
		Owner:      optString(b.Owner), ID: b.Name.String(), Access: domain.AccessPrivate, Record: b.Record,
	}
	if b.Owner != "" {
		// An email owner shows as its local part; owner keeps the address.
		short, _, _ := strings.Cut(b.Owner, "@")
		v.ID = short + "/" + b.Name.String()
	}
	if b.Open() {
		v.Access = domain.AccessOpen
	}
	if s.routable(b) {
		v.Proxy = &connectionJSON{Host: s.Host, Port: s.ProxyPorts[b.Runtime.Engine], User: b.User}
	}
	if s.remote(b.Node) {
		v.Checkpoints = s.Cluster.NodeCheckpoints(b.Node)
		if u, ok := s.Cluster.BranchUsage(b.Node, b.Name.String()); ok {
			v.Storage = &storageJSON{Used: u.Used, Referenced: u.Referenced}
		}
	} else {
		v.Checkpoints = svc.CheckpointsAvailable()
		if info, err := svc.BranchStorage(ctx, b.Name); err == nil {
			v.Storage = &storageJSON{Used: info.Used, Referenced: info.Referenced}
		}
	}
	if op, ok := s.Ops.Active("branch", b.Name.String()); ok {
		v.Operation = &opRef{ID: op.ID, Kind: op.Kind}
	}
	return v
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request, svc Branches) {
	list, err := svc.ListBranches(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name.String() < list[j].Name.String() })
	out := make([]branchJSON, 0, len(list))
	for _, b := range list {
		out = append(out, s.branchView(r.Context(), svc, b))
	}
	writeJSON(w, http.StatusOK, items(out))
}

func (s *Server) getBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	b, err := svc.GetBranch(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.branchView(r.Context(), svc, b))
}

func (s *Server) branchDSN(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	b, err := svc.GetBranch(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	if r.URL.Query().Get("via") == "proxy" {
		if !s.routable(b) {
			writeError(w, http.StatusConflict, "not_routable", "this branch cannot be reached through the proxy here (no proxy, or the branch has no user of its own yet)")
			return
		}
		via := b
		via.Port = s.ProxyPorts[b.Runtime.Engine]
		writeJSON(w, http.StatusOK, map[string]string{"dsn": via.DSN(s.Host)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"dsn": b.DSN(s.hostOf(b.Node))})
}

type createBranchJSON struct {
	Owner    string  `json:"owner,omitempty"`
	Access   string  `json:"access,omitempty"`
	Name     string  `json:"name"`
	Base     string  `json:"base"`
	Snapshot *string `json:"snapshot"`
	InitSQL  string  `json:"init_sql"`
	TTL      string  `json:"ttl"`
	Node     string  `json:"node,omitempty"`
	// Record asks for the branch's changes to be recorded (needs the history
	// module, a login, and a MySQL base).
	Record bool `json:"record,omitempty"`
}

func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%w: ttl %q: use a positive duration such as 72h, or omit it", domain.ErrInvalidTTL, s)
	}
	return d, nil
}

func (s *Server) createBranch(w http.ResponseWriter, r *http.Request, svc Branches) {
	var in createBranchJSON
	if !decode(w, r, &in) {
		return
	}
	// Callers without a login have an hourly allowance per address; a request
	// refused for any other reason gives its place back. A request another
	// node forwarded was counted there already.
	refund := func() {}
	if principalOf(r).ID == Anonymous.ID && !forwardedFromNode(r) {
		var err error
		if refund, err = s.chargeAnonymous(r); err != nil {
			fail(w, err)
			return
		}
	}
	req, err := func() (dbbranch.CreateBranchRequest, error) {
		base, err := domain.ParseName(in.Base)
		if err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		ttl, err := parseTTL(in.TTL)
		if in.TTL == "" && s.DefaultTTL != nil {
			ttl = s.DefaultTTL()
		}
		if err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		if err := s.branchOwner(r.Context(), svc, principalOf(r), &in, &ttl); err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		if err := s.userBranchName(principalOf(r), r, &in); err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		n, err := domain.ParseName(in.Name)
		if err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		if err := s.checkBranchPolicy(r.Context(), svc, principalOf(r), in, ttl); err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		if in.Record {
			if err := s.checkRecord(r.Context(), svc, principalOf(r), in); err != nil {
				return dbbranch.CreateBranchRequest{}, err
			}
		}
		req := dbbranch.CreateBranchRequest{Name: n, Base: base, InitSQL: in.InitSQL, TTL: ttl, Node: in.Node, Owner: in.Owner, Access: in.Access, Record: in.Record}
		if in.Snapshot != nil {
			req.Snapshot = *in.Snapshot
		}
		return req, nil
	}()
	if err != nil {
		refund()
		fail(w, err)
		return
	}
	// Report the obvious mistakes now instead of in a failed operation.
	if _, err := svc.GetBranch(r.Context(), req.Name); err == nil {
		refund()
		fail(w, fmt.Errorf("branch %s: %w; names are unique across the cluster, so pick another name or delete the existing branch first", req.Name, domain.ErrAlreadyExists))
		return
	}
	node, err := svc.PlaceBranch(r.Context(), req)
	if err != nil {
		refund()
		fail(w, err)
		return
	}
	if s.remote(node) {
		in.Node = node
		body, _ := json.Marshal(in)
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		s.forward(w, r, node)
		return
	}
	req.Node = node
	s.accepted(w, "branch.create", operation.Target{Resource: "branch", Name: req.Name.String()},
		func(ctx context.Context, report func(string)) error {
			b, err := svc.CreateBranch(ctx, req, report)
			s.publishBranch(ctx, svc, b.Name)
			return err
		})
}

// PublishBranch sends the branch's current state to event subscribers.
func (s *Server) PublishBranch(ctx context.Context, n domain.Name) {
	if svc, err := s.Branches(); err == nil {
		s.publishBranchNow(ctx, svc, n)
	}
}

// publishBranch announces a change made here. In a cluster the replicated
// store announces every change on every node instead (PublishBranch).
func (s *Server) publishBranch(ctx context.Context, svc Branches, n domain.Name) {
	if s.Cluster == nil {
		s.publishBranchNow(ctx, svc, n)
	}
}

func (s *Server) publishBranchNow(ctx context.Context, svc Branches, n domain.Name) {
	b, err := svc.GetBranch(ctx, n)
	if err != nil {
		s.Bus.Publish("branch.deleted", map[string]string{"name": n.String()})
		return
	}
	s.Bus.Publish("branch.updated", s.branchView(ctx, svc, b))
}

func (s *Server) patchBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in struct {
		TTL string `json:"ttl"`
	}
	if !decode(w, r, &in) {
		return
	}
	ttl, err := time.ParseDuration(in.TTL)
	if err != nil {
		fail(w, fmt.Errorf("%w: %v", domain.ErrInvalidTTL, err))
		return
	}
	b, err := svc.ExtendBranch(r.Context(), n, ttl)
	if err != nil {
		fail(w, err)
		return
	}
	v := s.branchView(r.Context(), svc, b)
	s.publishBranch(r.Context(), svc, n)
	writeJSON(w, http.StatusOK, v)
}

// branchAction checks the branch can take the action now, then runs it in the background.
func (s *Server) branchAction(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name, kind string, check func(domain.Branch) error,
	run func(ctx context.Context, svc Branches, n domain.Name, report func(string)) error) {
	b, err := svc.GetBranch(r.Context(), n)
	// A lost branch's node cannot be reached; deleting it is recorded here
	// and finished by that node when it returns.
	abandon := kind == "branch.delete" && b.Status == domain.BranchLost
	if err == nil && !abandon && s.forward(w, r, b.Node) {
		return
	}
	if err == nil {
		err = check(b)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.accepted(w, kind, operation.Target{Resource: "branch", Name: n.String()},
		func(ctx context.Context, report func(string)) error {
			err := run(ctx, svc, n, report)
			s.publishBranch(ctx, svc, n)
			return err
		})
}

func (s *Server) resetBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	s.branchAction(w, r, svc, n, "branch.reset", domain.Branch.CanReset,
		func(ctx context.Context, svc Branches, n domain.Name, report func(string)) error {
			_, err := svc.ResetBranch(ctx, n, report)
			return err
		})
}

func (s *Server) deleteBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	s.branchAction(w, r, svc, n, "branch.delete", domain.Branch.CanDelete,
		func(ctx context.Context, svc Branches, n domain.Name, report func(string)) error {
			return svc.DeleteBranch(ctx, n, report)
		})
}

// recreateBranch builds a lost branch again on a node holding its snapshot
// (chosen, or given as ?node=).
func (s *Server) recreateBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	b, err := svc.GetBranch(r.Context(), n)
	if err == nil {
		err = b.CanRecreate()
	}
	node := r.URL.Query().Get("node")
	if err == nil && node == "" {
		node, err = svc.PlaceRecreate(r.Context(), n)
	}
	if err != nil {
		fail(w, err)
		return
	}
	if s.remote(node) {
		r.URL.RawQuery = url.Values{"node": {node}}.Encode()
		s.forward(w, r, node)
		return
	}
	s.accepted(w, "branch.recreate", operation.Target{Resource: "branch", Name: n.String()},
		func(ctx context.Context, report func(string)) error {
			_, err := svc.RecreateBranch(ctx, n, report)
			s.publishBranch(ctx, svc, n)
			return err
		})
}

// routable reports whether the proxy here reaches b by its user name.
func (s *Server) routable(b domain.Branch) bool {
	return s.ProxyPorts[b.Runtime.Engine] > 0 && b.User == b.Name.String()
}
