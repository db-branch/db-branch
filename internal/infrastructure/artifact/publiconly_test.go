package artifact_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
)

func TestPublicOnlyControl_非公開アドレスへの接続を拒否する(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "10.1.2.3:443", "169.254.169.254:80", "[::ffff:192.168.0.1]:443", "0.0.0.0:80"} {
		if err := artifact.PublicOnlyControl("tcp", addr, nil); err == nil {
			t.Errorf("%s was allowed", addr)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:4700:4700::1111]:443"} {
		if err := artifact.PublicOnlyControl("tcp", addr, nil); err != nil {
			t.Errorf("%s = %v", addr, err)
		}
	}
	for _, addr := range []string{"nonsense", "example.com:443", ""} { // only resolved addresses reach a dialer
		if err := artifact.PublicOnlyControl("tcp", addr, nil); err == nil {
			t.Errorf("%q was allowed", addr)
		}
	}
}

func TestNewPublicOnlyClient_ループバックのサーバーには繋がない(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer srv.Close()
	client := artifact.NewPublicOnlyClient()
	client.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig

	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("the client reached a loopback server")
	}
	if !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("err = %v", err)
	}
}

func TestPublicOnlyRedirect_https以外とプライベートな宛先とリダイレクトの行き過ぎを拒否する(t *testing.T) {
	req := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		return &http.Request{URL: u}
	}
	if err := artifact.PublicOnlyRedirect(req("https://cdn.example.com/x"), []*http.Request{req("https://example.com/")}); err != nil {
		t.Errorf("https redirect = %v", err)
	}
	for _, target := range []string{"http://cdn.example.com/x", "https://127.0.0.1/x", "https://[::1]/x", "https://169.254.169.254/x", "https://user:pw@cdn.example.com/x", "file:///etc/passwd"} {
		if err := artifact.PublicOnlyRedirect(req(target), []*http.Request{req("https://example.com/")}); err == nil {
			t.Errorf("redirect to %s was allowed", target)
		}
	}
	via := make([]*http.Request, 5)
	if err := artifact.PublicOnlyRedirect(req("https://cdn.example.com/x"), via); err == nil {
		t.Error("too many redirects were allowed")
	}
}

func TestSystemResolver_ホスト名のアドレスを返す(t *testing.T) {
	ips, err := artifact.SystemResolver(context.Background(), "localhost")
	if err != nil || len(ips) == 0 {
		t.Errorf("SystemResolver(localhost) = %v, %v", ips, err)
	}
	if _, err := artifact.SystemResolver(context.Background(), "no-such-host.invalid"); err == nil {
		t.Error("an unresolvable name resolved")
	}
}

func TestNewPublicOnlyClient_応答の来ない相手を待ち続けない(t *testing.T) {
	tr := artifact.NewPublicOnlyClient().Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout <= 0 || tr.TLSHandshakeTimeout <= 0 || tr.Proxy != nil {
		t.Errorf("transport = response header %v, tls %v, proxy set %v", tr.ResponseHeaderTimeout, tr.TLSHandshakeTimeout, tr.Proxy != nil)
	}
}

