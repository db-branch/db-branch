// Package notifier delivers notices to Slack and to webhooks.
package notifier

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/notify"
)

var (
	publicClient  = NewClient(false)
	privateClient = NewClient(true)
	// awsIMDSv6 is AWS EC2's IPv6 instance metadata endpoint (inside ULA fc00::/7).
	// It must stay blocked even when allow_private_ips is enabled.
	awsIMDSv6 = net.ParseIP("fd00:ec2::254")
)

func defaultClient(allowPrivate bool) *http.Client {
	if allowPrivate {
		return privateClient
	}
	return publicClient
}

func allowedIP(ip net.IP, allowPrivate bool) bool {
	if ip == nil || ip.Equal(awsIMDSv6) {
		return false
	}
	if domain.IsPublicIP(ip) {
		return true
	}
	if !allowPrivate {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.IsPrivate()
	}
	return len(ip) == net.IPv6len && ip.IsPrivate()
}

// Control returns a net.Dialer.Control that refuses connections to disallowed
// IP addresses after name resolution (preventing DNS rebinding and SSRF).
// Loopback and link-local addresses (including cloud metadata endpoints) are
// always refused; RFC 1918 and IPv6 ULA ranges are allowed only when
// allowPrivate is true.
func Control(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil {
			return fmt.Errorf("%w: invalid address", domain.ErrUnsafeURL)
		}
		if !allowedIP(ip, allowPrivate) {
			return fmt.Errorf("%w: %s is not an allowed address", domain.ErrUnsafeURL, ip)
		}
		return nil
	}
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return fmt.Errorf("%w: redirects are not allowed", domain.ErrUnsafeURL)
}

// NewClient returns an HTTP client hardened for outbound notifications:
// redirects are refused, proxies are disabled, and Dialer.Control enforces
// the IP policy after DNS resolution.
func NewClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: Control(allowPrivate)}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			Proxy:                 nil,
		},
		CheckRedirect: refuseRedirect,
	}
}

// ValidateURL checks that raw is an HTTPS notification URL without credentials
// and does not target localhost, loopback, link-local, or (unless allowPrivate
// is true) private network IP literals. Errors never echo the URL path or query.
func ValidateURL(raw string, allowPrivate bool) error {
	_, err := parseAndValidateURL(raw, allowPrivate)
	return err
}

func parseAndValidateURL(raw string, allowPrivate bool) (*url.URL, error) {
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\\' {
			return nil, fmt.Errorf("%w: contains a space, backslash or control character", domain.ErrUnsafeURL)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL", domain.ErrUnsafeURL)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%w: only https is allowed", domain.ErrUnsafeURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: credentials in the URL are not allowed", domain.ErrUnsafeURL)
	}
	rawHost := strings.ToLower(u.Hostname())
	if strings.Contains(rawHost, "%") {
		return nil, fmt.Errorf("%w: invalid host", domain.ErrUnsafeURL)
	}
	host := strings.TrimSuffix(rawHost, ".")
	if host == "" || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return nil, fmt.Errorf("%w: invalid host", domain.ErrUnsafeURL)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%w: invalid port", domain.ErrUnsafeURL)
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("%w: localhost is not allowed", domain.ErrUnsafeURL)
	}
	if ip := net.ParseIP(host); ip != nil && !allowedIP(ip, allowPrivate) {
		return nil, fmt.Errorf("%w: %s is not an allowed address", domain.ErrUnsafeURL, ip)
	}
	return u, nil
}

func unwrapURLError(err error) error {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) {
			return err
		}
		if ue.Err == nil {
			return errors.New("request failed")
		}
		err = ue.Err
	}
}

func post(ctx context.Context, rawURL string, allowPrivate bool, c *http.Client, body []byte, header map[string]string) error {
	u, err := parseAndValidateURL(rawURL, allowPrivate)
	if err != nil {
		return err
	}
	if c == nil {
		c = defaultClient(allowPrivate)
	}
	// Keep only scheme://host in error messages so secret tokens in path/query never leak.
	target := u.Scheme + "://" + u.Host
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("post %s: invalid request", target)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("post %s: %w", target, unwrapURLError(err))
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s", target, resp.Status)
	}
	return nil
}

var icons = map[string]string{"info": ":information_source:", "warning": ":warning:", "error": ":rotating_light:"}

// Slack posts to an incoming webhook (URL "" turns it off).
type Slack struct {
	URL             func() string
	AllowPrivateIPs bool
	Client          *http.Client
}

func (s Slack) Send(ctx context.Context, n notify.Notice) error {
	u := s.URL()
	if u == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"text": icons[n.Level] + " *db-branch* " + n.Text})
	return post(ctx, u, s.AllowPrivateIPs, s.Client, b, nil)
}

// Webhook posts each notice as JSON, signed with Secret when set:
// X-Db-Branch-Signature: sha256=<hex HMAC-SHA256 of the body>.
type Webhook struct {
	URL             func() string
	Secret          func() string
	AllowPrivateIPs bool
	Client          *http.Client
}

func (w Webhook) Send(ctx context.Context, n notify.Notice) error {
	u := w.URL()
	if u == "" {
		return nil
	}
	b, _ := json.Marshal(n)
	h := map[string]string{"X-Db-Branch-Event": n.Event}
	if s := w.Secret(); s != "" {
		mac := hmac.New(sha256.New, []byte(s))
		mac.Write(b)
		h["X-Db-Branch-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return post(ctx, u, w.AllowPrivateIPs, w.Client, b, h)
}
