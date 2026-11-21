package nodeapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/interface/nodeapi"
)

var ctx = context.Background()

type member struct {
	store  *raftstore.Store
	cert   *clusterpki.NodeCert
	api    string
	raft   string
	server *http.Server
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// start runs a real node: Raft over TLS and the node API over mutual TLS.
func start(t *testing.T, ca *clusterpki.CA, id string) *member {
	t.Helper()
	return startWithAPI(t, ca, id, nil)
}

func startWithAPI(t *testing.T, ca *clusterpki.CA, id string, api http.Handler) *member {
	t.Helper()
	return startWith(t, ca, id, api, nil)
}

func startWith(t *testing.T, ca *clusterpki.CA, id string, api http.Handler, send func(context.Context, domain.Name, string, string, io.Writer) error) *member {
	t.Helper()
	return startFull(t, ca, id, &nodeapi.Server{API: api, SendBase: send})
}

// startFull runs a node whose node API has the handlers set in srv.
func startFull(t *testing.T, ca *clusterpki.CA, id string, srv *nodeapi.Server) *member {
	t.Helper()
	cert, err := ca.IssueNode(t.TempDir(), id, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	m := &member{cert: cert, api: freeAddr(t), raft: freeAddr(t)}
	tr, err := raftstore.NewTLSTransport(m.raft, m.raft, cert.ServerTLS(true), cert.ClientTLS(), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := nodeapi.NewClient(cert.ClientTLS())
	m.store, err = raftstore.Open(raftstore.Options{NodeID: id, APIAddr: m.api, Transport: tr, InMemory: true, Fast: true, Forwarder: client})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", m.api)
	if err != nil {
		t.Fatal(err)
	}
	srv.Store, srv.Client, srv.CA = m.store, client, ca
	m.server = &http.Server{Handler: srv.Handler(), TLSConfig: cert.ServerTLS(false)}
	go func() { _ = m.server.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = m.server.Close(); _ = m.store.Close() })
	return m
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestJoin_トークンでCAを受け取り投票者として加わり書き込みが転送される(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	a := start(t, ca, "node-a")
	if err := a.store.Bootstrap([]raft.Server{{ID: "node-a", Address: raft.ServerAddress(a.raft)}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, a.store.IsLeader)
	_ = a.store.PutNode(ctx, domain.Node{Name: "node-a", Address: a.api, RaftAddress: a.raft})
	secret := clusterpki.NewSecret()
	_ = a.store.SetJoinSecretHash(ctx, clusterpki.HashSecret(secret))

	// The joiner has no certificate yet: it pins the CA from the token.
	tok := clusterpki.Token{Fingerprint: ca.Fingerprint(), Secret: secret}
	info, err := nodeapi.Join(ctx, a.api, tok)
	if err != nil {
		t.Fatal(err)
	}
	if info.ServiceIDs.Valid() {
		t.Errorf("a server without ids told %v", info.ServiceIDs)
	}
	got, err := clusterpki.ParseCA(info.CACert, info.CAKey)
	if err != nil || got.Fingerprint() != ca.Fingerprint() {
		t.Fatalf("received CA: %v", err)
	}

	b := start(t, got, "node-b")
	if err := nodeapi.NewClient(b.cert.ClientTLS()).AddMember(ctx, a.api, "node-b", b.raft); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { nodes, _ := b.store.ListNodes(ctx); return len(nodes) == 1 })

	// node-b follows; its writes travel to node-a over the node API.
	if err := b.store.PutBase(ctx, domain.Base{Name: mustName("prod")}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, err := a.store.GetBase(ctx, mustName("prod")); return err == nil })
}

func TestJoin_秘密値や指紋が違えば参加できない(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	a := start(t, ca, "node-a")
	_ = a.store.Bootstrap([]raft.Server{{ID: "node-a", Address: raft.ServerAddress(a.raft)}})
	eventually(t, a.store.IsLeader)
	_ = a.store.SetJoinSecretHash(ctx, clusterpki.HashSecret("right"))

	if _, err := nodeapi.Join(ctx, a.api, clusterpki.Token{Fingerprint: ca.Fingerprint(), Secret: "wrong"}); !errors.Is(err, nodeapi.ErrRejected) {
		t.Errorf("wrong secret: %v", err)
	}
	other, _ := clusterpki.InitCA(t.TempDir())
	if _, err := nodeapi.Join(ctx, a.api, clusterpki.Token{Fingerprint: other.Fingerprint(), Secret: "right"}); err == nil {
		t.Error("wrong fingerprint: want error")
	}
}

func Test証明書のない接続は書き込みや投票者追加ができない(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	a := start(t, ca, "node-a")
	_ = a.store.Bootstrap([]raft.Server{{ID: "node-a", Address: raft.ServerAddress(a.raft)}})
	eventually(t, a.store.IsLeader)

	anon := nodeapi.NewClient(clusterpki.PinnedTLS(ca.Fingerprint()))
	if _, err := anon.Forward(ctx, a.api, raftstore.PutBase(domain.Base{Name: mustName("x")})); !errors.Is(err, nodeapi.ErrRejected) {
		t.Errorf("anonymous apply: %v", err)
	}
	if err := anon.AddMember(ctx, a.api, "evil", "127.0.0.1:1"); !errors.Is(err, nodeapi.ErrRejected) {
		t.Errorf("anonymous add voter: %v", err)
	}
}

func Test届かない相手やリーダー不在はエラー(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	n, _ := ca.IssueNode(t.TempDir(), "n", nil)
	c := nodeapi.NewClient(n.ClientTLS())
	dead := freeAddr(t)
	if _, err := c.Forward(ctx, dead, raftstore.PutBase(domain.Base{Name: mustName("x")})); err == nil {
		t.Error("Forward to nowhere: want error")
	}
	if err := c.AddMember(ctx, dead, "x", "y"); err == nil {
		t.Error("AddVoter to nowhere: want error")
	}
	if _, err := nodeapi.Join(ctx, dead, clusterpki.Token{Fingerprint: ca.Fingerprint(), Secret: "s"}); err == nil {
		t.Error("Join nowhere: want error")
	}

	// A node that never bootstrapped has no leader.
	lonely := start(t, ca, "lonely")
	if err := c.AddMember(ctx, lonely.api, "x", "127.0.0.1:1"); !errors.Is(err, domain.ErrNoLeader) {
		t.Errorf("no leader: %v", err)
	}
}

func mustName(s string) domain.Name { n, _ := domain.ParseName(s); return n }

func TestProxy_他のノードのAPIへ証明書付きで中継しCookieは渡さない(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, cookie := r.Header["Cookie"]
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, "%s %s cookie=%v", r.Method, r.URL.Path, cookie)
	})
	a := startWithAPI(t, ca, "node-a", api)
	n, _ := ca.IssueNode(t.TempDir(), "node-b", nil)

	r := httptest.NewRequest("POST", "/v1/branches/x/reset", nil)
	r.Header.Set("Cookie", "dbb_session=secret")
	r.Header.Set("X-Db-Branch-Forwarded", "node-b")
	r.Header.Set("X-Db-Branch-Principal", `{"id":"local","name":"local","roles":["admin"]}`)
	w := httptest.NewRecorder()
	nodeapi.NewClient(n.ClientTLS()).Proxy(a.api).ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || w.Body.String() != "POST /v1/branches/x/reset cookie=false" {
		t.Errorf("proxied: %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	nodeapi.NewClient(clusterpki.PinnedTLS(ca.Fingerprint())).Proxy(a.api).ServeHTTP(w, httptest.NewRequest("GET", "/v1/branches", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("without a member certificate: %d", w.Code)
	}

	w = httptest.NewRecorder()
	nodeapi.NewClient(n.ClientTLS()).Proxy(freeAddr(t)).ServeHTTP(w, httptest.NewRequest("GET", "/v1/branches", nil))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "node_unreachable") {
		t.Errorf("unreachable: %d %s", w.Code, w.Body.String())
	}
}

