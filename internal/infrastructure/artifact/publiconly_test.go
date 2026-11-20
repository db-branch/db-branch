package artifact_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
