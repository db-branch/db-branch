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
	if err != nil || p.ID != alice.ID || p.Name != alice.Name || len(p.Roles) != 1 || p.Roles[0] != "scope:branches" {
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
