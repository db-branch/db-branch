package domain_test

import (
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestParseRuntimeRef_エンジンとバージョンを読み取る(t *testing.T) {
	cases := map[string]domain.RuntimeRef{
		"mysql@8.4.13":        {Engine: domain.MySQL, Version: "8.4.13"},
		"mysql@26.10.0":       {Engine: domain.MySQL, Version: "26.10.0"},
		"postgres@17.2":       {Engine: domain.Postgres, Version: "17.2"},
		"mysql@8.4.13-custom": {Engine: domain.MySQL, Version: "8.4.13-custom"},
	}
	for s, want := range cases {
		got, err := domain.ParseRuntimeRef(s)
		if err != nil {
			t.Errorf("ParseRuntimeRef(%q) error: %v", s, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRuntimeRef(%q) = %+v, want %+v", s, got, want)
		}
		if got.String() != s {
			t.Errorf("String() = %q, want %q", got.String(), s)
		}
	}
}

func TestParseRuntimeRef_不正な指定を拒否する(t *testing.T) {
	for _, s := range []string{
		"", "mysql", "mysql@", "@8.4", "oracle@19.0", "mysql@8", "mysql@latest",
		"mysql@8.4.13/../x", "mysql@8.4 .1", "MySQL@8.4.13", "mysql@8.4.13-",
	} {
		if _, err := domain.ParseRuntimeRef(s); !errors.Is(err, domain.ErrInvalidRuntime) {
			t.Errorf("ParseRuntimeRef(%q) error = %v, want ErrInvalidRuntime", s, err)
		}
	}
}

func TestPlatform_アーキテクチャとディストリビューションを組み立てて分ける(t *testing.T) {
	if got := domain.Platform("amd64", ""); got != "amd64" {
		t.Errorf("Platform without distro = %q", got)
	}
	p := domain.Platform("arm64", "ubuntu-26.04")
	arch, distro := domain.SplitPlatform(p)
	if p != "arm64/ubuntu-26.04" || arch != "arm64" || distro != "ubuntu-26.04" {
		t.Errorf("Platform = %q, split = %q %q", p, arch, distro)
	}
	if a, d := domain.SplitPlatform("amd64"); a != "amd64" || d != "" {
		t.Errorf("split of arch alone = %q %q", a, d)
	}
}

func TestPeerPlatform_ディストリビューションのない古いノードはUbuntu2404とみなす(t *testing.T) {
	if got := domain.PeerPlatform("amd64"); got != "amd64/ubuntu-24.04" {
		t.Errorf("legacy = %q", got)
	}
	if got := domain.PeerPlatform("amd64/ubuntu-26.04"); got != "amd64/ubuntu-26.04" {
		t.Errorf("explicit = %q", got)
	}
}
