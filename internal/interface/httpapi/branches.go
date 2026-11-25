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
	"github.com/db-branch/db-branch/pkg/ext"
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
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	StatusReason *string `json:"status_reason"`
	Base         string  `json:"base"`
	Snapshot     string  `json:"snapshot"`
	// ParentBranch and ParentSnapshot are set when the branch was forked from another branch.
	ParentBranch   *string `json:"parent_branch,omitempty"`
	ParentSnapshot *string `json:"parent_snapshot,omitempty"`
	Runtime        string  `json:"runtime"`
	// BaseRuntime is the runtime of the base; Runtime differs once the branch is upgraded.
	BaseRuntime string         `json:"base_runtime"`
	Node        string         `json:"node"`
	CreatedAt   string         `json:"created_at"`
	ExpiresAt   *string        `json:"expires_at"`
	ResetAt     *string        `json:"reset_at"`
	Storage     *storageJSON   `json:"storage"`
	Connection  connectionJSON `json:"connection"`
	// Proxy is how to reach the branch through this node's proxy, by user name.
	Proxy     *connectionJSON `json:"proxy"`
	Operation *opRef          `json:"operation"`
	// Owner is the immutable principal ID who manages the branch; OwnerName is
	// their display name; ID is "owner/name" ("name" without an owner).
	Owner     *string `json:"owner"`
	OwnerName *string `json:"owner_name"`
	ID        string  `json:"id"`
	Access    string  `json:"access"`
	// Record: the branch records its changes, from its creation to its
	// deletion (the history module records). Fixed when the branch is
	// created, by `record` or the module's record_all setting at that moment;
	// nothing changes it afterwards.
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
		Owner:      optString(b.Owner), OwnerName: optString(b.DisplayOwner()), ID: b.Name.String(), Access: domain.AccessPrivate, Record: b.Record,
	}
	if b.ParentBranch != (domain.Name{}) {
		v.ParentBranch = optString(b.ParentBranch.String())
		v.ParentSnapshot = optString(b.ParentSnapshot)
	}
	if display := b.DisplayOwner(); display != "" {
		// An email owner shows as its local part; owner_name keeps the display label.
		short, _, _ := strings.Cut(display, "@")
		v.ID = short + "/" + b.Name.String()
	}
	if b.Open() {
		v.Access = domain.AccessOpen
	}
	v.BaseRuntime = v.Runtime
	if base, err := svc.GetBase(ctx, b.Base); err == nil {
		v.BaseRuntime = base.Runtime.String()
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
	// A node with ZFS can still run an engine that has no checkpoints.
	v.Checkpoints = v.Checkpoints && s.requireFeature(ctx, b.Runtime.Engine, "checkpoints") == nil
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
	p := principalOf(r)
	out := make([]branchJSON, 0, len(list))
	for _, b := range list {
		if !s.canReadBranch(r.Context(), p, b) {
			continue
		}
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
	Owner     string  `json:"owner,omitempty"`
	OwnerName string  `json:"owner_name,omitempty"`
	Access    string  `json:"access,omitempty"`
	Name      string  `json:"name"`
	Base      string  `json:"base"`
	Snapshot  *string `json:"snapshot"`
	InitSQL   string  `json:"init_sql"`
	TTL       string  `json:"ttl"`
	Node      string  `json:"node,omitempty"`
	// Record asks for the branch's changes to be recorded (needs the history
	// module, a login, and a MySQL base).
	Record bool `json:"record,omitempty"`
	// Runtime upgrades the new branch to it right away ("engine@version").
	Runtime string `json:"runtime,omitempty"`
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
	// A branch being made counts against the anonymous caps from the moment
	// it is accepted until it shows in the list.
	unreserve, forwarded := func() {}, false
	w, sw := track(w)
	defer func() {
		if sw.status != http.StatusAccepted || forwarded {
			unreserve() // refused, or running on another node: nothing of ours is pending
		}
	}()
	req, err := func() (dbbranch.CreateBranchRequest, error) {
		base, err := domain.ParseName(in.Base)
		if err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		ttl, err := parseTTL(in.TTL)
		if in.TTL == "" && s.DefaultTTL != nil {
			ttl = s.clampBranchTTL(r.Context(), principalOf(r), base.String(), s.DefaultTTL())
		}
		if err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		if unreserve, err = s.branchOwner(r.Context(), svc, principalOf(r), &in, &ttl); err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		if err := s.userBranchName(principalOf(r), r, &in); err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		auditAs(r, in.Name)
		n, err := domain.ParseName(in.Name)
		if err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		unreservePol, err := s.reserveBranchPolicy(r.Context(), svc, principalOf(r), in, ttl)
		if err != nil {
			return dbbranch.CreateBranchRequest{}, err
		}
		prev := unreserve
		unreserve = func() {
			unreservePol()
			prev()
		}
		if in.Record {
			if err := s.checkRecord(r.Context(), svc, principalOf(r), in); err != nil {
				return dbbranch.CreateBranchRequest{}, err
			}
		}
		req := dbbranch.CreateBranchRequest{Name: n, Base: base, InitSQL: in.InitSQL, TTL: ttl, Node: in.Node, Owner: in.Owner, OwnerName: in.OwnerName, Access: in.Access, Record: in.Record}
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
	var target, baseRuntime domain.RuntimeRef
	if in.Runtime != "" {
		var err error
		if target, baseRuntime, err = s.createRuntime(r, svc, in, req.Base); err != nil {
			refund()
			fail(w, err)
			return
		}
		req.Runtime, req.PoolFree = target, s.poolFree()
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
		forwarded = true
		if _, status := s.forwardStatus(w, r, node); status != http.StatusAccepted {
			refund() // the other node refused: the allowance is not used
		} else {
			// The branch shows in the list once the other node has made it;
			// until then this node keeps counting it.
			time.AfterFunc(anonForwardHold, unreserve)
			forwarded = false
		}
		return
	}
	req.Node = node
	if target != (domain.RuntimeRef{}) {
		if err := svc.ValidateTarget(r.Context(), baseRuntime, target); err != nil {
			refund()
			fail(w, err)
			return
		}
		s.acceptedResult(w, r, "branch.create", operation.Target{Resource: "branch", Name: req.Name.String()},
			func(ctx context.Context, report func(string)) (any, error) {
				defer unreserve()
				b, res, err := svc.CreateBranchUpgraded(ctx, req, report)
				s.publishBranch(ctx, svc, b.Name)
				return res, err
			})
		return
	}
	s.accepted(w, r, "branch.create", operation.Target{Resource: "branch", Name: req.Name.String()},
		func(ctx context.Context, report func(string)) error {
			defer unreserve()
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
	// One at a time, so that the events leave in the order the states were read.
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	b, err := svc.GetBranch(ctx, n)
	if err != nil {
		// The branch is gone, so who could see it comes from the last event about it.
		prev, _ := s.branchAudiences.LoadAndDelete(n.String())
		aud, _ := prev.(operation.Audience)
		s.Bus.PublishTo(aud, "branch.deleted", map[string]string{"name": n.String()})
		return
	}
	aud := operation.Audience{Owner: b.Owner, Open: b.Open(), Base: b.Base.String()}
	if prev, ok := s.branchAudiences.Swap(n.String(), aud); ok && prev.(operation.Audience).Open && !aud.Open {
		// Made private: tell the people who saw it open to drop it.
		s.Bus.PublishTo(operation.Audience{Open: true, Base: b.Base.String()}, "branch.deleted", map[string]string{"name": n.String()})
	}
	s.Bus.PublishTo(aud, "branch.updated", s.branchView(ctx, svc, b))
}

// seedBranchAudiences learns who sees each branch, once, so that a branch
// deleted before any event about it (after a restart) still reaches them.
func (s *Server) seedBranchAudiences(ctx context.Context) {
	s.seedMu.Lock()
	defer s.seedMu.Unlock()
	if s.seeded {
		return
	}
	svc, err := s.Branches()
	if err != nil {
		return // not initialized yet: try again on the next stream
	}
	list, err := svc.ListBranches(ctx)
	if err != nil {
		return
	}
	s.seeded = true
	for _, b := range list {
		s.branchAudiences.LoadOrStore(b.Name.String(), operation.Audience{Owner: b.Owner, Open: b.Open(), Base: b.Base.String()})
	}
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
	if err := s.checkExtendPolicy(r.Context(), svc, principalOf(r), n, ttl); err != nil {
		fail(w, err)
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
	s.accepted(w, r, kind, operation.Target{Resource: "branch", Name: n.String()},
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

func (s *Server) pauseBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	s.branchAction(w, r, svc, n, "branch.pause", domain.Branch.CanPause,
		func(ctx context.Context, svc Branches, n domain.Name, report func(string)) error {
			_, err := svc.PauseBranch(ctx, n, report)
			return err
		})
}

func (s *Server) resumeBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	s.branchAction(w, r, svc, n, "branch.resume", domain.Branch.CanResume,
		func(ctx context.Context, svc Branches, n domain.Name, report func(string)) error {
			_, err := svc.ResumeBranch(ctx, n, report)
			return err
		})
}

func (s *Server) deleteBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	check := func(b domain.Branch) error {
		if err := b.CanDelete(); err != nil {
			return err
		}
		return checkNoForkChildren(r.Context(), svc, b.Name)
	}
	s.branchAction(w, r, svc, n, "branch.delete", check,
		func(ctx context.Context, svc Branches, n domain.Name, report func(string)) error {
			return svc.DeleteBranch(ctx, n, report)
		})
}

func checkNoForkChildren(ctx context.Context, svc Branches, parent domain.Name) error {
	list, err := svc.ListBranches(ctx)
	if err != nil {
		return err
	}
	var children []string
	for _, b := range list {
		if b.ParentBranch == parent {
			children = append(children, b.Name.String())
		}
	}
	if len(children) == 0 {
		return nil
	}
	sort.Strings(children)
	return fmt.Errorf("%w: branch %s has forked child branch(es): %s; delete them first", domain.ErrInUse, parent, strings.Join(children, ", "))
}

type forkBranchJSON struct {
	Name       string `json:"name"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Clean      bool   `json:"clean,omitempty"`
	InitSQL    string `json:"init_sql,omitempty"`
	TTL        string `json:"ttl,omitempty"`
	Node       string `json:"node,omitempty"`
	Owner      string `json:"owner,omitempty"`
	OwnerName  string `json:"owner_name,omitempty"`
	Access     string `json:"access,omitempty"`
	Record     bool   `json:"record,omitempty"`
}

func (s *Server) forkBranch(w http.ResponseWriter, r *http.Request, svc Branches, source domain.Name) {
	var in forkBranchJSON
	if !decode(w, r, &in) {
		return
	}
	parent, err := svc.GetBranch(r.Context(), source)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.requireFeature(r.Context(), parent.Runtime.Engine, "checkpoints"); err != nil {
		fail(w, err)
		return
	}
	if err := parent.CanFork(); err != nil {
		fail(w, err)
		return
	}
	parentNode := s.nodeOf(parent.Node)
	if in.Node != "" && in.Node != parentNode {
		fail(w, fmt.Errorf("%w: forked branch must be created on parent's node %s, not %s", domain.ErrConflict, parentNode, in.Node))
		return
	}
	ttl, err := parseTTL(in.TTL)
	if in.TTL == "" && s.DefaultTTL != nil {
		ttl = s.clampBranchTTL(r.Context(), principalOf(r), parent.Base.String(), s.DefaultTTL())
	}
	if err != nil {
		fail(w, err)
		return
	}
	p := principalOf(r)
	if err := resolveNamedOwner(p, &in.Owner, &in.OwnerName, in.Access); err != nil {
		fail(w, err)
		return
	}
	if err := s.prefixUserBranchName(p, r, &in.Name); err != nil {
		fail(w, err)
		return
	}
	childName, err := domain.ParseName(in.Name)
	if err != nil {
		fail(w, err)
		return
	}
	if in.Checkpoint != "" && in.Checkpoint != domain.ResetPoint {
		if _, err := domain.ParseCheckpointName(in.Checkpoint); err != nil {
			fail(w, err)
			return
		}
	}
	if in.Record {
		rt, exact := s.forkRecordRuntime(r.Context(), svc, parent, in.Checkpoint)
		if err := s.checkForkRecord(r.Context(), p, in.Name, in.Owner, rt, exact); err != nil {
			fail(w, err)
			return
		}
	}
	snap := parent.Snapshot
	unreserve, forwarded := func() {}, false
	w, sw := track(w)
	defer func() {
		if sw.status != http.StatusAccepted || forwarded {
			unreserve()
		}
	}()
	var polErr error
	unreserve, polErr = s.reserveBranchPolicy(r.Context(), svc, p, createBranchJSON{
		Name: in.Name, Base: parent.Base.String(), Snapshot: &snap, Owner: in.Owner, OwnerName: in.OwnerName, Access: in.Access,
	}, ttl)
	if polErr != nil {
		fail(w, polErr)
		return
	}
	if _, err := svc.GetBranch(r.Context(), childName); err == nil {
		fail(w, fmt.Errorf("branch %s: %w; names are unique across the cluster, so pick another name or delete the existing branch first", childName, domain.ErrAlreadyExists))
		return
	}
	if s.Cluster != nil {
		st, err := s.Cluster.Status(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if st.Protocol > 0 && st.Protocol < domain.ProtocolBranchFork {
			fail(w, fmt.Errorf("%w: a branch can only be forked once every node runs the new version", domain.ErrClusterUpgrading))
			return
		}
	}
	if s.remote(parent.Node) {
		if in.TTL == "" && ttl > 0 {
			in.TTL = ttl.String()
		}
		body, _ := json.Marshal(in)
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		forwarded = true
		if _, status := s.forwardStatus(w, r, parent.Node); status == http.StatusAccepted {
			time.AfterFunc(anonForwardHold, unreserve)
			forwarded = false
		}
		return
	}
	req := dbbranch.ForkBranchRequest{
		Source: source, Name: childName, Checkpoint: in.Checkpoint, Clean: in.Clean,
		InitSQL: in.InitSQL, TTL: ttl, Node: in.Node, Owner: in.Owner, OwnerName: in.OwnerName,
		Access: in.Access, Record: in.Record,
	}
	if err := svc.ValidateFork(r.Context(), req); err != nil {
		fail(w, err)
		return
	}
	s.accepted(w, r, "branch.fork", operation.Target{Resource: "branch", Name: req.Name.String()},
		func(ctx context.Context, report func(string)) error {
			defer unreserve()
			b, err := svc.ForkBranch(ctx, req, report)
			if b.Name != (domain.Name{}) {
				s.publishBranch(ctx, svc, b.Name)
			} else if _, gerr := svc.GetBranch(ctx, req.Name); gerr == nil {
				s.publishBranch(ctx, svc, req.Name)
			}
			s.publishBranch(ctx, svc, source)
			return err
		})
}

// forkRecordRuntime resolves the runtime the forked child will actually run so
// history-recording checks use the checkpoint's runtime instead of the parent's
// current one. When forwarding a named checkpoint to another node, exact is
// false because the checkpoint's metadata lives only on the parent's node; the
// owner node runs the version-specific check after forwarding.
func (s *Server) forkRecordRuntime(ctx context.Context, svc Branches, parent domain.Branch, checkpoint string) (domain.RuntimeRef, bool) {
	if checkpoint == "" {
		return parent.Runtime, true
	}
	fallback := func() domain.RuntimeRef {
		if parent.ForkRuntime != (domain.RuntimeRef{}) {
			return parent.ForkRuntime
		}
		if base, err := svc.GetBase(ctx, parent.Base); err == nil && base.Runtime != (domain.RuntimeRef{}) {
			return base.Runtime
		}
		return parent.Runtime
	}
	if checkpoint == domain.ResetPoint {
		return fallback(), true
	}
	if s.remote(parent.Node) {
		return parent.Runtime, false
	}
	if cps, err := svc.ListCheckpoints(ctx, parent.Name); err == nil {
		for _, c := range cps {
			if c.Name == checkpoint {
				if rt, err := domain.ParseRuntimeRef(c.Runtime); err == nil {
					return rt, true
				}
				return fallback(), true
			}
		}
	}
	return parent.Runtime, false
}

func (s *Server) checkForkRecord(ctx context.Context, p ext.Principal, name, owner string, rt domain.RuntimeRef, exact bool) error {
	if exact {
		return s.checkRecordRuntime(ctx, p, name, owner, rt)
	}
	if p.ID == Anonymous.ID {
		return fmt.Errorf("%w: recording needs a login", ext.ErrForbidden)
	}
	if s.Ext.ServerOptioner == nil {
		return fmt.Errorf("%w: recording needs the history module, which is not installed (db-branch module install history)", domain.ErrInvalidName)
	}
	return s.requireFeature(ctx, rt.Engine, "history_record")
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
	s.accepted(w, r, "branch.recreate", operation.Target{Resource: "branch", Name: n.String()},
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