func TestProxy_Connectionヘッダで転送ヘッダを除去できず転送ヘッダ欠落要求は拒否される(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "fwd=%s prin=%s cookie=%v",
			r.Header.Get("X-Db-Branch-Forwarded"),
			r.Header.Get("X-Db-Branch-Principal"),
			r.Header.Get("Cookie") != "")
	})
	a := startWithAPI(t, ca, "node-a", api)
	n, _ := ca.IssueNode(t.TempDir(), "node-b", nil)
	proxy := nodeapi.NewClient(n.ClientTLS()).Proxy(a.api)

	// 1. Multi-line/lowercase Connection headers and non-canonical keys cannot drop forwarding headers.
	r := httptest.NewRequest("POST", "/v1/branches/x/reset", nil)
	r.Header.Add("Connection", "keep-alive")
	r.Header.Add("Connection", "x-db-branch-forwarded, x-db-branch-principal")
	r.Header["X-DB-BRANCH-FORWARDED"] = []string{"", "node-b"}
	r.Header["x-db-branch-principal"] = []string{`{"id":"sso:alice","roles":["viewer"]}`}
	r.Header["COOKIE"] = []string{"dbb_session=secret"}
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	want := `fwd=node-b prin={"id":"sso:alice","roles":["viewer"]} cookie=false`
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("proxied: %d %q, want 200 %q", w.Code, w.Body.String(), want)
	}

	// 2. Receiving /internal/v1/api/ rejects requests missing forwarding headers (e.g. stripped by an older node).
	for _, tc := range []struct {
		name string
		set  func(http.Header)
	}{
		{"none", func(http.Header) {}},
		{"forwarded_only", func(h http.Header) { h.Set("X-Db-Branch-Forwarded", "node-b") }},
		{"principal_only", func(h http.Header) { h.Set("X-Db-Branch-Principal", `{"id":"sso:alice"}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/branches", nil)
			tc.set(req.Header)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d %s, want 403", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestForward_リーダーで名前が重複したらAlreadyExistsを返す(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	a := start(t, ca, "node-a")
	_ = a.store.Bootstrap([]raft.Server{{ID: "node-a", Address: raft.ServerAddress(a.raft)}})
	eventually(t, a.store.IsLeader)
	_ = a.store.PutBase(ctx, domain.Base{Name: mustName("x")})
	n, _ := ca.IssueNode(t.TempDir(), "n", nil)
	_, err := nodeapi.NewClient(n.ClientTLS()).Forward(ctx, a.api, raftstore.InsertBase(domain.Base{Name: mustName("x")}))
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("err = %v", err)
	}
}

func TestStream_ベースのスナップショットを流し誤りは状態で返す(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	send := func(_ context.Context, base domain.Name, from, to string, w io.Writer) error {
		switch to {
		case "missing":
			return fmt.Errorf("snapshot: %w", domain.ErrNotFound)
		case "broken":
			return errors.New("zfs send failed")
		case "midway":
			_, _ = io.WriteString(w, "partial")
			return errors.New("zfs send died")
		case "empty":
			return nil
		}
		_, err := fmt.Fprintf(w, "%s:%s..%s", base, from, to)
		return err
	}
	a := startWith(t, ca, "node-a", nil, send)
	n, _ := ca.IssueNode(t.TempDir(), "node-b", nil)
	c := nodeapi.NewClient(n.ClientTLS())

	rc, err := c.OpenStream(ctx, a.api, mustName("shop"), "s1", "s2")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "shop:s1..s2" {
		t.Errorf("stream = %q", b)
	}
	if rc, err := c.OpenStream(ctx, a.api, mustName("shop"), "", "empty"); err != nil {
		t.Errorf("empty: %v", err)
	} else {
		rc.Close()
	}
	if _, err := c.OpenStream(ctx, a.api, mustName("shop"), "", "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := c.OpenStream(ctx, a.api, mustName("shop"), "", "broken"); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("broken: %v", err)
	}
	rc, err = c.OpenStream(ctx, a.api, mustName("shop"), "", "midway")
	if err == nil {
		_, err = io.ReadAll(rc)
		rc.Close()
	}
	if err == nil {
		t.Error("a stream cut midway must not read as complete")
	}
	if _, err := nodeapi.NewClient(clusterpki.PinnedTLS(ca.Fingerprint())).OpenStream(ctx, a.api, mustName("shop"), "", "s1"); err == nil {
		t.Error("without a member certificate: want error")
	}
	if _, err := c.OpenStream(ctx, freeAddr(t), mustName("shop"), "", "s1"); err == nil {
		t.Error("unreachable: want error")
	}
	w := httptest.NewRecorder()
	(&nodeapi.Server{SendBase: send}).Handler().ServeHTTP(w, asMember(httptest.NewRequest("GET", "/internal/v1/bases/Bad/stream?to=s1", nil)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", w.Code)
	}
}

func TestArtifact_リーダーがランタイムの書庫を配る(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	open := func(_ context.Context, ref domain.RuntimeRef, arch string, part int) (io.ReadCloser, error) {
		switch arch {
		case "amd64":
			return io.NopCloser(strings.NewReader(fmt.Sprintf("archive of %s part %d", ref, part))), nil
		case "riscv64":
			return nil, fmt.Errorf("%w: no build", domain.ErrRuntimeNotFound)
		}
		return nil, errors.New("upstream down")
	}
	a := startFull(t, ca, "node-a", &nodeapi.Server{Artifact: open})
	n, _ := ca.IssueNode(t.TempDir(), "node-b", nil)
	c := nodeapi.NewClient(n.ClientTLS())
	ref := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}

	rc, err := c.OpenArtifact(ctx, a.api, ref, "amd64", 2)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "archive of mysql@8.4.13 part 2" {
		t.Errorf("body %q", b)
	}
	if _, err := c.OpenArtifact(ctx, a.api, ref, "riscv64", 0); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no build: %v", err)
	}
	if _, err := c.OpenArtifact(ctx, a.api, ref, "arm64", 0); err == nil {
		t.Error("upstream failure: want error")
	}
	if _, err := c.OpenArtifact(ctx, a.api, domain.RuntimeRef{Engine: "oracle", Version: "1"}, "amd64", 0); err == nil {
		t.Error("bad runtime: want error")
	}
}

func TestForward_走っているロールアウトがあればErrRolloutRunningを返す(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	a := start(t, ca, "node-a")
	_ = a.store.Bootstrap([]raft.Server{{ID: "node-a", Address: raft.ServerAddress(a.raft)}})
	eventually(t, a.store.IsLeader)
	r := domain.NewRollout("r1", "1.1.0", []string{"x"}, map[string]string{"x": "1.0.0"}, time.Unix(1, 0))
	_ = a.store.StartRollout(ctx, r)
	n, _ := ca.IssueNode(t.TempDir(), "n", nil)
	_, err := nodeapi.NewClient(n.ClientTLS()).Forward(ctx, a.api, raftstore.StartRollout(r))
	if !errors.Is(err, domain.ErrRolloutRunning) || !errors.Is(err, domain.ErrConflict) {
		t.Errorf("err = %v", err)
	}
}
