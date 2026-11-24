package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

type leanMatrixCoverage struct {
	Routes []struct {
		Endpoint         string `json:"endpoint"`
		Method           string `json:"method"`
		Path             string `json:"path"`
		Action           string `json:"action"`
		SocketOnly       bool   `json:"socket_only"`
		PreMuxPublic     bool   `json:"pre_mux_public"`
		PublicExt        bool   `json:"public_ext"`
		AnonymousAllowed bool   `json:"anonymous_allowed"`
	} `json:"routes"`
	ActionAliases map[string][]string `json:"action_aliases"`
	TokenScopes   []struct {
		Action string `json:"action"`
	} `json:"token_scope_cases"`
	HTTPCases []struct {
		Endpoint string `json:"endpoint"`
	} `json:"http_cases"`
}

func readLeanCoverageMatrix(t *testing.T) leanMatrixCoverage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "formal", "lean", "decision_matrix.json"))
	if err != nil {
		t.Fatalf("read decision_matrix.json: %v", err)
	}
	var m leanMatrixCoverage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal decision_matrix.json: %v", err)
	}
	if len(m.Routes) == 0 || len(m.ActionAliases) == 0 || len(m.TokenScopes) == 0 || len(m.HTTPCases) == 0 {
		t.Fatal("decision_matrix.json is missing routes, action_aliases, token_scope_cases, or http_cases")
	}
	return m
}

func concretePathForPattern(pattern string) string {
	r := strings.NewReplacer(
		"{name}", "b1",
		"{checkpoint}", "cp1",
		"{snapshot}", "s1",
		"{id}", "id1",
		"{scope}", "core",
		"{engine}", "mysql",
		"{version}", "8.4.14",
		"{file}", "f1",
	)
	return r.Replace(pattern)
}

