package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

// Anonymous acts for requests without credentials, which may only make
// branches from public snapshots and use open branches.
var errInvalidAccess = fmt.Errorf("%w: invalid access", domain.ErrInvalidName)

var Anonymous = ext.Principal{ID: "anonymous", Name: "anonymous", Roles: []string{"anonymous"}}

// The paths that answer an anonymous caller (everything else needs a login).
var anonymousPaths = []struct {
	method string
	path   *regexp.Regexp
}{
	{http.MethodPost, regexp.MustCompile(`^/v1/branches$`)},
	{http.MethodGet, regexp.MustCompile(`^/v1/branches/[^/]+(/dsn)?$`)},
	{http.MethodGet, regexp.MustCompile(`^/v1/operations/[^/]+$`)},
	{http.MethodGet, regexp.MustCompile(`^/v1/public/bases$`)},
}

// publicExt reports whether r is for a module's public path.
func (s *Server) publicExt(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/ext/")
	if !ok {
		return false
	}
	module, path, _ := strings.Cut(rest, "/")
	return s.isPublic(module, "/"+path)
}

func anonymousAllowed(r *http.Request) bool {
	for _, p := range anonymousPaths {
		if r.Method == p.method && p.path.MatchString(r.URL.Path) {
			return true
		}
	}
	return false
}

// isAdmin: the admin password, the node's socket, a full-scope token, or a
// module's admin role.
func isAdmin(p ext.Principal) bool {
	return slices.Contains(p.Roles, "admin") || slices.Contains(p.Roles, "scope:"+domain.ScopeFull)
}

// ownerID is the immutable identifier of a principal as a branch owner.
func ownerID(p ext.Principal) string {
	return p.ID
}

// ownerDisplayName is how a principal is shown for display and branch name prefixes.
func ownerDisplayName(p ext.Principal) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// ownership lets people other than admins change and use only their own
// branches (and connect to open ones), and anonymous callers only make
// branches and use open ones.
type ownership struct{ s *Server }

func (o ownership) branch(ctx context.Context, name string) (domain.Branch, bool) {
	n, err := domain.ParseName(name)
	if err != nil || o.s.Branches == nil {
		return domain.Branch{}, false
	}
	svc, err := o.s.Branches()
	if err != nil {
		return domain.Branch{}, false
	}
	b, err := svc.GetBranch(ctx, n)
	return b, err == nil
}

// canManageBranch reports whether p owns b or shares branches with b's owner
// (or is an admin). Legacy branches without an owner are admin-only.
func (s *Server) canManageBranch(ctx context.Context, p ext.Principal, b domain.Branch) bool {
	if isAdmin(p) {
		return true
	}
	if p.ID == Anonymous.ID || b.Owner == "" {
		return false
	}
	if ownsBranch(p, b) {
		return true
	}
	return s.Ext.BranchPolicy != nil && s.Ext.BranchPolicy.SharesBranches(ctx, p, b.Owner)
}

// ownsBranch reports whether p is the owner of b. Owners are immutable IDs,
// except for a legacy branch whose old display-name owner migration could not
// resolve to an ID (OwnerName == ""): that one is still matched by display
// name, but never by a service token, so a token named like the old owner
// cannot take it.
func ownsBranch(p ext.Principal, b domain.Branch) bool {
	if b.OwnerName == "" {
		return !strings.HasPrefix(b.Owner, "token:") && !strings.HasPrefix(p.ID, "token:") && ownerDisplayName(p) == b.Owner
	}
	return b.Owner == ownerID(p)
}

// canSeeOperation reports whether p may read op: admins, the principal that
// started it and whoever shares that principal's branches. Operations nobody
// owns (started by the system) are admin-only. Anonymous callers see only
// operations anonymous callers started.
func (s *Server) canSeeOperation(ctx context.Context, p ext.Principal, op operation.Operation) bool {
	switch {
	case isAdmin(p):
		return true
	case op.Owner == "":
		return false
	case op.Owner == p.ID:
		return true
	case p.ID == Anonymous.ID || op.Owner == Anonymous.ID:
		return false
	}
	return s.Ext.BranchPolicy != nil && s.Ext.BranchPolicy.SharesBranches(ctx, p, op.Owner)
}

