package httpapi

import (
	"context"

	"github.com/db-branch/db-branch/internal/domain"
)

// engineSpec is what an engine declares: its runtime provider's word where it
// has one (it knows what is wired), else the registry's.
func (s *Server) engineSpec(ctx context.Context, e domain.Engine) (domain.EngineSpec, bool, error) {
	if s.Providers != nil {
		if p, ok := s.Providers.Provider(e); ok {
			spec, err := p.Describe(ctx)
			return spec, err == nil, err
		}
	}
	spec, ok := domain.DefaultRuntimes.Spec(e)
	return spec, ok, nil
}

// requireFeature fails with ErrFeatureNotSupported unless the engine offers feature.
func (s *Server) requireFeature(ctx context.Context, e domain.Engine, feature string) error {
	spec, ok, err := s.engineSpec(ctx, e)
	if err != nil {
		return err
	}
	if !ok {
		return domain.FeatureNotSupported(feature)
	}
	return spec.Capabilities.Require(feature)
}

type protocolJSON struct {
	Name    string `json:"name"`
	Primary bool   `json:"primary"`
}

type engineJSON struct {
	DisplayName    string          `json:"display_name"`
	CollationBound bool            `json:"collation_bound"`
	Protocols      []protocolJSON  `json:"protocols"`
	Features       map[string]bool `json:"features"`
}

// engineViews describes every registered engine for GET /v1/capabilities.
// Features are all listed, true or false: nothing is left to a fallback. Most
// come from the engine's declaration; history_record also needs a module that
// records and accepts the engine (its licence).
func (s *Server) engineViews(ctx context.Context, recordable map[domain.Engine]bool) map[string]engineJSON {
	out := map[string]engineJSON{}
	for _, e := range domain.DefaultRuntimes.Engines() {
		spec, ok, err := s.engineSpec(ctx, e)
		if err != nil || !ok {
			spec, _ = domain.DefaultRuntimes.Spec(e) // a provider that cannot answer must not hide the engine
		}
		features := spec.Capabilities.Features()
		features["history_record"] = features["history_record"] && recordable[e]
		ports := make([]protocolJSON, len(spec.Protocols))
		for i, p := range spec.Protocols {
			ports[i] = protocolJSON{Name: p.Name, Primary: p.Primary}
		}
		out[string(e)] = engineJSON{DisplayName: spec.DisplayName, CollationBound: spec.CollationBound, Protocols: ports, Features: features}
	}
	return out
}
