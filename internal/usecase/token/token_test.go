package token_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

var ctx = context.Background()

func newService(t *testing.T) (*token.Service, *time.Time) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	return &token.Service{Store: &filestore.Store{Path: filepath.Join(t.TempDir(), "s.json")}, Clock: func() time.Time { return now }, Settle: time.Millisecond}, &now
}

func TestCreate_一度だけ見せハッシュだけ残し認証に使える(t *testing.T) {
	s, now := newService(t)
	secret, rec, err := s.Create(ctx, "claude-code", domain.ScopeBranches, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "dbb_"+rec.ID+"_") || strings.Contains(rec.Hash, strings.Split(secret, "_")[2]) || rec.ExpiresAt == nil {
		t.Errorf("secret %q rec %+v", secret, rec)
	}
	p, err := s.Authenticate(ctx, ext.Credentials{Token: secret})
	if err != nil || p.ID != "token:"+rec.ID || p.Name != "claude-code" || p.Roles[0] != "scope:branches" {
		t.Errorf("principal %+v %v", p, err)
	}
	*now = now.Add(25 * time.Hour)
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: secret}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("expired: %v", err)
	}
	forever, rec2, _ := s.Create(ctx, "ci", domain.ScopeFull, 0)
	if rec2.ExpiresAt != nil {
		t.Error("ttl 0 never expires")
	}
	if err := s.Revoke(ctx, rec2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: forever}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("revoked: %v", err)
	}
	if list, _ := s.List(ctx); len(list) != 1 {
		t.Errorf("list = %+v", list)
	}
}

func TestAuthenticate_他の形式は次の認証へ回し偽物は断る(t *testing.T) {
	s, _ := newService(t)
	secret, rec, _ := s.Create(ctx, "x", domain.ScopeFull, 0)
	for _, c := range []ext.Credentials{{}, {Username: "admin", Password: "p"}, {Token: "other-format"}, {Token: "dbb_nounderscore"}} {
		if _, err := s.Authenticate(ctx, c); !errors.Is(err, ext.ErrUnauthenticated) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: "dbb_" + rec.ID + "_wrong"}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("wrong secret: %v", err)
	}
	_ = secret
}

func TestCreate_名前と範囲と期限を確かめる(t *testing.T) {
	s, _ := newService(t)
	for _, tc := range []struct {
		name, scope string
		ttl         time.Duration
	}{{"", "full", 0}, {strings.Repeat("x", 65), "full", 0}, {"x", "god", 0}, {"x", "full", -time.Hour}} {
		if _, _, err := s.Create(ctx, tc.name, tc.scope, tc.ttl); !errors.Is(err, token.ErrInvalid) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

type broken struct{ domain.TokenStore }

func (broken) ListTokens(context.Context) ([]domain.APIToken, error) { return nil, errors.New("down") }
func (broken) PutToken(context.Context, domain.APIToken) error       { return errors.New("down") }

func Test保存先の失敗はエラー(t *testing.T) {
	s := &token.Service{Store: broken{}, Clock: time.Now}
	if _, _, err := s.Create(ctx, "x", "full", 0); err == nil {
		t.Error("create")
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: "dbb_a_b"}); err == nil || errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("authenticate: %v", err)
	}
}

func TestScoped_ブランチ用のトークンはブランチ操作と読み取りだけ(t *testing.T) {
	p := ext.Principal{ID: "token:x", Roles: []string{"scope:branches"}}
	for action, ok := range map[string]bool{"branch.create": true, "branch.query": true, "base.read": true, "base.public": true, "mcp.use": true,
		"base.delete": false, "runtime.install": false, "token.write": false, "setting.write": false,
		// Reads that expose secrets or others' access stay with full tokens.
		"setting.read": false, "token.read": false, "route-user.read": false, "account.token.read": true} {
		if err := (token.Scoped{}).Authorize(ctx, p, action, ext.Resource{}); (err == nil) != ok {
			t.Errorf("%s: %v", action, err)
		}
	}
	if err := (token.Scoped{}).Authorize(ctx, ext.Principal{ID: "admin"}, "base.delete", ext.Resource{}); err != nil {
		t.Errorf("not a token: %v", err)
	}
}

