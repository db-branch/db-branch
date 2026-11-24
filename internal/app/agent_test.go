package app_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/app"
	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/infrastructure/tlscert"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

func TestRunAgent_ソケットでAPIを出し止める指示で終わる(t *testing.T) {
	c := cfg(t)
	c.Socket = filepath.Join(t.TempDir(), "run", "agent.sock")
	c.Console.Listen = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunAgent(ctx, c, app.AgentOptions{Version: "test"}) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(c.Socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket did not appear")
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := os.Stat(c.Socket)
	// Without the db-branch-admin group (a test machine) the socket is root's alone.
	if _, err := user.LookupGroup(app.AdminGroup); err != nil {
		if info.Mode().Perm() != 0o600 {
			t.Errorf("socket mode = %v, want root only without the %s group", info.Mode().Perm(), app.AdminGroup)
		}
	} else if info.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v", info.Mode().Perm())
	}
	var caps map[string]any
	if err := cli.NewSocketClient(c.Socket).Do(ctx, "GET", "/v1/capabilities", nil, &caps); err != nil || caps["version"] != "test" {
		t.Fatalf("capabilities = %v, %v", caps, err)
	}
	var list map[string]any
	err := cli.NewSocketClient(c.Socket).Do(ctx, "GET", "/v1/branches", nil, &list)
	if apiErr, ok := err.(*cli.APIError); !ok || apiErr.Code != "not_initialized" {
		t.Errorf("before init: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunAgent = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not stop")
	}
}

func TestRunAgent_待ち受けられなければエラー(t *testing.T) {
	c := cfg(t)
	c.Socket = filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c.Console.Listen = l.Addr().String()
	if err := app.RunAgent(context.Background(), c, app.AgentOptions{}); err == nil {
		t.Error("port in use: want error")
	}
	c.Console.Listen = "127.0.0.1:0"
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	c.Socket = filepath.Join(file, "agent.sock")
	if err := app.RunAgent(context.Background(), c, app.AgentOptions{}); err == nil {
		t.Error("socket under a file: want error")
	}
}

func TestAdvertiseHost_設定がなければループバック以外のIPv4(t *testing.T) {
	c := cfg(t)
	c.Advertise = "db.example"
	if got := app.AdvertiseHost(c); got != "db.example" {
		t.Errorf("configured = %q", got)
	}
	c.Advertise = ""
	got := app.AdvertiseHost(c)
	if ip := net.ParseIP(got); ip == nil || ip.IsLoopback() && got != "127.0.0.1" {
		t.Errorf("detected = %q", got)
	}
}

func TestRunAgent_初期化済みなら復旧と期限切れの見回りをする(t *testing.T) {
	c := cfg(t)
	c.Socket = filepath.Join(t.TempDir(), "agent.sock")
	c.Console.Listen = "127.0.0.1:0"
	if err := app.SetPool(c, "tank"); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := app.RunAgent(ctx, c, app.AgentOptions{
		Log:         slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil)),
		ExpireEvery: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "agent stopped") {
		t.Errorf("logs = %q", logs.String())
	}
}

