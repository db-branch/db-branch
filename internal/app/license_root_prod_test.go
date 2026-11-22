//go:build !dev

package app

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"fmt"
	"strings"
	"testing"
)

//go:embed dev_license_root.pem
var devKeyForTest []byte

func TestLicenseRoot_非devビルドで本番ルート公開鍵がロードされ有効なPEMとしてパースできる(t *testing.T) {
	if licenseRoot() == nil {
		t.Fatal("release build has no licence root key")
	}
	if bytes.Equal(bytes.TrimSpace(licenseRootPEM), bytes.TrimSpace(devKeyForTest)) {
		t.Fatal("release build carries the development root key")
	}
}

// The module repository's licensing/prod_root.pem is the same key; a
// different key here would make every issued licence unverifiable.
func TestLicenseRoot_本番ルート公開鍵の指紋が固定されている(t *testing.T) {
	k := licenseRoot()
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	const want = "7916669c" // prefix of the SHA-256 of the DER public key
	if got := fmt.Sprintf("%x", sha256.Sum256(der)); !strings.HasPrefix(got, want) {
		t.Errorf("fingerprint %s, want prefix %s", got, want)
	}
}
