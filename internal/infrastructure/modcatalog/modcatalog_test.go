package modcatalog_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/modcatalog"
)

type source struct {
	entries []domain.CatalogEntry
	err     error
	calls   int
}

func (s *source) Catalog(context.Context) ([]domain.CatalogEntry, error) {
	s.calls++
	return s.entries, s.err
}

type base struct{ looked []domain.RuntimeRef }

func (b *base) Lookup(ref domain.RuntimeRef, platform string) ([]domain.Artifact, error) {
	b.looked = append(b.looked, ref)
	return []domain.Artifact{{URL: "https://core/x", Ref: ref, Arch: platform}}, nil
}
func (b *base) Available(e domain.Engine, platform string) []domain.RuntimeRef {
	return []domain.RuntimeRef{{Engine: e, Version: "1.0"}}
}

var publicIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func entries() []domain.CatalogEntry {
	return []domain.CatalogEntry{
		{Version: "24.8.1", Platform: "amd64", URL: "https://builds.example.com/ch-24.8.1.tar.xz", SHA256: digest, Size: 100},
		{Version: "24.10", Platform: "amd64", URL: "https://builds.example.com/ch-24.10.tar.xz", SHA256: digest, Size: 200},
		{Version: "24.8.1", Platform: "arm64", URL: "https://builds.example.com/ch-arm.tar.xz", SHA256: digest, Size: 300},
		{Version: "25.1", Platform: "amd64/ubuntu-26.04", URL: "https://builds.example.com/ch-25.1-u26.tar.xz", SHA256: digest, Size: 400},
	}
}

func newCatalog(src *source) (*modcatalog.Catalog, *base) {
	b := &base{}
	return &modcatalog.Catalog{
		Base: b, Modules: map[domain.Engine]modcatalog.Source{"clickhouse": src}, Resolve: publicIP,
		TTL: time.Minute, Now: time.Now,
	}, b
}

func TestLookup_モジュールのカタログからアーティファクトを返す(t *testing.T) {
	c, b := newCatalog(&source{entries: entries()})
	got, err := c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "24.8.1"}, "amd64")
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	a := got[0]
	if a.URL != "https://builds.example.com/ch-24.8.1.tar.xz" || a.SHA256 != digest || a.Size != 100 ||
		a.Ref != (domain.RuntimeRef{Engine: "clickhouse", Version: "24.8.1"}) || a.Arch != "amd64" || a.Part != 0 {
		t.Fatalf("%+v", a)
	}
	if len(b.looked) != 0 {
		t.Fatal("the core's catalog must not be asked for a module's engine")
	}
}

func TestLookup_ディストリビューション付きのプラットフォームは完全一致を優先し無ければアーキテクチャだけを見る(t *testing.T) {
	c, _ := newCatalog(&source{entries: entries()})
	got, err := c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "25.1"}, "amd64/ubuntu-26.04")
	if err != nil || got[0].Size != 400 {
		t.Fatalf("%v %v", got, err)
	}
	got, err = c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "24.8.1"}, "amd64/ubuntu-26.04")
	if err != nil || got[0].Size != 100 || got[0].Arch != "amd64/ubuntu-26.04" {
		t.Fatalf("falls back to the architecture: %v %v", got, err)
	}
}

func TestLookup_無いバージョンはErrRuntimeNotFound(t *testing.T) {
	c, _ := newCatalog(&source{entries: entries()})
	if _, err := c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "9.9"}, "amd64"); !errors.Is(err, domain.ErrRuntimeNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLookup_モジュール以外のエンジンはコアのカタログに任せる(t *testing.T) {
	c, b := newCatalog(&source{entries: entries()})
	got, err := c.Lookup(domain.RuntimeRef{Engine: "mysql", Version: "8.4.13"}, "amd64")
	if err != nil || got[0].URL != "https://core/x" || len(b.looked) != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestLookup_安全でないURLは返さない(t *testing.T) {
	for _, url := range []string{"http://builds.example.com/x", "https://127.0.0.1/x", "https://user:pw@builds.example.com/x"} {
		src := &source{entries: []domain.CatalogEntry{{Version: "1.0", Platform: "amd64", URL: url, SHA256: digest, Size: 1}}}
		c, _ := newCatalog(src)
		if _, err := c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "1.0"}, "amd64"); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("%s: err = %v", url, err)
		}
	}
}

