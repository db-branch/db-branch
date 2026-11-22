package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"github.com/db-branch/db-branch/pkg/ext"
)

type runtimeJSON struct {
	Runtime string `json:"runtime"`
	Engine  string `json:"engine"`
	Version string `json:"version"`
}

func runtimeViews(refs []domain.RuntimeRef) []runtimeJSON {
	out := make([]runtimeJSON, 0, len(refs))
	for _, r := range refs {
		out = append(out, runtimeJSON{Runtime: r.String(), Engine: string(r.Engine), Version: r.Version})
	}
	return out
}

func (s *Server) listRuntimes(w http.ResponseWriter, r *http.Request) {
	if s.forward(w, r, r.URL.Query().Get("node")) {
		return
	}
	refs, err := s.Runtimes.List(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items(runtimeViews(refs)))
}

func (s *Server) availableRuntimes(w http.ResponseWriter, _ *http.Request) {
	var refs []domain.RuntimeRef
	for _, e := range domain.DefaultRuntimes.Engines() {
		refs = append(refs, s.Runtimes.Available(e)...)
	}
	writeJSON(w, http.StatusOK, items(runtimeViews(refs)))
}

func (s *Server) installRuntime(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Runtime string `json:"runtime"`
	}
	if !decode(w, r, &in) {
		return
	}
	ref, err := domain.ParseRuntimeRef(in.Runtime)
	if err != nil {
		fail(w, err)
		return
	}
	// In a cluster an engine is always installed everywhere: a branch may be
	// placed or recreated on any node.
	if s.Cluster != nil {
		s.installEverywhere(w, r, ref)
		return
	}
	s.accepted(w, r, "runtime.install", operation.Target{Resource: "runtime", Name: ref.String()},
		func(ctx context.Context, report func(string)) error {
			report("downloading")
			err := s.Runtimes.Install(ctx, ref)
			s.Bus.PublishTo(operation.Audience{Action: "runtime.read"}, "runtime.updated", runtimeViews([]domain.RuntimeRef{ref})[0])
			s.runtimesChanged()
			return err
		})
}

// runtimesChanged lets the cluster announce this node's engines at once.
func (s *Server) runtimesChanged() {
	if s.OnRuntimesChanged != nil {
		s.OnRuntimesChanged()
	}
}

// installEverywhere records ref for every node (each installs it, fetching
// from the leader) and installs it here now.
func (s *Server) installEverywhere(w http.ResponseWriter, r *http.Request, ref domain.RuntimeRef) {
	offered := false
	for _, a := range s.Runtimes.Available(ref.Engine) {
		offered = offered || a == ref
	}
	if !offered {
		fail(w, fmt.Errorf("%s: %w", ref, domain.ErrRuntimeNotFound))
		return
	}
	if err := s.Cluster.AddRuntime(r.Context(), ref); err != nil {
		fail(w, err)
		return
	}
	s.accepted(w, r, "runtime.install", operation.Target{Resource: "runtime", Name: ref.String()},
		func(ctx context.Context, report func(string)) error {
			err := s.Runtimes.Install(ctx, ref)
			if errors.Is(err, domain.ErrRuntimeInstalled) {
				return nil
			}
			s.runtimesChanged()
			return err
		})
}