func TestScoped_branchesトークンはホワイトリストの操作だけを許可し管理系や拡張モジュール読み取りを拒否する(t *testing.T) {
	p := ext.Principal{ID: "token:x", Roles: []string{"scope:branches"}}
	allowed := []string{
		"branch.read", "branch.create", "branch.extend", "branch.delete", "branch.reset",
		"branch.pause", "branch.resume", "branch.update",
		"branch.fork", "branch.recreate",
		"branch.checkpoint.read", "branch.checkpoint", "branch.restore",
		"branch.checkpoint.delete", "branch.checkpoint_delete",
		"branch.upgrade", "branch.merge", "branch.connect", "branch.query",
		"base.read", "base.public", "runtime.read", "operation.read",
		"event.read", "capability.read", "metrics.read",
		"account.preference.read", "account.token.read", "mcp.use",
	}
	for _, action := range allowed {
		if err := (token.Scoped{}).Authorize(ctx, p, action, ext.Resource{}); err != nil {
			t.Errorf("allowed action %q rejected: %v", action, err)
		}
	}
	denied := []string{
		"ext.auth.read", "ext.auth.write", "ext.audit.read", "ext.history.read", "ext.diff.read",
		"license.read", "license.verify", "license.write",
		"cluster.read", "cluster.remove", "cluster.token",
		"module.read", "module.install", "module.remove", "module.forget",
		"update.read", "update.check", "update.apply",
		"setting.read", "setting.write",
		"token.read", "token.write",
		"route-user.read", "route-user.write", "route-user.delete",
		"backup.read", "backup.create", "backup.download", "backup.restore",
		"base.create", "base.describe", "base.replicas", "base.publish", "base.delete",
		"base.snapshot", "base.snapshot.delete", "base.snapshot_delete",
		"base.start", "base.stop", "base.sql", "base.load",
		"base.refresh.read", "base.refresh", "base.upload", "base.mask",
		"runtime.install", "runtime.remove",
		"account.preference.write", "account.token.write",
		"auth.session", "auth.logout", "auth.password", "auth.reset",
		"branch.unknown",
	}
	for _, action := range denied {
		if err := (token.Scoped{}).Authorize(ctx, p, action, ext.Resource{}); !errors.Is(err, ext.ErrForbidden) {
			t.Errorf("denied action %q: got %v, want ErrForbidden", action, err)
		}
	}
}

type lagging struct {
	domain.TokenStore
	calls int
}

func (l *lagging) ListTokens(ctx context.Context) ([]domain.APIToken, error) {
	l.calls++
	if l.calls < 3 {
		return nil, nil // not replicated here yet
	}
	return l.TokenStore.ListTokens(ctx)
}

