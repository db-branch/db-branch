// Package remotehttp provides the TLS 1.3 HTTPS client with CA and TOFU pinning.
package remotehttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// APIError is an error envelope returned by the remote agent.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (%s)", e.Message, e.Code) }

// APIErrorParts exposes the HTTP status, code, and message for interface adapters.
func (e *APIError) APIErrorParts() (int, string, string) {
	return e.Status, e.Code, e.Message
}

// CertInfo summarizes a server leaf certificate.
type CertInfo struct {
	Subject     string
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
	SANs        []string
	SHA256      string
	SANMismatch bool
}

// UntrustedCertError indicates the server certificate is not trusted by system CAs.
type UntrustedCertError struct {
	Cert CertInfo
	Err  error
}

func (e *UntrustedCertError) Error() string {
	return fmt.Sprintf("untrusted TLS certificate (sha256:%s): %v", e.Cert.SHA256, e.Err)
}

func (e *UntrustedCertError) Unwrap() error { return e.Err }

// PinnedCertMismatchError indicates the server certificate changed from the pinned SHA-256.
type PinnedCertMismatchError struct {
	URL      string
	Expected string
	Actual   CertInfo
}

func (e *PinnedCertMismatchError) Error() string {
	target := e.URL
	if target == "" {
		target = "<url>"
	}
	return fmt.Sprintf(
		"WARNING: SERVER TLS CERTIFICATE CHANGED!\n"+
			"  Expected SHA-256: %s\n"+
			"  Received SHA-256: %s\n"+
			"  Subject:          %s\n"+
			"  Valid:            %s to %s\n"+
			"This may be a routine certificate renewal (every <= 825 days) or a man-in-the-middle attack.\n"+
			"Verify on the server with:\n"+
			"  openssl x509 -in /var/lib/db-branch/tls/cert.pem -noout -fingerprint -sha256\n"+
			"If the new fingerprint matches, re-trust with:\n"+
			"  db-branch login --server %s --trust-sha256 %s",
		e.Expected, e.Actual.SHA256, e.Actual.Subject,
		e.Actual.NotBefore.UTC().Format(time.RFC3339),
		e.Actual.NotAfter.UTC().Format(time.RFC3339),
		target, e.Actual.SHA256,
	)
}

// NormalizeSHA256 normalizes a SHA-256 fingerprint to lowercase 64-char hex.
func NormalizeSHA256(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	for _, pfx := range []string{"sha256 fingerprint=", "sha256:"} {
		if strings.HasPrefix(s, pfx) {
			s = strings.TrimSpace(s[len(pfx):])
			break
		}
	}
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, " ", "")
	if len(s) != 64 {
		return "", fmt.Errorf("invalid SHA-256 fingerprint %q: want 64 hex characters", raw)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("invalid SHA-256 fingerprint %q: %w", raw, err)
	}
	return s, nil
}

// ValidateServerURL validates that raw is an https:// URL with a host and no userinfo.
func ValidateServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("server URL %q must use https://", raw)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("server URL %q is missing a host", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("server URL %q must not embed credentials", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

// Options configures a remote HTTPS API client.
type Options struct {
	URL          string
	Token        string
	CACert       string
	PinnedSHA256 string
	Timeout      time.Duration
}

// Client calls the remote agent's /v1 API over TLS 1.3 HTTPS.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func buildTLSConfig(rawURL, host, caCertPath, expectedSHA256 string) (*tls.Config, error) {
	var pin string
	if expectedSHA256 != "" {
		var err error
		pin, err = NormalizeSHA256(expectedSHA256)
		if err != nil {
			return nil, err
		}
	}
	var roots *x509.CertPool
	if caCertPath != "" {
		pemBytes, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read CA certificate %s: %w", caCertPath, err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("CA certificate %s contains no valid PEM certificates", caCertPath)
		}
	}
	var serverName string
	if net.ParseIP(host) == nil {
		serverName = host
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: true, // Verified explicitly in VerifyConnection below.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no TLS certificate")
			}
			leaf := cs.PeerCertificates[0]
			info := extractCertInfo(leaf, host)
			now := time.Now()
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return fmt.Errorf("server certificate is expired or not yet valid (%s to %s)",
					leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339))
			}
			if pin != "" && caCertPath == "" {
				if info.SHA256 != pin {
					return &PinnedCertMismatchError{URL: rawURL, Expected: pin, Actual: info}
				}
				return nil
			}
			intermediates := x509.NewCertPool()
			for _, ic := range cs.PeerCertificates[1:] {
				intermediates.AddCert(ic)
			}
			_, verifyErr := leaf.Verify(x509.VerifyOptions{
				DNSName:       host,
				Intermediates: intermediates,
				Roots:         roots,
				CurrentTime:   now,
			})
			if verifyErr == nil {
				return nil
			}
			if caCertPath == "" && isUntrustedError(verifyErr) {
				return &UntrustedCertError{Cert: info, Err: verifyErr}
			}
			return verifyErr
		},
	}, nil
}

func isUntrustedError(err error) bool {
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	return errors.As(err, &ua) || errors.As(err, &he)
}

func extractCertInfo(leaf *x509.Certificate, host string) CertInfo {
	sum := sha256.Sum256(leaf.Raw)
	sans := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	return CertInfo{
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		SANs:        sans,
		SHA256:      hex.EncodeToString(sum[:]),
		SANMismatch: leaf.VerifyHostname(host) != nil,
	}
}

// ProbeTLS performs a TLS 1.3 handshake to rawURL and validates the certificate.
func ProbeTLS(ctx context.Context, rawURL, caCertPath, expectedSHA256 string) (CertInfo, error) {
	u, err := ValidateServerURL(rawURL)
	if err != nil {
		return CertInfo{}, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	tlsCfg, err := buildTLSConfig(u.String(), u.Hostname(), caCertPath, expectedSHA256)
	if err != nil {
		return CertInfo{}, err
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: tlsCfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return CertInfo{}, err
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	return extractCertInfo(state.PeerCertificates[0], u.Hostname()), nil
}

// NewClient constructs a remote HTTPS client enforcing TLS 1.3 and no redirects.
func NewClient(opts Options) (*Client, error) {
	u, err := ValidateServerURL(opts.URL)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := buildTLSConfig(u.String(), u.Hostname(), opts.CACert, opts.PinnedSHA256)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	tr := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tlsCfg,
	}
	hc := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("HTTP redirects are not allowed")
		},
	}
	return &Client{baseURL: u.String(), token: opts.Token, http: hc}, nil
}

// Do sends an API request and decodes the JSON response into out (when non-nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = json.Unmarshal(raw, &env)
		msg := env.Error.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		code := env.Error.Code
		if code == "" {
			code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Code: code, Message: msg}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(ct), "application/json") {
		return fmt.Errorf("unexpected response Content-Type %q (expected application/json)", ct)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
