package dbbranch_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

func TestCreateBranch_所有者IDと表示名の両方を保存する(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name("prod"), Runtime: mysql84}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.CreateBranch(ctx, dbbranch.CreateBranchRequest{
		Name: name("feat"), Base: name("prod"), Owner: "sso:alice", OwnerName: "alice@example.com",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Owner != "sso:alice" || b.OwnerName != "alice@example.com" {
		t.Errorf("created = %+v", b)
	}
	got, _ := e.svc.GetBranch(ctx, name("feat"))
	if got.Owner != "sso:alice" || got.OwnerName != "alice@example.com" {
		t.Errorf("stored = %+v", got)
	}
}

func TestMigrateBranchOwners_既存ブランチの所有者をトークン情報から不変IDへ安全に移行する(t *testing.T) {
	store := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	t1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)

	// Tokens:
	// - service token "ci" created BEFORE branch -> unambiguous match
	// - service token "admin" created BEFORE branch -> must NOT steal built-in "admin"
	// - personal token for alice ("oidc:alice" / "alice@example.com") -> unambiguous match
	// - duplicate service tokens named "dup" -> ambiguous, must NOT guess
	// - service token "late" created AFTER branch -> must NOT hijack pre-existing branch
	// - personal token AND service token both named "clash" -> ambiguous, must NOT guess
	// - service token "tok-existing" with ID "exist1" for branch already storing "token:exist1"
	for _, tok := range []domain.APIToken{
		{ID: "ci01", Name: "ci", Scope: domain.ScopeBranches, CreatedAt: t1},
		{ID: "adm01", Name: "admin", Scope: domain.ScopeBranches, CreatedAt: t1},
		{ID: "per01", Name: "laptop", Scope: domain.ScopeBranches, Owner: "oidc:alice", OwnerName: "alice@example.com", CreatedAt: t3},
		{ID: "dup01", Name: "dup", Scope: domain.ScopeBranches, CreatedAt: t1},
		{ID: "dup02", Name: "dup", Scope: domain.ScopeBranches, CreatedAt: t1},
		{ID: "late01", Name: "late", Scope: domain.ScopeBranches, CreatedAt: t3},
		{ID: "cl01", Name: "clash", Scope: domain.ScopeBranches, CreatedAt: t1},
		{ID: "cl02", Name: "my-tok", Scope: domain.ScopeBranches, Owner: "sso:clash", OwnerName: "clash", CreatedAt: t1},
		{ID: "exist1", Name: "ci-runner", Scope: domain.ScopeBranches, CreatedAt: t1},
	} {
		if err := store.PutToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}

	branches := []domain.Branch{
		{Name: name("legacy-none"), Base: name("prod"), Status: domain.BranchReady, CreatedAt: t2},
		{Name: name("already"), Base: name("prod"), Status: domain.BranchReady, Owner: "sso:bob", OwnerName: "bob", CreatedAt: t2},
		{Name: name("by-admin"), Base: name("prod"), Status: domain.BranchReady, Owner: "admin", CreatedAt: t2},
		{Name: name("by-local"), Base: name("prod"), Status: domain.BranchReady, Owner: "local", CreatedAt: t2},
		{Name: name("by-anon"), Base: name("prod"), Status: domain.BranchReady, Owner: "anonymous", Access: domain.AccessOpen, CreatedAt: t2},
		{Name: name("by-ci"), Base: name("prod"), Status: domain.BranchReady, Owner: "ci", CreatedAt: t2},
		{Name: name("by-alice"), Base: name("prod"), Status: domain.BranchReady, Owner: "alice@example.com", CreatedAt: t2},
		{Name: name("by-alice-id"), Base: name("prod"), Status: domain.BranchReady, Owner: "oidc:alice", CreatedAt: t2},
		{Name: name("by-dup"), Base: name("prod"), Status: domain.BranchReady, Owner: "dup", CreatedAt: t2},
		{Name: name("by-late"), Base: name("prod"), Status: domain.BranchReady, Owner: "late", CreatedAt: t2},
		{Name: name("by-clash"), Base: name("prod"), Status: domain.BranchReady, Owner: "clash", CreatedAt: t2},
		{Name: name("by-unknown"), Base: name("prod"), Status: domain.BranchReady, Owner: "ghost@example.com", CreatedAt: t2},
		{Name: name("by-tokid"), Base: name("prod"), Status: domain.BranchReady, Owner: "token:exist1", CreatedAt: t2},
		{Name: name("by-gone-tokid"), Base: name("prod"), Status: domain.BranchReady, Owner: "token:deleted", CreatedAt: t2},
	}
	for _, b := range branches {
		if err := store.PutBranch(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	if err := dbbranch.MigrateBranchOwners(ctx, store, store, store); err != nil {
		t.Fatal(err)
	}

	want := map[string][2]string{
		"legacy-none":   {"", ""},
		"already":       {"sso:bob", "bob"},
		"by-admin":      {"admin", "admin"},
		"by-local":      {"local", "local"},
		"by-anon":       {"anonymous", "anonymous"},
		"by-ci":         {"token:ci01", "ci"},
		"by-alice":      {"oidc:alice", "alice@example.com"},
		"by-alice-id":   {"oidc:alice", "alice@example.com"},
		"by-dup":        {"dup", ""},
		"by-late":       {"late", ""},
		"by-clash":      {"clash", ""},
		"by-unknown":    {"ghost@example.com", ""},
		"by-tokid":      {"token:exist1", "ci-runner"},
		"by-gone-tokid": {"token:deleted", "token:deleted"},
	}
	for n, w := range want {
		got, err := store.GetBranch(ctx, name(n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if got.Owner != w[0] || got.OwnerName != w[1] {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", n, got.Owner, got.OwnerName, w[0], w[1])
		}
	}

	// Creating a new service token named "ghost@example.com" later and re-running
	// migration must NOT hijack "by-unknown" (the token is newer than the branch).
	_ = store.PutToken(ctx, domain.APIToken{ID: "evil99", Name: "ghost@example.com", Scope: domain.ScopeBranches, CreatedAt: t3})
	if err := dbbranch.MigrateBranchOwners(ctx, store, store, store); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetBranch(ctx, name("by-unknown")); got.Owner != "ghost@example.com" || got.OwnerName != "" {
		t.Errorf("sealed branch was hijacked on re-run: %+v", got)
	}
}

type failingMigrator struct{ err error }

func (f failingMigrator) MigrateBranchOwner(context.Context, domain.Name, string, string) error {
	return f.err
}

type failingTokenStore struct{ err error }

func (f failingTokenStore) ListTokens(context.Context) ([]domain.APIToken, error) { return nil, f.err }
func (f failingTokenStore) PutToken(context.Context, domain.APIToken) error       { return f.err }
func (f failingTokenStore) DeleteToken(context.Context, string) error             { return f.err }

func TestMigrateBranchOwners_移行不要時とエラー伝播を扱う(t *testing.T) {
	store := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	// No pending branches: nil migrator / failing token store are not touched.
	_ = store.PutBranch(ctx, domain.Branch{Name: name("ok"), Base: name("prod"), Owner: "admin", OwnerName: "admin"})
	if err := dbbranch.MigrateBranchOwners(ctx, store, nil, failingTokenStore{err: errors.New("boom")}); err != nil {
		t.Errorf("nothing to migrate: %v", err)
	}

	// Pending branch with failing migrator returns error (e.g. ErrClusterUpgrading).
	_ = store.PutBranch(ctx, domain.Branch{Name: name("old"), Base: name("prod"), Owner: "admin"})
	if err := dbbranch.MigrateBranchOwners(ctx, store, failingMigrator{err: domain.ErrClusterUpgrading}, nil); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("upgrading: got %v, want ErrClusterUpgrading", err)
	}
	// Failing token store returns error when pending branches exist.
	if err := dbbranch.MigrateBranchOwners(ctx, store, store, failingTokenStore{err: errors.New("tokens down")}); err == nil {
		t.Error("failing token store: want error")
	}
	// Nil migrator when pending branches exist is a no-op.
	if err := dbbranch.MigrateBranchOwners(ctx, store, nil, nil); err != nil {
		t.Errorf("nil migrator: %v", err)
	}
	// Non-upgrading migrator error is collected and returned.
	if err := dbbranch.MigrateBranchOwners(ctx, store, failingMigrator{err: errors.New("raft down")}, nil); err == nil || errors.Is(err, domain.ErrClusterUpgrading) {
		t.Errorf("migrator error = %v", err)
	}
	// Personal token with empty OwnerName still resolves b.Owner == t.Owner to b.Owner.
	_ = store.PutToken(ctx, domain.APIToken{ID: "pt01", Name: "t", Scope: domain.ScopeBranches, Owner: "sso:noname"})
	_ = store.PutBranch(ctx, domain.Branch{Name: name("noname"), Base: name("prod"), Owner: "sso:noname"})
	if err := dbbranch.MigrateBranchOwners(ctx, store, store, store); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetBranch(ctx, name("noname")); got.Owner != "sso:noname" || got.OwnerName != "sso:noname" {
		t.Errorf("noname = %+v", got)
	}
	// Store ListBranches error is returned.
	if err := dbbranch.MigrateBranchOwners(ctx, failingBranchStore{err: errors.New("store down")}, store, nil); err == nil {
		t.Error("store error: want error")
	}
}

type failingBranchStore struct {
	domain.Store
	err error
}

func (f failingBranchStore) ListBranches(context.Context) ([]domain.Branch, error) {
	return nil, f.err
}
