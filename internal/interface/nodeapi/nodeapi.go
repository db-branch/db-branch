// Package nodeapi is the node-to-node API (port 7070): forwarding writes to
// the leader, adding voters and admitting new nodes with a join token.
package nodeapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
)

// ErrRejected is returned when the peer refuses: no member certificate or a wrong join secret.
var ErrRejected = errors.New("rejected by cluster")

// Store is the part of the Raft store the node API uses.
type Store interface {
	ApplyAt(ctx context.Context, c raftstore.Command) (uint64, error)
	IsLeader() bool
	Leader() (id, raftAddr string)
	AddNonvoter(id, addr string) error
	JoinSecretHash() string
	State() raftstore.State
}

// Server answers other nodes.
type Server struct {
	Store  Store
	Client *Client
	CA     *clusterpki.CA
	// API is this node's own /v1 handler (the socket's, trusted), reached by
	// other members under /internal/v1/api/ to run work on this node.
	API http.Handler
	// Artifact opens a runtime archive for another node (the leader serves
	// them so only it downloads from upstream). Optional.
	Artifact func(ctx context.Context, ref domain.RuntimeRef, arch string, part int) (io.ReadCloser, error)
	// SendBase streams a base's snapshots (see dbbranch.Service.SendBase). Optional.
	SendBase func(ctx context.Context, base domain.Name, from, to string, w io.Writer) error
}

// APIPrefix is where a member's /v1 API is mounted on the node API.
const APIPrefix = "/internal/v1/api"

// Handler returns the routes. The TLS listener must use ServerTLS(false) so a
// joining node without a certificate can reach /join; every other route
// requires a verified member certificate.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/join", s.join)
	mux.HandleFunc("POST /internal/v1/apply", member(s.apply))
	mux.HandleFunc("POST /internal/v1/voters", member(s.addMember))
	if s.Artifact != nil {
		mux.HandleFunc("GET /internal/v1/artifacts/{engine}/{version}", member(s.artifact))
	}
	if s.SendBase != nil {
		mux.HandleFunc("GET /internal/v1/bases/{name}/stream", member(s.stream))
	}
	if s.API != nil {
		mux.Handle(APIPrefix+"/", member(http.StripPrefix(APIPrefix, s.API).ServeHTTP))
	}
	return mux
}

func member(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "member certificate required", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func reply(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrNoLeader):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, domain.ErrRolloutRunning):
		http.Error(w, err.Error(), http.StatusLocked) // 409 means "already exists" to Forward
	case errors.Is(err, domain.ErrAlreadyExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, domain.ErrClusterUpgrading):
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type joinRequest struct {
	Secret string `json:"secret"`
}

