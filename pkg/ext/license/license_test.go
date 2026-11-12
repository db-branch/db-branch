package license_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/pkg/ext/license"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
)

func issue(t *testing.T, root, issuing license.Signer, l license.License) string {
	t.Helper()
	cert, err := license.Certify(ctx, root, issuing.Public(), "issuer-2026", t0.AddDate(0, -1, 0), t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	key, err := license.Issue(ctx, issuing, cert, l)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func lic() license.License {
	return license.License{ID: "L-1", Customer: "Acme", Modules: []string{"auth", "audit"}, MaxNodes: 3, IssuedAt: t0, Expires: t0.AddDate(1, 0, 0), GraceDays: 14}
}

func TestCheck_正しく発行されたライセンスは通り状態を判断する(t *testing.T) {
	root, issuing := license.NewLocalSigner(), license.NewLocalSigner()
	key := issue(t, root, issuing, lic())
	pub, err := license.ParsePublicKey(license.PublicPEM(root.Public()))
	if err != nil {
		t.Fatal(err)
	}
	l, st, err := license.Check(pub, key, "audit", t0.AddDate(0, 6, 0), 3)
	if err != nil || st != license.Valid || l.Customer != "Acme" || !st.Usable() {
		t.Fatalf("Check = %+v %s %v", l, st, err)
	}
	for when, want := range map[time.Time]license.State{
		t0.AddDate(1, 0, 3):  license.Grace,
		t0.AddDate(1, 0, 15): license.Expired,
	} {
		if _, st, _ := license.Check(pub, key, "audit", when, 3); st != want {
			t.Errorf("%v: %s, want %s", when, st, want)
		}
	}
	if license.Expired.Usable() || license.OverNodes.Usable() {
		t.Error("expired and over-nodes are not usable")
	}
	if _, st, _ := license.Check(pub, key, "audit", t0, 4); st != license.OverNodes {
		t.Errorf("4 nodes: %s", st)
	}
	if _, _, err := license.Check(pub, key, "billing", t0, 1); !errors.Is(err, license.ErrNotCovered) {
		t.Errorf("module not bought: %v", err)
	}
	all := lic()
	all.Modules, all.MaxNodes = []string{"*"}, 0
	if _, st, err := license.Check(pub, issue(t, root, issuing, all), "anything", t0, 100); err != nil || st != license.Valid {
		t.Errorf("all modules, no node limit: %s %v", st, err)
	}
}

func TestVerify_偽物や改ざんや別の根の鍵は断る(t *testing.T) {
	root, issuing := license.NewLocalSigner(), license.NewLocalSigner()
	key := issue(t, root, issuing, lic())
	parts := strings.Split(key, ".")

	other := license.NewLocalSigner()
	if _, err := license.Verify(other.Public(), key); !errors.Is(err, license.ErrInvalid) {
		t.Errorf("another root: %v", err)
	}
	// A licence signed by a key the root never certified.
	rogue := license.NewLocalSigner()
	forged := issue(t, rogue, rogue, lic())
	fp := strings.Split(forged, ".")
	if _, err := license.Verify(root.Public(), strings.Join(append(fp[:3], parts[3], parts[4]), ".")); !errors.Is(err, license.ErrInvalid) {
		t.Errorf("signature by an uncertified key: %v", err)
	}
	// Tampered payload (more nodes).
	tampered := append([]string{}, parts...)
	tampered[1] = strings.Replace(parts[1], "A", "B", 1)
	if _, err := license.Verify(root.Public(), strings.Join(tampered, ".")); !errors.Is(err, license.ErrInvalid) {
		t.Errorf("tampered: %v", err)
	}
	for _, bad := range []string{"", "dbbl1.a.b", "xxxx.a.b.c.d", "dbbl1.!!.b.c.d", "dbbl1." + parts[1] + "." + parts[2] + ".!!.x"} {
		if _, err := license.Verify(root.Public(), bad); !errors.Is(err, license.ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestVerify_証明書の期間外に発行したものは断る(t *testing.T) {
	root, issuing := license.NewLocalSigner(), license.NewLocalSigner()
	late := lic()
	late.IssuedAt = t0.AddDate(2, 0, 0) // after the issuer certificate ran out
	if _, err := license.Verify(root.Public(), issue(t, root, issuing, late)); !errors.Is(err, license.ErrInvalid) {
		t.Errorf("issued after the certificate: %v", err)
	}
}

type badSigner struct{ license.LocalSigner }

func (badSigner) Sign(context.Context, []byte) ([]byte, error) { return nil, errors.New("kms down") }

func Test鍵の扱いの失敗(t *testing.T) {
	if _, err := license.ParsePublicKey([]byte("nope")); !errors.Is(err, license.ErrInvalid) {
		t.Error("no pem")
	}
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := license.ParsePublicKey(license.PublicPEM(&k384.PublicKey)); !errors.Is(err, license.ErrInvalid) {
		t.Error("P-384 accepted")
	}
	root := license.NewLocalSigner()
	if _, err := license.Certify(ctx, badSigner{root}, root.Public(), "x", t0, t0); err == nil {
		t.Error("root signing failure")
	}
	if _, err := license.Certify(ctx, root, &ecdsa.PublicKey{}, "x", t0, t0); err == nil {
		t.Error("bad issuing key")
	}
	if _, err := license.Issue(ctx, badSigner{root}, "c.d", lic()); err == nil {
		t.Error("issuing failure")
	}
	// An issuer certificate holding a non-P-256 key.
	cert, _ := license.Certify(ctx, root, &k384.PublicKey, "x", t0.AddDate(-1, 0, 0), t0.AddDate(1, 0, 0))
	issuing := license.LocalSigner{Key: k384}
	key, _ := license.Issue(ctx, issuing, cert, lic())
	if _, err := license.Verify(root.Public(), key); !errors.Is(err, license.ErrInvalid) {
		t.Errorf("P-384 issuer: %v", err)
	}
}

func TestPlan_プランごとに使える機能が増える(t *testing.T) {
	for _, c := range []struct {
		plan string
		has  []string
		not  []string
	}{
		{license.Team, []string{license.FeatureUsers}, []string{license.FeatureAudit, license.FeatureSSO, license.FeatureMasking}},
		{license.Business, []string{license.FeatureUsers, license.FeatureAudit, license.FeatureSSO, license.FeatureMasking}, nil},
		// Keys issued under the earlier plans read as the plan that replaced them.
		{"startup", []string{license.FeatureUsers}, []string{license.FeatureAudit}},
		{"growth", []string{license.FeatureUsers}, []string{license.FeatureSSO}},
		{"enterprise", []string{license.FeatureAudit, license.FeatureSSO, license.FeatureMasking}, nil},
		{"", []string{license.FeatureAudit, license.FeatureSSO, license.FeatureMasking}, nil},
	} {
		l := license.License{Plan: c.plan}
		for _, f := range c.has {
			if !l.Has(f) {
				t.Errorf("%q should have %s", c.plan, f)
			}
		}
		for _, f := range c.not {
			if l.Has(f) {
				t.Errorf("%q should not have %s", c.plan, f)
			}
		}
	}
	if (license.License{Plan: "bogus"}).Has(license.FeatureUsers) {
		t.Error("an unknown plan has nothing")
	}
	if !license.ValidPlan(license.Team) || !license.ValidPlan(license.Business) || license.ValidPlan("growth") || license.ValidPlan("bogus") {
		t.Error("ValidPlan: keys are issued for team or business only")
	}
}

func TestPlan_プランがノード数の上限を決める(t *testing.T) {
	if license.PlanNodes(license.Team) != 5 || license.PlanNodes(license.Business) != 10 || license.PlanNodes("bogus") != 0 {
		t.Error("nodes a plan allows")
	}
}

func TestVerify_以前のプランのキーは今のプランとして読む(t *testing.T) {
	root, issuing := license.NewLocalSigner(), license.NewLocalSigner()
	for old, now := range map[string]string{"startup": license.Team, "growth": license.Team, "enterprise": license.Business, "": license.Business, license.Team: license.Team} {
		l := lic()
		l.Plan = old
		got, err := license.Verify(root.Public(), issue(t, root, issuing, l))
		if err != nil || got.Plan != now {
			t.Errorf("%q reads as %q (%v), want %q", old, got.Plan, err, now)
		}
	}
}

func TestTrial_キーなしで30日間すべての機能を試せる(t *testing.T) {
	l := license.Trial(t0)
	if !l.Covers("auth") || !l.Covers("masking") || !l.Has(license.FeatureSSO) || !l.Has(license.FeatureMasking) {
		t.Errorf("a trial covers everything: %+v", l)
	}
	if s := l.State(t0.AddDate(0, 0, 29), 50); s != license.Valid {
		t.Errorf("day 29, any cluster size: %s", s)
	}
	if s := l.State(t0.AddDate(0, 0, 30), 1); s != license.Expired {
		t.Errorf("day 30: %s (no grace period)", s)
	}
	if license.TrialDays != 30 || !l.Expires.Equal(t0.AddDate(0, 0, 30)) {
		t.Errorf("expires %s", l.Expires)
	}
}

func TestVerify_サブスクリプションの項目は署名されて戻り古い鍵はそれを持たない(t *testing.T) {
	root, issuing := license.NewLocalSigner(), license.NewLocalSigner()
	l := lic()
	l.Billing, l.Subscription, l.RenewURL = license.Monthly, "sub_123", "https://licenses.example.com/renew"
	got, err := license.Verify(root.Public(), issue(t, root, issuing, l))
	if err != nil || got.Billing != "monthly" || got.Subscription != "sub_123" || got.RenewURL != l.RenewURL {
		t.Fatalf("%+v %v", got, err)
	}
	old, err := license.Verify(root.Public(), issue(t, root, issuing, lic()))
	if err != nil || old.Billing != "" || old.Subscription != "" || old.RenewURL != "" {
		t.Errorf("%+v %v", old, err)
	}
	if license.Yearly != "yearly" {
		t.Error(license.Yearly)
	}
}
