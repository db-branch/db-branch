package clusterpki_test

import (
	"crypto/tls"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/clusterpki"
)

func TestInitCA_CAを作り読み直せて指紋が安定している(t *testing.T) {
	dir := t.TempDir()
	ca, err := clusterpki.InitCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := clusterpki.LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ca.Fingerprint() != again.Fingerprint() || len(ca.Fingerprint()) != 64 {
		t.Errorf("fingerprints %q %q", ca.Fingerprint(), again.Fingerprint())
	}
	if info, _ := os.Stat(filepath.Join(dir, "ca-key.pem")); info.Mode().Perm() != 0o600 {
		t.Errorf("ca key mode %v", info.Mode().Perm())
	}
	if _, err := clusterpki.InitCA(dir); err == nil {
		t.Error("InitCA over an existing CA should fail")
	}
}

func TestPEMから読み込んだCAで同じ指紋になる(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	certPEM, keyPEM := ca.PEM()
	other, err := clusterpki.ParseCA(certPEM, keyPEM)
	if err != nil || other.Fingerprint() != ca.Fingerprint() {
		t.Errorf("ParseCA = %v %v", other, err)
	}
	if _, err := clusterpki.ParseCA([]byte("x"), keyPEM); err == nil {
		t.Error("bad cert: want error")
	}
	if _, err := clusterpki.ParseCA(certPEM, []byte("x")); err == nil {
		t.Error("bad key: want error")
	}
	dir := t.TempDir()
	if err := other.Save(dir); err != nil {
		t.Fatal(err)
	}
	if loaded, err := clusterpki.LoadCA(dir); err != nil || loaded.Fingerprint() != ca.Fingerprint() {
		t.Errorf("saved CA reload: %v", err)
	}
}

