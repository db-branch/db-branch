package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/authfile"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/auth"
	"github.com/db-branch/db-branch/internal/usecase/token"
	"github.com/db-branch/db-branch/pkg/ext"
)

type fastHasher struct{}

func (fastHasher) Hash(pw string, salt []byte) []byte {
	h := sha256.Sum256(append(append([]byte{}, salt...), pw...))
	return h[:]
}

func TestUserHostAdapter_And_ErrorMapping(t *testing.T) {
	dir := t.TempDir()
	fs := &filestore.Store{Path: filepath.Join(dir, "state.json")}
	creds := authfile.Store{Path: filepath.Join(dir, "auth.json")}
	a := &auth.Service{Store: creds, Users: fs, Hasher: fastHasher{}, Clock: time.Now}
	if err := a.SetPassword("admin initial password"); err != nil {
		t.Fatal(err)
	}
	toks := &token.Service{Store: fs, Clock: time.Now}
	host := newUserHost(a, toks, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// CreateUser with EmptyPassword: true (SSO user), then set password and create personal token.
	u, err := host.CreateUser(ctx, ext.UserCreate{Email: "Alice@Example.com", Name: "Alice", EmptyPassword: true})
	if err != nil || u.ID != "user:alice@example.com" || u.HasPassword {
		t.Fatalf("CreateUser: u=%+v err=%v", u, err)
	}
	pw := "alice super secret pass"
	if u, err = host.UpdateUser(ctx, u.ID, ext.UserUpdate{Password: &pw}); err != nil || !u.HasPassword {
		t.Fatalf("UpdateUser set password: u=%+v err=%v", u, err)
	}
	if got, err := host.GetUser(ctx, "alice@example.com"); err != nil || got.ID != u.ID {
		t.Fatalf("GetUser: got=%+v err=%v", got, err)
	}
	list, err := host.ListUsers(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListUsers: len=%d err=%v", len(list), err)
	}

	// VerifyPassword succeeds, wrong password returns ext.ErrUnauthenticated, rate limit returns ext.RateLimitedError.
	if verified, err := host.VerifyPassword(ctx, "10.0.0.1", "alice@example.com", pw); err != nil || verified.ID != u.ID {
		t.Fatalf("VerifyPassword valid: %+v err=%v", verified, err)
	}
	for i := 0; i < 5; i++ {
		if _, err := host.VerifyPassword(ctx, "10.0.0.9", "alice@example.com", "wrong"); !errors.Is(err, ext.ErrUnauthenticated) {
			t.Fatalf("wrong pw attempt %d: %v", i, err)
		}
	}
	var rl *ext.RateLimitedError
	if _, err := host.VerifyPassword(ctx, "10.0.0.9", "alice@example.com", pw); !errors.As(err, &rl) {
		t.Fatalf("expected ext.RateLimitedError, got %v", err)
	}

	// Disabling user revokes their personal tokens even when authSvc.RevokeTokens is nil.
	sec := "alice_personal_secret"
	sh := sha256.Sum256([]byte(sec))
	if err := fs.PutToken(ctx, domain.APIToken{ID: "t-alice", Name: "t", Hash: hex.EncodeToString(sh[:]), Scope: domain.ScopeBranches, Owner: u.ID, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	disabled := true
	if _, err := host.UpdateUser(ctx, u.ID, ext.UserUpdate{Disabled: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if owned, _ := toks.ListOwned(ctx, u.ID); len(owned) != 0 {
		t.Fatalf("expected personal tokens revoked on disable, left=%d", len(owned))
	}

	// Seed another token and delete user -> token revoked.
	if err := fs.PutToken(ctx, domain.APIToken{ID: "t-alice2", Name: "t2", Hash: hex.EncodeToString(sh[:]), Scope: domain.ScopeBranches, Owner: u.ID, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := host.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if owned, _ := toks.ListOwned(ctx, u.ID); len(owned) != 0 {
		t.Fatalf("expected personal tokens revoked on delete, left=%d", len(owned))
	}

	// Verify mapUserErr sentinel translations.
	for _, tc := range []struct {
		in   error
		want error
	}{
		{domain.ErrNotFound, ext.ErrNotFound},
		{domain.ErrAlreadyExists, ext.ErrAlreadyExists},
		{domain.ErrConflict, ext.ErrConflict},
		{domain.ErrClusterUpgrading, ext.ErrClusterUpgrading},
		{domain.ErrInvalidName, ext.ErrInvalid},
		{auth.ErrWeakPassword, ext.ErrWeakPassword},
	} {
		if got := mapUserErr(tc.in); !errors.Is(got, tc.want) {
			t.Errorf("mapUserErr(%v): got %v, want %v", tc.in, got, tc.want)
		}
	}
	if newUserHost(&auth.Service{}, nil, nil) != nil {
		t.Fatal("newUserHost with nil Users should return nil")
	}
}

func TestMergeSingleNodeUsers_And_MigrateAdminIfEmpty(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	// Older state.json admin overridden by newer auth.json creds.
	merged := mergeSingleNodeUsers(
		[]domain.User{
			{ID: "admin", Name: "admin", Hash: []byte("old"), SessionGen: 1, UpdatedAt: t0},
			{ID: "user:alice@example.com", Email: "alice@example.com", Name: "Alice", UpdatedAt: t0},
		},
		auth.Credentials{Hash: []byte("new"), Generation: 3, UpdatedAt: t1, MustChange: true},
		true,
	)
	if len(merged) != 2 || merged[0].ID != "admin" || string(merged[0].Hash) != "new" || merged[0].SessionGen != 3 || !merged[0].MustChange {
		t.Fatalf("expected newer auth.json admin to win: %+v", merged)
	}

	// Synthesize admin when snap lacks admin.
	synth := mergeSingleNodeUsers(nil, auth.Credentials{Hash: []byte("only-cred"), Generation: 2, UpdatedAt: t0}, true)
	if len(synth) != 1 || synth[0].ID != "admin" || synth[0].SessionGen != 2 {
		t.Fatalf("expected synthesized admin: %+v", synth)
	}

	// migrateAdminUserIfEmpty migrates once when empty, and never recreates a deliberately deleted admin.
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir}
	a := authService(cfg)
	a.Hasher = fastHasher{}
	_ = a.Store.Save(auth.Credentials{Salt: []byte("s"), Hash: fastHasher{}.Hash("admin pass 1234", []byte("s")), Generation: 1, UpdatedAt: t0})
	migrateAdminUserIfEmpty(context.Background(), a, nil)
	users, err := a.ListUsers(context.Background())
	if err != nil || len(users) != 1 || users[0].ID != "admin" {
		t.Fatalf("expected admin migrated: %+v err=%v", users, err)
	}
	if _, err := a.CreateUser(context.Background(), auth.CreateUserInput{Email: "bob@example.com", Password: "bob secret pass"}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteUser(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	migrateAdminUserIfEmpty(context.Background(), a, nil)
	if _, err := a.GetUser(context.Background(), "admin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted admin must never be recreated, got %v", err)
	}
}
