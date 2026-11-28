package catalog_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/catalog"
)

const sha = "5dfbb764f731df40ef9e4f5d69aed32bdac8ba1af20bd96b7d40a942d83d4ed2"

func sample() string {
	return `{"runtimes": [
  {"engine": "mysql", "version": "8.0.46", "artifacts": {"amd64": {"url": "https://x.test/80.tar.xz", "sha256": "` + sha + `", "size": 1}}},
  {"engine": "mysql", "version": "8.4.13", "artifacts": {
    "amd64": {"url": "https://x.test/84-amd64.tar.xz", "sha256": "` + sha + `", "size": 2},
    "arm64": {"url": "https://x.test/84-arm64.tar.xz", "sha256": "` + sha + `", "size": 3}}},
  {"engine": "mysql", "version": "26.10.0", "artifacts": {"arm64": {"url": "https://x.test/26.tar.xz", "sha256": "` + sha + `", "size": 4}}},
  {"engine": "postgres", "version": "17.2", "artifacts": {"amd64": {"url": "https://x.test/pg.tar.xz", "sha256": "` + sha + `", "size": 5}}}
]}`
}

func ref(s string) domain.RuntimeRef {
	r, err := domain.ParseRuntimeRef(s)
	if err != nil {
		panic(err)
	}
	return r
}

