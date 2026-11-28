package domain_test

import (
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestProtocolUsers_プロトコル30が予約されている(t *testing.T) {
	if domain.ProtocolUsers != 30 {
		t.Fatalf("ProtocolUsers = %d, want 30", domain.ProtocolUsers)
	}
	if domain.ProtocolPHCPassword != 32 {
		t.Fatalf("ProtocolPHCPassword = %d, want 32", domain.ProtocolPHCPassword)
	}
}

func TestUserEmail_正規化と検証(t *testing.T) {
	if got := domain.NormalizeEmail("  Alice+Test@Example.COM "); got != "alice+test@example.com" {
		t.Fatalf("NormalizeEmail = %q", got)
	}
	for _, valid := range []string{"alice@example.com", "a.b_c-1+tag@sub.example.co.jp"} {
		if err := domain.ValidateEmail(valid); err != nil {
			t.Errorf("ValidateEmail(%q) = %v, want nil", valid, err)
		}
	}
	for _, invalid := range []string{"", "admin", "a@b", "bad@example", "@example.com", "a b@example.com"} {
		if err := domain.ValidateEmail(invalid); !errors.Is(err, domain.ErrInvalidName) {
			t.Errorf("ValidateEmail(%q) = %v, want ErrInvalidName", invalid, err)
		}
	}
}

func TestUserID_不変IDの生成と識別子解決(t *testing.T) {
	if got := domain.UserIDForEmail(" Alice@Example.COM "); got != "user:alice@example.com" {
		t.Fatalf("UserIDForEmail = %q", got)
	}
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"admin", "admin", true},
		{" ADMIN ", "admin", true},
		{"Alice@Example.COM", "user:alice@example.com", true},
		{"user:Alice@Example.COM", "user:alice@example.com", true},
		{"", "", false},
		{"user:bad", "", false},
		{"not-an-email", "", false},
	}
	for _, tc := range cases {
		got, err := domain.ResolveUserID(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ResolveUserID(%q) = (%q, %v), want (%q, ok=%v)", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestActivePasswordUsers_最後の1人保護の集計(t *testing.T) {
	users := map[string]domain.User{
		"admin":                  {ID: "admin", Hash: []byte("h1")},
		"user:alice@example.com": {ID: "user:alice@example.com", Email: "alice@example.com", Hash: []byte("h2"), Disabled: true},
		"user:sso@example.com":   {ID: "user:sso@example.com", Email: "sso@example.com"},
	}
	if n := domain.CountActivePasswordUsers(users); n != 1 {
		t.Fatalf("CountActivePasswordUsers = %d, want 1", n)
	}
	u := users["user:alice@example.com"]
	if u.ActiveWithPassword() {
		t.Error("disabled user should not be active with password")
	}
	if !users["admin"].ActiveWithPassword() {
		t.Error("admin should be active with password")
	}
	if got := (domain.User{ID: "admin"}).DisplayName(); got != "admin" {
		t.Errorf("DisplayName(admin) = %q", got)
	}
	if got := (domain.User{ID: "user:a@example.com", Email: "a@example.com"}).DisplayName(); got != "a@example.com" {
		t.Errorf("DisplayName(email) = %q", got)
	}
	if got := (domain.User{ID: "user:a@example.com", Email: "a@example.com", Name: "Alice"}).DisplayName(); got != "Alice" {
		t.Errorf("DisplayName(name) = %q", got)
	}
	c := users["admin"].Clone()
	c.Hash[0] = 'X'
	if users["admin"].Hash[0] == 'X' {
		t.Error("Clone shared Hash backing array")
	}

	// EnsureActivePasswordUserAfterUpdate / AfterDelete
	if err := domain.EnsureActivePasswordUserAfterUpdate(users, domain.User{ID: "admin", Disabled: true, Hash: []byte("h1")}, false); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("disabling last active user = %v, want ErrConflict", err)
	}
	if err := domain.EnsureActivePasswordUserAfterUpdate(users, domain.User{ID: "admin", Disabled: true}, true); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("disabling admin overriding legacyAdmin = %v, want ErrConflict", err)
	}
	if err := domain.EnsureActivePasswordUserAfterUpdate(map[string]domain.User{}, domain.User{ID: "user:b@example.com", Hash: []byte("h")}, true); err != nil {
		t.Errorf("adding new user with legacyAdmin = %v", err)
	}
	if err := domain.EnsureActivePasswordUserAfterDelete(users, "admin", false); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("deleting last active user = %v, want ErrConflict", err)
	}
	// Stale legacyAdminActive must NOT allow disabling or deleting the only remaining user once admin was deleted.
	onlyAlice := map[string]domain.User{"user:a@example.com": {ID: "user:a@example.com", Hash: []byte("h")}}
	if err := domain.EnsureActivePasswordUserAfterUpdate(onlyAlice, domain.User{ID: "user:a@example.com", Disabled: true, Hash: []byte("h")}, true); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("disabling last remaining user after admin deletion = %v, want ErrConflict", err)
	}
	if err := domain.EnsureActivePasswordUserAfterDelete(onlyAlice, "user:a@example.com", true); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("deleting last remaining user after admin deletion = %v, want ErrConflict", err)
	}
}
