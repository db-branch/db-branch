package httpapi

import (
	"context"
	"github.com/db-branch/db-branch/internal/usecase/membership"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/pkg/ext"
)

type stubCluster struct {
	leader     string
	forwarded  int
	gotFwd     string
	gotConn    string
	gotPrincip string
	gotKeys    []string
}

func (c *stubCluster) Status(context.Context) (ClusterStatus, error) {
	return ClusterStatus{Mode: "cluster", Leader: c.leader}, nil
}
func (c *stubCluster) NewToken(context.Context) (string, error) { return "", nil }
func (c *stubCluster) Forward(string) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.forwarded++
		c.gotFwd = r.Header.Get(forwardedHeader)
		c.gotConn = r.Header.Get("Connection")
		c.gotPrincip = r.Header.Get(principalHeader)
		c.gotKeys = nil
		for k := range r.Header {
			c.gotKeys = append(c.gotKeys, k)
		}
		w.WriteHeader(http.StatusOK)
	}), nil
}
func (c *stubCluster) NodeHost(string) string                                 { return "" }
func (c *stubCluster) BranchUsage(string, string) (domain.Usage, bool)        { return domain.Usage{}, false }
func (c *stubCluster) NodeCheckpoints(string) bool                            { return false }
func (c *stubCluster) AddRuntime(context.Context, domain.RuntimeRef) error    { return nil }
func (c *stubCluster) RemoveRuntime(context.Context, domain.RuntimeRef) error { return nil }
func (c *stubCluster) RemoveNode(context.Context, string, bool) (membership.RemoveReport, error) {
	return membership.RemoveReport{}, nil
}

type stubRenewal struct{}

func (stubRenewal) Status(context.Context) licensing.RenewalStatus {
	return licensing.RenewalStatus{}
}
func (stubRenewal) Leading() bool { return false }
func (stubRenewal) CheckNow(context.Context) (licensing.RenewalStatus, error) {
	return licensing.RenewalStatus{}, nil
}

func TestForwarded_生ヘッダのみでcontext未設定なら転送済みとみなさない(t *testing.T) {
	c := &stubCluster{leader: "n2"}
	s := &Server{Node: "node1", Cluster: c, Renewal: stubRenewal{}}

	req := httptest.NewRequest(http.MethodPost, "/v1/branches/there/reset", nil)
	req = req.WithContext(withPrincipal(req.Context(), ext.Principal{ID: "sso:alice", Name: "Alice", Roles: []string{"viewer"}}))
	req.Header.Set(forwardedHeader, "spoof")
	req.Header.Set(principalHeader, `{"id":"evil"}`)
	req.Header.Set("Connection", "X-Db-Branch-Forwarded, X-Db-Branch-Principal")
	req.Header["X-DB-BRANCH-PRINCIPAL"] = []string{`{"id":"noncanonical"}`}

	// forward() must rely on forwardedFromNode(r), not the raw header.
	rec := httptest.NewRecorder()
	if !s.forward(rec, req, "n2") || rec.Code != http.StatusOK || c.forwarded != 1 {
		t.Fatalf("forward with raw header only: status=%d forwarded=%d, want 200 and 1", rec.Code, c.forwarded)
	}
	if c.gotFwd != "node1" || c.gotConn != "" {
		t.Errorf("forwarded request headers: fwd=%q conn=%q, want node1 and empty", c.gotFwd, c.gotConn)
	}
	if c.gotPrincip != `{"id":"sso:alice","name":"Alice","roles":["viewer"]}` {
		t.Errorf("forwarded principal = %q, want sso:alice", c.gotPrincip)
	}
	for _, k := range c.gotKeys {
		if k == "X-DB-BRANCH-PRINCIPAL" {
			t.Errorf("non-canonical principal header leaked: %v", c.gotKeys)
		}
	}
	if req.Header.Get(forwardedHeader) != "spoof" || req.Header.Get(principalHeader) != `{"id":"evil"}` {
		t.Errorf("forwardTo mutated caller header: fwd=%q principal=%q", req.Header.Get(forwardedHeader), req.Header.Get(principalHeader))
	}

	// leaderNode() must rely on forwardedFromNode(r), not the raw header.
	leader, err := s.leaderNode(req)
	if err != nil || leader != "n2" {
		t.Fatalf("leaderNode with raw header only: leader=%q err=%v, want \"n2\"", leader, err)
	}

	// forwardTo must refuse to send an empty X-Db-Branch-Forwarded header when Node is unset.
	unnamed := &Server{Cluster: c}
	if _, err := unnamed.forwardTo(req, "n2"); err == nil {
		t.Error("forwardTo with empty Node: want error")
	}
}

func TestRequireSession_公開入口で偽装ヘッダを大小文字や空値を含め除去する(t *testing.T) {
	s := &Server{}
	var seen http.Header
	h := s.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name string
		set  func(http.Header)
	}{
		{"canonical", func(hdr http.Header) {
			hdr.Set(forwardedHeader, "n9")
			hdr.Set(principalHeader, `{"id":"local"}`)
		}},
		{"empty_and_multi", func(hdr http.Header) {
			hdr[forwardedHeader] = []string{"", "n9"}
			hdr[principalHeader] = []string{""}
		}},
		{"non_canonical_key", func(hdr http.Header) {
			hdr["X-DB-BRANCH-FORWARDED"] = []string{"n9"}
			hdr["x-db-branch-principal"] = []string{`{"id":"local"}`}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen = nil
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
			tc.set(req.Header)
			origCount := len(req.Header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			for k := range seen {
				if k == forwardedHeader || k == principalHeader || k == "X-DB-BRANCH-FORWARDED" || k == "x-db-branch-principal" {
					t.Errorf("header %q leaked to downstream handler: %v", k, seen[k])
				}
			}
			if len(req.Header) != origCount {
				t.Errorf("caller request header mutated: got %v", req.Header)
			}
		})
	}
}
