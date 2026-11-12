package httpapi

import (
	"net/http"

	"github.com/db-branch/db-branch/internal/domain"
)

type routeUserJSON struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
}

func (s *Server) withRouting(w http.ResponseWriter) bool {
	if s.Routing == nil {
		writeError(w, http.StatusConflict, "no_proxy", "this node has no proxy (install the db-branch-proxysql package)")
		return false
	}
	return true
}

// listRouteUsers never returns passwords.
func (s *Server) listRouteUsers(w http.ResponseWriter, r *http.Request) {
	if !s.withRouting(w) {
		return
	}
	users, err := s.Routing.ListUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]routeUserJSON, 0, len(users))
	for _, u := range users {
		out = append(out, routeUserJSON{Name: u.Name, DefaultBranch: u.DefaultBranch.String()})
	}
	writeJSON(w, http.StatusOK, items(out))
}

func (s *Server) putRouteUser(w http.ResponseWriter, r *http.Request) {
	if !s.withRouting(w) {
		return
	}
	var in struct {
		Password      string `json:"password"`
		DefaultBranch string `json:"default_branch"`
	}
	if !decode(w, r, &in) {
		return
	}
	def, err := domain.ParseName(in.DefaultBranch)
	if err == nil {
		err = s.Routing.AddUser(r.Context(), domain.RouteUser{Name: r.PathValue("name"), Password: in.Password, DefaultBranch: def})
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, routeUserJSON{Name: r.PathValue("name"), DefaultBranch: def.String()})
}

func (s *Server) deleteRouteUser(w http.ResponseWriter, r *http.Request) {
	if !s.withRouting(w) {
		return
	}
	if err := s.Routing.RemoveUser(r.Context(), r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
