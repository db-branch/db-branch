package httpapi

import (
	"context"
	"net/http"
	"slices"
)

// Languages the console speaks ("" follows the browser).
var Languages = []string{"en", "ja"}

func prefScope(principal string) string { return "pref:" + principal }

// language is the principal's chosen language ("" when none or unknown).
func (s *Server) language(ctx context.Context, principal string) string {
	if s.Preferences == nil || principal == "" {
		return ""
	}
	v, err := s.Preferences.GetSettings(ctx, prefScope(principal))
	if err != nil {
		return ""
	}
	return v["language"]
}

func (s *Server) getPreferences(w http.ResponseWriter, r *http.Request) {
	if s.Preferences == nil {
		writeError(w, http.StatusServiceUnavailable, "not_initialized", "preferences are not available on this node")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"language": s.language(r.Context(), principalOf(r).ID), "languages": Languages})
}

func (s *Server) putPreferences(w http.ResponseWriter, r *http.Request) {
	if s.Preferences == nil {
		writeError(w, http.StatusServiceUnavailable, "not_initialized", "preferences are not available on this node")
		return
	}
	var in struct {
		Language string `json:"language"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Language != "" && !slices.Contains(Languages, in.Language) {
		writeError(w, http.StatusBadRequest, "invalid_request", "language: one of en, ja, or empty to follow the browser")
		return
	}
	if err := s.Preferences.PutSettings(r.Context(), prefScope(principalOf(r).ID), map[string]string{"language": in.Language}); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"language": in.Language})
}