func TestLookup_名前解決が内部アドレスを返すURLは返さない(t *testing.T) {
	c, _ := newCatalog(&source{entries: entries()})
	c.Resolve = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.5")}, nil }
	if _, err := c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "24.8.1"}, "amd64"); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Fatalf("err = %v", err)
	}
}

func TestLookup_ダイジェストが不正なエントリは返さない(t *testing.T) {
	src := &source{entries: []domain.CatalogEntry{{Version: "1.0", Platform: "amd64", URL: "https://builds.example.com/x", SHA256: "xyz", Size: 1}}}
	c, _ := newCatalog(src)
	_, err := c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "1.0"}, "amd64")
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v", err)
	}
}

func TestLookup_モジュールの失敗はそのまま返す(t *testing.T) {
	c, _ := newCatalog(&source{err: errors.New("module down")})
	if _, err := c.Lookup(domain.RuntimeRef{Engine: "clickhouse", Version: "1.0"}, "amd64"); err == nil || !strings.Contains(err.Error(), "module down") {
		t.Fatalf("err = %v", err)
	}
}

func TestAvailable_プラットフォームで絞り新しい順に並べる(t *testing.T) {
	c, _ := newCatalog(&source{entries: entries()})
	got := c.Available("clickhouse", "amd64")
	want := []domain.RuntimeRef{{Engine: "clickhouse", Version: "24.10"}, {Engine: "clickhouse", Version: "24.8.1"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v", got)
	}
	if got := c.Available("mysql", "amd64"); len(got) != 1 {
		t.Fatalf("the core's catalog answers for mysql: %v", got)
	}
	bad, _ := newCatalog(&source{err: errors.New("down")})
	if got := bad.Available("clickhouse", "amd64"); len(got) != 0 {
		t.Fatalf("a failing module offers nothing: %v", got)
	}
}

func TestLookup_カタログはTTLの間だけ使い回す(t *testing.T) {
	src := &source{entries: entries()}
	c, _ := newCatalog(src)
	now := time.Unix(1000, 0)
	c.Now = func() time.Time { return now }
	ref := domain.RuntimeRef{Engine: "clickhouse", Version: "24.8.1"}
	_, _ = c.Lookup(ref, "amd64")
	_, _ = c.Lookup(ref, "amd64")
	if src.calls != 1 {
		t.Fatalf("module asked %d times within the TTL", src.calls)
	}
	now = now.Add(2 * time.Minute)
	_, _ = c.Lookup(ref, "amd64")
	if src.calls != 2 {
		t.Fatalf("module asked %d times after the TTL", src.calls)
	}
}

type recCache struct{ got []domain.Artifact }

func (r *recCache) Get(_ context.Context, a domain.Artifact) (string, error) {
	r.got = append(r.got, a)
	return "/cache/" + a.SHA256, nil
}

func TestCache_モジュールのエンジンだけ公開アドレス限定のキャッシュで取る(t *testing.T) {
	core, external := &recCache{}, &recCache{}
	c := modcatalog.Cache{Core: core, External: external, Engines: func(e domain.Engine) bool { return e == "clickhouse" }}
	if _, err := c.Get(context.Background(), domain.Artifact{Ref: domain.RuntimeRef{Engine: "clickhouse", Version: "1"}, SHA256: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), domain.Artifact{Ref: domain.RuntimeRef{Engine: "mysql", Version: "8.4.13"}, SHA256: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(external.got) != 1 || external.got[0].SHA256 != "a" || len(core.got) != 1 || core.got[0].SHA256 != "b" {
		t.Fatalf("external %v core %v", external.got, core.got)
	}
}
