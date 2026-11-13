package licenseclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/licenseclient"
	"github.com/db-branch/db-branch/internal/usecase/licensing"
	"github.com/db-branch/db-branch/pkg/ext/license"
)

type memSettings map[string]string

func (m memSettings) Values(_ context.Context, _ string) (map[string]string, error) { return m, nil }
func (m memSettings) Put(_ context.Context, _ string, v map[string]string) error {
	for k, x := range v {
		m[k] = x
	}
	return nil
}

// A renewer with the real client gets the new key from a server that
// follows the contract, and saves it.
func TestRenewer_契約どおりのサーバーから更新された鍵を受け取り保存する(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root, issuing := license.NewLocalSigner(), license.NewLocalSigner()
	cert, _ := license.Certify(ctx, root, issuing.Public(), "t", now.AddDate(-1, 0, 0), now.AddDate(3, 0, 0))
	mk := func(expires time.Time, renewURL string) string {
		k, err := license.Issue(ctx, issuing, cert, license.License{ID: "L1", Customer: "Acme", Modules: []string{"audit"}, IssuedAt: now,
			Expires: expires, Billing: license.Monthly, Subscription: "sub_1", RenewURL: renewURL})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	var next, seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in struct{ Key string }
		_ = json.Unmarshal(b, &in)
		seen = in.Key
		_ = json.NewEncoder(w).Encode(map[string]string{"key": next})
	}))
	defer srv.Close()
	cur := mk(now.AddDate(0, 0, 2), srv.URL+"/renew")
	next = mk(now.AddDate(0, 1, 2), srv.URL+"/renew")
	set := memSettings{licensing.KeySetting: cur}
	svc := &licensing.Service{Root: root.Public(), Clock: func() time.Time { return now }, Nodes: func() int { return 1 }, Settings: set}
	r := &licensing.Renewer{Service: svc, Client: &licenseclient.Client{Version: "test"}, Leader: func() bool { return true }}
	st, err := r.CheckNow(ctx)
	if err != nil || st.LastResult != licensing.ResultRenewed || set[licensing.KeySetting] != next || seen != cur {
		t.Fatalf("%+v %v", st, err)
	}
}

type memClusterID struct{ id string }

func (m *memClusterID) ClusterID(context.Context) string { return m.id }
func (m *memClusterID) InitClusterID(_ context.Context, id string) (string, error) {
	if m.id == "" {
		m.id = id
	}
	return m.id, nil
}

// The renewer draws the cluster id once and the real client sends it next to
// the key, in every request; the server sees nothing else.
func TestRenewer_クラスタidを一度だけ作り鍵と一緒に毎回送る(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	root, issuing := license.NewLocalSigner(), license.NewLocalSigner()
	cert, _ := license.Certify(ctx, root, issuing.Public(), "t", now.AddDate(-1, 0, 0), now.AddDate(3, 0, 0))
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in map[string]string
		_ = json.Unmarshal(b, &in)
		bodies = append(bodies, in)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	cur, err := license.Issue(ctx, issuing, cert, license.License{ID: "L1", Customer: "Acme", Modules: []string{"audit"}, IssuedAt: now,
		Expires: now.AddDate(0, 0, 2), Billing: license.Monthly, Subscription: "sub_1", RenewURL: srv.URL + "/renew"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &licensing.Service{Root: root.Public(), Clock: func() time.Time { return now }, Nodes: func() int { return 1 }, Settings: memSettings{licensing.KeySetting: cur}}
	r := &licensing.Renewer{Service: svc, Client: &licenseclient.Client{Version: "test"}, Leader: func() bool { return true }, Cluster: &memClusterID{}}
	for i := 0; i < 2; i++ {
		now = now.Add(time.Minute)
		if _, err := r.CheckNow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(bodies) != 2 || len(bodies[0]) != 2 || bodies[0]["key"] != cur || !domain.ValidClusterID(bodies[0]["cluster"]) || bodies[1]["cluster"] != bodies[0]["cluster"] {
		t.Errorf("送ったもの: %v", bodies)
	}
}
