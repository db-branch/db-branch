package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

type userJSON struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Disabled   bool   `json:"disabled"`
	MustChange bool   `json:"must_change_password"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func userView(u domain.User) userJSON {
	kind := "user"
	if u.ID == domain.AdminUserID {
		kind = domain.AdminUserID
	}
	return userJSON{
		ID:         u.ID,
		Email:      u.Email,
		Name:       u.DisplayName(),
		Kind:       kind,
		Disabled:   u.Disabled,
		MustChange: u.MustChange,
		CreatedAt:  ts(u.CreatedAt),
		UpdatedAt:  ts(u.UpdatedAt),
	}
}

func (s *Server) requireUsers(w http.ResponseWriter) bool {
	if s.Auth == nil || s.Auth.Users == nil {
		writeError(w, http.StatusServiceUnavailable, "not_initialized", "user store is not configured")
		return false
	}
	return true
}

func (s *Server) resolveTargetUserID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := domain.ResolveUserID(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return "", false
	}
	return id, true
}

func (s *Server) revokeUserTokens(r *http.Request, id string) error {
	if s.Auth != nil && s.Auth.RevokeTokens != nil {
		return nil // already revoked inside Auth.UpdateUser / Auth.DeleteUser
	}
	if s.Tokens != nil {
		return s.Tokens.RevokeByOwner(r.Context(), id)
	}
	return nil
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	users, err := s.Auth.ListUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	items := make([]userJSON, len(users))
	for i, u := range users {
		items[i] = userView(u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	id, ok := s.resolveTargetUserID(w, r)
	if !ok {
		return
	}
	u, err := s.Auth.GetUser(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, userView(u))
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	var in struct {
		Email      string  `json:"email"`
		Name       string  `json:"name"`
		Password   *string `json:"password"`
		MustChange *bool   `json:"must_change_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	var pw, initial string
	mustChange := false
	if in.Password == nil {
		pw = auth.GeneratePassword()
		initial = pw
		mustChange = true
	} else {
		pw = *in.Password
	}
	if in.MustChange != nil {
		mustChange = *in.MustChange
	}
	u, err := s.Auth.CreateUser(r.Context(), auth.CreateUserInput{
		Email:      in.Email,
		Name:       in.Name,
		Password:   pw,
		MustChange: mustChange,
	})
	if err != nil {
		fail(w, err)
		return
	}
	res := map[string]any{"user": userView(u)}
	if initial != "" {
		res["initial_password"] = initial
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	id, ok := s.resolveTargetUserID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name       *string `json:"name"`
		Disabled   *bool   `json:"disabled"`
		Password   *string `json:"password"`
		MustChange *bool   `json:"must_change_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	p := principalOf(r)
	if p.ID != "" && p.ID == id && in.Disabled != nil && *in.Disabled {
		writeError(w, http.StatusBadRequest, "invalid_request", "you cannot disable your own account")
		return
	}
	u, err := s.Auth.UpdateUser(r.Context(), id, auth.UpdateUserInput{
		Name:       in.Name,
		Disabled:   in.Disabled,
		Password:   in.Password,
		MustChange: in.MustChange,
	})
	if err != nil {
		fail(w, err)
		return
	}
	if in.Disabled != nil && *in.Disabled {
		if rerr := s.revokeUserTokens(r, id); rerr != nil {
			fail(w, rerr)
			return
		}
	}
	writeJSON(w, http.StatusOK, userView(u))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	id, ok := s.resolveTargetUserID(w, r)
	if !ok {
		return
	}
	p := principalOf(r)
	if p.ID != "" && p.ID == id {
		writeError(w, http.StatusBadRequest, "invalid_request", "you cannot delete your own account")
		return
	}
	if err := s.Auth.DeleteUser(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	if err := s.revokeUserTokens(r, id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireUsers(w) {
		return
	}
	id, ok := s.resolveTargetUserID(w, r)
	if !ok {
		return
	}
	var in struct {
		MustChange *bool `json:"must_change_password"`
	}
	if r.Body != nil && r.Body != http.NoBody {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
			return
		}
	}
	mustChange := true
	if in.MustChange != nil {
		mustChange = *in.MustChange
	}
	u, pw, err := s.Auth.ResetUserPassword(r.Context(), id, mustChange)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userView(u), "password": pw})
}
