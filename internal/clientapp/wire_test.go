package clientapp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/clientapp"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

func TestConfigure_リモートフックを構築し疎通できる(t *testing.T) {
	var app cli.App
	clientapp.Configure(&app)
	if app.NewClientStores == nil || app.RemoteClient == nil || app.ProbeTLS == nil {
		t.Fatal("Configure must set all remote hooks")
	}

	if _, _, err := app.NewClientStores(""); err != nil {
		t.Fatalf("NewClientStores(\"\") = %v", err)
	}
	if st, sec, err := app.NewClientStores(filepath.Join(t.TempDir(), "client.toml")); err != nil || st == nil || sec == nil {
		t.Fatalf("NewClientStores(custom) = (%v, %v, %v)", st, sec, err)
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, _, err := app.NewClientStores(""); err == nil {
		t.Error("NewClientStores(\"\") without HOME/XDG should fail")
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, _ := tls.X509KeyPair(certPEM, keyPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.0.0"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()

	sum, err := app.ProbeTLS(context.Background(), srv.URL, "", "")
	var untrusted *cli.UntrustedCertError
	if !errors.As(err, &untrusted) || sum.SHA256 == "" {
		t.Fatalf("ProbeTLS untrusted = (%+v, %v)", sum, err)
	}
	if _, err := app.ProbeTLS(context.Background(), "http://bad", "", ""); err == nil {
		t.Error("invalid URL ProbeTLS should fail")
	}
	if _, err := app.ProbeTLS(context.Background(), srv.URL, "", sum.SHA256); err != nil {
		t.Fatalf("ProbeTLS pinned = %v", err)
	}

	c, err := app.RemoteClient(cli.RemoteOptions{URL: srv.URL, PinnedSHA256: sum.SHA256})
	if err != nil {
		t.Fatalf("RemoteClient = %v", err)
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := c.Do(context.Background(), "GET", "/v1/capabilities", nil, &out); err != nil || out.Version != "1.0.0" {
		t.Fatalf("Do = (%+v, %v)", out, err)
	}
}