// removeEverywhere stops installing ref on new nodes and removes it from
// every node where nothing uses it; nodes that still use it are named.
func (s *Server) removeEverywhere(w http.ResponseWriter, r *http.Request, ref domain.RuntimeRef) {
	st, err := s.Cluster.Status(r.Context())
	if err == nil {
		err = s.Cluster.RemoveRuntime(r.Context(), ref)
	}
	if err != nil {
		fail(w, err)
		return
	}
	var problems []string
	for _, n := range st.Nodes {
		if n.Down {
			problems = append(problems, n.Name+": down")
			continue
		}
		if n.Name == s.Node {
			if err := s.Runtimes.Remove(r.Context(), ref); err != nil && !errors.Is(err, domain.ErrRuntimeNotInstalled) {
				problems = append(problems, n.Name+": "+err.Error())
			}
			s.runtimesChanged()
			continue
		}
		sub := r.Clone(r.Context())
		sub.URL.RawQuery = url.Values{"node": {n.Name}}.Encode()
		rec, err := s.forwardTo(sub, n.Name)
		if err == nil && rec.status >= 300 && rec.status != http.StatusNotFound {
			err = errors.New(errorMessage(rec.body.Bytes()))
		}
		if err != nil {
			problems = append(problems, n.Name+": "+err.Error())
		}
	}
	if len(problems) > 0 {
		fail(w, fmt.Errorf("%w: %s was not removed everywhere (%s)", domain.ErrInUse, ref, strings.Join(problems, "; ")))
		return
	}
	s.Bus.PublishTo(operation.Audience{Action: "runtime.read"}, "runtime.deleted", map[string]string{"runtime": ref.String()})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeRuntime(w http.ResponseWriter, r *http.Request) {
	if s.forward(w, r, r.URL.Query().Get("node")) {
		return
	}
	if s.Cluster != nil && r.URL.Query().Get("node") == "" {
		ref, err := domain.ParseRuntimeRef(r.PathValue("engine") + "@" + r.PathValue("version"))
		if err != nil {
			fail(w, err)
			return
		}
		s.removeEverywhere(w, r, ref)
		return
	}
	ref, err := domain.ParseRuntimeRef(r.PathValue("engine") + "@" + r.PathValue("version"))
	if err == nil {
		err = s.Runtimes.Remove(r.Context(), ref)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.Bus.PublishTo(operation.Audience{Action: "runtime.read"}, "runtime.deleted", map[string]string{"runtime": ref.String()})
	s.runtimesChanged()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	list := []operation.Operation{}
	for _, op := range s.Ops.List() {
		if s.canSeeOperation(r.Context(), p, op) {
			list = append(list, op)
		}
	}
	writeJSON(w, http.StatusOK, items(list))
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.Ops.Get(r.PathValue("id"))
	if err == nil && !s.canSeeOperation(r.Context(), principalOf(r), op) {
		err = operation.ErrNotFound // do not reveal that the operation exists
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

// events streams changes as Server-Sent Events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	lastID, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	p := principalOf(r)
	s.seedBranchAudiences(r.Context())
	sub := s.Bus.Subscribe(lastID)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if sub.Resync {
		fmt.Fprint(w, "event: resync\ndata: {}\n\n")
	}
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-sub.Events():
			if !ok {
				return // dropped for reading too slowly; the client reconnects
			}
			if !s.canReceive(r.Context(), p, ev.Audience) {
				continue
			}
			data, _ := json.Marshal(ev.Data)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data)
			flusher.Flush()
		}
	}
}

type sessionJSON struct {
	User      map[string]any `json:"user"`
	CSRFToken string         `json:"csrf_token"`
	ExpiresAt string         `json:"expires_at"`
	// CanChangePassword: the account page may offer a password change.
	CanChangePassword bool `json:"can_change_password"`
	// MustChangePassword: the password is an initial one; the console asks for a
	// new one before anything else (every other call is refused until then).
	MustChangePassword bool `json:"must_change_password"`
	// Language is the user's chosen console language ("": the browser's).
	Language string `json:"language"`
	// Can tells the console which of consoleActions the user may do, so
	// it shows only the pages and buttons that work for them.
	Can map[string]bool `json:"can"`
	// BranchPrefix is put in front of the branch names this user types on
	// the user page ("" for admins, who name branches exactly).
	BranchPrefix string `json:"branch_prefix"`
}

// consoleActions are the actions whose pages and buttons the console
// shows or hides.
var consoleActions = []string{"base.read", "base.create", "base.delete", "base.load", "base.snapshot", "base.publish", "base.refresh", "base.refresh.read",
	"runtime.install", "cluster.read", "setting.write", "module.install", "token.write", "backup.read", "route-user.write", "update.read", "update.check", "update.apply", "license.write",
	"branch.checkpoint", "branch.checkpoint_delete", "branch.restore", "branch.upgrade"}

// adminUser is the session user of the built-in admin password.
const adminUser = "admin"

// passwordChanger is who changes the signed-in user's password: the
// built-in admin password, a module that supports it, or no one.
func (s *Server) passwordChanger(sess auth.Session) (ext.PasswordChanger, bool) {
	if sess.User == adminUser {
		return nil, true
	}
	if slices.Contains(sess.Roles, ext.NoPasswordRole) {
		return nil, false
	}
	pc, ok := s.Ext.Authenticator.(ext.PasswordChanger)
	return pc, ok
}

