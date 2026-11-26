package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

type principalKey struct{}

func withPrincipal(ctx context.Context, p ext.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principalOf returns who is acting (empty before authentication).
func principalOf(r *http.Request) ext.Principal {
	p, _ := r.Context().Value(principalKey{}).(ext.Principal)
	return p
}

// forwardedKey marks, in the context, a request another node forwarded over the
// trusted channel. The header alone proves nothing on the public listener, where
// anyone can send it.
type forwardedKey struct{}

func forwardedFromNode(r *http.Request) bool {
	v, _ := r.Context().Value(forwardedKey{}).(bool)
	return v
}

// deactivatedLogoutKey marks a valid cookie session whose user has been
// disabled or removed externally, so POST /v1/auth/logout can still clear it.
type deactivatedLogoutKey struct{}

func withDeactivatedLogout(ctx context.Context) context.Context {
	return context.WithValue(ctx, deactivatedLogoutKey{}, true)
}

func isDeactivatedLogout(ctx context.Context) bool {
	v, _ := ctx.Value(deactivatedLogoutKey{}).(bool)
	return v
}

// allowSocketOnly reports whether r arrived directly on the local UNIX socket
// (not over HTTPS and not forwarded from another node). Otherwise it answers
// 403 socket_only with hint.
func allowSocketOnly(w http.ResponseWriter, r *http.Request, local bool, hint string) bool {
	if local && !forwardedFromNode(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "socket_only", hint)
	return false
}

// principalHeader carries the principal of a request one node forwards to
// another (over mutual TLS, into the trusted socket handler).
const principalHeader = "X-Db-Branch-Principal"

func hasHeaderFold(h http.Header, names ...string) bool {
	for k := range h {
		for _, n := range names {
			if strings.EqualFold(k, n) {
				return true
			}
		}
	}
	return false
}

func delHeadersFold(h http.Header, names ...string) {
	for k := range h {
		for _, n := range names {
			if strings.EqualFold(k, n) {
				delete(h, k)
				break
			}
		}
	}
}

// LocalPrincipal acts for whoever can reach the agent's socket (root or the
// db-branch-admin group on the node).
var LocalPrincipal = ext.Principal{ID: "local", Name: "local", Roles: []string{"admin"}}

func (s *Server) localPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ctx := LocalPrincipal, r.Context()
		if hasHeaderFold(r.Header, forwardedHeader, principalHeader) {
			// Fail closed: any request carrying a forwarding header without a
			// single valid forwarded node and principal must never fall back to
			// LocalPrincipal (admin).
			p = ext.Principal{}
			fwdVals := r.Header.Values(forwardedHeader)
			prinVals := r.Header.Values(principalHeader)
			if len(fwdVals) == 1 && fwdVals[0] != "" && len(prinVals) == 1 {
				var fwd ext.Principal
				if json.Unmarshal([]byte(prinVals[0]), &fwd) == nil && fwd.ID != "" {
					p = fwd
				}
			}
			ctx = context.WithValue(ctx, forwardedKey{}, true)
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, p)))
	})
}

func (s *Server) pending() []string {
	if s.PendingModules == nil {
		return nil
	}
	return s.PendingModules()
}

// authorizer: logged in, within the token's scope, and allowed by modules.
func (s *Server) authorizer() ext.Authorizer {
	all := ext.AllOf{ext.AllowAuthenticated{}, token.Scoped{}, ownership{s}}
	if s.Ext.Authorizer != nil {
		all = append(all, legacyActionAuthorizer{s.Ext.Authorizer})
	}
	return all
}

// legacyActionAliases lets an older auth module authorize requests that use
// the 1.0 dot-separated and split action names (and lets legacy UI action checks
// work against a newer auth module during rollout).
var legacyActionAliases = map[string][]string{
	"branch.pause":             {"branch.update"},
	"branch.resume":            {"branch.update"},
	"branch.update":            {"branch.pause", "branch.resume"},
	"branch.checkpoint.delete": {"branch.checkpoint_delete"},
	"branch.checkpoint_delete": {"branch.checkpoint.delete"},
	"base.snapshot.delete":     {"base.snapshot_delete"},
	"base.snapshot_delete":     {"base.snapshot.delete"},
}