// canReceive reports whether p may receive an event meant for aud. Admins
// receive everything; an event with no audience is for them alone.
func (s *Server) canReceive(ctx context.Context, p ext.Principal, aud operation.Audience) bool {
	switch {
	case isAdmin(p):
		return true
	case p.ID == Anonymous.ID:
		return false
	case aud.Open:
		return true
	case aud.Owner != "" && (aud.Owner == p.ID || (aud.Owner != Anonymous.ID && s.Ext.BranchPolicy != nil && s.Ext.BranchPolicy.SharesBranches(ctx, p, aud.Owner))):
		return true
	case aud.Action != "":
		kind, _, _ := strings.Cut(aud.Action, ".")
		return s.authorizer().Authorize(ctx, p, aud.Action, ext.Resource{Kind: kind}) == nil
	}
	return false
}

// canReadBranch reports whether p may read, connect to, or query b: managers
// of b plus anyone for open branches.
func (s *Server) canReadBranch(ctx context.Context, p ext.Principal, b domain.Branch) bool {
	return b.Open() || s.canManageBranch(ctx, p, b)
}

func (o ownership) Authorize(ctx context.Context, p ext.Principal, action string, r ext.Resource) error {
	if p.ID == Anonymous.ID {
		switch action {
		case "branch.create", "operation.read", "base.public":
			return nil
		case "branch.read", "branch.connect":
			if b, ok := o.branch(ctx, r.Name); ok && r.Name != "" && b.Open() {
				return nil
			}
		}
		return fmt.Errorf("%w: log in for %s (without a login: branches from public snapshots, and open branches)", ext.ErrForbidden, action)
	}
	if isAdmin(p) || r.Kind != "branch" || r.Name == "" || action == "branch.create" {
		return nil
	}
	b, ok := o.branch(ctx, r.Name)
	if !ok {
		return nil // unknown branches answer 404 in the handler
	}
	switch action {
	case "branch.read", "branch.connect", "branch.query":
		if o.s.canReadBranch(ctx, p, b) {
			return nil
		}
	default:
		if o.s.canManageBranch(ctx, p, b) {
			return nil
		}
	}
	if b.Owner == "" {
		return fmt.Errorf("%w: branch %s has no owner (only an admin may manage it)", ext.ErrForbidden, b.Name)
	}
	return fmt.Errorf("%w: branch %s belongs to another user", ext.ErrForbidden, b.Name)
}

// branchOwner decides the owner and access of a new branch for p: people
// may name another owner only as an admin; anonymous callers get an open
// branch from a public snapshot, within the anonymous limits.
func (s *Server) branchOwner(ctx context.Context, svc Branches, p ext.Principal, in *createBranchJSON, ttl *time.Duration) error {
	if !domain.ValidAccess(in.Access) {
		return fmt.Errorf("%w: access must be %q or %q", errInvalidAccess, domain.AccessPrivate, domain.AccessOpen)
	}
	if p.ID != Anonymous.ID {
		id, display := ownerID(p), ownerDisplayName(p)
		switch {
		case in.Owner == "":
			in.Owner = id
			in.OwnerName = display
		case in.Owner == id:
			if !isAdmin(p) || in.OwnerName == "" {
				in.OwnerName = display
			}
		case !isAdmin(p):
			return fmt.Errorf("%w: only an admin may make a branch for %s", ext.ErrForbidden, in.Owner)
		default:
			if in.OwnerName == "" {
				in.OwnerName = in.Owner
			}
		}
		return nil
	}
	if strings.TrimSpace(in.InitSQL) != "" {
		return fmt.Errorf("%w: log in to run SQL after the copy (init_sql); branches made without a login take none", ext.ErrForbidden)
	}
	base, err := domain.ParseName(in.Base)
	if err != nil {
		return err
	}
	b, err := s.publicSnapshot(ctx, svc, base, in.Snapshot)
	if err != nil {
		return err
	}
	if b.Anonymous.Disabled() {
		return fmt.Errorf("%w: log in to branch from base %s (it allows no branches without a login)", ext.ErrForbidden, base)
	}
	maxTTL, maxCount := s.anonymousLimits()
	list, err := svc.ListBranches(ctx)
	if err != nil {
		return err
	}
	total, perBase := anonymousCounts(list)
	if total >= maxCount {
		return fmt.Errorf("%w: %d branches without a login exist already (the most allowed); try later or log in", domain.ErrConflict, total)
	}
	if most := b.Anonymous.MaxBranches; most != nil && perBase[base.String()] >= *most {
		return fmt.Errorf("%w: %d branches without a login exist from base %s already (the most it allows); try later or log in", domain.ErrConflict, perBase[base.String()], base)
	}
	in.Owner, in.OwnerName, in.Access = Anonymous.ID, Anonymous.Name, domain.AccessOpen
	if eff := b.Anonymous.EffectiveTTL(maxTTL); eff > 0 && (*ttl == 0 || *ttl > eff) {
		*ttl = eff
	}
	return nil
}