func TestLookup_アーキテクチャに合うアーカイブを返す(t *testing.T) {
	c, err := catalog.Parse([]byte(sample()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Lookup(ref("mysql@8.4.13"), "arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Artifact{{URL: "https://x.test/84-arm64.tar.xz", SHA256: sha, Size: 3, Ref: ref("mysql@8.4.13"), Arch: "arm64"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Lookup = %+v, want %+v", got, want)
	}
}

func TestLookup_ないバージョンやアーキテクチャはNotFound(t *testing.T) {
	c, _ := catalog.Parse([]byte(sample()))
	for _, tc := range []struct{ ref, arch string }{{"mysql@9.9.9", "amd64"}, {"mysql@26.10.0", "amd64"}} {
		if _, err := c.Lookup(ref(tc.ref), tc.arch); !errors.Is(err, domain.ErrRuntimeNotFound) {
			t.Errorf("%s/%s: error = %v", tc.ref, tc.arch, err)
		}
	}
}

func TestAvailable_新しい版から順に返す(t *testing.T) {
	c, _ := catalog.Parse([]byte(sample()))

	got := c.Available(domain.MySQL, "amd64")
	want := []domain.RuntimeRef{ref("mysql@8.4.13"), ref("mysql@8.0.46")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Available(mysql, amd64) = %v, want %v", got, want)
	}
	got = c.Available(domain.MySQL, "arm64")
	want = []domain.RuntimeRef{ref("mysql@26.10.0"), ref("mysql@8.4.13")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Available(mysql, arm64) = %v, want %v", got, want)
	}
}

func TestAvailable_桁数が違う版も数値として比べる(t *testing.T) {
	body := `{"runtimes": [
  {"engine": "mysql", "version": "8.4", "artifacts": {"amd64": {"url": "https://x.test/a", "sha256": "` + sha + `"}}},
  {"engine": "mysql", "version": "8.10.1", "artifacts": {"amd64": {"url": "https://x.test/b", "sha256": "` + sha + `"}}},
  {"engine": "mysql", "version": "8.4.1", "artifacts": {"amd64": {"url": "https://x.test/c", "sha256": "` + sha + `"}}}
]}`
	c, err := catalog.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Available(domain.MySQL, "amd64")
	want := []domain.RuntimeRef{ref("mysql@8.10.1"), ref("mysql@8.4.1"), ref("mysql@8.4")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Available = %v, want %v", got, want)
	}
}

func TestAvailable_プレリリース版は正式版より後ろに並ぶ(t *testing.T) {
	body := `{"runtimes": [
  {"engine": "mysql", "version": "8.4.0-alpha.1", "artifacts": {"amd64": {"url": "https://x.test/a", "sha256": "` + sha + `"}}},
  {"engine": "mysql", "version": "8.4.0-rc.1", "artifacts": {"amd64": {"url": "https://x.test/b", "sha256": "` + sha + `"}}},
  {"engine": "mysql", "version": "8.4.0-beta", "artifacts": {"amd64": {"url": "https://x.test/c", "sha256": "` + sha + `"}}},
  {"engine": "mysql", "version": "8.4.0", "artifacts": {"amd64": {"url": "https://x.test/d", "sha256": "` + sha + `"}}},
  {"engine": "mysql", "version": "8.4-rc.1", "artifacts": {"amd64": {"url": "https://x.test/e", "sha256": "` + sha + `"}}},
  {"engine": "mysql", "version": "8.3", "artifacts": {"amd64": {"url": "https://x.test/f", "sha256": "` + sha + `"}}}
]}`
	c, err := catalog.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Available(domain.MySQL, "amd64")
	want := []domain.RuntimeRef{
		ref("mysql@8.4.0"),
		ref("mysql@8.4.0-rc.1"),
		ref("mysql@8.4-rc.1"),
		ref("mysql@8.4.0-beta"),
		ref("mysql@8.4.0-alpha.1"),
		ref("mysql@8.3"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Available = %v, want %v", got, want)
	}
}

func TestParse_不正なカタログを拒否する(t *testing.T) {
	entry := func(engine, version, url, digest string) string {
		return `{"runtimes": [{"engine": "` + engine + `", "version": "` + version +
			`", "artifacts": {"amd64": {"url": "` + url + `", "sha256": "` + digest + `", "size": 1}}}]}`
	}
	cases := map[string]string{
		"broken json":   `{`,
		"bad engine":    entry("oracle", "1.0", "https://x.test/a", sha),
		"bad version":   entry("mysql", "latest", "https://x.test/a", sha),
		"plain http":    entry("mysql", "8.4.13", "http://x.test/a", sha),
		"short digest":  entry("mysql", "8.4.13", "https://x.test/a", "abc"),
		"upper digest":  entry("mysql", "8.4.13", "https://x.test/a", strings.ToUpper(sha)),
		"duplicate":     `{"runtimes": [` + strings.TrimSuffix(strings.TrimPrefix(entry("mysql", "8.4.13", "https://x.test/a", sha), `{"runtimes": [`), `]}`) + `,` + strings.TrimSuffix(strings.TrimPrefix(entry("mysql", "8.4.13", "https://x.test/a", sha), `{"runtimes": [`), `]}`) + `]}`,
		"unknown field": `{"runtimes": [], "extra": 1}`,
	}
	for label, body := range cases {
		if _, err := catalog.Parse([]byte(body)); err == nil {
			t.Errorf("%s: want error, got nil", label)
		}
	}
}

func TestDefault_同梱のカタログが読み込めMySQL84を含む(t *testing.T) {
	c, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup(ref("mysql@8.4.13"), "amd64"); err != nil {
		t.Errorf("default catalog should offer mysql@8.4.13 for amd64: %v", err)
	}
}

func TestDefault_同梱のカタログはarm64にもMySQL8系を提供する(t *testing.T) {
	c, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"mysql@8.0.46", "mysql@8.4.13", "mysql@9.6.0"} {
		parts, err := c.Lookup(ref(v), "arm64")
		if err != nil {
			t.Errorf("default catalog should offer %s for arm64: %v", v, err)
			continue
		}
		if len(parts) != 1 || !strings.HasSuffix(parts[0].URL, "-aarch64.tar.xz") {
			t.Errorf("%s arm64: want one aarch64 tarball, got %v", v, parts)
		}
	}
}

func TestLookup_複数のアーカイブからなるランタイムは順に返す(t *testing.T) {
	c, err := catalog.Parse([]byte(`{"runtimes": [{"engine": "postgres", "version": "18.6", "artifacts": {"amd64": {"parts": [
		{"url": "https://x.test/server.deb", "sha256": "` + sha + `", "size": 7},
		{"url": "https://x.test/libpq.deb", "sha256": "` + sha + `", "size": 8}]}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Lookup(ref("postgres@18.6"), "amd64")
	if err != nil || len(got) != 2 || got[0].URL != "https://x.test/server.deb" || got[1].Part != 1 || got[1].Size != 8 {
		t.Errorf("Lookup = %+v %v", got, err)
	}
	if _, err := catalog.Parse([]byte(`{"runtimes": [{"engine": "postgres", "version": "18.6", "artifacts": {"amd64": {"parts": [{"url": "http://x", "sha256": "` + sha + `"}]}}}]}`)); err == nil {
		t.Error("a part over http: want error")
	}
}

func distroSample() string {
	return `{"runtimes": [
  {"engine": "mysql", "version": "8.4.13", "artifacts": {"amd64": {"url": "https://x.test/my.tar.xz", "sha256": "` + sha + `", "size": 1}}},
  {"engine": "postgres", "version": "18.6", "artifacts": {"amd64": {"distros": {
    "ubuntu-24.04": {"parts": [{"url": "https://x.test/pg-24.deb", "sha256": "` + sha + `", "size": 2}]},
    "ubuntu-26.04": {"parts": [{"url": "https://x.test/pg-26.deb", "sha256": "` + sha + `", "size": 3}, {"url": "https://x.test/libpq-26.deb", "sha256": "` + sha + `", "size": 4}]}}}}}
]}`
}

func TestLookup_ディストリビューション別のアーカイブはOSに合うものを返す(t *testing.T) {
	c, err := catalog.Parse([]byte(distroSample()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Lookup(ref("postgres@18.6"), "amd64/ubuntu-26.04")
	if err != nil || len(got) != 2 || got[0].URL != "https://x.test/pg-26.deb" || got[1].Part != 1 || got[0].Arch != "amd64/ubuntu-26.04" {
		t.Errorf("26.04: %+v %v", got, err)
	}
	got, err = c.Lookup(ref("postgres@18.6"), "amd64/ubuntu-24.04")
	if err != nil || len(got) != 1 || got[0].URL != "https://x.test/pg-24.deb" {
		t.Errorf("24.04: %+v %v", got, err)
	}
}

func TestLookup_ディストリビューションに依らないランタイムはOSを問わない(t *testing.T) {
	c, _ := catalog.Parse([]byte(distroSample()))
	for _, p := range []string{"amd64", "amd64/ubuntu-22.04", "amd64/debian-13"} {
		if _, err := c.Lookup(ref("mysql@8.4.13"), p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestLookup_未対応のOSは対応OSを挙げて断る(t *testing.T) {
	c, _ := catalog.Parse([]byte(distroSample()))
	_, err := c.Lookup(ref("postgres@18.6"), "amd64/ubuntu-22.04")
	if !errors.Is(err, domain.ErrUnsupportedDistro) {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{"ubuntu-22.04", "ubuntu-24.04", "ubuntu-26.04"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	_, err = c.Lookup(ref("postgres@18.6"), "amd64")
	if !errors.Is(err, domain.ErrUnsupportedDistro) || !strings.Contains(err.Error(), "could not be detected") {
		t.Errorf("unknown OS: %v", err)
	}
}

func TestAvailable_対応するOSのランタイムだけ挙げる(t *testing.T) {
	c, _ := catalog.Parse([]byte(distroSample()))
	if got := c.Available(domain.Postgres, "amd64/ubuntu-26.04"); len(got) != 1 {
		t.Errorf("26.04 = %v", got)
	}
	if got := c.Available(domain.Postgres, "amd64/ubuntu-22.04"); len(got) != 0 {
		t.Errorf("22.04 = %v", got)
	}
	if got := c.Available(domain.MySQL, "amd64/ubuntu-22.04"); len(got) != 1 {
		t.Errorf("mysql on 22.04 = %v", got)
	}
}

func TestParse_ディストリビューション別の定義の誤りを断る(t *testing.T) {
	part := `{"url": "https://x.test/a.deb", "sha256": "` + sha + `", "size": 1}`
	for name, art := range map[string]string{
		"both":       `{"url": "https://x.test/a", "sha256": "` + sha + `", "distros": {"ubuntu-24.04": {"parts": [` + part + `]}}}`,
		"http":       `{"distros": {"ubuntu-24.04": {"parts": [{"url": "http://x", "sha256": "` + sha + `"}]}}}`,
		"bad digest": `{"distros": {"ubuntu-24.04": {"parts": [{"url": "https://x", "sha256": "zz"}]}}}`,
		"nested":     `{"distros": {"ubuntu-24.04": {"distros": {"ubuntu-26.04": {"parts": [` + part + `]}}}}}`,
	} {
		body := `{"runtimes": [{"engine": "postgres", "version": "18.6", "artifacts": {"amd64": ` + art + `}}]}`
		if _, err := catalog.Parse([]byte(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestDefault_同梱のPostgreSQLは対応する全ディストリビューションとアーキテクチャで揃う(t *testing.T) {
	c, _ := catalog.Default()
	wantVersions := []string{"postgres@19.0", "postgres@18.7", "postgres@17.12"}
	for _, v := range wantVersions {
		for _, arch := range []string{"amd64", "arm64"} {
			for distro, tag := range map[string]string{"ubuntu-24.04": "pgdg24.04", "ubuntu-26.04": "pgdg26.04"} {
				parts, err := c.Lookup(ref(v), arch+"/"+distro)
				if err != nil || len(parts) != 3 {
					t.Fatalf("%s %s %s: %d parts, %v", v, arch, distro, len(parts), err)
				}
				for _, p := range parts {
					if !strings.Contains(p.URL, tag) || !strings.HasSuffix(p.URL, "_"+arch+".deb") {
						t.Errorf("%s %s %s: %s", v, arch, distro, p.URL)
					}
				}
			}
		}
	}
	gotAvail := c.Available(domain.Postgres, "amd64/ubuntu-24.04")
	wantAvail := []domain.RuntimeRef{ref("postgres@19.0"), ref("postgres@18.7"), ref("postgres@17.12")}
	if !reflect.DeepEqual(gotAvail, wantAvail) {
		t.Errorf("Available(postgres, amd64/ubuntu-24.04) = %v, want %v", gotAvail, wantAvail)
	}
	if _, err := c.Lookup(ref("postgres@19.0"), "amd64/ubuntu-22.04"); !errors.Is(err, domain.ErrUnsupportedDistro) {
		t.Errorf("22.04: %v", err)
	}
}

func TestDefault_同梱のカタログにMySQL5系があり_amd64だけで_チェックサムを持つ(t *testing.T) {
	c, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"5.7.44", "5.6.51"} {
		ref := domain.RuntimeRef{Engine: domain.MySQL, Version: v}
		arts, err := c.Lookup(ref, "amd64")
		if err != nil || len(arts) != 1 {
			t.Fatalf("%s: %v %v", v, arts, err)
		}
		if !strings.HasPrefix(arts[0].URL, "https://cdn.mysql.com/archives/mysql-"+v[:3]+"/") || !strings.HasSuffix(arts[0].URL, ".tar.gz") || len(arts[0].SHA256) != 64 || arts[0].Size == 0 {
			t.Errorf("%s: %+v", v, arts[0])
		}
		// Oracle publishes no arm64 build of 5.x.
		if _, err := c.Lookup(ref, "arm64"); err == nil {
			t.Errorf("%s has no arm64 build", v)
		}
	}
}

func TestNewerVersion_境界ケースを比較する(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.0.0", "1.0", true},
		{"v1.1.0", "1.0.0", true},
		{"1-rc.1", "1-beta.1", true},
		{"1.0.0", "bad", true},
		{"bad", "1.0.0", false},
		{"", "1.0.0", false},
		{"b", "a", true},
		{"1.2.3.4-rc.1", "bad", true},
		{"1.2.3.5", "1.2.3.4-rc.1", true},
		{"1.2.3.4-rc.1", "1.2.3.5", false},
		{"1.2.3.10", "1.2.3.2", true},
		{"1.2.3.2", "1.2.3.1a", true},
		{"1.2.3.10", "1.2.3.1a", true},
		{"1.2.3.1a", "1.2.3.10", false},
		{"10.0.0.1", "2.0.0.1", true},
		{"2024.02.1", "2024.01.1", true},
	}
	for _, tc := range cases {
		if got := catalog.NewerVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("NewerVersion(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