type legacyActionAuthorizer struct{ next ext.Authorizer }

func (a legacyActionAuthorizer) Authorize(ctx context.Context, p ext.Principal, action string, r ext.Resource) error {
	err := a.next.Authorize(ctx, p, action, r)
	if errors.Is(err, ext.ErrForbidden) {
		for _, alias := range legacyActionAliases[action] {
			if aerr := a.next.Authorize(ctx, p, alias, r); aerr == nil {
				return nil
			}
		}
	}
	return err
}

// authenticator tries modules, then API tokens, then the admin password.
func (s *Server) authenticator() ext.Authenticator {
	var chain ext.Chain
	if s.Ext.Authenticator != nil {
		chain = append(chain, s.Ext.Authenticator)
	}
	if s.TokenAuth != nil {
		chain = append(chain, s.TokenAuth)
	}
	return append(chain, passwordAuthenticator{s.Auth})
}

func (s *Server) sink() ext.AuditSink {
	sinks := ext.Sinks{logSink{s}}
	if s.Ext.AuditSink != nil {
		sinks = append(sinks, s.Ext.AuditSink)
	}
	return sinks
}

// passwordAuthenticator authenticates built-in admin and email users against
// core storage.
type passwordAuthenticator struct{ a *auth.Service }

func (p passwordAuthenticator) Authenticate(ctx context.Context, c ext.Credentials) (ext.Principal, error) {
	if c.Username == "" {
		return ext.Principal{}, ext.ErrUnauthenticated
	}
	u, err := p.a.VerifyUserFrom(auth.ClientFrom(ctx), c.Username, c.Password)
	if err != nil {
		return ext.Principal{}, err
	}
	return ext.Principal{ID: u.ID, Name: u.DisplayName(), Roles: []string{"admin"}}, nil
}

// logSink is the OSS audit trail: one structured log line per event.
type logSink struct{ s *Server }

func (l logSink) Record(_ context.Context, e ext.Event) error {
	if l.s.Log != nil {
		l.s.Log.Info("audit", "action", e.Action, "principal", e.Principal.ID, "resource", e.Resource.Kind+"/"+e.Resource.Name,
			"outcome", e.Outcome, "status", e.Status, "remote", e.Remote)
	}
	return nil
}

// statusWriter remembers the status a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// flushingWriter is a statusWriter over a writer that can stream (server-sent
// events); a plain statusWriter must not claim it can.
type flushingWriter struct{ *statusWriter }

