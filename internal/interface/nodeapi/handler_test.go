package nodeapi_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/interface/nodeapi"
	"github.com/db-branch/db-branch/internal/usecase/membership"
)

type fakeStore struct {
	servers  []membership.Server
	leader   bool
	leaderID string
	state    raftstore.State
	applyErr error
	voterErr error
	joinHash string
}

func (f *fakeStore) ApplyAt(context.Context, raftstore.Command) (uint64, error) {
	return 7, f.applyErr
}
func (f *fakeStore) IsLeader() bool                   { return f.leader }
func (f *fakeStore) Leader() (string, string)         { return f.leaderID, "" }
func (f *fakeStore) AddNonvoter(string, string) error { return f.voterErr }
func (f *fakeStore) Servers(context.Context) ([]membership.Server, error) {
	return f.servers, nil
}
func (f *fakeStore) JoinSecretHash() string { return f.joinHash }
func (f *fakeStore) State() raftstore.State { return f.state }

func asMember(r *http.Request) *http.Request {
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	return r
}

func call(t *testing.T, s *nodeapi.Server, path, body string, mem bool) int {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	if mem {
		r = asMember(r)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}

func Testハンドラーは入力の誤りと内部エラーを区別して返す(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	st := &fakeStore{leader: true}
	s := &nodeapi.Server{Store: st, CA: ca}

	for _, p := range []string{"/internal/v1/join", "/internal/v1/apply", "/internal/v1/voters"} {
		if code := call(t, s, p, "{", true); code != http.StatusBadRequest {
			t.Errorf("%s broken body: %d", p, code)
		}
	}
	if code := call(t, s, "/internal/v1/join", `{"secret":"x"}`, false); code != http.StatusForbidden {
		t.Errorf("join without a configured secret: %d", code)
	}
	st.applyErr = errors.New("disk full")
	if code := call(t, s, "/internal/v1/apply", `{"op":"put_node"}`, true); code != http.StatusInternalServerError {
		t.Errorf("apply failure: %d", code)
	}
	st.voterErr = errors.New("raft busy")
	if code := call(t, s, "/internal/v1/voters", `{"id":"n"}`, true); code != http.StatusInternalServerError {
		t.Errorf("add voter failure: %d", code)
	}
}

func Testフォロワーは投票者追加をリーダーへ回しリーダー不明なら503(t *testing.T) {
	var got string
	leader := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer leader.Close()
	addr := strings.TrimPrefix(leader.URL, "https://")
	st := &fakeStore{leaderID: "lead", state: raftstore.State{Nodes: map[string]domain.Node{"lead": {Name: "lead", Address: addr}}}}
	s := &nodeapi.Server{Store: st, Client: nodeapi.NewClient(&tls.Config{InsecureSkipVerify: true})}

	if code := call(t, s, "/internal/v1/voters", `{"id":"n","raft_address":"x:7071"}`, true); code != http.StatusNoContent || got != "/internal/v1/voters" {
		t.Errorf("forwarded: %d %q", code, got)
	}
	st.leaderID = ""
	if code := call(t, s, "/internal/v1/voters", `{"id":"n"}`, true); code != http.StatusServiceUnavailable {
		t.Errorf("no leader: %d", code)
	}
}

func Testクライアントは想定外の応答をエラーにする(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/join" {
			_, _ = w.Write([]byte("not json"))
			return
		}
		http.Error(w, "teapot", http.StatusTeapot)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	c := nodeapi.NewClient(&tls.Config{InsecureSkipVerify: true})
	if _, err := c.Forward(context.Background(), addr, raftstore.Command{}); err == nil || !strings.Contains(err.Error(), "teapot") {
		t.Errorf("Forward = %v", err)
	}
	if _, err := c.Forward(context.Background(), "bad host:1", raftstore.Command{}); err == nil {
		t.Error("bad address: want error")
	}
}

func Testクラスタの更新待ちは412で返しクライアントで元のエラーに戻す(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	st := &fakeStore{leader: true, applyErr: domain.ErrClusterUpgrading}
	s := &nodeapi.Server{Store: st, CA: ca}
	if code := call(t, s, "/internal/v1/apply", `{"op":"add_replica"}`, true); code != http.StatusPreconditionFailed {
		t.Errorf("status = %d", code)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "add_replica needs protocol 4", http.StatusPreconditionFailed)
	}))
	defer srv.Close()
	c := nodeapi.NewClient(&tls.Config{InsecureSkipVerify: true})
	if _, err := c.Forward(context.Background(), strings.TrimPrefix(srv.URL, "https://"), raftstore.Command{Op: "add_replica"}); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("client: %v", err)
	}
}

func Test参加の応答にクラスタのユーザーidを載せる(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	secret := "s3cret"
	st := &fakeStore{leader: true}
	st.joinHash = clusterpki.HashSecret(secret)
	for _, tc := range []struct {
		name string
		ids  func() domain.ServiceIDs
		want domain.ServiceIDs
	}{
		{"recorded", func() domain.ServiceIDs { return domain.ServiceIDs{UID: 109, GID: 112} }, domain.ServiceIDs{UID: 109, GID: 112}},
		{"not recorded", func() domain.ServiceIDs { return domain.ServiceIDs{} }, domain.ServiceIDs{}},
		{"no source", nil, domain.ServiceIDs{}},
	} {
		s := &nodeapi.Server{Store: st, CA: ca, ServiceIDs: tc.ids}
		r := httptest.NewRequest("POST", "/internal/v1/join", strings.NewReader(`{"secret":"`+secret+`"}`))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var out struct {
			IDs *domain.ServiceIDs `json:"service_ids"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		got := domain.ServiceIDs{}
		if out.IDs != nil {
			got = *out.IDs
		}
		if got != tc.want {
			t.Errorf("%s: ids %v, want %v", tc.name, got, tc.want)
		}
	}
}

func Testリーダーは構成から外れたノードのハートビートを受け付けない(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	st := &fakeStore{leader: true, servers: []membership.Server{{ID: "n1", Voter: true}, {ID: "n2"}}}
	s := &nodeapi.Server{Store: st, CA: ca}
	put := func(name string) int {
		return call(t, s, "/internal/v1/apply", `{"op":"put_node","node":{"name":"`+name+`"}}`, true)
	}
	if code := put("n2"); code != http.StatusNoContent && code != http.StatusOK {
		t.Errorf("a member: %d", code)
	}
	if code := put("n3"); code != http.StatusNotFound {
		t.Errorf("a node outside the configuration: %d", code)
	}
	// Other commands are not affected, and a follower leaves the check to the leader.
	if code := call(t, s, "/internal/v1/apply", `{"op":"delete_node","name":"n3"}`, true); code != http.StatusOK {
		t.Errorf("delete_node: %d", code)
	}
	st.leader = false
	if code := put("n3"); code != http.StatusOK {
		t.Errorf("on a follower: %d", code)
	}
}
