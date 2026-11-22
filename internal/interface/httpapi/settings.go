package httpapi

import (
	"context"
	"fmt"
	"github.com/db-branch/db-branch/internal/usecase/operation"
	"net/http"

	"github.com/db-branch/db-branch/internal/usecase/settings"
	"github.com/db-branch/db-branch/pkg/ext"
)

// Settings is the settings use case the API needs.
type Settings interface {
	Scopes() []settings.Scope
	Values(ctx context.Context, scope string) (map[string]string, error)
	Put(ctx context.Context, scope string, values map[string]string) error
}

type settingJSON struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Help    string `json:"help,omitempty"`
	Type    string `json:"type"`
	Default string `json:"default"`
	// Value is empty for a secret: secrets are write-only, and SecretSet
	// says whether one is stored.
	Value     string `json:"value"`
	SecretSet *bool  `json:"secret_set,omitempty"`

	Translations map[string]ext.SettingText `json:"translations,omitempty"`
}

type scopeJSON struct {
	Scope    string        `json:"scope"`
	Title    string        `json:"title"`
	Settings []settingJSON `json:"settings"`
}

func (s *Server) scopeView(ctx context.Context, sc settings.Scope) (scopeJSON, error) {
	values, err := s.Settings.Values(ctx, sc.Name)
	if err != nil {
		return scopeJSON{}, err
	}
	v := scopeJSON{Scope: sc.Name, Title: sc.Title, Settings: []settingJSON{}}
	for _, d := range sc.Settings {
		st := settingJSON{Key: d.Key, Label: d.Label, Help: d.Help, Type: d.Type, Default: d.Default, Value: values[d.Key], Translations: d.Translations}
		if d.Type == "secret" {
			set := st.Value != "" && st.Value != d.Default
			st.Value, st.SecretSet = "", &set
		}
		v.Settings = append(v.Settings, st)
	}
	return v, nil
}

func (s *Server) withSettings(w http.ResponseWriter) bool {
	if s.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "not_initialized", "settings are not available on this node")
		return false
	}
	return true
}

func (s *Server) listSettings(w http.ResponseWriter, r *http.Request) {
	if !s.withSettings(w) {
		return
	}
	out := []scopeJSON{}
	for _, sc := range s.Settings.Scopes() {
		v, err := s.scopeView(r.Context(), sc)
		if err != nil {
			fail(w, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, items(out))
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	if !s.withSettings(w) {
		return
	}
	var in struct {
		Values map[string]string `json:"values"`
		// Clear names secrets to remove; a secret left out or sent empty keeps its value.
		Clear []string `json:"clear"`
	}
	if !decode(w, r, &in) {
		return
	}
	scope := r.PathValue("scope")
	values, err := s.secretSafeValues(scope, in.Values, in.Clear)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.Settings.Put(r.Context(), scope, values); err != nil {
		fail(w, err)
		return
	}
	for _, sc := range s.Settings.Scopes() {
		if sc.Name == scope {
			v, err := s.scopeView(r.Context(), sc)
			if err != nil {
				fail(w, err)
				return
			}
			s.Bus.PublishTo(operation.Audience{Action: "setting.read"}, "setting.updated", v)
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
}

// secretSafeValues turns a PUT body into what the use case stores: an empty
// secret keeps its value (it is never shown, so a form cannot send it back),
// and only a name in clear removes it.
func (s *Server) secretSafeValues(scope string, in map[string]string, clear []string) (map[string]string, error) {
	types := map[string]string{}
	for _, sc := range s.Settings.Scopes() {
		if sc.Name == scope {
			for _, d := range sc.Settings {
				types[d.Key] = d.Type
			}
		}
	}
	out := map[string]string{}
	for k, v := range in {
		if v == "" && types[k] == "secret" {
			continue
		}
		out[k] = v
	}
	for _, k := range clear {
		if types[k] != "secret" {
			return nil, fmt.Errorf("%w: %q is not a secret setting", settings.ErrInvalid, k)
		}
		out[k] = ""
	}
	return out, nil
}
