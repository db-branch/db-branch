package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

type refreshSourceJSON struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// PasswordSet says whether a password is stored; it is never returned.
	PasswordSet bool     `json:"password_set"`
	Databases   []string `json:"databases"`
}

type refreshRunJSON struct {
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at"`
	OK         bool    `json:"ok"`
	Error      *string `json:"error"`
}

type refreshJSON struct {
	Base      string            `json:"base"`
	Engine    string            `json:"engine"`
	Source    refreshSourceJSON `json:"source"`
	Every     string            `json:"every"`
	UpdatedAt string            `json:"updated_at"`
	LastRun   *refreshRunJSON   `json:"last_run"`
	NextRun   string            `json:"next_run"`
	// Operation is the refresh running now, if any.
	Operation *opRef `json:"operation"`
}

func (s *Server) refreshView(r domain.BaseRefresh, engine domain.Engine) refreshJSON {
	v := refreshJSON{
		Base: r.Base.String(), Engine: string(engine), Every: r.Every.String(), UpdatedAt: ts(r.UpdatedAt), NextRun: ts(r.NextRun()),
		Source: refreshSourceJSON{Host: r.Source.Host, Port: r.Source.Port, User: r.Source.User,
			PasswordSet: r.Source.Password != "", Databases: append([]string{}, r.Source.Databases...)},
	}
	if run := r.LastRun; run != nil {
		v.LastRun = &refreshRunJSON{StartedAt: ts(run.StartedAt), FinishedAt: ts(run.FinishedAt), OK: run.OK, Error: optString(run.Error)}
	}
	if op, ok := s.Ops.Active("base", r.Base.String()); ok && op.Kind == "base.refresh" {
		v.Operation = &opRef{ID: op.ID, Kind: op.Kind}
	}
	return v
}

func (s *Server) writeRefresh(w http.ResponseWriter, ctx context.Context, svc Branches, r domain.BaseRefresh, status int) {
	base, err := svc.GetBase(ctx, r.Base)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, status, s.refreshView(r, base.Runtime.Engine))
}

func (s *Server) getRefresh(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	ref, err := svc.GetRefresh(r.Context(), n)
	if err != nil {
		fail(w, err)
		return
	}
	s.writeRefresh(w, r.Context(), svc, ref, http.StatusOK)
}

// putRefresh schedules refreshes of the base from a source database. Without
// a password the stored one stays.
func (s *Server) putRefresh(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	var in struct {
		Source struct {
			Host      string   `json:"host"`
			Port      int      `json:"port"`
			User      string   `json:"user"`
			Password  string   `json:"password"`
			Databases []string `json:"databases"`
		} `json:"source"`
		Every string `json:"every"`
	}
	if !decode(w, r, &in) {
		return
	}
	every, err := time.ParseDuration(in.Every)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "every must be a duration such as 24h (at least "+domain.MinRefreshEvery.String()+")")
		return
	}
	ref, err := svc.SetRefresh(r.Context(), n, dbbranch.RefreshRequest{
		Source: domain.RefreshSource{Host: in.Source.Host, Port: in.Source.Port, User: in.Source.User, Password: in.Source.Password, Databases: in.Source.Databases},
		Every:  every,
	})
	if err != nil {
		fail(w, err)
		return
	}
	s.publishBase(r.Context(), svc, n)
	s.writeRefresh(w, r.Context(), svc, ref, http.StatusOK)
}

func (s *Server) deleteRefresh(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if err := svc.ClearRefresh(r.Context(), n); err != nil {
		fail(w, err)
		return
	}
	s.publishBase(r.Context(), svc, n)
	w.WriteHeader(http.StatusNoContent)
}

// runRefresh refreshes the base from its source now (on the node holding it).
func (s *Server) runRefresh(w http.ResponseWriter, r *http.Request, svc Branches, n domain.Name) {
	if _, err := svc.GetRefresh(r.Context(), n); err != nil {
		fail(w, err)
		return
	}
	if s.forwardBase(w, r, svc, n) {
		return
	}
	s.accepted(w, r, "base.refresh", operation.Target{Resource: "base", Name: n.String()},
		func(ctx context.Context, report func(string)) error {
			err := svc.RefreshBase(ctx, n, report)
			s.publishBase(ctx, svc, n)
			return err
		})
}
