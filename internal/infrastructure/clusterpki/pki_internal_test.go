package clusterpki

import (
	"crypto/ecdsa"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type failReader struct {
	failOnCall int
	calls      int
}

func (f *failReader) Read(p []byte) (int, error) {
	f.calls++
	if f.calls == f.failOnCall {
		return 0, errors.New("rng failure")
	}
	return rand.Read(p)
}

func TestClusterPKI_乱数失敗や鍵エンコード失敗を握り潰さずエラーにする(t *testing.T) {
	if _, err := serial(&failReader{failOnCall: 1}); err == nil {
		t.Error("serial should fail on rng error")
	}
	if _, err := keyPEM(&ecdsa.PrivateKey{}); err == nil {
		t.Error("keyPEM with empty key should fail")
	}

	counter := &failReader{}
	ca, err := initCA(t.TempDir(), counter)
	if err != nil {
		t.Fatal(err)
	}
	for call := 1; call <= counter.calls; call++ {
		if _, err := initCA(t.TempDir(), &failReader{failOnCall: call}); err == nil {
			t.Errorf("initCA(failOnCall=%d/%d) should fail", call, counter.calls)
		}
	}

	nodeCounter := &failReader{}
	if _, err := ca.issueNode(t.TempDir(), "n", nil, nodeCounter); err != nil {
		t.Fatal(err)
	}
	for call := 1; call <= nodeCounter.calls; call++ {
		if _, err := ca.issueNode(t.TempDir(), "n", nil, &failReader{failOnCall: call}); err == nil {
			t.Errorf("issueNode(failOnCall=%d/%d) should fail", call, nodeCounter.calls)
		}
	}

	// Cover ca.Save failure inside initCA when ca.pem does not exist yet.
	blockedDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(blockedDir, "ca-key.pem"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InitCA(blockedDir); err == nil {
		t.Error("InitCA with blocked ca-key.pem should fail in ca.Save")
	}
	var _ io.Reader = counter
}