func (s *Server) sessionView(ctx context.Context, sess auth.Session) sessionJSON {
	v := sessionView(sess)
	v.MustChangePassword = sess.MustChange
	p, az := ext.Principal{ID: sess.User, Name: sess.Name, Roles: sess.Roles}, s.authorizer()
	v.Can = map[string]bool{}
	actions := append([]string{}, consoleActions...)
	for _, m := range s.Modules { // a module's pages and API; entries are "name version"
		name, _, _ := strings.Cut(m, " ")
		actions = append(actions, "ext."+name+".read", "ext."+name+".write")
	}
	for _, a := range actions {
		kind, _, _ := strings.Cut(a, ".")
		v.Can[a] = az.Authorize(ctx, p, a, ext.Resource{Kind: kind}) == nil
	}
	v.BranchPrefix = branchPrefix(p)
	_, v.CanChangePassword = s.passwordChanger(sess)
	v.Language = s.language(ctx, sess.User)
	return v
}

func sessionView(sess auth.Session) sessionJSON {
	name := sess.Name
	if name == "" {
		name = sess.User
	}
	roles := sess.Roles
	if roles == nil {
		roles = []string{}
	}
	return sessionJSON{User: map[string]any{"id": sess.User, "name": name, "roles": roles}, CSRFToken: sess.CSRF, ExpiresAt: ts(sess.ExpiresAt)}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	who := ext.Principal{ID: in.Username, Name: in.Username}
	p, err := s.authenticator().Authenticate(auth.WithClient(r.Context(), ClientKey(r.RemoteAddr)), ext.Credentials{Username: in.Username, Password: in.Password})
	var sess auth.Session
	if err == nil {
		who = p
		sess, err = s.Auth.Begin(p.ID, p.Name, p.Roles)
	}
	if err != nil {
		s.audit(r, who, "auth.login", ext.Resource{Kind: "auth"}, ext.OutcomeFailed, http.StatusUnauthorized)
		fail(w, err)
		return
	}
	s.audit(r, who, "auth.login", ext.Resource{Kind: "auth"}, ext.OutcomeOK, http.StatusOK)
	setSessionCookie(w, sess)
	writeJSON(w, http.StatusOK, s.sessionView(r.Context(), sess))
}

func (s *Server) current(r *http.Request) (auth.Session, bool) {
	sess, ok := r.Context().Value(ctxKey{}).(auth.Session)
	return sess, ok
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "local connections have no console session")
		return
	}
	writeJSON(w, http.StatusOK, s.sessionView(r.Context(), sess))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	var revokeErr error
	if sess, ok := s.current(r); ok {
		revokeErr = s.Auth.Logout(sess.Token)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	// While the cluster upgrades the logout holds on this node only (as it
	// always did). Any other failure is reported: the cookie may still work
	// on other nodes, so the caller should retry.
	if revokeErr != nil && !errors.Is(revokeErr, domain.ErrClusterUpgrading) {
		writeError(w, http.StatusServiceUnavailable, "logout_not_recorded", "signed out here, but the logout could not be recorded for the other nodes; try again")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	sess, ok := s.current(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "use `db-branch admin reset-password` on the node")
		return
	}
	pc, ok := s.passwordChanger(sess)
	switch {
	case !ok:
		writeError(w, http.StatusForbidden, "managed_elsewhere", "this account's password is managed by your sign-in provider")
		return
	case pc != nil:
		if err := pc.ChangePassword(r.Context(), ext.Principal{ID: sess.User, Name: sess.Name, Roles: sess.Roles}, in.Current, in.New); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fresh, err := s.Auth.ChangePassword(sess.Token, in.Current, in.New)
	if err != nil {
		fail(w, err)
		return
	}
	setSessionCookie(w, fresh)
	w.WriteHeader(http.StatusNoContent)
}

// resetPassword sets a new random admin password (for the whole cluster in
// cluster mode) and ends every session. Socket only: whoever can reach the
// socket is root or in the db-branch group on a node.
func (s *Server) resetPassword(local bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !local {
			writeError(w, http.StatusForbidden, "socket_only", "run `db-branch admin reset-password` on a node")
			return
		}
		password := auth.GeneratePassword()
		if err := s.Auth.SetPassword(password); err != nil {
			fail(w, err)
			return
		}
		if s.OnPasswordReset != nil {
			s.OnPasswordReset()
		}
		writeJSON(w, http.StatusOK, map[string]string{"password": password})
	}
}

// errorMessage extracts the message from an error envelope, or returns the body.
func errorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return strings.TrimSpace(string(body))
}

// setSessionCookie stores the session until it expires, so it survives
// closing the browser.
func setSessionCookie(w http.ResponseWriter, sess auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.Token, Path: "/", Expires: sess.ExpiresAt,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}