func TestPublicOnlyHostRedirect_許可ホストへのHTTPSリダイレクトのみ通す(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) = %v", raw, err)
		}
		return &http.Request{URL: u}
	}
	check := artifact.PublicOnlyHostRedirect([]string{"GitHub.com.", "objects.githubusercontent.com"})

	for _, target := range []string{
		"https://objects.githubusercontent.com/asset.tar.gz",
		"https://OBJECTS.GITHUBUSERCONTENT.COM./asset.tar.gz",
		"https://github.com:443/acme/repo/releases/download/v0.2.0/a.tar.gz",
	} {
		if err := check(req(target), []*http.Request{req("https://github.com/start")}); err != nil {
			t.Errorf("allowed redirect to %s = %v, want nil", target, err)
		}
	}

	// 5回目のリダイレクト（len(via) == 4）は許可、6回目（len(via) == 5）は拒否
	if err := check(req("https://objects.githubusercontent.com/ok"), make([]*http.Request, 4)); err != nil {
		t.Errorf("5th redirect = %v, want nil", err)
	}
	if err := check(req("https://objects.githubusercontent.com/too-many"), make([]*http.Request, 5)); err == nil {
		t.Error("6th redirect was allowed")
	}

	badTargets := map[string]string{
		"未許可ホスト":      "https://evil.example.com/asset.tar.gz",
		"サブドメイン偽装":    "https://sub.github.com/asset.tar.gz",
		"接尾辞偽装":       "https://github.com.evil.com/asset.tar.gz",
		"ホスト欠落":       "https:///asset.tar.gz",
		"非443ポート":     "https://objects.githubusercontent.com:8443/asset.tar.gz",
		"80番ポート":      "https://github.com:80/asset.tar.gz",
		"ユーザー情報付きURL": "https://user:pw@objects.githubusercontent.com/asset.tar.gz",
		"httpスキーム":    "http://objects.githubusercontent.com/asset.tar.gz",
		"ループバックIP":    "https://127.0.0.1/asset.tar.gz",
	}
	for name, target := range badTargets {
		if err := check(req(target), []*http.Request{req("https://github.com/start")}); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("%s (%s): err = %v, want ErrUnsafeURL", name, target, err)
		}
	}

	// 非公開IPが許可リストに入っていても拒否する
	checkWithPrivate := artifact.PublicOnlyHostRedirect([]string{"127.0.0.1"})
	if err := checkWithPrivate(req("https://127.0.0.1/x"), []*http.Request{req("https://github.com/")}); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("private IP in allowlist: err = %v, want ErrUnsafeURL", err)
	}

	// 空の許可リストはすべて拒否する
	checkEmpty := artifact.PublicOnlyHostRedirect(nil)
	if err := checkEmpty(req("https://github.com/x"), []*http.Request{req("https://github.com/")}); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("empty allowlist: err = %v, want ErrUnsafeURL", err)
	}
}

func TestNewPublicOnlyHostClient_プライベートIPと未許可ホストへの接続を拒否する(t *testing.T) {
	client := artifact.NewPublicOnlyHostClient([]string{"api.github.com", "objects.githubusercontent.com"})

	badInitial := map[string]string{
		"未許可ホスト":      "https://evil.example.com/x",
		"非443ポート":     "https://api.github.com:8443/x",
		"httpスキーム":    "http://api.github.com/x",
		"ユーザー情報付きURL": "https://user:pw@api.github.com/x",
		"プライベートIP":    "https://127.0.0.1/x",
	}
	for name, target := range badInitial {
		resp, err := client.Get(target)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s (%s) was allowed", name, target)
		}
		if !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("%s (%s): err = %v, want ErrUnsafeURL", name, target, err)
		}
	}

	// ループバックTLSサーバーへの接続も拒否する
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer srv.Close()
	if resp, err := client.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("the client reached a loopback server")
	} else if !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("loopback server err = %v, want ErrUnsafeURL", err)
	}

	// 127.0.0.1 が許可リストに入っていても非公開IPは拒否する
	ipClient := artifact.NewPublicOnlyHostClient([]string{"127.0.0.1"})
	if resp, err := ipClient.Get("https://127.0.0.1/x"); err == nil {
		resp.Body.Close()
		t.Fatal("private IP literal in allowlist was allowed")
	} else if !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("private IP literal err = %v, want ErrUnsafeURL", err)
	}

	// ホスト名が非公開アドレス(127.0.0.1 / ::1)に解決される場合は PublicOnlyControl で拒否される
	localClient := artifact.NewPublicOnlyHostClient([]string{"localhost"})
	if resp, err := localClient.Get("https://localhost/x"); err == nil {
		resp.Body.Close()
		t.Fatal("localhost was allowed to connect")
	} else if !errors.Is(err, domain.ErrUnsafeURL) || !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("localhost err = %v, want ErrUnsafeURL (not a public address)", err)
	}

	// CheckRedirect も許可ホスト制限を強制する
	req := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		return &http.Request{URL: u}
	}
	if err := client.CheckRedirect(req("https://objects.githubusercontent.com/a"), []*http.Request{req("https://api.github.com/r")}); err != nil {
		t.Errorf("allowed redirect = %v", err)
	}
	if err := client.CheckRedirect(req("https://evil.example.com/a"), []*http.Request{req("https://api.github.com/r")}); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("disallowed redirect err = %v, want ErrUnsafeURL", err)
	}
}
