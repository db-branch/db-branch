package notifier_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/notifier"
	"github.com/db-branch/db-branch/internal/usecase/notify"
)

var n = notify.Notice{Event: "branch.expiring", Level: "warning", Subject: "x", Text: "Branch x expires in 30m.", Time: time.Unix(0, 0).UTC(), Owner: "a@x.io"}

type got struct {
	header http.Header
	body   []byte
}

func server(t *testing.T, status int) (*httptest.Server, *[]got) {
	var all []got
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		all = append(all, got{r.Header.Clone(), b})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &all
}

func TestSlack_文面を送る(t *testing.T) {
	srv, all := server(t, 200)
	s := notifier.Slack{URL: func() string { return "https://hooks.slack.com/services/T1/B2/tok" }, Client: newTestTLSClient(srv, false)}
	if err := s.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.Unmarshal((*all)[0].body, &body)
	if !strings.Contains(body["text"], "Branch x expires") || !strings.Contains(body["text"], ":warning:") {
		t.Errorf("%v", body)
	}
	if err := (notifier.Slack{URL: func() string { return "" }}).Send(context.Background(), n); err != nil {
		t.Errorf("off: %v", err)
	}
}

func TestWebhook_署名付きのJSONを送る(t *testing.T) {
	srv, all := server(t, 204)
	c := newTestTLSClient(srv, false)
	w := notifier.Webhook{URL: func() string { return "https://hooks.example.com/notify" }, Secret: func() string { return "s3" }, Client: c}
	if err := w.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	g := (*all)[0]
	mac := hmac.New(sha256.New, []byte("s3"))
	mac.Write(g.body)
	if g.header.Get("X-Db-Branch-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || g.header.Get("X-Db-Branch-Event") != "branch.expiring" {
		t.Errorf("headers %v", g.header)
	}
	var back notify.Notice
	if json.Unmarshal(g.body, &back) != nil || back.Owner != "a@x.io" {
		t.Errorf("body %s", g.body)
	}
	unsigned := notifier.Webhook{URL: func() string { return "https://hooks.example.com/notify" }, Secret: func() string { return "" }, Client: c}
	_ = unsigned.Send(context.Background(), n)
	if (*all)[1].header.Get("X-Db-Branch-Signature") != "" {
		t.Error("signed without a secret")
	}
}

func TestSend_送り先の失敗を伝える(t *testing.T) {
	srv, _ := server(t, 500)
	c := newTestTLSClient(srv, false)
	for _, s := range []notify.Sender{
		notifier.Slack{URL: func() string { return "https://hooks.slack.com/services/T1/B2/tok" }, Client: c},
		notifier.Webhook{URL: func() string { return "https://hooks.example.com/notify" }, Secret: func() string { return "" }, Client: c},
		notifier.Webhook{URL: func() string { return "http://127.0.0.1:1" }, Secret: func() string { return "" }},
		notifier.Webhook{URL: func() string { return "://bad" }, Secret: func() string { return "" }},
	} {
		if err := s.Send(context.Background(), n); err == nil {
			t.Errorf("%T: no error", s)
		}
	}
	if err := (notifier.Webhook{URL: func() string { return "" }}).Send(context.Background(), n); err != nil {
		t.Errorf("off: %v", err)
	}
}

func newTestTLSClient(srv *httptest.Server, allowPrivate bool) *http.Client {
	c := notifier.NewClient(allowPrivate)
	baseTr := srv.Client().Transport.(*http.Transport)
	tlsCfg := baseTr.TLSClientConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	tlsCfg.ServerName = "127.0.0.1"
	addr := srv.Listener.Addr().String()
	c.Transport = &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		Proxy: nil,
	}
	return c
}

