package httpapi

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// Modules is the cluster module use case the API needs.
type Modules interface {
	Install(ctx context.Context, name, version string) (domain.ModuleSpec, error)
	Remove(ctx context.Context, name string) error
	List(ctx context.Context) ([]domain.ModuleSpec, error)
	// Available lists modules the APT repositories offer, with descriptions.
	Available(ctx context.Context) (map[string]string, error)
}

// moduleExempt actions work while modules are pending, so a node can be
// inspected and its modules fixed.
func moduleExempt(action string) bool {
	switch {
	case strings.HasPrefix(action, "auth."), strings.HasPrefix(action, "module."), strings.HasPrefix(action, "setting."), strings.HasPrefix(action, "license."):
		return true
	}
	switch action {
	case "cluster.read", "capability.read", "event.read", "operation.read":
		return true
	}
	return false
}

type moduleNodeJSON struct {
	Node    string `json:"node"`
	Loaded  string `json:"loaded"` // version running there, "" if none
	Current bool   `json:"current"`
	// Problem is the module's complaint on that node, e.g. about the licence.
	Problem string `json:"problem,omitempty"`
}

type moduleJSON struct {
	Name    string           `json:"name"`
	Version string           `json:"version"`
	Wanted  bool             `json:"wanted"`
	Nodes   []moduleNodeJSON `json:"nodes"`
	// RequiresLicense: the module works only under a licence key.
	RequiresLicense bool `json:"requires_license"`
}

// loadedModules maps node -> module -> version from heartbeats (or this
// node), and node -> module -> problem.
func (s *Server) loadedModules(ctx context.Context) (map[string]map[string]string, map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	problems := map[string]map[string]string{}
	add := func(node string, mods []string) {
		out[node] = map[string]string{}
		for _, m := range mods {
			name, version, _ := strings.Cut(m, " ")
			out[node][name] = version
		}
	}
	if s.Cluster == nil {
		add(s.Node, s.Modules)
		if s.ModuleProblems != nil {
			problems[s.Node] = s.ModuleProblems()
		}
		return out, problems, nil
	}
	st, err := s.Cluster.Status(ctx)
	for _, n := range st.Nodes {
		add(n.Name, n.Modules)
		problems[n.Name] = n.ModuleProblems
	}
	return out, problems, err
}

func (s *Server) withModules(w http.ResponseWriter) bool {
	if s.ModuleAdmin == nil {
		writeError(w, http.StatusServiceUnavailable, "not_initialized", "modules are not available on this node")
		return false
	}
	return true
}

func (s *Server) listModules(w http.ResponseWriter, r *http.Request) {
	if !s.withModules(w) {
		return
	}
	specs, err := s.ModuleAdmin.List(r.Context())
	var loaded, problems map[string]map[string]string
	if err == nil {
		loaded, problems, err = s.loadedModules(r.Context())
	}
	if err != nil {
		fail(w, err)
		return
	}
	nodes := make([]string, 0, len(loaded))
	for n := range loaded {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	out := make([]moduleJSON, 0, len(specs))
	for _, m := range specs {
		v := moduleJSON{Name: m.Name, Version: m.Version, Wanted: m.Wanted, Nodes: []moduleNodeJSON{}, RequiresLicense: slices.Contains(s.PaidModules, m.Name)}
		for _, n := range nodes {
			got := loaded[n][m.Name]
			v.Nodes = append(v.Nodes, moduleNodeJSON{Node: n, Loaded: got, Current: got != "" && got == m.Version, Problem: problems[n][m.Name]})
		}
		out = append(out, v)
	}
	// What the repositories offer; without them (offline), just what is recorded.
	available, err := s.ModuleAdmin.Available(r.Context())
	if err != nil {
		available = map[string]string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "available": available})
}

func (s *Server) installModule(w http.ResponseWriter, r *http.Request) {
	if !s.withModules(w) {
		return
	}
	var in struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	m, err := s.ModuleAdmin.Install(r.Context(), in.Name, in.Version)
	if err != nil {
		fail(w, err)
		return
	}
	s.Bus.Publish("module.updated", m)
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) removeModule(w http.ResponseWriter, r *http.Request) {
	if !s.withModules(w) {
		return
	}
	if err := s.ModuleAdmin.Remove(r.Context(), r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	s.Bus.Publish("module.updated", map[string]string{"name": r.PathValue("name")})
	w.WriteHeader(http.StatusNoContent)
}
