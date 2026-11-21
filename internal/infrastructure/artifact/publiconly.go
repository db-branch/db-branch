package artifact

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// maxRedirects bounds the redirects one download follows.
const maxRedirects = 5

// PublicOnlyControl is a net.Dialer.Control that refuses to connect to an
// address that is not on the public internet. It runs on the address the
// dialer is about to connect to, after name resolution, so a name that
// resolves differently from the check (DNS rebinding) or a redirect cannot
// reach the node's own network.
func PublicOnlyControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		return fmt.Errorf("%w %q: not an address", domain.ErrUnsafeURL, address)
	}
	if !domain.IsPublicIP(ip) {
		return fmt.Errorf("%w: %s is not a public address", domain.ErrUnsafeURL, ip)
	}
	return nil
}

// PublicOnlyRedirect lets a download follow a few redirects, to https URLs
// without credentials whose host, when it is an address, is public.
func PublicOnlyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	u := req.URL
	if u.Scheme != "https" || u.User != nil {
		return fmt.Errorf("%w: redirect to %s", domain.ErrUnsafeURL, u.Redacted())
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !domain.IsPublicIP(ip) {
		return fmt.Errorf("%w: redirect to %s, which is not a public address", domain.ErrUnsafeURL, ip)
	}
	return nil
}

// PublicOnlyHostRedirect builds on PublicOnlyRedirect by also requiring every
// redirect target to match one of allowedHosts (case-insensitive, trailing dot
// normalized) and to use port 443 (explicit or default).
func PublicOnlyHostRedirect(allowedHosts []string) func(*http.Request, []*http.Request) error {
	allow := normalizeAllowedHosts(allowedHosts)
	return func(req *http.Request, via []*http.Request) error {
		if err := PublicOnlyRedirect(req, via); err != nil {
			return err
		}
		return checkAllowedHostAndPort(req.URL, allow)
	}
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

func normalizeAllowedHosts(hosts []string) map[string]bool {
	allow := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if n := normalizeHost(h); n != "" {
			allow[n] = true
		}
	}
	return allow
}

func checkAllowedHostAndPort(u *url.URL, allow map[string]bool) error {
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("%w: port %q is not allowed (only 443)", domain.ErrUnsafeURL, p)
	}
	host := normalizeHost(u.Hostname())
	if host == "" || !allow[host] {
		return fmt.Errorf("%w: host %q is not allowed", domain.ErrUnsafeURL, host)
	}
	return nil
}

// NewPublicOnlyClient is the HTTP client for downloads a module asked for:
// it connects only to public addresses and follows few, safe redirects.
func NewPublicOnlyClient() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, Control: PublicOnlyControl}
	return &http.Client{
		Transport:     &http.Transport{DialContext: dialer.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 60 * time.Second, Proxy: nil},
		CheckRedirect: PublicOnlyRedirect,
	}
}

// NewPublicOnlyHostClient builds on NewPublicOnlyClient by restricting both the
// initial request and every redirect to HTTPS on port 443 at a host in
// allowedHosts (case-insensitive, trailing dot normalized), while still
// blocking non-public IP connections via PublicOnlyControl.
func NewPublicOnlyHostClient(allowedHosts []string) *http.Client {
	c := NewPublicOnlyClient()
	allow := normalizeAllowedHosts(allowedHosts)
	c.Transport = &hostRestrictTransport{base: c.Transport, allow: allow}
	c.CheckRedirect = PublicOnlyHostRedirect(allowedHosts)
	return c
}

type hostRestrictTransport struct {
	base  http.RoundTripper
	allow map[string]bool
}

func (t *hostRestrictTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if u.Scheme != "https" || u.User != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrUnsafeURL, u.Redacted())
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !domain.IsPublicIP(ip) {
		return nil, fmt.Errorf("%w: %s is not a public address", domain.ErrUnsafeURL, ip)
	}
	if err := checkAllowedHostAndPort(u, t.allow); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// SystemResolver is the domain.Resolver of the operating system.
func SystemResolver(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, err
}
