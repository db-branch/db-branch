package tlscert_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/tlscert"
)

func TestEnsure_なければ自己署名証明書を作り次からは再利用する(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	cert, key, err := tlscert.Ensure(dir, []string{"node1", "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "node1" || len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "192.0.2.10" {
		t.Errorf("SANs dns=%v ip=%v", leaf.DNSNames, leaf.IPAddresses)
	}
	if info, _ := os.Stat(key); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %v", info.Mode().Perm())
	}
	before, _ := os.ReadFile(cert)
	if _, _, err := tlscert.Ensure(dir, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cert)
	if string(before) != string(after) {
		t.Error("existing certificate should be kept")
	}
}

func TestEnsure_書けなければエラー(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, _, err := tlscert.Ensure(filepath.Join(file, "tls"), nil); err == nil {
		t.Error("want error")
	}
}

func TestEnsure_鍵や証明書を書けなければエラー(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "key.pem", "x"), 0o700)
	if _, _, err := tlscert.Ensure(dir, nil); err == nil {
		t.Error("key.pem is a directory: want error")
	}
	// A dangling cert.pem symlink is "missing" for Stat but cannot be written through.
	dir = t.TempDir()
	_ = os.Symlink(filepath.Join(dir, "no", "such", "cert.pem"), filepath.Join(dir, "cert.pem"))
	if _, _, err := tlscert.Ensure(dir, nil); err == nil {
		t.Error("unwritable cert.pem: want error")
	}
}

func TestEnsure_証明書だけあって鍵が欠けているか壊れていればエラー(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := tlscert.Ensure(dir, []string{"node1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tlscert.Ensure(dir, []string{"node1"}); err == nil {
		t.Error("key.pem missing: want error")
	}
	_ = os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600)
	if _, _, err := tlscert.Ensure(dir, []string{"node1"}); err == nil {
		t.Error("key.pem broken: want error")
	}
}