// mTLS works between two nodes issued by the same CA and fails for a stranger.
func TestIssueNode_同じCAのノード同士だけが相互認証で通信できる(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	a, err := ca.IssueNode(t.TempDir(), "node-a", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ca.IssueNode(t.TempDir(), "node-b", []string{"127.0.0.1"})
	stranger, _ := clusterpki.InitCA(t.TempDir())
	s, _ := stranger.IssueNode(t.TempDir(), "evil", []string{"127.0.0.1"})

	l, err := tls.Listen("tcp", "127.0.0.1:0", a.ServerTLS(true))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	talk := func(cfg *tls.Config) error {
		c, err := tls.Dial("tcp", l.Addr().String(), cfg)
		if err != nil {
			return err
		}
		defer c.Close()
		if _, err := c.Write([]byte("ping")); err != nil {
			return err
		}
		buf := make([]byte, 4)
		_, err = io.ReadFull(c, buf)
		return err
	}
	if err := talk(b.ClientTLS()); err != nil {
		t.Errorf("member: %v", err)
	}
	if err := talk(s.ClientTLS()); err == nil {
		t.Error("stranger should be rejected")
	}
}

func TestLoadNode_保存したノード証明書を読み直す(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	dir := t.TempDir()
	if _, err := ca.IssueNode(dir, "n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := clusterpki.LoadNode(dir, ca); err != nil {
		t.Fatal(err)
	}
	if _, err := clusterpki.LoadNode(t.TempDir(), ca); err == nil {
		t.Error("missing node cert: want error")
	}
}

func TestToken_指紋と秘密値を往復できる(t *testing.T) {
	tok := clusterpki.Token{Fingerprint: strings.Repeat("a", 64), Secret: "s3cr3t"}
	parsed, err := clusterpki.ParseToken(tok.String())
	if err != nil || parsed != tok {
		t.Errorf("ParseToken = %+v %v", parsed, err)
	}
	for _, bad := range []string{"", "dbb1.short.x", "xyz1." + strings.Repeat("a", 64) + ".s", "dbb1." + strings.Repeat("a", 64) + "."} {
		if _, err := clusterpki.ParseToken(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if clusterpki.HashSecret("x") == clusterpki.HashSecret("y") || len(clusterpki.NewSecret()) < 32 {
		t.Error("secret helpers")
	}
	if clusterpki.DefaultJoinTokenTTL != 24*time.Hour {
		t.Errorf("DefaultJoinTokenTTL = %v, want 24h", clusterpki.DefaultJoinTokenTTL)
	}
}

func TestPinnedTLS_指紋が一致するCAのサーバーだけを信用する(t *testing.T) {
	ca, _ := clusterpki.InitCA(t.TempDir())
	n, _ := ca.IssueNode(t.TempDir(), "n", []string{"127.0.0.1"})
	l, _ := tls.Listen("tcp", "127.0.0.1:0", n.ServerTLS(false))
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	dial := func(fp string) error {
		c, err := tls.DialWithDialer(&net.Dialer{}, "tcp", l.Addr().String(), clusterpki.PinnedTLS(fp))
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial(ca.Fingerprint()); err != nil {
		t.Errorf("right fingerprint: %v", err)
	}
	if err := dial(strings.Repeat("0", 64)); err == nil {
		t.Error("wrong fingerprint should fail")
	}
}

func Test保存先に書けなければエラー(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := clusterpki.InitCA(filepath.Join(file, "ca")); err == nil {
		t.Error("InitCA: want error")
	}
	if _, err := clusterpki.LoadCA(t.TempDir()); err == nil {
		t.Error("LoadCA of empty dir: want error")
	}
	ca, _ := clusterpki.InitCA(t.TempDir())
	if _, err := ca.IssueNode(filepath.Join(file, "node"), "n", nil); err == nil {
		t.Error("IssueNode: want error")
	}
	if err := ca.Save(filepath.Join(file, "x")); err == nil {
		t.Error("Save: want error")
	}
}

func Test証明書を示さないサーバーは信用しない(t *testing.T) {
	for _, cfg := range []*tls.Config{clusterpki.PinnedTLS(strings.Repeat("0", 64))} {
		if err := cfg.VerifyPeerCertificate(nil, nil); err == nil {
			t.Error("no certificates: want error")
		}
		if err := cfg.VerifyPeerCertificate([][]byte{[]byte("junk"), []byte("junk")}, nil); err == nil {
			t.Error("junk: want error")
		}
	}
	ca, _ := clusterpki.InitCA(t.TempDir())
	n, _ := ca.IssueNode(t.TempDir(), "n", nil)
	if err := n.ClientTLS().VerifyPeerCertificate(nil, nil); err == nil {
		t.Error("client: no certificates should fail")
	}
	if err := n.ClientTLS().VerifyPeerCertificate([][]byte{[]byte("junk")}, nil); err == nil {
		t.Error("client: junk should fail")
	}
	certPEM, _ := ca.PEM()
	if err := clusterpki.PinnedTLS(ca.Fingerprint()).VerifyPeerCertificate([][]byte{[]byte("junk"), pemDER(certPEM)}, nil); err == nil {
		t.Error("pinned: junk leaf should fail")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, 0o644)
	if _, err := clusterpki.LoadCA(dir); err == nil {
		t.Error("LoadCA without key: want error")
	}
	if _, err := clusterpki.ParseCA([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), nil); err == nil {
		t.Error("ParseCA garbage DER: want error")
	}
	_, keyPEM := ca.PEM()
	if _, err := clusterpki.ParseCA(certPEM, []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n")); err == nil || keyPEM == nil {
		t.Error("ParseCA garbage key: want error")
	}
	_ = os.MkdirAll(filepath.Join(dir, "x", "node-key.pem", "y"), 0o700)
	if _, err := ca.IssueNode(filepath.Join(dir, "x"), "n", nil); err == nil {
		t.Error("IssueNode into a blocked path: want error")
	}
}

func pemDER(p []byte) []byte {
	b, _ := pem.Decode(p)
	return b.Bytes
}
