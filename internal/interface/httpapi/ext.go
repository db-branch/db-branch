package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/pkg/ext"
)

// maxExtBody bounds what a module's API accepts in one request.
const maxExtBody = 1 << 20

// ModulePage is a console page offered by a loaded module.
type ModulePage struct {
	Module string `json:"module"`
	Path   string `json:"path"`
	Title  string `json:"title"`
}

// LoginMethod is a module's way to sign in, as the login screen shows it.
type LoginMethod struct {
	Module     string `json:"module"`
	Label      string `json:"label"`
	Path       string `json:"path"`
	StatusPath string `json:"status_path,omitempty"`
}

// isPublic reports whether path of module answers without a login.
func (s *Server) isPublic(module, path string) bool {
	for _, p := range s.ExtPublic[module] {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// loginMethods lists the modules' sign-in methods (no login needed).
func (s *Server) loginMethods(w http.ResponseWriter, r *http.Request) {
	out := []map[string]string{}
	for _, m := range s.LoginMethods {
		if m.StatusPath != "" {
			h, ok := s.ExtHandlers[m.Module]
			if !ok {
				continue
			}
			resp, err := h.Serve(r.Context(), ext.Request{Method: http.MethodGet, Path: m.StatusPath, Principal: Anonymous})
			var st struct {
				Enabled bool `json:"enabled"`
			}
			if err != nil || resp.Status != http.StatusOK || json.Unmarshal(resp.Body, &st) != nil || !st.Enabled {
				continue
			}
		}
		out = append(out, map[string]string{"module": m.Module, "label": m.Label, "url": "/v1/ext/" + m.Module + m.Path})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// extAPI serves /v1/ext/{name}/{path...}: a module's own API and pages.
// GET is the action "ext.<module>.read", anything else "ext.<module>.write"
// (audited). ?node=<name> runs it on that node, e.g. to read its local files.
func (s *Server) extAPI(w http.ResponseWriter, r *http.Request) {
	module := r.PathValue("name")
	h, ok := s.ExtHandlers[module]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no module "+module+" with an API on this node")
		return
	}
	action := "ext." + module + ".write"
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		action = "ext." + module + ".read"
	}
	serve := func(w http.ResponseWriter, r *http.Request) {
		if s.forward(w, r, s.extNode(r)) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxExtBody+1))
		if err != nil || len(body) > maxExtBody {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body over 1 MiB")
			return
		}
		prefix := "dbbx_" + module + "_"
		cookies := map[string]string{}
		for _, c := range r.Cookies() {
			if strings.HasPrefix(c.Name, prefix) {
				cookies[c.Name] = c.Value
			}
		}
		resp, err := h.Serve(r.Context(), ext.Request{Method: r.Method, Path: "/" + r.PathValue("path"),
			RawQuery: r.URL.RawQuery, Body: body, Principal: principalOf(r), Cookies: cookies})
		if err != nil {
			fail(w, err)
			return
		}
		if resp.ContentType == "" {
			resp.ContentType = "application/json"
		}
		for k, v := range resp.Headers {
			switch http.CanonicalHeaderKey(k) {
			case "Location", "Cache-Control":
				w.Header().Set(k, v)
			case "Set-Cookie":
				if strings.HasPrefix(v, prefix) {
					w.Header().Add("Set-Cookie", v)
				}
			}
		}
		w.Header().Set("Content-Type", resp.ContentType)
		if strings.HasPrefix(resp.ContentType, "text/html") {
			// Module pages are shown in a frame of the console (same origin only).
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'self'")
		}
		if resp.Status == 0 {
			resp.Status = http.StatusOK
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
	}
	if s.isPublic(module, "/"+r.PathValue("path")) {
		// Public paths (e.g. single sign-on) answer anyone; the module checks
		// what it is given.
		serve(w, r)
		return
	}
	s.guard(action, serve)(w, r)
}

// extNode is the node that answers a module request: the one named by
// ?node=, else, for a path below /branches/<name>/, the node holding that
// branch (a module sees only its own node's branches, see ext.BranchHost).
func (s *Server) extNode(r *http.Request) string {
	if node := r.URL.Query().Get("node"); node != "" {
		return node
	}
	rest, ok := strings.CutPrefix("/"+r.PathValue("path"), "/branches/")
	if !ok || s.Branches == nil {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	n, err := domain.ParseName(name)
	if err != nil {
		return ""
	}
	svc, err := s.Branches()
	if err != nil {
		return ""
	}
	b, err := svc.GetBranch(r.Context(), n)
	if err != nil {
		return ""
	}
	return b.Node
}

// modulePages lists the loaded modules' console pages, by module.
func (s *Server) modulePages() []ModulePage {
	out := append([]ModulePage{}, s.ExtPages...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}