func writeTestCertPair(t *testing.T, certFile, keyFile string, notBefore, notAfter time.Time, dnsName string) {
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

func TestEnsure_自己署名証明書の有効期間は825日以下で825日超の既存証明書を作り直す(t *testing.T) {
	// 1. Newly generated certificate is <= 825 days.
	dir := filepath.Join(t.TempDir(), "tls")
	cert, key, err := tlscert.Ensure(dir, []string{"node1"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := leaf.NotAfter.Sub(leaf.NotBefore); got > tlscert.MaxSelfSignedValidity {
		t.Errorf("NotAfter - NotBefore = %v, want <= %v", got, tlscert.MaxSelfSignedValidity)
	}
	if leaf.NotAfter.After(time.Now().Add(tlscert.MaxSelfSignedValidity)) {
		t.Errorf("NotAfter = %v, want <= now + %v", leaf.NotAfter, tlscert.MaxSelfSignedValidity)
	}

	// 2. Legacy 10-year certificate (both fresh 10y and older 10y with <825d left) is regenerated.
	for _, tc := range []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
	}{
		{"fresh-10y", time.Now().Add(-time.Hour), time.Now().AddDate(10, 0, 0)},
		{"old-10y-span", time.Now().AddDate(-8, 0, 0), time.Now().AddDate(2, 0, 0)},
		{"future-over-825d", time.Now().Add(-24 * time.Hour), time.Now().Add(826 * 24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := filepath.Join(t.TempDir(), "tls")
			certPath, keyPath := filepath.Join(d, "cert.pem"), filepath.Join(d, "key.pem")
			writeTestCertPair(t, certPath, keyPath, tc.notBefore, tc.notAfter, "old-host")
			oldCert, _ := os.ReadFile(certPath)
			oldKey, _ := os.ReadFile(keyPath)

			gotCert, gotKey, err := tlscert.Ensure(d, []string{"new-host", "192.0.2.20"})
			if err != nil {
				t.Fatal(err)
			}
			newCert, _ := os.ReadFile(gotCert)
			newKey, _ := os.ReadFile(gotKey)
			if string(newCert) == string(oldCert) || string(newKey) == string(oldKey) {
				t.Fatal("expected >825d certificate and key to be regenerated")
			}
			pair, err := tls.LoadX509KeyPair(gotCert, gotKey)
			if err != nil {
				t.Fatal(err)
			}
			regenerated, _ := x509.ParseCertificate(pair.Certificate[0])
			if regenerated.NotAfter.Sub(regenerated.NotBefore) > tlscert.MaxSelfSignedValidity {
				t.Errorf("regenerated span = %v > %v", regenerated.NotAfter.Sub(regenerated.NotBefore), tlscert.MaxSelfSignedValidity)
			}
			if len(regenerated.DNSNames) != 1 || regenerated.DNSNames[0] != "new-host" {
				t.Errorf("regenerated SANs = %v, want [new-host]", regenerated.DNSNames)
			}
		})
	}

	// 3. Existing certificate <= 825 days is preserved unchanged.
	dKeep := filepath.Join(t.TempDir(), "tls")
	certKeep, keyKeep := filepath.Join(dKeep, "cert.pem"), filepath.Join(dKeep, "key.pem")
	writeTestCertPair(t, certKeep, keyKeep, time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour), "kept-host")
	beforeKeep, _ := os.ReadFile(certKeep)
	if _, _, err := tlscert.Ensure(dKeep, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	afterKeep, _ := os.ReadFile(certKeep)
	if string(beforeKeep) != string(afterKeep) {
		t.Error("existing <=825d certificate should not be regenerated")
	}

	// 4. Existing >825d cert with missing key.pem still returns error without replacing.
	dBroken := filepath.Join(t.TempDir(), "tls")
	certBroken, keyBroken := filepath.Join(dBroken, "cert.pem"), filepath.Join(dBroken, "key.pem")
	writeTestCertPair(t, certBroken, keyBroken, time.Now().Add(-time.Hour), time.Now().AddDate(10, 0, 0), "broken")
	_ = os.Remove(keyBroken)
	if _, _, err := tlscert.Ensure(dBroken, []string{"broken"}); err == nil {
		t.Error("10y cert with missing key.pem should still error")
	}
}

func TestResolve_指定があればそのパスを使い未指定なら自己署名を用意する(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "tls")
	if _, _, err := tlscert.Resolve("/tmp/cert.pem", "", fallback, nil); err == nil {
		t.Error("only certPath set: want error")
	}
	if _, _, err := tlscert.Resolve("", "/tmp/key.pem", fallback, nil); err == nil {
		t.Error("only keyPath set: want error")
	}

	gotCert, gotKey, err := tlscert.Resolve("/custom/cert.pem", "/custom/key.pem", fallback, []string{"node1"})
	if err != nil {
		t.Fatal(err)
	}
	if gotCert != "/custom/cert.pem" || gotKey != "/custom/key.pem" {
		t.Errorf("got = (%q, %q)", gotCert, gotKey)
	}
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Errorf("fallback dir should not be created when custom paths are given")
	}

	gotCert, gotKey, err = tlscert.Resolve("", "", fallback, []string{"node1"})
	if err != nil {
		t.Fatal(err)
	}
	if gotCert != filepath.Join(fallback, "cert.pem") || gotKey != filepath.Join(fallback, "key.pem") {
		t.Errorf("fallback = (%q, %q)", gotCert, gotKey)
	}
}

