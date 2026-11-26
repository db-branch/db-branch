package remotehttp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/remotehttp"
)

func makeSelfSignedCert(t *testing.T, hosts []string, notBefore, notAfter time.Time) (tls.Certificate, []byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "db-branch-test"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return pair, certPEM, hex.EncodeToString(sum[:])
}

func startTLSServer(t *testing.T, cert tls.Certificate, minVer uint16, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestNormalizeSHA256_様々な表現を小文字64桁hexに正規化する(t *testing.T) {
	raw := strings.Repeat("AB:", 31) + "CD"
	got, err := remotehttp.NormalizeSHA256("SHA256 Fingerprint=" + raw)
	if err != nil || len(got) != 64 || !strings.HasSuffix(got, "cd") {
		t.Fatalf("got (%q, %v)", got, err)
	}
	got2, err := remotehttp.NormalizeSHA256("Sha256: " + strings.Repeat("01", 32))
	if err != nil || got2 != strings.Repeat("01", 32) {
		t.Fatalf("got2 (%q, %v)", got2, err)
	}
	for _, bad := range []string{"", "short", strings.Repeat("zz", 32)} {
		if _, err := remotehttp.NormalizeSHA256(bad); err == nil {
			t.Errorf("NormalizeSHA256(%q) = nil, want error", bad)
		}
	}
}

func TestValidateServerURL_HTTPS必須で不正URLを拒否する(t *testing.T) {
	if u, err := remotehttp.ValidateServerURL("https://10.0.0.1:7080/"); err != nil || u.String() != "https://10.0.0.1:7080" {
		t.Fatalf("valid url = (%v, %v)", u, err)
	}
	for _, bad := range []string{"", "http://10.0.0.1:7080", "https://", "https://user:pw@10.0.0.1:7080", "://bad"} {
		if _, err := remotehttp.ValidateServerURL(bad); err == nil {
			t.Errorf("ValidateServerURL(%q) = nil, want error", bad)
		}
	}
}

func TestProbeTLSAndClient_TOFUとCA検証とピン不一致とTLS13強制(t *testing.T) {
	now := time.Now()
	cert1, cert1PEM, fp1 := makeSelfSignedCert(t, []string{"127.0.0.1", "localhost"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	cert2, _, fp2 := makeSelfSignedCert(t, []string{"127.0.0.1"}, now.Add(-time.Hour), now.Add(24*time.Hour))

	srv := startTLSServer(t, cert1, tls.VersionTLS13, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/v1/branches", http.StatusFound)
			return
		}
		if r.URL.Path == "/html" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>login</html>"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer dbb_abc_secret" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"invalid token"}}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/err" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"already exists"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.0.0","node":"node1"}`))
	}))

	ctx := context.Background()

	// 1. No CA and no pin -> returns UntrustedCertError with fingerprint.
	_, err := remotehttp.ProbeTLS(ctx, srv.URL, "", "")
	var untrusted *remotehttp.UntrustedCertError
	if !errors.As(err, &untrusted) || untrusted.Cert.SHA256 != fp1 || untrusted.Error() == "" || untrusted.Unwrap() == nil {
		t.Fatalf("ProbeTLS untrusted = %v, want UntrustedCertError with %s", err, fp1)
	}

	// 2. Pin matches -> succeeds.
	info, err := remotehttp.ProbeTLS(ctx, srv.URL, "", fp1)
	if err != nil || info.SHA256 != fp1 {
		t.Fatalf("ProbeTLS pinned = (%+v, %v)", info, err)
	}

	// 3. Pin mismatch (e.g. rotated cert or MITM) -> returns PinnedCertMismatchError with actual URL.
	_, err = remotehttp.ProbeTLS(ctx, srv.URL, "", fp2)
	var mismatch *remotehttp.PinnedCertMismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != fp2 || mismatch.Actual.SHA256 != fp1 || !strings.Contains(mismatch.Error(), fp2) || !strings.Contains(mismatch.Error(), srv.URL) {
		t.Fatalf("ProbeTLS mismatch = %v", err)
	}

	// 4. --ca file trusts cert1, rejects cert2, and rejects bad/missing CA files.
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, cert1PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	badCAPath := filepath.Join(t.TempDir(), "badca.pem")
	if err := os.WriteFile(badCAPath, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := remotehttp.ProbeTLS(ctx, srv.URL, filepath.Join(t.TempDir(), "missing.pem"), ""); err == nil {
		t.Error("missing CA file should fail")
	}
	if _, err := remotehttp.ProbeTLS(ctx, srv.URL, badCAPath, ""); err == nil {
		t.Error("invalid CA PEM should fail")
	}
	if _, err := remotehttp.ProbeTLS(ctx, srv.URL, caPath, ""); err != nil {
		t.Fatalf("ProbeTLS with CA = %v", err)
	}
	srv2 := startTLSServer(t, cert2, tls.VersionTLS13, http.NotFoundHandler())
	if _, err := remotehttp.ProbeTLS(ctx, srv2.URL, caPath, ""); err == nil {
		t.Error("wrong CA should fail")
	}

	// 5. Expired cert is rejected even when pinned.
	expiredCert, _, expFP := makeSelfSignedCert(t, []string{"127.0.0.1"}, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	srvExp := startTLSServer(t, expiredCert, tls.VersionTLS13, http.NotFoundHandler())
	if _, err := remotehttp.ProbeTLS(ctx, srvExp.URL, "", expFP); err == nil {
		t.Error("expired cert must be rejected even if pinned")
	}

	// 6. TLS 1.2-only server is rejected (TLS 1.3 required).
	srv12 := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv12.TLS = &tls.Config{Certificates: []tls.Certificate{cert1}, MaxVersion: tls.VersionTLS12}
	srv12.StartTLS()
	t.Cleanup(srv12.Close)
	if _, err := remotehttp.ProbeTLS(ctx, srv12.URL, "", fp1); err == nil {
		t.Error("TLS 1.2 server must be rejected")
	}

	// 7. Client requests: Bearer auth, JSON decode, APIError, redirect rejection, non-JSON rejection.
	if _, err := remotehttp.ProbeTLS(ctx, "http://bad", "", ""); err == nil {
		t.Error("non-https ProbeTLS should fail")
	}
	if _, err := remotehttp.ProbeTLS(ctx, "https://127.0.0.1", "", "bad-pin"); err == nil {
		t.Error("bad pin ProbeTLS should fail")
	}
	if _, err := remotehttp.NewClient(remotehttp.Options{URL: "http://bad"}); err == nil {
		t.Error("non-https NewClient should fail")
	}
	if _, err := remotehttp.NewClient(remotehttp.Options{URL: srv.URL, PinnedSHA256: "bad"}); err == nil {
		t.Error("bad pin NewClient should fail")
	}
	c, err := remotehttp.NewClient(remotehttp.Options{
		URL:          srv.URL,
		Token:        "dbb_abc_secret",
		PinnedSHA256: fp1,
	})
	if err != nil {
		t.Fatalf("NewClient = %v", err)
	}
	var caps struct {
		Version string `json:"version"`
		Node    string `json:"node"`
	}
	if err := c.Do(ctx, "GET", "/v1/capabilities", nil, &caps); err != nil || caps.Version != "1.0.0" {
		t.Fatalf("GET capabilities = (%+v, %v)", caps, err)
	}
	if err := c.Do(ctx, "GET", "/v1/capabilities", nil, nil); err != nil {
		t.Fatalf("nil out Do = %v", err)
	}
	if err := c.Do(ctx, "GET", "/redirect", nil, &caps); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect should be rejected, got %v", err)
	}
	if err := c.Do(ctx, "GET", "/html", nil, &caps); err == nil {
		t.Error("HTML response for JSON decode should fail")
	}
	if err := c.Do(ctx, "GET", "/v1/capabilities", make(chan int), nil); err == nil {
		t.Error("unencodable body should fail")
	}
	if err := c.Do(ctx, "BAD METHOD", "/v1/capabilities", nil, nil); err == nil {
		t.Error("bad method should fail")
	}
	err = c.Do(ctx, "POST", "/v1/err", map[string]string{"k": "v"}, nil)
	var apiErr *remotehttp.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Code != "conflict" || !strings.Contains(apiErr.Error(), "conflict") {
		t.Errorf("APIError = %v", err)
	}
	st, code, msg := apiErr.APIErrorParts()
	if st != 409 || code != "conflict" || msg != "already exists" {
		t.Errorf("APIErrorParts = (%d, %q, %q)", st, code, msg)
	}
	// Plain 502 without JSON envelope still returns APIError with http_502.
	cBadTok, _ := remotehttp.NewClient(remotehttp.Options{URL: srv.URL, PinnedSHA256: fp1})
	err = cBadTok.Do(ctx, "GET", "/v1/capabilities", nil, nil)
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || apiErr.Code != "unauthenticated" {
		t.Errorf("401 APIError = %v", err)
	}
	srvPlain := startTLSServer(t, cert1, tls.VersionTLS13, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/no-ct" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":"1.0.0"}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	cPlain, _ := remotehttp.NewClient(remotehttp.Options{URL: srvPlain.URL, PinnedSHA256: fp1})
	if err := cPlain.Do(ctx, "GET", "/no-ct", nil, &caps); err == nil {
		t.Error("missing Content-Type header should fail JSON decode")
	}
	err = cPlain.Do(ctx, "GET", "/v1/capabilities", nil, nil)
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || apiErr.Code != "http_502" {
		t.Errorf("plain 502 APIError = %v", err)
	}
}
