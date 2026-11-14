// Package license is the licence format of db-branch's paid modules and the
// code that checks it, so every module verifies licences the same way.
//
// Two keys (ECDSA P-256, so any cloud KMS can hold them):
//
//   - the root key, kept offline; modules embed only its public key;
//   - an issuing key (in a KMS), certified by the root key for a period.
//     Licences are signed with it and carry its certificate, so a leaked
//     issuing key is replaced without shipping new modules.
//
// A licence key is one string an admin pastes into Settings:
//
//	dbbl1.<payload>.<signature>.<issuer certificate>
//	certificate: <payload>.<signature>   (all base64url)
package license

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	// ErrInvalid: the key is malformed or a signature does not verify.
	ErrInvalid = errors.New("invalid licence")
	// ErrNotCovered: the licence does not include this module.
	ErrNotCovered = errors.New("licence does not cover this module")
)

const prefix = "dbbl1"

// License is what a customer bought.
type License struct {
	ID       string    `json:"id"`
	Customer string    `json:"customer"`
	Modules  []string  `json:"modules"`
	MaxNodes int       `json:"max_nodes"` // 0: no limit
	IssuedAt time.Time `json:"issued_at"`
	Expires  time.Time `json:"expires"`
	// GraceDays after Expires the modules keep working (and warn).
	GraceDays int `json:"grace_days"`
	// Plan is the pricing tier, which decides the features (Has).
	Plan string `json:"plan,omitempty"`
	// Billing is how the licence is paid for: Monthly, Yearly, or "" for a
	// licence that is bought once and renewed by hand.
	Billing string `json:"billing,omitempty"`
	// Subscription is an opaque id; when set, the key can be renewed online
	// at RenewURL (the cluster's leader fetches the next key itself).
	Subscription string `json:"subscription,omitempty"`
	// RenewURL is the https endpoint that issues the renewed key.
	RenewURL string `json:"renew_url,omitempty"`
}

// Billing periods of a subscription.
const (
	Monthly = "monthly"
	Yearly  = "yearly"
)

// Plans. Lite: history, on up to 3 nodes. Team adds accounts with roles and
// teams, on up to 5. Business adds single sign-on, the audit log and masking
// governance, on up to 10. No plan includes diff.
const (
	Lite     = "lite"
	Team     = "team"
	Business = "business"
)

// Features the paid modules check.
const (
	FeatureUsers   = "users"   // accounts that sign in, their roles and teams (auth)
	FeatureAudit   = "audit"   // the audit log (audit)
	FeatureSSO     = "sso"     // single sign-on (auth)
	FeatureMasking = "masking" // masking governance (masking): in no plan, opt-in (see optIn)
	FeatureHistory = "history" // recording a branch's changes and restoring to any point (history)
	FeatureDiff    = "diff"    // comparing the data and schema of two branches (diff)
)

// planFeatures is the one place that says which plan includes which feature.
// To move a feature between plans, change only these lists. FeatureDiff is in
// no plan (see optIn).
var planFeatures = map[string][]string{
	Lite:     {FeatureHistory},
	Team:     {FeatureUsers, FeatureHistory},
	Business: {FeatureUsers, FeatureAudit, FeatureSSO, FeatureHistory},
}

// optIn are features no plan includes (yet). A licence has one only when its
// Modules field names the module explicitly: "*" does not count, so neither
// a plain key of any plan nor the trial enables them. Masking governance is
// one: it comes back later as an opt-in beta. So is diff: it is offered with no
// plan for now.
var optIn = map[string]string{FeatureMasking: "masking", FeatureDiff: "diff"}

var planNodes = map[string]int{Lite: 3, Team: 5, Business: 10}

// Plans keys were issued with before Team and Business ("" before plans
// existed), read as the plan that replaced them.
var earlierPlans = map[string]string{"startup": Team, "growth": Team, "enterprise": Business, "": Business}

func currentPlan(p string) string {
	if now, ok := earlierPlans[p]; ok {
		return now
	}
	return p
}

// ValidPlan reports whether keys are issued for plan p.
func ValidPlan(p string) bool { _, ok := planFeatures[p]; return ok }

// PlanNodes is how many nodes one unit of a plan allows (0 when unknown).
func PlanNodes(plan string) int { return planNodes[plan] }

// Has reports whether the licence's plan includes feature, or, for an opt-in
// feature (masking governance), whether Modules names its module explicitly.
func (l License) Has(feature string) bool {
	if module, ok := optIn[feature]; ok {
		return slices.Contains(l.Modules, module)
	}
	for _, f := range planFeatures[currentPlan(l.Plan)] {
		if f == feature {
			return true
		}
	}
	return false
}

// TrialDays is how long a cluster may use the paid modules without a key.
const TrialDays = 30

// Trial is the licence a cluster has without a key, from when its first
// paid module was loaded: every module and feature, on any number of
// nodes, for TrialDays.
func Trial(start time.Time) License {
	return License{ID: "trial", Modules: []string{"*"}, IssuedAt: start, Expires: start.AddDate(0, 0, TrialDays), Plan: Business}
}

// Covers reports whether the licence includes module.
func (l License) Covers(module string) bool {
	for _, m := range l.Modules {
		if m == module || m == "*" {
			return true
		}
	}
	return false
}

// State of a licence at a time, for a cluster size.
type State string