func (w flushingWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func track(w http.ResponseWriter) (http.ResponseWriter, *statusWriter) {
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	if _, ok := w.(http.Flusher); ok {
		return flushingWriter{sw}, sw
	}
	return sw, sw
}

// guard authorizes the call as action on the resource it names, runs it, and
// audits every change and every denial. Calls forwarded from another node
// were audited there.
func (s *Server) guard(action string, h http.HandlerFunc) http.HandlerFunc {
	kind, _, _ := strings.Cut(action, ".")
	return func(w http.ResponseWriter, r *http.Request) {
		// Only requests that arrived over the trusted node channel carry forwardedKey.
		forwardedIn := forwardedFromNode(r)
		target := &auditTarget{}
		audit := func(outcome string, status int) {
			if !forwardedIn {
				s.audit(r, principalOf(r), action, ext.Resource{Kind: kind, Name: target.name}, outcome, status)
			}
		}
		p := principalOf(r)
		res := ext.Resource{Kind: kind, Name: pathResource(r)}
		if res.Name == "" && r.Method == http.MethodPost {
			res.Name = peekName(r)
		}
		target.name = res.Name
		r = r.WithContext(context.WithValue(r.Context(), resourceKey{}, target))
		if pending := s.pending(); len(pending) > 0 && !moduleExempt(action) && !forwardedIn && p.ID != LocalPrincipal.ID {
			writeError(w, http.StatusServiceUnavailable, "modules_pending",
				"this node does not run every module the cluster requires yet ("+strings.Join(pending, ", ")+"); use another node or wait")
			return
		}
		if !(action == "auth.logout" && isDeactivatedLogout(r.Context())) {
			if err := s.authorizer().Authorize(r.Context(), p, action, res); err != nil {
				audit(ext.OutcomeDenied, http.StatusForbidden)
				s.requests.add(action, ext.OutcomeDenied)
				fail(w, err)
				return
			}
		}
		if changes(r, action) && !forwardedIn && p.ID != LocalPrincipal.ID && s.auditFailClosed() {
			// Fail closed: the intent is on record before the change runs.
			if err := s.record(r, p, action, res, ext.OutcomeAttempt, 0); err != nil {
				s.requests.add(action, ext.OutcomeFailed)
				writeError(w, http.StatusServiceUnavailable, "audit_unavailable", "the change cannot be recorded in the audit log, so it was not made")
				return
			}
		}
		tw, sw := track(w)
		h(tw, r)
		outcome := ext.OutcomeOK
		if sw.status >= 400 {
			outcome = ext.OutcomeFailed
		}
		s.requests.add(action, outcome)
		if changes(r, action) {
			audit(outcome, sw.status)
		}
	}
}

// auditAuthFailure records a request refused before any route ran (a bad
// token or session, a missing CSRF token): the trail of break-in attempts.
func (s *Server) auditAuthFailure(r *http.Request, p ext.Principal, action string, status int) {
	s.audit(r, p, action, ext.Resource{Kind: "auth"}, ext.OutcomeDenied, status)
}

// auditedReads are reads that hand out secrets or data, so the audit log
// records them like changes: a branch's connection (with its password)
// and a backup file.
var auditedReads = map[string]bool{"branch.connect": true, "backup.download": true}

type resourceKey struct{}

// auditTarget is the resource name the audit event carries. A handler that
// resolves the name (a user's branch name gets its owner prefix) sets the
// resolved one with auditAs.
type auditTarget struct{ name string }

func auditAs(r *http.Request, name string) {
	if t, ok := r.Context().Value(resourceKey{}).(*auditTarget); ok {
		t.name = name
	}
}

// pathResource names what the route's path addresses: a branch or base
// ({name}), a token ({id}), a settings scope, a backup file, a runtime.
func pathResource(r *http.Request) string {
	for _, k := range []string{"name", "id", "scope", "file"} {
		if v := r.PathValue(k); v != "" {
			return v
		}
	}
	if e := r.PathValue("engine"); e != "" {
		return e + "@" + r.PathValue("version")
	}
	if r.URL.Path == "/v1/uploads" || r.URL.Path == "/v1/uploads/room" {
		return r.URL.Query().Get("base")
	}
	return ""
}

// changes: the request changes something or reads what the audit log must show.
func changes(r *http.Request, action string) bool {
	return r.Method != http.MethodGet || auditedReads[action]
}

func (s *Server) auditFailClosed() bool { return s.AuditFailClosed != nil && s.AuditFailClosed() }

func (s *Server) audit(r *http.Request, p ext.Principal, action string, res ext.Resource, outcome string, status int) {
	_ = s.record(r, p, action, res, outcome, status)
}

// record sends the event to the audit sinks; a failure is logged and returned.
func (s *Server) record(r *http.Request, p ext.Principal, action string, res ext.Resource, outcome string, status int) error {
	remote := ""
	if r.RemoteAddr != "@" {
		remote = r.RemoteAddr
	}
	e := ext.Event{Time: time.Now().UTC(), Node: s.Node, Principal: p, Action: action, Resource: res, Outcome: outcome, Status: status, Remote: remote}
	err := s.sink().Record(r.Context(), e)
	if err != nil && s.Log != nil {
		s.Log.Error("audit sink failed", "action", action, "err", err)
	}
	return err
}

// peekName reads the "name" of a JSON body without consuming it. Other
// bodies (uploads, streamed to disk) are left alone; their name is in the
// query.
func peekName(r *http.Request) string {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return r.URL.Query().Get("name")
	}
	if r.Body == nil {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	// Keep what was not read: a large body continues after the peeked part.
	r.Body = readCloser{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
	if err != nil {
		return ""
	}
	var in struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(b, &in)
	return in.Name
}

type readCloser struct {
	io.Reader
	io.Closer
}
