// Package clusterpki is the cluster's private CA: node certificates for mutual
// TLS between agents, and join tokens that pin the CA by fingerprint.
package clusterpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CA signs node certificates.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	keyPEM  []byte
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func keyPEM(k *ecdsa.PrivateKey) []byte {
	der, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func writeFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, data := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "key.pem") {
			mode = 0o600
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
			return err
		}
	}
	return nil
}

// InitCA creates a new CA in dir. It refuses to overwrite an existing one.
func InitCA(dir string) (*CA, error) {
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err == nil {
		return nil, fmt.Errorf("a cluster CA already exists in %s", dir)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "db-branch cluster CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca, _ := ParseCA(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM(key))
	return ca, ca.Save(dir)
}

// ParseCA reads a CA from PEM data (as handed to a joining node).
func ParseCA(certPEM, keyPEMData []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, errors.New("CA certificate: no PEM data")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEMData)
	if kb == nil {
		return nil, errors.New("CA key: no PEM data")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, keyPEM: keyPEMData}, nil
}

// LoadCA reads the CA saved in dir.
func LoadCA(dir string) (*CA, error) {
	c, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	k, err := os.ReadFile(filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		return nil, err
	}
	return ParseCA(c, k)
}

// Save writes the CA to dir.
func (ca *CA) Save(dir string) error {
	return writeFiles(dir, map[string][]byte{"ca.pem": ca.certPEM, "ca-key.pem": ca.keyPEM})
}

// PEM returns the certificate and key.
func (ca *CA) PEM() (cert, key []byte) { return ca.certPEM, ca.keyPEM }

// Fingerprint is the SHA-256 of the CA certificate, in hex.
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return hex.EncodeToString(sum[:])
}

func (ca *CA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// NodeCert is a node's certificate and key plus the CA that issued it.
type NodeCert struct {
	pair tls.Certificate
	ca   *CA
}

// IssueNode creates and saves a certificate for a node, valid as both client and server.
func (ca *CA) IssueNode(dir, name string, hosts []string) (*NodeCert, error) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFiles(dir, map[string][]byte{"node.pem": certPEM, "node-key.pem": keyPEM(key)}); err != nil {
		return nil, err
	}
	return LoadNode(dir, ca)
}

// LoadNode reads a node certificate saved by IssueNode.
func LoadNode(dir string, ca *CA) (*NodeCert, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "node.pem"), filepath.Join(dir, "node-key.pem"))
	if err != nil {
		return nil, err
	}
	// Send the CA too, so a joining node can check it against the token's pin.
	pair.Certificate = append(pair.Certificate, ca.cert.Raw)
	return &NodeCert{pair: pair, ca: ca}, nil
}

// ServerTLS accepts connections from cluster members. With requireClient
// false, clients without a certificate may connect (only the join endpoint
// allows that; handlers must check r.TLS.VerifiedChains otherwise).
func (n *NodeCert) ServerTLS(requireClient bool) *tls.Config {
	auth := tls.VerifyClientCertIfGiven
	if requireClient {
		auth = tls.RequireAndVerifyClientCert
	}
	return &tls.Config{Certificates: []tls.Certificate{n.pair}, ClientCAs: n.ca.pool(), ClientAuth: auth, MinVersion: tls.VersionTLS13}
}

// ClientTLS connects to cluster members, presenting this node's certificate.
// Server names are not checked: membership is proven by the CA signature.
func (n *NodeCert) ClientTLS() *tls.Config {
	pool := n.ca.pool()
	return &tls.Config{
		Certificates:       []tls.Certificate{n.pair},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			return verifyChain(raw, pool)
		},
	}
}

func verifyChain(raw [][]byte, pool *x509.CertPool) error {
	if len(raw) == 0 {
		return errors.New("no peer certificate")
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return err
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

// PinnedTLS is used by a joining node that has no certificate yet: it trusts
// a server only if the server's chain ends in the CA with the given fingerprint.
func PinnedTLS(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no peer certificate")
			}
			for _, der := range raw[1:] {
				sum := sha256.Sum256(der)
				if hex.EncodeToString(sum[:]) != fingerprint {
					continue
				}
				ca, err := x509.ParseCertificate(der)
				if err != nil {
					return err
				}
				pool := x509.NewCertPool()
				pool.AddCert(ca)
				return verifyChain(raw, pool)
			}
			return errors.New("server is not part of the expected cluster (CA fingerprint mismatch)")
		},
	}
}

// Token lets a new node join: it pins the CA and proves the node was invited.
type Token struct {
	Fingerprint string
	Secret      string
}

const tokenPrefix = "dbb1"

var tokenPattern = regexp.MustCompile(`^dbb1\.([0-9a-f]{64})\.([A-Za-z0-9_-]+)$`)

func (t Token) String() string { return tokenPrefix + "." + t.Fingerprint + "." + t.Secret }

// ParseToken reads "dbb1.<fingerprint>.<secret>".
func ParseToken(s string) (Token, error) {
	m := tokenPattern.FindStringSubmatch(s)
	if m == nil {
		return Token{}, errors.New("invalid join token")
	}
	return Token{Fingerprint: m[1], Secret: m[2]}, nil
}

// NewSecret returns a random join secret.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashSecret is what the cluster stores instead of the secret.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