func TestNotifier_HTTPスキームとメタデータIPとリダイレクトを拒否し失敗エラーに秘密URLを含めない(t *testing.T) {
	ctx := context.Background()

	// 1. Disallowed schemes, credentials, malformed URLs, loopback, metadata, link-local,
	//    and (by default) private IPs are all refused without leaking the secret URL path/query.
	badURLs := []string{
		"https://2130706433/SECRET_TOKEN",
		"https://0x7f.1/SECRET_TOKEN",
		"https://127.1/SECRET_TOKEN",
		"http://hooks.slack.com/services/T1/B2/SECRET_TOKEN?x=SECRET_Q",
		"HTTP://hooks.slack.com/services/T1/B2/SECRET_TOKEN",
		"ftp://hooks.example.com/SECRET_TOKEN",
		"file:///etc/passwd?k=SECRET_TOKEN",
		"://bad/SECRET_TOKEN",
		"https:///SECRET_TOKEN",
		"https://user:SECRET_PASS@hooks.slack.com/services/SECRET_TOKEN",
		"https://hooks.slack.com/services/SECRET_TOKEN\r\nX-Inject: 1",
		"https://hooks.slack.com/services/SECRET_TOKEN extra",
		`https://evil.com\@127.0.0.1/SECRET_TOKEN`,
		"https://hooks.slack.com:0/SECRET_TOKEN",
		"https://hooks.slack.com:99999/SECRET_TOKEN",
		"https://localhost/SECRET_TOKEN",
		"https://localhost./SECRET_TOKEN",
		"https://localhost../SECRET_TOKEN",
		"https://sub.localhost/SECRET_TOKEN",
		"https://127.0.0.1/SECRET_TOKEN",
		"https://127.0.0.1../SECRET_TOKEN",
		"https://[::1]/SECRET_TOKEN",
		"https://[::1%25lo]/SECRET_TOKEN",
		"https://169.254.169.254/latest/meta-data/SECRET_TOKEN",
		"https://169.254.169.254../latest/meta-data/SECRET_TOKEN",
		"https://[fe80::1]/SECRET_TOKEN",
		"https://[fe80::1%25eth0]/SECRET_TOKEN",
		"https://[fd00:ec2::254]/latest/meta-data/SECRET_TOKEN",
		"https://0.0.0.0/SECRET_TOKEN",
		"https://[::ffff:127.0.0.1]/SECRET_TOKEN",
		"https://[::ffff:7f00:1]/SECRET_TOKEN",
		"https://[::ffff:169.254.169.254]/SECRET_TOKEN",
		"https://10.0.0.1/SECRET_TOKEN",
		"https://172.16.0.1/SECRET_TOKEN",
		"https://192.168.1.1/SECRET_TOKEN",
		"https://[fc00::1]/SECRET_TOKEN",
		"https://[fd12:3456::1]/SECRET_TOKEN",
	}
	for _, u := range badURLs {
		for _, snd := range []notify.Sender{
			notifier.Slack{URL: func() string { return u }},
			notifier.Webhook{URL: func() string { return u }, Secret: func() string { return "sec" }},
		} {
			err := snd.Send(ctx, n)
			if !errors.Is(err, domain.ErrUnsafeURL) {
				t.Errorf("%T(%q): err = %v, want ErrUnsafeURL", snd, u, err)
				continue
			}
			for _, leak := range []string{"SECRET_TOKEN", "SECRET_Q", "SECRET_PASS"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("%T(%q): error leaked secret %q: %v", snd, u, leak, err)
				}
			}
		}
	}

	// 2. AllowPrivateIPs=true permits RFC1918 and IPv6 ULA, while still blocking
	//    loopback, link-local/metadata (including AWS IMDSv6 fd00:ec2::254), unspecified, CGNAT, multicast, etc.
	alwaysBlocked := []string{
		"127.0.0.1:443", "[::1]:443", "169.254.169.254:443", "169.254.0.1:443",
		"[fe80::1]:443", "[fd00:ec2::254]:443", "0.0.0.0:443", "[::]:443",
		"[::ffff:127.0.0.1]:443", "[::ffff:169.254.169.254]:443", "[::10.0.0.1]:443",
		"100.64.0.1:443", "224.0.0.1:443", "nonsense", "example.com:443", "",
	}
	for _, allowPrivate := range []bool{false, true} {
		ctrl := notifier.Control(allowPrivate)
		for _, addr := range alwaysBlocked {
			if err := ctrl("tcp", addr, nil); !errors.Is(err, domain.ErrUnsafeURL) {
				t.Errorf("Control(%v)(%q) = %v, want ErrUnsafeURL", allowPrivate, addr, err)
			}
		}
		for _, addr := range []string{"93.184.216.34:443", "[2606:4700:4700::1111]:443"} {
			if err := ctrl("tcp", addr, nil); err != nil {
				t.Errorf("Control(%v)(%q) = %v, want nil", allowPrivate, addr, err)
			}
		}
	}
	privateAddrs := []string{"10.0.0.1:443", "172.16.0.1:443", "192.168.1.1:443", "[::ffff:10.0.0.1]:443", "[fc00::1]:443", "[fd12:3456::1]:443"}
	for _, addr := range privateAddrs {
		if err := notifier.Control(false)("tcp", addr, nil); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("Control(false)(%q) = %v, want ErrUnsafeURL", addr, err)
		}
		if err := notifier.Control(true)("tcp", addr, nil); err != nil {
			t.Errorf("Control(true)(%q) = %v, want nil", addr, err)
		}
	}
	for _, u := range []string{"https://10.0.0.1/hook", "https://172.16.0.1/hook", "https://192.168.1.1/hook", "https://[fd00::1]/hook"} {
		if err := notifier.ValidateURL(u, false); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("ValidateURL(%q, false) = %v, want ErrUnsafeURL", u, err)
		}
		if err := notifier.ValidateURL(u, true); err != nil {
			t.Errorf("ValidateURL(%q, true) = %v, want nil", u, err)
		}
	}
	for _, u := range []string{"HTTPS://hooks.slack.com./services/ok", "https://hooks.slack.com:8443/ok"} {
		if err := notifier.ValidateURL(u, false); err != nil {
			t.Errorf("ValidateURL(%q, false) = %v, want nil", u, err)
		}
	}
	for _, u := range []string{
		"https://127.0.0.1/hook", "https://169.254.169.254/latest/meta-data/",
		"https://[fe80::1]/hook", "https://[fe80::1%25eth0]/hook",
		"https://[fd00:ec2::254]/latest/meta-data/", "https://localhost/hook", "https://localhost../hook",
	} {
		if err := notifier.ValidateURL(u, true); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("ValidateURL(%q, true) = %v, want ErrUnsafeURL", u, err)
		}
	}

	// 3. NewClient sets Proxy=nil (ignores HTTPS_PROXY), timeouts, and refuses loopback at dial time via Dialer.Control.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	directClient := notifier.NewClient(false)
	tr := directClient.Transport.(*http.Transport)
	if tr.Proxy != nil || tr.TLSHandshakeTimeout <= 0 || tr.ResponseHeaderTimeout <= 0 {
		t.Errorf("transport hardened settings missing: proxy=%v tls=%v resp=%v", tr.Proxy != nil, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
	loopSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer loopSrv.Close()
	tr.TLSClientConfig = loopSrv.Client().Transport.(*http.Transport).TLSClientConfig
	if resp, err := directClient.Post(loopSrv.URL+"/SECRET_LOOP", "application/json", strings.NewReader("{}")); err == nil {
		resp.Body.Close()
		t.Fatal("NewClient reached loopback server")
	} else if !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("NewClient loopback dial: err = %v, want ErrUnsafeURL", err)
	}
	// Dial-time Control rejection through Slack.Send masks secret URL path/query.
	dialControlClient := notifier.NewClient(false)
	dialControlClient.Transport = &http.Transport{
		TLSClientConfig: loopSrv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Control: notifier.Control(false)}).DialContext(ctx, network, loopSrv.Listener.Addr().String())
		},
		Proxy: nil,
	}
	if err := (notifier.Slack{
		URL:    func() string { return "https://hooks.slack.com/services/T1/B2/SECRET_DIAL?k=SECRET_Q" },
		Client: dialControlClient,
	}).Send(ctx, n); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("Slack.Send dial Control reject: err = %v, want ErrUnsafeURL", err)
	} else if strings.Contains(err.Error(), "SECRET_DIAL") || strings.Contains(err.Error(), "SECRET_Q") {
		t.Errorf("Slack.Send dial Control reject leaked secret: %v", err)
	}
	// Default clients (public and private) respect context cancellation and never leak secret URL.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		allowPrivate bool
		raw          string
	}{
		{false, "https://hooks.slack.com/services/T1/B2/SECRET_CANCEL"},
		{true, "https://10.0.0.1/services/T1/B2/SECRET_CANCEL"},
	} {
		err := (notifier.Slack{URL: func() string { return tc.raw }, AllowPrivateIPs: tc.allowPrivate}).Send(canceledCtx, n)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("defaultClient(allowPrivate=%v): err = %v, want context.Canceled", tc.allowPrivate, err)
		}
		if strings.Contains(err.Error(), "SECRET_CANCEL") {
			t.Errorf("defaultClient error leaked secret: %v", err)
		}
	}

	// 4. All redirect statuses (301/302/303/307/308) are strictly refused (never followed)
	//    and neither original secret nor Location leaks; 302 without Location also fails cleanly.
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		followed := false
		redirSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/followed" {
				followed = true
				w.WriteHeader(200)
				return
			}
			http.Redirect(w, r, "https://hooks.slack.com/followed?k=SECRET_REDIRECT", code)
		}))
		redirClient := newTestTLSClient(redirSrv, false)
		err := (notifier.Slack{
			URL:    func() string { return "https://hooks.slack.com/services/T1/B2/SECRET_SLACK_URL" },
			Client: redirClient,
		}).Send(ctx, n)
		redirSrv.Close()
		if !errors.Is(err, domain.ErrUnsafeURL) {
			t.Fatalf("redirect %d: err = %v, want ErrUnsafeURL", code, err)
		}
		if followed {
			t.Errorf("redirect %d target was followed", code)
		}
		for _, leak := range []string{"SECRET_SLACK_URL", "SECRET_REDIRECT", "/services/T1/B2"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("redirect %d error leaked %q: %v", code, leak, err)
			}
		}
	}
	noLocSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer noLocSrv.Close()
	if err := (notifier.Slack{
		URL:    func() string { return "https://hooks.slack.com/services/T1/B2/SECRET_NOLOC" },
		Client: newTestTLSClient(noLocSrv, false),
	}).Send(ctx, n); err == nil || strings.Contains(err.Error(), "SECRET_NOLOC") {
		t.Errorf("302 without Location: err = %v", err)
	}

	// 5. Nil-inner *url.Error never leaks its URL field.
	nilInnerClient := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Post", URL: "https://hooks.slack.com/services/T1/B2/SECRET_NIL_ERR", Err: nil}
		}),
	}
	if err := (notifier.Slack{
		URL:    func() string { return "https://hooks.slack.com/services/T1/B2/SECRET_NIL_ERR" },
		Client: nilInnerClient,
	}).Send(ctx, n); err == nil || strings.Contains(err.Error(), "SECRET_NIL_ERR") {
		t.Errorf("nil-inner url.Error leaked secret: %v", err)
	}

	// 6. HTTP 500 and network errors never include the secret URL path or query.
	errSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	errClient := newTestTLSClient(errSrv, false)
	secretURL := "https://hooks.slack.com/services/T000/B000/SUPER_SECRET_TOKEN?token=QUERY_SECRET"
	err := (notifier.Slack{URL: func() string { return secretURL }, Client: errClient}).Send(ctx, n)
	if err == nil {
		t.Fatal("500 response: want error")
	}
	if !strings.Contains(err.Error(), "https://hooks.slack.com") || !strings.Contains(err.Error(), "500") {
		t.Errorf("500 error should keep host and status: %v", err)
	}
	for _, leak := range []string{"SUPER_SECRET_TOKEN", "QUERY_SECRET", "T000", "B000"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("500 error leaked %q: %v", leak, err)
		}
	}

	// Transport failure (closed server) also masks the secret URL.
	errSrv.Close()
	err = (notifier.Webhook{
		URL:    func() string { return secretURL },
		Secret: func() string { return "hmac-secret" },
		Client: errClient,
	}).Send(ctx, n)
	if err == nil {
		t.Fatal("closed server: want error")
	}
	for _, leak := range []string{"SUPER_SECRET_TOKEN", "QUERY_SECRET", "T000", "B000", "hmac-secret"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("transport error leaked %q: %v", leak, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestValidateURL_正規形でないIPv4リテラルを保存時に拒否する(t *testing.T) {
	for _, u := range []string{"https://2130706433/x", "https://0x7f.1/x", "https://127.1/x", "https://0177.0.0.1/x"} {
		if err := notifier.ValidateURL(u, true); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("ValidateURL(%q) = %v, want ErrUnsafeURL", u, err)
		}
	}
	if err := notifier.ValidateURL("https://hooks.slack.com/services/x", false); err != nil {
		t.Errorf("通常のホスト名は通る: %v", err)
	}
}
