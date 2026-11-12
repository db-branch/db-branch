package tlscert_test

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

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