func TestLeanAuthz_RouterAndAliasesMatchDecisionMatrix(t *testing.T) {
	m := readLeanCoverageMatrix(t)

	httpEndpoints := map[string]bool{}
	for _, c := range m.HTTPCases {
		httpEndpoints[c.Endpoint] = true
	}

	type routeEntry struct {
		action           string
		socketOnly       bool
		preMuxPublic     bool
		anonymousAllowed bool
		endpoints        []string
	}
	matrixByRoute := map[string]*routeEntry{}
	extEndpoints := map[string]struct {
		action    string
		publicExt bool
	}{}
	for _, r := range m.Routes {
		if !httpEndpoints[r.Endpoint] {
			t.Errorf("endpoint %q is in routes but missing from http_cases in decision_matrix.json", r.Endpoint)
		}
		if r.Path == moduleAPIPrefix+"{name}/{path...}" {
			extEndpoints[r.Endpoint] = struct {
				action    string
				publicExt bool
			}{action: r.Action, publicExt: r.PublicExt}
			continue
		}
		if r.PublicExt {
			t.Errorf("non-ext endpoint %q unexpectedly marked public_ext=true", r.Endpoint)
		}
		key := r.Method + " " + r.Path
		if cur, ok := matrixByRoute[key]; ok {
			if cur.action != r.Action || cur.socketOnly != r.SocketOnly || cur.preMuxPublic != r.PreMuxPublic || cur.anonymousAllowed != r.AnonymousAllowed {
				t.Errorf("conflicting Lean route metadata for %s", key)
			}
			cur.endpoints = append(cur.endpoints, r.Endpoint)
		} else {
			matrixByRoute[key] = &routeEntry{
				action:           r.Action,
				socketOnly:       r.SocketOnly,
				preMuxPublic:     r.PreMuxPublic,
				anonymousAllowed: r.AnonymousAllowed,
				endpoints:        []string{r.Endpoint},
			}
		}
	}

	// 1. Every route registered in server.go routes(local=false and local=true) + outerPatterns must be in decision_matrix.json.
	served := map[string]string{}
	var hasExtWildcard bool
	for _, localFlag := range []bool{false, true} {
		rec := &patternRecorder{actions: map[string]string{}}
		(&Server{}).routes(rec, localFlag)
		for _, p := range append(rec.patterns, outerPatterns...) {
			if p == moduleAPIPrefix+"{name}/{path...}" {
				hasExtWildcard = true
				continue
			}
			method, path, ok := strings.Cut(p, " ")
			if !ok {
				t.Fatalf("local=%v: unexpected methodless route pattern %q", localFlag, p)
			}
			key := method + " " + path
			action := rec.actions[p]
			if key == "POST /v1/auth/login" && action == "" {
				action = "auth.login"
			}
			if prev, exists := served[key]; exists && prev != action {
				t.Errorf("route %s has differing actions between local=false (%q) and local=true (%q)", key, prev, action)
			}
			served[key] = action
			entry, inMatrix := matrixByRoute[key]
			if !inMatrix {
				t.Errorf("server route %s (local=%v, action %q) is missing from formal/lean decision_matrix.json; update formal/lean and re-run ./formal/lean/verify.sh", key, localFlag, action)
				continue
			}
			if entry.action != action {
				t.Errorf("route %s: server action=%q, decision_matrix.json action=%q", key, action, entry.action)
			}
			// Check pre-mux, anonymous allowlist, and socket_only expectations.
			if slices.Contains(outerPatterns, p) != entry.preMuxPublic {
				t.Errorf("route %s: preMuxPublic=%v, outerPatterns=%v", key, entry.preMuxPublic, slices.Contains(outerPatterns, p))
			}
			if !entry.preMuxPublic {
				req := httptest.NewRequest(method, concretePathForPattern(path), nil)
				if gotAnon := anonymousAllowed(req); gotAnon != entry.anonymousAllowed {
					t.Errorf("route %s: anonymousAllowed(Go)=%v, decision_matrix.json=%v", key, gotAnon, entry.anonymousAllowed)
				}
				wantBeforePW := slices.Contains([]string{"GET /v1/auth/session", "POST /v1/auth/logout", "PUT /v1/auth/password"}, key)
				if gotBeforePW := allowedBeforePasswordChange(req); gotBeforePW != wantBeforePW {
					t.Errorf("route %s: allowedBeforePasswordChange(Go)=%v, expected=%v", key, gotBeforePW, wantBeforePW)
				}
			}
		}
	}
	if !hasExtWildcard {
		t.Errorf("expected module wildcard route %s{name}/{path...} in server.routes", moduleAPIPrefix)
	}
	wantExt := map[string]struct {
		action    string
		publicExt bool
	}{
		"ext_private_read":  {action: "ext.mod.read", publicExt: false},
		"ext_private_write": {action: "ext.mod.write", publicExt: false},
		"ext_public_read":   {action: "ext.mod.read", publicExt: true},
		"ext_public_write":  {action: "ext.mod.write", publicExt: true},
	}
	if len(extEndpoints) != len(wantExt) {
		t.Errorf("ext endpoints count = %d, want %d (%v)", len(extEndpoints), len(wantExt), extEndpoints)
	}
	for ep, want := range wantExt {
		if got, ok := extEndpoints[ep]; !ok || got != want {
			t.Errorf("ext endpoint %q: got %+v (present=%v), want %+v", ep, got, ok, want)
		}
	}

	// Verify exact socket_only set in routes.
	wantSocketOnly := []string{"POST /v1/auth/reset", "POST /v1/backups/restore", "POST /v1/cluster/token"}
	var gotSocketOnly []string
	for key, entry := range matrixByRoute {
		if entry.socketOnly {
			gotSocketOnly = append(gotSocketOnly, key)
		}
	}
	sort.Strings(gotSocketOnly)
	if !slices.Equal(gotSocketOnly, wantSocketOnly) {
		t.Errorf("socket_only routes in decision_matrix.json = %v, want %v", gotSocketOnly, wantSocketOnly)
	}

	// 2. Every route in decision_matrix.json must still exist in server.go (or internalMaskTarget).
	for key, entry := range matrixByRoute {
		if key == "POST /v1/bases/{name}/mask" {
			req := httptest.NewRequest(http.MethodPost, "/v1/bases/prod/mask", nil)
			req = req.WithContext(context.WithValue(req.Context(), forwardedKey{}, true))
			if target, ok := internalMaskTarget(req); !ok || target != "prod" || entry.action != "base.mask" {
				t.Errorf("internalMaskTarget mismatch: target=%q ok=%v action=%q", target, ok, entry.action)
			}
			continue
		}
		if _, ok := served[key]; !ok {
			t.Errorf("stale route %s in decision_matrix.json is not served by server.go", key)
		}
	}

	// 3. legacyActionAliases in access.go must match action_aliases in decision_matrix.json,
	//    and every route action + legacy alias must be covered by token_scope_cases.
	if len(legacyActionAliases) != len(m.ActionAliases) {
		t.Errorf("legacyActionAliases has %d keys, decision_matrix.json has %d keys", len(legacyActionAliases), len(m.ActionAliases))
	}
	scopeCovered := map[string]bool{}
	for _, ts := range m.TokenScopes {
		scopeCovered[ts.Action] = true
	}
	for act, goAliases := range legacyActionAliases {
		leanAliases, ok := m.ActionAliases[act]
		if !ok {
			t.Errorf("legacyActionAliases[%q] missing from decision_matrix.json action_aliases", act)
			continue
		}
		g := append([]string(nil), goAliases...)
		l := append([]string(nil), leanAliases...)
		sort.Strings(g)
		sort.Strings(l)
		if !slices.Equal(g, l) {
			t.Errorf("legacyActionAliases[%q] = %v, decision_matrix.json = %v", act, g, l)
		}
		if !scopeCovered[act] {
			t.Errorf("legacy alias %q missing from token_scope_cases", act)
		}
		for _, a := range goAliases {
			if !scopeCovered[a] {
				t.Errorf("legacy alias target %q missing from token_scope_cases", a)
			}
		}
	}
	for _, r := range m.Routes {
		if r.Action != "" && !scopeCovered[r.Action] {
			t.Errorf("route %s action %q missing from token_scope_cases", r.Endpoint, r.Action)
		}
	}
}