type joinResponse struct {
	CACert []byte `json:"ca_cert"`
	CAKey  []byte `json:"ca_key"`
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var in joinRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	want := s.Store.JoinSecretHash()
	got := clusterpki.HashSecret(in.Secret)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		http.Error(w, "invalid join secret", http.StatusForbidden)
		return
	}
	cert, key := s.CA.PEM()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(joinResponse{CACert: cert, CAKey: key})
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := raftstore.DecodeCommand(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idx, err := s.Store.ApplyAt(r.Context(), c)
	if err != nil {
		reply(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(applyResponse{Index: idx})
}

// lazyWriter sends the 200 header with the first byte of the stream, so an
// error found before streaming can still become an error status.
type lazyWriter struct {
	w       http.ResponseWriter
	started bool
}

func (l *lazyWriter) Write(b []byte) (int, error) {
	if !l.started {
		l.started = true
		l.w.Header().Set("Content-Type", "application/octet-stream")
		l.w.WriteHeader(http.StatusOK)
	}
	return l.w.Write(b)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	name, err := domain.ParseName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	lw := &lazyWriter{w: w}
	err = s.SendBase(r.Context(), name, q.Get("from"), q.Get("to"), lw)
	switch {
	case err == nil && !lw.started:
		w.WriteHeader(http.StatusOK)
	case err == nil:
	case lw.started:
		// Cut the connection so the receiver sees a broken stream, never a short one.
		panic(http.ErrAbortHandler)
	case errors.Is(err, domain.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	ref, err := domain.ParseRuntimeRef(r.PathValue("engine") + "@" + r.PathValue("version"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	part, _ := strconv.Atoi(r.URL.Query().Get("part"))
	rc, err := s.Artifact(r.Context(), ref, r.URL.Query().Get("arch"), part)
	switch {
	case errors.Is(err, domain.ErrRuntimeNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := io.Copy(w, rc); err != nil {
		panic(http.ErrAbortHandler)
	}
}

type applyResponse struct {
	Index uint64 `json:"index"`
}

type voterRequest struct {
	ID          string `json:"id"`
	RaftAddress string `json:"raft_address"`
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	var in voterRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Store.IsLeader() {
		reply(w, s.Store.AddNonvoter(in.ID, in.RaftAddress))
		return
	}
	id, _ := s.Store.Leader()
	leader, ok := s.Store.State().Nodes[id]
	if id == "" || !ok {
		reply(w, domain.ErrNoLeader)
		return
	}
	reply(w, s.Client.AddMember(r.Context(), leader.Address, in.ID, in.RaftAddress))
}

// Client calls other nodes with this node's member certificate.
type Client struct {
	http *http.Client
	tr   *http.Transport
	// streams has no overall timeout: a snapshot stream may take long.
	streams *http.Client
}

var _ raftstore.Forwarder = (*Client)(nil)

// NewClient returns a client using tlsConfig (normally NodeCert.ClientTLS).
func NewClient(tlsConfig *tls.Config) *Client {
	tr := &http.Transport{TLSClientConfig: tlsConfig}
	return &Client{http: &http.Client{Timeout: 30 * time.Second, Transport: tr}, tr: tr, streams: &http.Client{Transport: tr}}
}

func (c *Client) post(ctx context.Context, addr, path string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var msg bytes.Buffer
	switch {
	case resp.StatusCode == http.StatusForbidden:
		_, _ = msg.ReadFrom(resp.Body)
		return fmt.Errorf("%w: %s", ErrRejected, bytes.TrimSpace(msg.Bytes()))
	case resp.StatusCode == http.StatusServiceUnavailable:
		return domain.ErrNoLeader
	case resp.StatusCode == http.StatusPreconditionFailed:
		_, _ = msg.ReadFrom(resp.Body)
		return fmt.Errorf("%w: %s", domain.ErrClusterUpgrading, bytes.TrimSpace(msg.Bytes()))
	case resp.StatusCode == http.StatusLocked:
		_, _ = msg.ReadFrom(resp.Body)
		return fmt.Errorf("%s: %w", bytes.TrimSpace(msg.Bytes()), domain.ErrRolloutRunning)
	case resp.StatusCode == http.StatusConflict:
		_, _ = msg.ReadFrom(resp.Body)
		return fmt.Errorf("%s: %w", bytes.TrimSpace(msg.Bytes()), domain.ErrAlreadyExists)
	case resp.StatusCode >= 300:
		_, _ = msg.ReadFrom(resp.Body)
		return fmt.Errorf("%s%s: %s: %s", addr, path, resp.Status, bytes.TrimSpace(msg.Bytes()))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Forward sends a command to the leader's node API.
func (c *Client) Forward(ctx context.Context, apiAddr string, cmd raftstore.Command) (uint64, error) {
	data, err := cmd.Encode()
	if err != nil {
		return 0, err
	}
	var out applyResponse
	err = c.post(ctx, apiAddr, "/internal/v1/apply", json.RawMessage(data), &out)
	return out.Index, err
}

// AddMember asks the cluster (via any member) to add this node as a non-voter
// (an older leader adds a voter instead; the route keeps its old name).
func (c *Client) AddMember(ctx context.Context, apiAddr, id, raftAddr string) error {
	return c.post(ctx, apiAddr, "/internal/v1/voters", voterRequest{ID: id, RaftAddress: raftAddr}, nil)
}

// Join presents the token's secret to a member, trusting it only if its CA
// matches the token's fingerprint, and returns the cluster CA.
func Join(ctx context.Context, apiAddr string, tok clusterpki.Token) (caCert, caKey []byte, err error) {
	var out joinResponse
	err = NewClient(clusterpki.PinnedTLS(tok.Fingerprint)).post(ctx, apiAddr, "/internal/v1/join", joinRequest{Secret: tok.Secret}, &out)
	return out.CACert, out.CAKey, err
}

// Proxy returns a handler that passes /v1 requests to the member at apiAddr,
// which runs them as if they had arrived on its own socket. The caller must
// have authenticated the request already.
func (c *Client) Proxy(apiAddr string) http.Handler {
	target := &url.URL{Scheme: "https", Host: apiAddr, Path: APIPrefix}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Header.Del("Cookie")
		},
		Transport: c.tr,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"code": "node_unreachable", "message": fmt.Sprintf("node %s: %v", apiAddr, err)}})
		},
	}
}

// OpenStream fetches a base's snapshot stream from the member at apiAddr.
func (c *Client) OpenStream(ctx context.Context, apiAddr string, base domain.Name, from, to string) (io.ReadCloser, error) {
	q := url.Values{"from": {from}, "to": {to}}
	u := url.URL{Scheme: "https", Host: apiAddr, Path: "/internal/v1/bases/" + base.String() + "/stream", RawQuery: q.Encode()}
	return c.get(ctx, u)
}

// get opens a streamed response; a 404 becomes domain.ErrNotFound.
func (c *Client) get(ctx context.Context, u url.URL) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.streams.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("%s: %s: %s", u.Path, resp.Status, bytes.TrimSpace(msg))
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %v", domain.ErrNotFound, err)
		}
		return nil, err
	}
	return resp.Body, nil
}

// OpenArtifact fetches a runtime archive for arch from the member at apiAddr.
// The caller verifies it against its own pinned digest.
func (c *Client) OpenArtifact(ctx context.Context, apiAddr string, ref domain.RuntimeRef, arch string, part int) (io.ReadCloser, error) {
	q := url.Values{"arch": {arch}, "part": {strconv.Itoa(part)}}
	u := url.URL{Scheme: "https", Host: apiAddr, Path: "/internal/v1/artifacts/" + string(ref.Engine) + "/" + ref.Version, RawQuery: q.Encode()}
	return c.get(ctx, u)
}
