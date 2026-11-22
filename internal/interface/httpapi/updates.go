package httpapi

import (
	"context"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"net/http"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/update"
)

// Updates is the package update use case the API needs.
type Updates interface {
	View(ctx context.Context) (update.View, error)
	// Check asks this node's APT for newer versions (the only network access).
	Check(ctx context.Context) (update.View, error)
	// Apply starts a rollout to target, for the named user.
	Apply(ctx context.Context, target, by string) (domain.Rollout, error)
	Cancel(ctx context.Context) error
}

func (s *Server) withUpdates(h func(http.ResponseWriter, *http.Request, Updates)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Updates == nil {
			writeError(w, http.StatusServiceUnavailable, "not_initialized", "updates are not available on this node")
			return
		}
		h(w, r, s.Updates)
	}
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request, u Updates) {
	v, err := u.View(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request, u Updates) {
	v, err := u.Check(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	s.Bus.PublishTo(operation.Audience{Action: "update.read"}, "update.updated", map[string]string{"what": "check"})
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request, u Updates) {
	var in struct {
		Target string `json:"target"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Target == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "target is required")
		return
	}
	rollout, err := u.Apply(r.Context(), in.Target, principalOf(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	s.Bus.PublishTo(operation.Audience{Action: "update.read"}, "update.updated", map[string]string{"what": "rollout"})
	writeJSON(w, http.StatusAccepted, rollout)
}

func (s *Server) cancelUpdate(w http.ResponseWriter, r *http.Request, u Updates) {
	if err := u.Cancel(r.Context()); err != nil {
		fail(w, err)
		return
	}
	s.Bus.PublishTo(operation.Audience{Action: "update.read"}, "update.updated", map[string]string{"what": "rollout"})
	w.WriteHeader(http.StatusNoContent)
}