func TestReloader_起動時の不備はエラーにし稼働中の差し替えは無停止で反映する(t *testing.T) {
	dir := t.TempDir()
	certA := filepath.Join(dir, "a.crt")
	keyA := filepath.Join(dir, "a.key")
	certB := filepath.Join(dir, "b.crt")
	keyB := filepath.Join(dir, "b.key")
	writeTestCertPair(t, certA, keyA, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), "first.example")
	writeTestCertPair(t, certB, keyB, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour), "second-updated.example")

	// Startup error cases: empty paths, missing cert, missing key, mismatched keypair.
	if _, err := tlscert.NewReloader("", keyA, nil); err == nil {
		t.Error("empty cert path: want error")
	}
	if _, err := tlscert.NewReloader(filepath.Join(dir, "missing.crt"), keyA, nil); err == nil {
		t.Error("missing cert: want error")
	}
	if _, err := tlscert.NewReloader(certA, filepath.Join(dir, "missing.key"), nil); err == nil {
		t.Error("missing key: want error")
	}
	if _, err := tlscert.NewReloader(certA, keyB, nil); err == nil {
		t.Error("mismatched cert/key: want error")
	}
	if _, err := tlscert.NewReloader(t.TempDir(), keyA, nil); err == nil {
		t.Error("directory as cert: want error")
	}
	if _, err := tlscert.NewReloader(certA, t.TempDir(), nil); err == nil {
		t.Error("directory as key: want error")
	}

	// Symlink targets (like Let's Encrypt) start with A and swap to B.
	liveCert := filepath.Join(dir, "live.crt")
	liveKey := filepath.Join(dir, "live.key")
	if err := os.Symlink(certA, liveCert); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keyA, liveKey); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	rel, err := tlscert.NewReloader(liveCert, liveKey, log)
	if err != nil {
		t.Fatal(err)
	}

	getDNS := func(r *tlscert.Reloader) string {
		t.Helper()
		p, err := r.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(p.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return leaf.DNSNames[0]
	}

	if got := getDNS(rel); got != "first.example" {
		t.Fatalf("initial DNS = %q, want first.example", got)
	}
	wantA, _ := os.ReadFile(certA)
	if !bytes.Equal(rel.CertPEM(), wantA) {
		t.Fatal("initial CertPEM mismatch")
	}

	// Partial update: cert points to B while key still points to A -> keep serving A and log warning.
	_ = os.Remove(liveCert)
	if err := os.Symlink(certB, liveCert); err != nil {
		t.Fatal(err)
	}
	if got := getDNS(rel); got != "first.example" {
		t.Fatalf("on partial mismatch DNS = %q, want first.example", got)
	}
	if !strings.Contains(logs.String(), "reloading console TLS certificate") {
		t.Errorf("expected reload warning log, got %q", logs.String())
	}

	// Complete update: key now points to B -> reloads B without restart.
	_ = os.Remove(liveKey)
	if err := os.Symlink(keyB, liveKey); err != nil {
		t.Fatal(err)
	}
	if got := getDNS(rel); got != "second-updated.example" {
		t.Fatalf("after update DNS = %q, want second-updated.example", got)
	}
	wantB, _ := os.ReadFile(certB)
	if !bytes.Equal(rel.CertPEM(), wantB) {
		t.Fatal("updated CertPEM mismatch")
	}

	// Missing file during runtime keeps serving last valid cert (B).
	_ = os.Remove(liveCert)
	if got := getDNS(rel); got != "second-updated.example" {
		t.Fatalf("when file temporarily absent DNS = %q, want second-updated.example", got)
	}

	// Restore B -> error state resets; another missing-file error logs a new warning.
	if err := os.Symlink(certB, liveCert); err != nil {
		t.Fatal(err)
	}
	if got := getDNS(rel); got != "second-updated.example" {
		t.Fatalf("restored B DNS = %q, want second-updated.example", got)
	}
	logsBefore := strings.Count(logs.String(), "reloading console TLS certificate failed")
	_ = os.Remove(liveCert)
	if got := getDNS(rel); got != "second-updated.example" {
		t.Fatalf("second missing file DNS = %q, want second-updated.example", got)
	}
	if gotCount := strings.Count(logs.String(), "reloading console TLS certificate failed"); gotCount != logsBefore+1 {
		t.Errorf("expected new warning after restore+re-error: before=%d after=%d logs=%s", logsBefore, gotCount, logs.String())
	}
}

func TestReloader_同一mtimeかつ同一サイズでも内容変更を検知し並行呼び出しで安全(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	fixedTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeTestCertPair(t, certFile, keyFile, fixedTime, fixedTime.Add(30*24*time.Hour), "alpha.example")
	_ = os.Chtimes(certFile, fixedTime, fixedTime)
	_ = os.Chtimes(keyFile, fixedTime, fixedTime)

	rel, err := tlscert.NewReloader(certFile, keyFile, slog.New(slog.NewTextHandler(&syncSafeBuf{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := rel.GetCertificate(nil)
	leaf1, _ := x509.ParseCertificate(p1.Certificate[0])

	// Replace with same-length DNS name and restore exact same mtime.
	writeTestCertPair(t, certFile, keyFile, fixedTime, fixedTime.Add(30*24*time.Hour), "bravo.example")
	_ = os.Chtimes(certFile, fixedTime, fixedTime)
	_ = os.Chtimes(keyFile, fixedTime, fixedTime)
	info1, _ := os.Stat(certFile)
	_ = info1

	p2, _ := rel.GetCertificate(nil)
	leaf2, _ := x509.ParseCertificate(p2.Certificate[0])
	if leaf2.DNSNames[0] != "bravo.example" || bytes.Equal(leaf1.Raw, leaf2.Raw) {
		t.Fatalf("same-mtime/size reload failed: got DNS %q", leaf2.DNSNames[0])
	}

	// Concurrent GetCertificate and CertPEM calls while rotating files.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					p, err := rel.GetCertificate(nil)
					if err != nil || p == nil || len(p.Certificate) == 0 {
						t.Errorf("GetCertificate = (%v, %v)", p, err)
						return
					}
					if len(rel.CertPEM()) == 0 {
						t.Error("CertPEM returned empty")
						return
					}
				}
			}
		}()
	}
	for _, dns := range []string{"charl.example", "delta.example"} {
		writeTestCertPair(t, certFile, keyFile, fixedTime, fixedTime.Add(30*24*time.Hour), dns)
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

type syncSafeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncSafeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