func TestAuthenticate_作られたばかりで未着のトークンは少し待つ(t *testing.T) {
	s, _ := newService(t)
	secret, _, _ := s.Create(ctx, "x", domain.ScopeFull, 0)
	lag := &lagging{TokenStore: s.Store}
	s.Store, s.Settle = lag, time.Second
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: secret}); err != nil || lag.calls != 3 {
		t.Errorf("err %v calls %d", err, lag.calls)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	s.Store = &lagging{TokenStore: s.Store, calls: -100}
	if _, err := s.Authenticate(c, ext.Credentials{Token: "dbb_zz_x"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestPersonal_本人として動き本人だけが一覧と取り消しをできる(t *testing.T) {
	s, _ := newService(t)
	alice := ext.Principal{ID: "user:alice@example.com", Name: "alice@example.com", Roles: []string{"developer"}}
	secret, rec, err := s.CreatePersonal(ctx, alice, "claude-code", 0)
	if err != nil || rec.Owner != alice.ID || rec.Scope != domain.ScopeBranches {
		t.Fatalf("%+v %v", rec, err)
	}
	p, err := s.Authenticate(ctx, ext.Credentials{Token: secret})
	// It acts as alice (her branches, her current role in the module), only
	// within the branches scope, and never as an admin.
	if err != nil || p.ID != alice.ID || p.Name != alice.Name || len(p.Roles) != 2 || p.Roles[0] != "scope:branches" || p.Roles[1] != "developer" {
		t.Errorf("principal %+v %v", p, err)
	}
	_, _, _ = s.Create(ctx, "ci", domain.ScopeFull, 0)
	bob := ext.Principal{ID: "user:bob@example.com", Name: "bob@example.com"}
	_, _, _ = s.CreatePersonal(ctx, bob, "laptop", 0)
	mine, _ := s.ListOwned(ctx, alice.ID)
	if len(mine) != 1 || mine[0].ID != rec.ID {
		t.Errorf("alice's tokens %+v", mine)
	}
	service, _ := s.ListService(ctx)
	if len(service) != 1 || service[0].Name != "ci" {
		t.Errorf("service tokens %+v", service)
	}
	if err := s.RevokeOwned(ctx, bob.ID, rec.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("bob revokes alice's: %v", err)
	}
	if err := s.RevokeOwned(ctx, alice.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: secret}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("revoked: %v", err)
	}
}

func TestPersonal_トークンや匿名では作れない(t *testing.T) {
	s, _ := newService(t)
	for _, p := range []ext.Principal{
		{ID: "token:abc", Roles: []string{"scope:full"}},
		{ID: "user:x", Roles: []string{"scope:branches"}},
		{ID: "anonymous"},
		{ID: "local"},
		{},
	} {
		if _, _, err := s.CreatePersonal(ctx, p, "x", 0); !errors.Is(err, token.ErrInvalid) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	if _, _, err := s.CreatePersonal(ctx, ext.Principal{ID: "admin", Name: "admin"}, "", 0); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("no name: %v", err)
	}
}

func TestAuthenticate_Settle未設定の単一ノードでは存在しないトークンを待たずに401を返す(t *testing.T) {
	s, _ := newService(t)
	s.Settle = 0
	lag := &lagging{TokenStore: s.Store, calls: -100}
	s.Store = lag
	start := time.Now()
	_, err := s.Authenticate(ctx, ext.Credentials{Token: "dbb_zz_x"})
	if !errors.Is(err, ext.ErrUnauthenticated) || lag.calls != -99 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("err %v calls %d", err, lag.calls)
	}
}

type blocking struct {
	domain.TokenStore
	entered chan struct{}
}

func (b *blocking) ListTokens(context.Context) ([]domain.APIToken, error) {
	b.entered <- struct{}{}
	return nil, nil
}

func TestAuthenticate_未着トークンの待機は同時数に上限があり超えた分は即時401(t *testing.T) {
	s, _ := newService(t)
	s.Settle = 3 * time.Second
	b := &blocking{TokenStore: s.Store, entered: make(chan struct{}, 1000)}
	s.Store = b
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	for i := 0; i < token.MaxSettlers; i++ {
		go func() { _, _ = s.Authenticate(c, ext.Credentials{Token: "dbb_zz_x"}); done <- struct{}{} }()
	}
	for len(b.entered) < token.MaxSettlers {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	_, err := s.Authenticate(ctx, ext.Credentials{Token: "dbb_zz_x"})
	if !errors.Is(err, ext.ErrUnauthenticated) || time.Since(start) > time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
	cancel()
	for i := 0; i < token.MaxSettlers; i++ {
		<-done
	}
}

type deleteErrStore struct {
	domain.TokenStore
	err error
}

func (d deleteErrStore) DeleteToken(context.Context, string) error { return d.err }

func TestRevokeByOwner_指定所有者の個人トークンだけを全て失効させサービストークンは残す(t *testing.T) {
	s, _ := newService(t)
	svcTok, _, _ := s.Create(ctx, "ci", domain.ScopeFull, 0)
	alice := ext.Principal{ID: "user:alice@example.com", Name: "alice@example.com"}
	aliceMixed := ext.Principal{ID: "user:Alice@Example.com", Name: "Alice@Example.com"}
	bob := ext.Principal{ID: "user:bob@example.com", Name: "bob@example.com"}
	a1, _, _ := s.CreatePersonal(ctx, alice, "laptop", 0)
	a2, _, _ := s.CreatePersonal(ctx, alice, "desktop", 24*time.Hour)
	a3, _, _ := s.CreatePersonal(ctx, aliceMixed, "mixed", 0)
	b1, _, _ := s.CreatePersonal(ctx, bob, "ci", 0)

	// Empty owner must never revoke service tokens.
	if err := s.RevokeByOwner(ctx, ""); err != nil {
		t.Fatalf("empty owner: %v", err)
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: svcTok}); err != nil {
		t.Fatalf("service token revoked on empty owner: %v", err)
	}

	if err := s.RevokeByOwner(ctx, alice.ID); err != nil {
		t.Fatalf("revoke alice: %v", err)
	}
	for _, tok := range []string{a1, a2, a3} {
		if _, err := s.Authenticate(ctx, ext.Credentials{Token: tok}); !errors.Is(err, ext.ErrUnauthenticated) {
			t.Errorf("alice token still authenticates: %v", err)
		}
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: b1}); err != nil {
		t.Errorf("bob token affected: %v", err)
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: svcTok}); err != nil {
		t.Errorf("service token affected: %v", err)
	}
	// Idempotent when none left.
	if err := s.RevokeByOwner(ctx, alice.ID); err != nil {
		t.Errorf("idempotent revoke: %v", err)
	}

	// Concurrent ErrNotFound on delete is ignored; other delete/list errors surface.
	s.Store = deleteErrStore{TokenStore: s.Store, err: domain.ErrNotFound}
	if err := s.RevokeByOwner(ctx, bob.ID); err != nil {
		t.Errorf("concurrent ErrNotFound should be ignored: %v", err)
	}
	s.Store = deleteErrStore{TokenStore: s.Store, err: errors.New("raft down")}
	if err := s.RevokeByOwner(ctx, bob.ID); err == nil {
		t.Error("delete error should surface")
	}
	s.Store = broken{}
	if err := s.RevokeByOwner(ctx, bob.ID); err == nil {
		t.Error("list error should surface")
	}
}