// anonymousCounts counts the branches made without a login: all of them, and
// by the base they were made from.
func anonymousCounts(list []domain.Branch) (int, map[string]int) {
	total, perBase := 0, map[string]int{}
	for _, x := range list {
		if x.Owner == Anonymous.ID {
			total++
			perBase[x.Base.String()]++
		}
	}
	return total, perBase
}

// userBranchName puts the owner's prefix in front of the name a signed-in
// non-admin typed on the user page (see domain.PrefixedName), so people can
// each have a branch called "test" without taking the name from one
// another. The user page works through a login session; API tokens (the
// CLI, MCP, CI), admins and anonymous callers name branches exactly, so
// scripts keep using the names they chose.
func (s *Server) userBranchName(p ext.Principal, r *http.Request, in *createBranchJSON) error {
	if _, session := s.current(r); !session || p.ID == Anonymous.ID || isAdmin(p) {
		return nil
	}
	full, err := domain.PrefixedName(ownerDisplayName(p), in.Name)
	if err != nil {
		return err
	}
	in.Name = full
	return nil
}

// branchPrefix is the prefix userBranchName adds for p ("" when none).
func branchPrefix(p ext.Principal) string {
	if p.ID == Anonymous.ID || isAdmin(p) {
		return ""
	}
	return domain.OwnerPrefix(ownerDisplayName(p)) + "-"
}

// PublicSettle is how long an anonymous request waits for a snapshot to be
// public: publishing is replicated, so this node may learn of it a moment
// after the node that handled it.
var PublicSettle = 2 * time.Second