const (
	Valid State = "valid"
	// Grace: expired, still working for GraceDays; renew.
	Grace State = "grace"
	// Expired: past the grace period; paid features stop (never the databases).
	Expired State = "expired"
	// OverNodes: the cluster has more nodes than bought.
	OverNodes State = "over_nodes"
)

// State tells where the licence stands at now with nodes cluster members.
func (l License) State(now time.Time, nodes int) State {
	switch {
	case l.MaxNodes > 0 && nodes > l.MaxNodes:
		return OverNodes
	case now.Before(l.Expires):
		return Valid
	case now.Before(l.Expires.AddDate(0, 0, l.GraceDays)):
		return Grace
	}
	return Expired
}

// Usable reports whether paid features may run in state s.
func (s State) Usable() bool { return s == Valid || s == Grace }

// IssuerCert certifies an issuing key for a period.
type IssuerCert struct {
	// Key is the issuing public key (PKIX DER).
	Key       []byte    `json:"key"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Name      string    `json:"name"`
}

// Signer signs SHA-256 digests with an ECDSA P-256 key (ASN.1 signatures):
// a key in memory or one held by a KMS.
type Signer interface {
	Sign(ctx context.Context, digest []byte) ([]byte, error)
	Public() *ecdsa.PublicKey
}

var b64 = base64.RawURLEncoding

func signed(ctx context.Context, s Signer, v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	d := sha256.Sum256(payload)
	sig, err := s.Sign(ctx, d[:])
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(sig), nil
}

func open(pub *ecdsa.PublicKey, payloadB64, sigB64 string, v any) error {
	payload, err1 := b64.DecodeString(payloadB64)
	sig, err2 := b64.DecodeString(sigB64)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("%w: not base64url", ErrInvalid)
	}
	d := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(pub, d[:], sig) {
		return fmt.Errorf("%w: bad signature", ErrInvalid)
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// Certify issues the certificate of an issuing key with the root key.
func Certify(ctx context.Context, root Signer, issuing *ecdsa.PublicKey, name string, notBefore, notAfter time.Time) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(issuing)
	if err != nil {
		return "", err
	}
	return signed(ctx, root, IssuerCert{Key: der, NotBefore: notBefore.UTC(), NotAfter: notAfter.UTC(), Name: name})
}

// Issue signs a licence with the issuing key and attaches its certificate.
func Issue(ctx context.Context, issuing Signer, cert string, l License) (string, error) {
	body, err := signed(ctx, issuing, l)
	if err != nil {
		return "", err
	}
	return prefix + "." + body + "." + cert, nil
}

// Verify checks a licence key against the root public key: the issuing
// key's certificate, that the licence was issued while the certificate was
// valid, and the licence's own signature. It does not judge expiry (State).
func Verify(root *ecdsa.PublicKey, key string) (License, error) {
	parts := strings.Split(strings.TrimSpace(key), ".")
	if len(parts) != 5 || parts[0] != prefix {
		return License{}, fmt.Errorf("%w: not a db-branch licence key", ErrInvalid)
	}
	var cert IssuerCert
	if err := open(root, parts[3], parts[4], &cert); err != nil {
		return License{}, fmt.Errorf("issuer certificate: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(cert.Key)
	issuing, ok := pub.(*ecdsa.PublicKey)
	if err != nil || !ok || issuing.Curve != elliptic.P256() {
		return License{}, fmt.Errorf("%w: issuer key is not ECDSA P-256", ErrInvalid)
	}
	var l License
	if err := open(issuing, parts[1], parts[2], &l); err != nil {
		return License{}, err
	}
	if l.IssuedAt.Before(cert.NotBefore) || !l.IssuedAt.Before(cert.NotAfter) {
		return License{}, fmt.Errorf("%w: issued outside the issuer certificate's validity", ErrInvalid)
	}
	l.Plan = currentPlan(l.Plan)
	return l, nil
}

// Check verifies key for module and returns the licence and its state.
func Check(root *ecdsa.PublicKey, key, module string, now time.Time, nodes int) (License, State, error) {
	l, err := Verify(root, key)
	if err != nil {
		return l, "", err
	}
	if !l.Covers(module) {
		return l, "", fmt.Errorf("%w: %s", ErrNotCovered, module)
	}
	return l, l.State(now, nodes), nil
}

// ParsePublicKey reads a PEM "PUBLIC KEY" (P-256), e.g. the embedded root key.
func ParsePublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block", ErrInvalid)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	k, ok := pub.(*ecdsa.PublicKey)
	if err != nil || !ok || k.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: not an ECDSA P-256 public key", ErrInvalid)
	}
	return k, nil
}

// LocalSigner signs with a key in memory: tests, development, and the
// offline root ceremony when no hardware token is used.
type LocalSigner struct{ Key *ecdsa.PrivateKey }

// NewLocalSigner makes a fresh P-256 key.
func NewLocalSigner() LocalSigner {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return LocalSigner{Key: k}
}

func (s LocalSigner) Sign(_ context.Context, digest []byte) ([]byte, error) {
	return s.Key.Sign(rand.Reader, digest, crypto.SHA256)
}

func (s LocalSigner) Public() *ecdsa.PublicKey { return &s.Key.PublicKey }

// PublicPEM encodes a public key as PEM (to embed in modules).
func PublicPEM(k *ecdsa.PublicKey) []byte {
	der, _ := x509.MarshalPKIXPublicKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
