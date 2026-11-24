package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
	"github.com/db-branch/db-branch/internal/usecase/membership"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

type maskTestCluster struct {
	forwarded []string
	peer      http.Handler
}

func (c *maskTestCluster) Status(context.Context) (ClusterStatus, error) {
	return ClusterStatus{Mode: "cluster", Leader: "node1"}, nil
}
func (c *maskTestCluster) NewToken(context.Context, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (c *maskTestCluster) Forward(node string) (http.Handler, error) {
	if c.peer != nil {
		return c.peer, nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.forwarded = append(c.forwarded, node+" "+r.Method+" "+r.URL.Path+" "+string(b))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"op_remote"}`))
	}), nil
}
func (c *maskTestCluster) NodeHost(string) string                          { return "" }
func (c *maskTestCluster) BranchUsage(string, string) (domain.Usage, bool) { return domain.Usage{}, false }
func (c *maskTestCluster) NodeCheckpoints(string) bool                     { return false }
func (c *maskTestCluster) AddRuntime(context.Context, domain.RuntimeRef) error {
	return nil
}
func (c *maskTestCluster) RemoveRuntime(context.Context, domain.RuntimeRef) error {
	return nil
}
func (c *maskTestCluster) RemoveNode(context.Context, string, bool) (membership.RemoveReport, error) {
	return membership.RemoveReport{}, nil
}

type maskTestBranches struct {
	Branches
	base   domain.Base
	masked chan dbbranch.MaskRequest
}

func (b *maskTestBranches) GetBase(context.Context, domain.Name) (domain.Base, error) {
	return b.base, nil
}

func (b *maskTestBranches) MaskBase(_ context.Context, req dbbranch.MaskRequest, _ dbbranch.Reporter) (domain.BaseSnapshot, error) {
	b.masked <- req
	return domain.BaseSnapshot{Name: "s3"}, nil
}

func TestHTTPAPI_MaskBase_他ノード転送時にリクエストボディが消失しない(t *testing.T) {
	c := &maskTestCluster{}
	n, _ := domain.ParseName("dev")
	bus := operation.NewBus(100)
	br := &maskTestBranches{base: domain.Base{Name: n, Node: "n2", Status: domain.BaseReady}, masked: make(chan dbbranch.MaskRequest, 1)}
	srv := &Server{
		Node:     "node1",
		Cluster:  c,
		Ops:      &operation.Manager{Bus: bus, Clock: time.Now},
		Bus:      bus,
		Branches: func() (Branches, error) { return br, nil },
	}
	payload := `{"source":"prod","snapshot":"s1","masker":"greenmask","rules":"x: y"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bases/dev/mask", strings.NewReader(payload))
	req.SetPathValue("name", "dev")
	req = req.WithContext(withPrincipal(req.Context(), LocalPrincipal))
	rec := httptest.NewRecorder()
	srv.named(srv.maskBase)(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if len(c.forwarded) != 1 || !bytes.Contains([]byte(c.forwarded[0]), []byte(payload)) {
		t.Fatalf("forwarded = %v, want payload %s", c.forwarded, payload)
	}

	// Local execution and validation (kept covered while route is hidden in 1.0).
	br.base.Node = "node1"
	for _, body := range []string{`{`, `{"source":"Bad","masker":"m"}`, `{"source":"prod"}`} {
		r := httptest.NewRequest(http.MethodPost, "/v1/bases/dev/mask", strings.NewReader(body))
		r.SetPathValue("name", "dev")
		r = r.WithContext(withPrincipal(r.Context(), LocalPrincipal))
		w := httptest.NewRecorder()
		srv.named(srv.maskBase)(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("bad body %s: status=%d", body, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/bases/dev/mask", strings.NewReader(payload))
	r.SetPathValue("name", "dev")
	r = r.WithContext(withPrincipal(r.Context(), LocalPrincipal))
	w := httptest.NewRecorder()
	srv.named(srv.maskBase)(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("local mask status=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case got := <-br.masked:
		if got.Source.String() != "prod" || got.Masker != "greenmask" || got.Rules != "x: y" {
			t.Errorf("lastMask = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MaskBase")
	}

	// StartMaskOn still works through the peer's real Handler(true) even though
	// local unforwarded Handler(true) and public Handler(false) return 404 in 1.0.
	peerBr := &maskTestBranches{base: domain.Base{Name: n, Node: "n2", Status: domain.BaseReady}, masked: make(chan dbbranch.MaskRequest, 1)}
	peerBus := operation.NewBus(100)
	peerSrv := &Server{
		Node:     "n2",
		Ops:      &operation.Manager{Bus: peerBus, Clock: time.Now},
		Bus:      peerBus,
		Branches: func() (Branches, error) { return peerBr, nil },
	}
	for _, tc := range []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{"unforwarded local", peerSrv.Handler(true), "/v1/bases/dev/mask"},
		{"public", peerSrv.Handler(false), "/v1/bases/dev/mask"},
	} {
		unfwd := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(payload))
		uw := httptest.NewRecorder()
		tc.handler.ServeHTTP(uw, unfwd)
		if uw.Code != http.StatusNotFound && uw.Code != http.StatusUnauthorized {
			t.Errorf("%s POST %s status = %d, want 404/401", tc.name, tc.path, uw.Code)
		}
	}
	c.peer = peerSrv.Handler(true)
	srcName, _ := domain.ParseName("prod")
	if err := srv.StartMaskOn(context.Background(), "n2", dbbranch.MaskRequest{Target: n, Source: srcName, Snapshot: "s1", Masker: "greenmask"}); err != nil {
		t.Fatalf("StartMaskOn via real peer Handler(true): %v", err)
	}
	select {
	case got := <-peerBr.masked:
		if got.Target.String() != "dev" || got.Source.String() != "prod" || got.Masker != "greenmask" {
			t.Errorf("peer masked = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for peer MaskBase")
	}
}