// publicSnapshot returns the base once the snapshot a branch would start from
// is public.
func (s *Server) publicSnapshot(ctx context.Context, svc Branches, base domain.Name, snapshot *string) (domain.Base, error) {
	deadline := time.Now().Add(PublicSettle)
	for {
		b, err := svc.GetBase(ctx, base)
		if err != nil {
			return b, err
		}
		snap, ok := b.LatestSnapshot()
		if snapshot != nil {
			snap, ok = b.Snapshot(*snapshot)
		}
		if ok && snap.Public {
			return b, nil
		}
		if time.Now().After(deadline) {
			return b, fmt.Errorf("%w: log in to branch from base %s (that snapshot is not public)", ext.ErrForbidden, base)
		}
		select {
		case <-ctx.Done():
			return b, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *Server) anonymousLimits() (time.Duration, int) {
	if s.Anonymous != nil {
		return s.Anonymous()
	}
	return 24 * time.Hour, 10
}

// anonymousRate is the most branches one client may create without a login
// per hour (0: unlimited).
func (s *Server) anonymousRate() int {
	if s.AnonymousRate != nil {
		return s.AnonymousRate()
	}
	return 5
}

// anonymousWindow is the period anonymousRate counts over.
const anonymousWindow = time.Hour

// tooManyCreations is the error of a client over its hourly allowance.
type tooManyCreations struct {
	limit      int
	retryAfter time.Duration
}

func (e *tooManyCreations) Error() string {
	return fmt.Sprintf("at most %d branches per hour may be created without a login from one address; try again in %s or log in",
		e.limit, e.retryAfter.Round(time.Second))
}

// chargeAnonymous counts one creation against the client of r. The returned
// function gives it back (call it when the request is refused after all).
func (s *Server) chargeAnonymous(r *http.Request) (refund func(), err error) {
	limit, key := s.anonymousRate(), ClientKey(r.RemoteAddr)
	if ok, retry := s.anonLimiter.Allow(key, limit, anonymousWindow); !ok {
		return nil, &tooManyCreations{limit: limit, retryAfter: retry}
	}
	return func() { s.anonLimiter.Refund(key) }, nil
}

type anonymousBaseOut struct {
	MaxBranches int    `json:"max_branches"`
	MaxTTL      string `json:"max_ttl"`
	Branches    int    `json:"branches"`
	Remaining   int    `json:"remaining"`
}

// publicBases lists, for anyone, the ready bases with public snapshots
// (only those snapshots), what may be done with each without logging in
// (its limits and how many branches are left), and the cluster-wide limits.
func (s *Server) publicBases(w http.ResponseWriter, r *http.Request, svc Branches) {
	bases, err := svc.ListBases(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	list, err := svc.ListBranches(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	maxTTL, maxCount := s.anonymousLimits()
	total, perBase := anonymousCounts(list)
	type baseOut struct {
		Name        string           `json:"name"`
		Runtime     string           `json:"runtime"`
		Description string           `json:"description"`
		Snapshots   []snapshotJSON   `json:"snapshots"`
		Anonymous   anonymousBaseOut `json:"anonymous"`
	}
	items := []baseOut{}
	for _, b := range bases {
		used := perBase[b.Name.String()]
		out := baseOut{Name: b.Name.String(), Runtime: b.Runtime.String(), Description: b.Description, Anonymous: anonymousBaseOut{
			MaxBranches: maxCount, MaxTTL: b.Anonymous.EffectiveTTL(maxTTL).String(), Branches: used,
			Remaining: b.Anonymous.Remaining(maxCount, total, used)}}
		if b.Anonymous.MaxBranches != nil {
			out.Anonymous.MaxBranches = min(maxCount, *b.Anonymous.MaxBranches)
		}
		for _, snap := range b.Snapshots {
			if snap.Public {
				out.Snapshots = append(out.Snapshots, snapshotJSON{Name: snap.Name, CreatedAt: ts(snap.CreatedAt), Public: true})
			}
		}
		if b.Status == domain.BaseReady && len(out.Snapshots) > 0 && !b.Anonymous.Disabled() {
			items = append(items, out)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items,
		"anonymous": map[string]any{"max_ttl": maxTTL.String(), "max_branches": maxCount, "branches": total, "rate_per_hour": s.anonymousRate()}})
}

// checkBranchPolicy asks the modules' branch policy (e.g. team limits,
// masked snapshots only) about a branch anyone is creating.
func (s *Server) checkBranchPolicy(ctx context.Context, svc Branches, p ext.Principal, in createBranchJSON, ttl time.Duration) error {
	pol := s.Ext.BranchPolicy
	if pol == nil {
		return nil // every policy decides whom it limits (the team limits skip admins)
	}
	list, err := svc.ListBranches(ctx)
	if err != nil {
		return err
	}
	req := ext.BranchRequest{Name: in.Name, Owner: in.Owner, Base: in.Base, TTL: ttl, Access: in.Access, Existing: []ext.BranchInfo{}}
	if in.Snapshot != nil {
		req.Snapshot = *in.Snapshot
	} else if n, err := domain.ParseName(in.Base); err == nil {
		// The policy learns which snapshot "the latest" is now.
		if b, err := svc.GetBase(ctx, n); err == nil {
			if latest, ok := b.LatestSnapshot(); ok {
				req.Snapshot = latest.Name
			}
		}
	}
	for _, b := range list {
		req.Existing = append(req.Existing, ext.BranchInfo{Name: b.Name.String(), Owner: b.Owner, Base: b.Base.String()})
	}
	return pol.CheckBranch(ctx, p, req)
}