func TestSweepExpired_期限切れトークンのみを削除し無期限と未期限切れは残す(t *testing.T) {
	s, now := newService(t)
	forever, _, _ := s.Create(ctx, "forever", domain.ScopeFull, 0)
	short, _, _ := s.Create(ctx, "short", domain.ScopeBranches, time.Hour)
	longer, _, _ := s.Create(ctx, "longer", domain.ScopeBranches, 24*time.Hour)

	if n, err := s.SweepExpired(ctx); err != nil || n != 0 {
		t.Fatalf("before expiry: n=%d err=%v", n, err)
	}
	*now = now.Add(time.Hour) // exact expiry boundary
	if n, err := s.SweepExpired(ctx); err != nil || n != 1 {
		t.Fatalf("at expiry: n=%d err=%v", n, err)
	}
	if list, _ := s.List(ctx); len(list) != 2 {
		t.Errorf("remaining = %+v, want 2", list)
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: short}); !errors.Is(err, ext.ErrUnauthenticated) {
		t.Errorf("swept token: %v", err)
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: forever}); err != nil {
		t.Errorf("forever token: %v", err)
	}
	if _, err := s.Authenticate(ctx, ext.Credentials{Token: longer}); err != nil {
		t.Errorf("longer token: %v", err)
	}

	*now = now.Add(24 * time.Hour)
	orig := s.Store
	s.Store = deleteErrStore{TokenStore: orig, err: domain.ErrNotFound}
	if n, err := s.SweepExpired(ctx); err != nil || n != 0 {
		t.Errorf("concurrent ErrNotFound: n=%d err=%v", n, err)
	}
	s.Store = deleteErrStore{TokenStore: orig, err: errors.New("raft down")}
	if _, err := s.SweepExpired(ctx); err == nil {
		t.Error("delete error should surface")
	}
	s.Store = broken{}
	if _, err := s.SweepExpired(ctx); err == nil {
		t.Error("list error should surface")
	}
}