func TestSystemNode_実機の部品を組み立てる(t *testing.T) {
	n := app.SystemNode(&strings.Builder{})
	if n.Storage == nil || n.ARC == nil || n.Restart == nil || n.InitialPasswordFile != "/etc/db-branch/initial-admin-password" {
		t.Errorf("node = %+v", n)
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestOutboundIP_既定経路のアドレスを返す(t *testing.T) {
	ip := app.OutboundIP()
	if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
		t.Errorf("OutboundIP = %q", ip)
	}
}

func TestRunAgent_起動時に既存ブランチ所有者を不変IDと表示名へ移行する(t *testing.T) {
	c := cfg(t)
	sockDir, err := os.MkdirTemp("", "dbb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	c.Socket = filepath.Join(sockDir, "a.sock")
	c.Console.Listen = "127.0.0.1:0"
	if err := app.SetPool(c, "tank"); err != nil {
		t.Fatal(err)
	}
	st := &filestore.Store{Path: filepath.Join(c.DataDir, "state.json")}
	t1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := st.PutToken(context.Background(), domain.APIToken{ID: "ci01", Name: "ci", Scope: domain.ScopeBranches, CreatedAt: t1}); err != nil {
		t.Fatal(err)
	}
	n, _ := domain.ParseName("feat")
	base, _ := domain.ParseName("prod")
	if err := st.PutBranch(context.Background(), domain.Branch{Name: n, Base: base, Status: domain.BranchPaused, Owner: "ci", CreatedAt: t1.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := app.RunAgent(ctx, c, app.AgentOptions{ExpireEvery: time.Hour}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBranch(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "token:ci01" || got.OwnerName != "ci" {
		t.Errorf("migrated branch = (%q, %q), want (token:ci01, ci)", got.Owner, got.OwnerName)
	}
}

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dbb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "a.sock")
}

func writeCertPair(t *testing.T, certFile, keyFile string, notBefore, notAfter time.Time, dnsName string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

var consoleAddrRe = regexp.MustCompile(`console=(\S+)`)

func startTestAgent(t *testing.T, c config.Config) (addr string, stop func()) {
	t.Helper()
	var logs strings.Builder
	sw := &syncWriter{w: &logs}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.RunAgent(ctx, c, app.AgentOptions{
			Version:     "test",
			Log:         slog.New(slog.NewTextHandler(sw, nil)),
			ExpireEvery: time.Hour,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		sw.mu.Lock()
		s := logs.String()
		sw.mu.Unlock()
		if m := consoleAddrRe.FindStringSubmatch(s); len(m) == 2 {
			addr = m[1]
			break
		}
		select {
		case err := <-done:
			t.Fatalf("agent exited early: %v (logs=%s)", err, s)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("timed out waiting for agent console; logs=%s", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return addr, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("RunAgent = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}
}

func TestRunAgent_コンソールはTLS1_3必須でTLS1_2を拒否し10年自己署名証明書を作り直す(t *testing.T) {
	c := cfg(t)
	c.Socket = shortSocket(t)
	c.Console.Listen = "127.0.0.1:0"

	// Seed a legacy 10-year self-signed certificate.
	tlsDir := filepath.Join(c.DataDir, "tls")
	writeCertPair(t, filepath.Join(tlsDir, "cert.pem"), filepath.Join(tlsDir, "key.pem"),
		time.Now().Add(-time.Hour), time.Now().AddDate(10, 0, 0), "legacy.local")

	addr, stop := startTestAgent(t, c)
	defer stop()

	// TLS 1.2 must be rejected.
	conn12, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	})
	if err == nil {
		conn12.Close()
		t.Fatal("expected TLS 1.2 handshake to fail")
	}

	// TLS 1.3 must succeed and present a regenerated <=825d certificate.
	conn13, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("TLS 1.3 dial failed: %v", err)
	}
	state := conn13.ConnectionState()
	conn13.Close()
	if state.Version != tls.VersionTLS13 {
		t.Errorf("negotiated TLS version = %x, want TLS 1.3 (%x)", state.Version, tls.VersionTLS13)
	}
	leaf := state.PeerCertificates[0]
	if span := leaf.NotAfter.Sub(leaf.NotBefore); span > tlscert.MaxSelfSignedValidity {
		t.Errorf("regenerated cert validity = %v > %v", span, tlscert.MaxSelfSignedValidity)
	}
	if len(leaf.DNSNames) > 0 && leaf.DNSNames[0] == "legacy.local" {
		t.Error("expected legacy 10y cert to be replaced")
	}
}

func TestRunAgent_設定されたTLS証明書を使い再起動なしで更新を反映する(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "console.crt")
	keyFile := filepath.Join(dir, "console.key")
	writeCertPair(t, certFile, keyFile, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), "first.example")

	c := cfg(t)
	c.Socket = shortSocket(t)
	c.Console.Listen = "127.0.0.1:0"
	c.Console.TLSCert = certFile
	c.Console.TLSKey = keyFile

	addr, stop := startTestAgent(t, c)
	defer stop()

	probe := func() (dns string, pemBody string) {
		t.Helper()
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		dns = conn.ConnectionState().PeerCertificates[0].DNSNames[0]
		conn.Close()

		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		resp, err := client.Get("https://" + addr + "/tls.pem")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return dns, string(b)
	}

	dns1, pem1 := probe()
	want1, _ := os.ReadFile(certFile)
	if dns1 != "first.example" || pem1 != string(want1) {
		t.Fatalf("first probe = (%q, match=%v)", dns1, pem1 == string(want1))
	}

	// Broken/mismatched update while running: keep serving first.example on both handshake and /tls.pem.
	otherCert := filepath.Join(dir, "other.crt")
	otherKey := filepath.Join(dir, "other.key")
	writeCertPair(t, otherCert, otherKey, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), "broken.example")
	brokenBytes, _ := os.ReadFile(otherCert)
	_ = os.WriteFile(certFile, brokenBytes, 0o644)
	dnsMid, pemMid := probe()
	if dnsMid != "first.example" || pemMid != string(want1) {
		t.Fatalf("during mismatched update = (%q, match=%v), want first.example", dnsMid, pemMid == string(want1))
	}

	// Replace cert/key on disk without restarting agent.
	writeCertPair(t, certFile, keyFile, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), "second-hot.example")
	dns2, pem2 := probe()
	want2, _ := os.ReadFile(certFile)
	if dns2 != "second-hot.example" || pem2 != string(want2) {
		t.Fatalf("after hot-reload = (%q, match=%v)", dns2, pem2 == string(want2))
	}
}

func TestRunAgent_TLS証明書設定の不備は起動時にエラーにする(t *testing.T) {
	dir := t.TempDir()
	certA := filepath.Join(dir, "a.crt")
	keyA := filepath.Join(dir, "a.key")
	certB := filepath.Join(dir, "b.crt")
	keyB := filepath.Join(dir, "b.key")
	writeCertPair(t, certA, keyA, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), "a.example")
	writeCertPair(t, certB, keyB, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), "b.example")

	for _, tc := range []struct {
		name string
		cert string
		key  string
	}{
		{"only-cert", certA, ""},
		{"only-key", "", keyA},
		{"missing-cert", filepath.Join(dir, "nope.crt"), keyA},
		{"missing-key", certA, filepath.Join(dir, "nope.key")},
		{"mismatch", certA, keyB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg(t)
			c.Socket = shortSocket(t)
			c.Console.Listen = "127.0.0.1:0"
			c.Console.TLSCert = tc.cert
			c.Console.TLSKey = tc.key
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := app.RunAgent(ctx, c, app.AgentOptions{}); err == nil {
				t.Errorf("%s: expected startup error", tc.name)
			}
		})
	}
}
