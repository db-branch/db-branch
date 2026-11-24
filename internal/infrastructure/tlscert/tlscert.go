// Package tlscert creates the console's self-signed certificate.
package tlscert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxSelfSignedValidity is the longest lifetime allowed for generated
// self-signed console certificates (825 days, matching modern TLS client limits).
const MaxSelfSignedValidity = 825 * 24 * time.Hour

// Ensure returns cert.pem and key.pem in dir, creating a self-signed ECDSA
// certificate valid for at most 825 days for hosts (names or IPs) if they do
// not exist. An existing pair that cannot be loaded (key.pem missing or
// broken) is an error: it is never replaced, so the cause is not hidden. An
// existing certificate whose validity exceeds 825 days (such as legacy 10-year
// certificates) is regenerated.
func Ensure(dir string, hosts []string) (certFile, keyFile string, err error) {
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	now := time.Now()
	if _, err := os.Stat(certFile); err == nil {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return "", "", fmt.Errorf("console TLS certificate in %s: %w", dir, err)
		}
		if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err != nil || !exceedsMaxValidity(leaf, now) {
			return certFile, keyFile, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader) // P-256 with crypto/rand does not fail
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	notBefore := now.Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "db-branch", Organization: []string{"db-branch"}},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(MaxSelfSignedValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func exceedsMaxValidity(leaf *x509.Certificate, now time.Time) bool {
	return leaf.NotAfter.Sub(leaf.NotBefore) > MaxSelfSignedValidity ||
		leaf.NotAfter.After(now.Add(MaxSelfSignedValidity))
}

// Resolve selects the configured certificate and key paths when both are set,
// rejects partial configuration, and otherwise ensures a self-signed pair in
// fallbackDir.
func Resolve(certPath, keyPath, fallbackDir string, hosts []string) (certFile, keyFile string, err error) {
	if (certPath == "") != (keyPath == "") {
		return "", "", errors.New("console TLS: both console.tls_cert and console.tls_key must be set")
	}
	if certPath != "" {
		return certPath, keyPath, nil
	}
	return Ensure(fallbackDir, hosts)
}

// Reloader loads a TLS certificate/key pair from disk and automatically reloads
// it on subsequent TLS handshakes when either file changes, keeping the last
// valid pair if reloading fails.
type Reloader struct {
	certFile string
	keyFile  string
	log      *slog.Logger

	mu          sync.RWMutex
	pair        *tls.Certificate
	certPEM     []byte
	certHash    [32]byte
	keyHash     [32]byte
	warnedState string
}

// NewReloader validates and loads certFile and keyFile immediately, returning
// an error if either file is unreadable or the key pair does not match.
func NewReloader(certFile, keyFile string, log *slog.Logger) (*Reloader, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("console TLS: certificate and key paths are required")
	}
	if log == nil {
		log = slog.Default()
	}
	certPEM, keyPEM, err := readPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("console TLS certificate %s: %w", certFile, err)
	}
	return &Reloader{
		certFile: certFile,
		keyFile:  keyFile,
		log:      log,
		pair:     &pair,
		certPEM:  certPEM,
		certHash: sha256.Sum256(certPEM),
		keyHash:  sha256.Sum256(keyPEM),
	}, nil
}

// GetCertificate implements tls.Config.GetCertificate, reloading the pair when
// either file changes on disk.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	pair, _ := r.current()
	return pair, nil
}

// CertPEM returns the current certificate PEM bytes (refreshing if changed).
func (r *Reloader) CertPEM() []byte {
	_, pemBytes := r.current()
	return bytes.Clone(pemBytes)
}

func (r *Reloader) current() (*tls.Certificate, []byte) {
	certPEM, keyPEM, readErr := readPair(r.certFile, r.keyFile)
	if readErr == nil {
		ch, kh := sha256.Sum256(certPEM), sha256.Sum256(keyPEM)
		r.mu.RLock()
		unchanged := ch == r.certHash && kh == r.keyHash && r.warnedState == ""
		pair, curPEM := r.pair, r.certPEM
		r.mu.RUnlock()
		if unchanged {
			return pair, curPEM
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-read inside the write lock so concurrent updates cannot log stale errors.
	certPEM, keyPEM, readErr = readPair(r.certFile, r.keyFile)
	if readErr != nil {
		r.warnOnce("read:"+readErr.Error(), readErr)
		return r.pair, r.certPEM
	}
	ch, kh := sha256.Sum256(certPEM), sha256.Sum256(keyPEM)
	if ch == r.certHash && kh == r.keyHash {
		r.warnedState = ""
		return r.pair, r.certPEM
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		r.warnOnce(fmt.Sprintf("pair:%x:%x", ch, kh), fmt.Errorf("console TLS certificate %s: %w", r.certFile, err))
		return r.pair, r.certPEM
	}
	r.pair = &pair
	r.certPEM = certPEM
	r.certHash = ch
	r.keyHash = kh
	r.warnedState = ""
	return r.pair, r.certPEM
}

func (r *Reloader) warnOnce(state string, err error) {
	if r.warnedState == state {
		return
	}
	r.warnedState = state
	r.log.Warn("reloading console TLS certificate failed; keeping current certificate", "cert", r.certFile, "key", r.keyFile, "err", err)
}

func readPair(certFile, keyFile string) (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("console TLS certificate %s: %w", certFile, err)
	}
	keyPEM, err = os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("console TLS key %s: %w", keyFile, err)
	}
	return certPEM, keyPEM, nil
}
