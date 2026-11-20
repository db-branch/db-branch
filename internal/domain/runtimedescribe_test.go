package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func describedLikeManifest(t *testing.T) (domain.RuntimeManifest, domain.EngineSpec) {
	t.Helper()
	m, err := domain.ParseManifest(manifestJSON(nil))
	if err != nil {
		t.Fatal(err)
	}
	return m, m.Spec().Clone()
}

func TestCheckDescribe_マニフェストと一致するDescribeを受理する(t *testing.T) {
	m, spec := describedLikeManifest(t)
	if err := m.CheckDescribe(spec); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDescribe_宣言が食い違えば食い違った項目を名指しして拒否する(t *testing.T) {
	cases := map[string]func(*domain.EngineSpec){
		"engine":          func(s *domain.EngineSpec) { s.Engine = "other" },
		"display_name":    func(s *domain.EngineSpec) { s.DisplayName = "Else" },
		"collation_bound": func(s *domain.EngineSpec) { s.CollationBound = true },
		"run_as_user":     func(s *domain.EngineSpec) { s.RunAsUser = "root" },
		"zfs_props":       func(s *domain.EngineSpec) { s.ZFSProps = map[string]string{"recordsize": "1M"} },
		"protocols":       func(s *domain.EngineSpec) { s.Protocols = s.Protocols[:1] },
		"capabilities":    func(s *domain.EngineSpec) { s.Capabilities.SourceDump = true },
	}
	for field, mutate := range cases {
		m, spec := describedLikeManifest(t)
		mutate(&spec)
		err := m.CheckDescribe(spec)
		if !errors.Is(err, domain.ErrInvalidManifest) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v", field, err)
		}
	}
}

func TestCheckDescribe_ZFSプロパティの空と未指定は同じとみなす(t *testing.T) {
	m, spec := describedLikeManifest(t)
	m.ZFSProps = map[string]string{}
	spec.ZFSProps = nil
	if err := m.CheckDescribe(spec); err != nil {
		t.Fatal(err)
	}
}
