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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/membership"
)

// ErrRejected is returned when the peer refuses: no member certificate or a wrong join secret.
var ErrRejected = errors.New("rejected by cluster")

// Store is the part of the Raft store the node API uses.
type Store interface {
	ApplyAt(ctx context.Context, c raftstore.Command) (uint64, error)
	IsLeader() bool
	Leader() (id, raftAddr string)
	AddNonvoter(id, addr string) error
	Servers(ctx context.Context) ([]membership.Server, error)
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
	// ServiceIDs returns the db-branch uid/gid of the cluster, told to nodes
	// that join so they can adopt them before any data arrives. Optional.
	ServiceIDs func() domain.ServiceIDs
}

// APIPrefix is where a member's /v1 API is mounted on the node API.
const APIPrefix = "/internal/v1/api"

const (
	forwardedHeader = "X-Db-Branch-Forwarded"
	principalHeader = "X-Db-Branch-Principal"
)

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
		mux.Handle(APIPrefix+"/", member(forwardedAPI(http.StripPrefix(APIPrefix, s.API)).ServeHTTP))
	}
	return mux
}

// forwardedAPI ensures requests reaching /internal/v1/api/ over mTLS carry both
// forwarding headers so they can never be mistaken for local UNIX socket calls.
func forwardedAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fwd := r.Header.Values(forwardedHeader)
		prin := r.Header.Values(principalHeader)
		if len(fwd) != 1 || fwd[0] == "" || len(prin) != 1 || prin[0] == "" {
			http.Error(w, "forwarded headers required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func delHeaderFold(h http.Header, name string) {
	for k := range h {
		if strings.EqualFold(k, name) {
			delete(h, k)
		}
	}
}

func firstHeaderFold(h http.Header, name string) string {
	for k, vs := range h {
		if strings.EqualFold(k, name) {
			for _, v := range vs {
				if v != "" {
					return v
				}
			}
		}
	}
	return ""
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
	case errors.Is(err, domain.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
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
	// ServiceIDs are the db-branch uid/gid the cluster uses (absent from
	// members of older versions).
	ServiceIDs *domain.ServiceIDs `json:"service_ids,omitempty"`
	// Nodes lists the current cluster members' names so a joiner can fail
	// fast if its hostname collides with an existing member.
	Nodes []string `json:"nodes,omitempty"`
}

// decodeSmall reads a small JSON request body (join and voter requests are
// a few hundred bytes), refusing more than maxSmallBody: /join takes
// requests without a certificate.
func decodeSmall(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSmallBody)).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return false
	}
	return true
}

const maxSmallBody = 64 << 10

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var in joinRequest
	if !decodeSmall(w, r, &in) {
		return
	}
	want := s.Store.JoinSecretHash()
	got := clusterpki.HashSecret(in.Secret)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		http.Error(w, "invalid join secret", http.StatusForbidden)
		return
	}
	cert, key := s.CA.PEM()
	out := joinResponse{CACert: cert, CAKey: key}
	if s.ServiceIDs != nil {
		if ids := s.ServiceIDs(); ids.Valid() {
			out.ServiceIDs = &ids
		}
	}
	seen := map[string]bool{}
	for name := range s.Store.State().Nodes {
		seen[name] = true
	}
	if srvs, err := s.Store.Servers(r.Context()); err == nil {
		for _, srv := range srvs {
			if srv.ID != "" {
				seen[srv.ID] = true
			}
		}
	}
	for name := range seen {
		out.Nodes = append(out.Nodes, name)
	}
	sort.Strings(out.Nodes)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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
	if err := s.checkMember(r.Context(), c); err != nil {
		reply(w, err)
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

// checkMember makes the leader refuse the heartbeat (put_node) of a node that
// is not in the Raft configuration: a node taken out of the cluster that still
// runs would otherwise bring its record back. A node is added to the
// configuration (join) before it first reports itself.
func (s *Server) checkMember(ctx context.Context, c raftstore.Command) error {
	if c.Op != "put_node" || c.Node == nil || !s.Store.IsLeader() {
		return nil
	}
	servers, err := s.Store.Servers(ctx)
	if err != nil || len(servers) == 0 {
		return nil // cannot tell: do not refuse
	}
	if !slices.ContainsFunc(servers, func(sv membership.Server) bool { return sv.ID == c.Node.Name }) {
		return fmt.Errorf("node %s is not a member of the cluster: %w", c.Node.Name, domain.ErrNotFound)
	}
	return nil
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
	if !decodeSmall(w, r, &in) {
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

// JoinInfo is what a member hands a joining node.
type JoinInfo struct {
	CACert, CAKey []byte
	// ServiceIDs are the cluster's db-branch uid/gid (zero from a member of
	// an older version, which does not know them).
	ServiceIDs domain.ServiceIDs
	// Nodes are the names of the current cluster members (empty from older versions).
	Nodes []string
}

// Join presents the token's secret to a member, trusting it only if its CA
// matches the token's fingerprint, and returns the cluster CA.
func Join(ctx context.Context, apiAddr string, tok clusterpki.Token) (JoinInfo, error) {
	var out joinResponse
	err := NewClient(clusterpki.PinnedTLS(tok.Fingerprint)).post(ctx, apiAddr, "/internal/v1/join", joinRequest{Secret: tok.Secret}, &out)
	info := JoinInfo{CACert: out.CACert, CAKey: out.CAKey, Nodes: out.Nodes}
	if out.ServiceIDs != nil {
		info.ServiceIDs = *out.ServiceIDs
	}
	return info, err
}

// Proxy returns a handler that passes /v1 requests to the member at apiAddr,
// which runs them as if they had arrived on its own socket. The caller must
// have authenticated the request already.
func (c *Client) Proxy(apiAddr string) http.Handler {
	target := &url.URL{Scheme: "https", Host: apiAddr, Path: APIPrefix}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			delHeaderFold(r.Out.Header, "Cookie")
			// Keep internal forwarding headers even if a client sent a Connection
			// hop-by-hop header naming them (ReverseProxy strips hop-by-hop headers
			// before Rewrite runs).
			for _, h := range []string{forwardedHeader, principalHeader} {
				delHeaderFold(r.Out.Header, h)
				if v := firstHeaderFold(r.In.Header, h); v != "" {
					r.Out.Header.Set(h, v)
				}
			}
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
