package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

// Licensing checks and applies licence keys for paid modules.
type Licensing interface {
	Verify(ctx context.Context, key string) (licensing.Report, error)
	Apply(ctx context.Context, key string) (licensing.Report, error)
	// Current checks the saved key (false when none is saved).
	Current(ctx context.Context) (licensing.Report, bool, error)
}

// Renewal is the automatic renewal of the saved key. Only the cluster's
// leader knows its status and may contact the licence service.
type Renewal interface {
	// Status is this node's view: "unknown" unless it leads.
	Status(ctx context.Context) licensing.RenewalStatus
	Leading() bool
	// CheckNow renews now (the leader only).
	CheckNow(ctx context.Context) (licensing.RenewalStatus, error)
}

func (s *Server) withLicensing(w http.ResponseWriter) bool {
	if s.Licensing == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "licences are not available on this node")
		return false
	}
	return true
}

func licenseFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, licensing.ErrNoRoot):
		writeError(w, http.StatusNotImplemented, "cannot_verify", err.Error())
	case errors.Is(err, licensing.ErrUnusable):
		writeError(w, http.StatusUnprocessableEntity, "license_unusable", err.Error())
	case errors.Is(err, license.ErrInvalid), errors.Is(err, license.ErrNotCovered):
		writeError(w, http.StatusUnprocessableEntity, "invalid_license", err.Error())
	default:
		fail(w, err)
	}
}

func (s *Server) licenseKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &in) {
		return "", false
	}
	if strings.TrimSpace(in.Key) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "key is empty")
		return "", false
	}
	return in.Key, true
}

// verifyLicense checks a key and says what it covers, without saving it.
func (s *Server) verifyLicense(w http.ResponseWriter, r *http.Request) {
	if !s.withLicensing(w) {
		return
	}
	key, ok := s.licenseKey(w, r)
	if !ok {
		return
	}
	rep, err := s.Licensing.Verify(r.Context(), key)
	if err != nil {
		licenseFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// applyLicense saves a usable key and enables the modules it covers.
func (s *Server) applyLicense(w http.ResponseWriter, r *http.Request) {
	if !s.withLicensing(w) {
		return
	}
	key, ok := s.licenseKey(w, r)
	if !ok {
		return
	}
	rep, err := s.Licensing.Apply(r.Context(), key)
	if err != nil {
		licenseFail(w, err)
		return
	}
	s.Bus.Publish("module.updated", map[string]string{})
	writeJSON(w, http.StatusOK, rep)
}

// currentLicense reports the saved key's state, without the key itself;
// without a key, the trial's once it began.
func (s *Server) currentLicense(w http.ResponseWriter, r *http.Request) {
	if !s.withLicensing(w) {
		return
	}
	rep, configured, err := s.Licensing.Current(r.Context())
	out := map[string]any{"configured": configured}
	if st := s.renewalStatus(r); st != nil {
		out["renewal"] = st
	}
	switch {
	case configured && err != nil:
		out["error"] = err.Error()
	case configured || rep.Trial:
		out["report"] = rep
	}
	writeJSON(w, http.StatusOK, out)
}

// leaderNode is the cluster's leader when it is another node ("" when this
// node leads, there is no cluster, or the request was forwarded here).
func (s *Server) leaderNode(r *http.Request) (string, error) {
	if s.Cluster == nil || s.Renewal.Leading() || forwardedFromNode(r) {
		return "", nil
	}
	st, err := s.Cluster.Status(r.Context())
	if err != nil {
		return "", err
	}
	if st.Leader == "" {
		return "", domain.ErrNoLeader
	}
	if st.Leader == s.Node {
		return "", nil
	}
	return st.Leader, nil
}

// renewalStatus is the leader's renewal status: asked of the leader when
// another node leads, and "unknown" when that fails.
func (s *Server) renewalStatus(r *http.Request) *licensing.RenewalStatus {
	if s.Renewal == nil {
		return nil
	}
	local := s.Renewal.Status(r.Context())
	leader, err := s.leaderNode(r)
	if err != nil || leader == "" {
		return &local
	}
	req := r.Clone(r.Context())
	req.Method, req.Body = http.MethodGet, nil
	req.URL.Path, req.URL.RawQuery = "/v1/license/renewal", ""
	rec, err := s.forwardTo(req, leader)
	var st licensing.RenewalStatus
	if err != nil || rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &st) != nil {
		return &local
	}
	return &st
}

func (s *Server) withRenewal(w http.ResponseWriter, r *http.Request) (proceed bool) {
	if s.Renewal == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "automatic renewal is not available on this node")
		return false
	}
	leader, err := s.leaderNode(r)
	if err != nil {
		fail(w, err)
		return false
	}
	return leader == "" || !s.forward(w, r, leader)
}

// getRenewal reports the renewal status, from the leader.
func (s *Server) getRenewal(w http.ResponseWriter, r *http.Request) {
	if !s.withRenewal(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.Renewal.Status(r.Context()))
}

// renewLicense asks the licence service for the renewed key now ("Check now").
func (s *Server) renewLicense(w http.ResponseWriter, r *http.Request) {
	if !s.withRenewal(w, r) {
		return
	}
	st, err := s.Renewal.CheckNow(r.Context())
	var soon *licensing.TooSoonError
	switch {
	case errors.As(err, &soon):
		w.Header().Set("Retry-After", strconv.Itoa(int(soon.RetryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, licensing.ErrAutoRenewOff):
		writeError(w, http.StatusConflict, "auto_renew_off", err.Error())
	case errors.Is(err, licensing.ErrNotRenewable):
		writeError(w, http.StatusConflict, "not_renewable", err.Error())
	case errors.Is(err, licensing.ErrInFlight):
		writeError(w, http.StatusConflict, "in_progress", err.Error())
	case errors.Is(err, licensing.ErrNotLeader):
		fail(w, domain.ErrNoLeader)
	case err != nil:
		licenseFail(w, err)
	default:
		if st.LastResult == licensing.ResultRenewed {
			s.Bus.Publish("module.updated", map[string]string{})
		}
		writeJSON(w, http.StatusOK, st)
	}
}
