package httpapi

import (
	"context"
	"net/http"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

type mergeBranchReq struct {
	SourceLine     *int     `json:"source_line"`
	Checkpoint     string   `json:"checkpoint"`
	DryRun         bool     `json:"dry_run"`
	Tables         []string `json:"tables"`
	AllowDDL       bool     `json:"allow_ddl"`
	IgnoreTriggers bool     `json:"ignore_triggers"`
}

func (s *Server) mergeBranch(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if s.Cluster != nil {
		st, err := s.Cluster.Status(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if st.Protocol < domain.ProtocolBranchMerge {
			fail(w, domain.ErrClusterUpgrading)
			return
		}
	}
	if _, ok := s.featureBranch(w, r, svc, n, "branch_merge"); !ok {
		return
	}
	var in mergeBranchReq
	if !decode(w, r, &in) {
		return
	}
	req := dbbranch.MergeBranchRequest{
		Name:           n,
		SourceLine:     in.SourceLine,
		Checkpoint:     in.Checkpoint,
		DryRun:         in.DryRun,
		Tables:         in.Tables,
		AllowDDL:       in.AllowDDL,
		IgnoreTriggers: in.IgnoreTriggers,
	}
	if _, err := svc.ValidateMerge(r.Context(), req); err != nil {
		fail(w, err)
		return
	}
	s.acceptedResult(w, r, "branch.merge", operation.Target{Resource: "branch", Name: n.String()},
		func(ctx context.Context, report func(string)) (any, error) {
			return svc.MergeBranch(ctx, req, report)
		})
}
