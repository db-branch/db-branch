package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

type upgradeJSON struct {
	// Runtime is the target, "engine@version".
	Runtime string `json:"runtime"`
	// CheckOnly runs the pre-check and changes nothing.
	CheckOnly bool `json:"check_only"`
	// NoRollback keeps a failed upgrade as it is.
	NoRollback bool `json:"no_rollback"`
	// Force upgrades although the pre-check found errors.
	Force bool `json:"force"`
}

// poolFree is this node's free pool space (0: unknown).
func (s *Server) poolFree() uint64 {
	if s.PoolFree == nil {
		return 0
	}
	if free := s.PoolFree(); free != math.MaxUint64 {
		return free
	}
	return 0
}

// acceptedResult is accepted for a task that returns a result.
func (s *Server) acceptedResult(w http.ResponseWriter, kind string, target operation.Target, f operation.ResultFunc) {
	op, err := s.Ops.StartResult(kind, target, f)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, op)
}

// upgradeBranch moves a branch to a newer runtime (or only checks that it
// could) in the background; the operation's result holds the pre-check and
// the log findings.
func (s *Server) upgradeBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in upgradeJSON
	if !decode(w, r, &in) {
		return
	}
	to, err := domain.ParseRuntimeRef(in.Runtime)
	if err != nil {
		fail(w, err)
		return
	}
	// Forwarding re-reads the body: hand it back.
	body, _ := json.Marshal(in)
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	if _, ok := s.featureBranch(w, r, svc, n, "upgrade"); !ok {
		return
	}
	if _, err := svc.ValidateUpgrade(r.Context(), n, to, in.CheckOnly); err != nil {
		fail(w, err)
		return
	}
	req := dbbranch.UpgradeRequest{Name: n, To: to, CheckOnly: in.CheckOnly, NoRollback: in.NoRollback, Force: in.Force, PoolFree: s.poolFree()}
	s.acceptedResult(w, "branch.upgrade", operation.Target{Resource: "branch", Name: n.String()},
		func(ctx context.Context, report func(string)) (any, error) {
			res, err := svc.UpgradeBranch(ctx, req, report)
			s.publishBranch(ctx, svc, n)
			return res, err
		})
}

// createRuntime checks a branch created with a runtime: who may, and the
// upgrade path from the base's runtime. It returns the target and the base's runtime.
func (s *Server) createRuntime(r *http.Request, svc Branches, in createBranchJSON, base domain.Name) (to, from domain.RuntimeRef, err error) {
	if to, err = domain.ParseRuntimeRef(in.Runtime); err != nil {
		return to, from, err
	}
	p := principalOf(r)
	if p.ID == Anonymous.ID {
		return to, from, fmt.Errorf("%w: log in to create a branch on another runtime", ext.ErrForbidden)
	}
	if err = s.authorizer().Authorize(r.Context(), p, "branch.upgrade", ext.Resource{Kind: "branch", Name: in.Name}); err != nil {
		return to, from, err
	}
	b, err := svc.GetBase(r.Context(), base)
	if err != nil {
		return to, from, err
	}
	return to, b.Runtime, domain.CheckUpgradePath(b.Runtime, to)
}