func TestPersonal_個人トークンは所有者の非adminロールを保持しadminロールは除外する(t *testing.T) {
	s, _ := newService(t)

	// Viewer keeps "viewer" role so role-based authorizers cannot be bypassed.
	viewer := ext.Principal{ID: "sso:alice", Name: "Alice", Roles: []string{"viewer", "viewer", ""}}
	vSec, vRec, err := s.CreatePersonal(ctx, viewer, "mcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vRec.OwnerRoles) != 1 || vRec.OwnerRoles[0] != "viewer" {
		t.Fatalf("vRec.OwnerRoles = %v, want [viewer]", vRec.OwnerRoles)
	}
	vp, err := s.Authenticate(ctx, ext.Credentials{Token: vSec})
	if err != nil || len(vp.Roles) != 2 || vp.Roles[0] != "scope:branches" || vp.Roles[1] != "viewer" {
		t.Errorf("viewer principal = %+v, err = %v", vp, err)
	}

	// Admin with secondary role drops "admin" but keeps the non-admin role.
	adminDev := ext.Principal{ID: "sso:boss", Name: "Boss", Roles: []string{"admin", "developer"}}
	adSec, adRec, err := s.CreatePersonal(ctx, adminDev, "cli", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(adRec.OwnerRoles) != 1 || adRec.OwnerRoles[0] != "developer" {
		t.Fatalf("adRec.OwnerRoles = %v, want [developer]", adRec.OwnerRoles)
	}
	adp, err := s.Authenticate(ctx, ext.Credentials{Token: adSec})
	if err != nil || len(adp.Roles) != 2 || adp.Roles[0] != "scope:branches" || adp.Roles[1] != "developer" {
		t.Errorf("admin+dev principal = %+v, err = %v", adp, err)
	}

	// Pure admin drops "admin" completely.
	pureAdmin := ext.Principal{ID: "admin", Name: "admin", Roles: []string{"admin"}}
	aSec, aRec, err := s.CreatePersonal(ctx, pureAdmin, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(aRec.OwnerRoles) != 0 {
		t.Fatalf("pureAdmin OwnerRoles = %v, want empty", aRec.OwnerRoles)
	}
	ap, err := s.Authenticate(ctx, ext.Credentials{Token: aSec})
	if err != nil || len(ap.Roles) != 1 || ap.Roles[0] != "scope:branches" {
		t.Errorf("pureAdmin principal = %+v, err = %v", ap, err)
	}

	// Tampered store record carrying "admin" or "scope:full" in OwnerRoles is still sanitized on Authenticate.
	vRec.OwnerRoles = []string{"admin", "scope:full", "viewer"}
	if err := s.Store.PutToken(ctx, vRec); err != nil {
		t.Fatal(err)
	}
	vp2, err := s.Authenticate(ctx, ext.Credentials{Token: vSec})
	if err != nil || len(vp2.Roles) != 2 || vp2.Roles[0] != "scope:branches" || vp2.Roles[1] != "viewer" {
		t.Errorf("sanitized principal = %+v, err = %v", vp2, err)
	}
}

func TestCreateService_BaseとTeam制限を検証しPrincipalへ引き継ぐ(t *testing.T) {
	s, _ := newService(t)
	sec, rec, err := s.CreateService(ctx, "ci-pay", domain.ScopeBranches, time.Hour, []string{"shop", " analytics ", "shop", ""}, " payments ")
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if len(rec.Bases) != 2 || rec.Bases[0] != "analytics" || rec.Bases[1] != "shop" || rec.Team != "payments" {
		t.Fatalf("rec = %+v", rec)
	}
	p, err := s.Authenticate(ctx, ext.Credentials{Token: sec})
	if err != nil {
		t.Fatal(err)
	}
	if bases := ext.TokenBases(p); len(bases) != 2 || bases[0] != "analytics" || bases[1] != "shop" || ext.TokenTeam(p) != "payments" {
		t.Errorf("authenticated roles = %v", p.Roles)
	}
	// Backward compatible: omitted bases/team keeps Roles = ["scope:branches"].
	unSec, _, err := s.CreateService(ctx, "ci-plain", domain.ScopeBranches, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	unP, err := s.Authenticate(ctx, ext.Credentials{Token: unSec})
	if err != nil || len(unP.Roles) != 1 || unP.Roles[0] != "scope:branches" {
		t.Errorf("plain service token = %+v %v", unP, err)
	}
	// Reject invalid combinations.
	for _, tc := range []struct {
		scope string
		bases []string
		team  string
	}{
		{domain.ScopeFull, []string{"shop"}, ""},
		{domain.ScopeFull, nil, "payments"},
		{domain.ScopeBranches, []string{"Bad Base!"}, ""},
		{domain.ScopeBranches, []string{""}, ""},
		{domain.ScopeBranches, []string{"  "}, ""},
		{domain.ScopeBranches, nil, "BAD_TEAM"},
	} {
		if _, _, err := s.CreateService(ctx, "bad", tc.scope, 0, tc.bases, tc.team); !errors.Is(err, token.ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", tc, err)
		}
	}
	// Personal tokens strip any injected base: or team: roles.
	_, perRec, err := s.CreatePersonal(ctx, ext.Principal{
		ID: "user:a@x.io", Name: "Alice", Roles: []string{"developer", "base:shop", "team:payments"},
	}, "mcp", 0)
	if err != nil || len(perRec.OwnerRoles) != 1 || perRec.OwnerRoles[0] != "developer" {
		t.Errorf("personal token OwnerRoles = %v %v", perRec.OwnerRoles, err)
	}
}
