package filestore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
)

func TestUserStore_作成取得更新削除と最後の1人保護(t *testing.T) {
	ctx := context.Background()
	st := &filestore.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	var _ domain.UserStore = st

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	u1 := domain.User{
		ID:        "admin",
		Name:      "admin",
		Salt:      []byte("s1"),
		Hash:      []byte("h1"),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.InsertUser(ctx, u1); err != nil {
		t.Fatalf("InsertUser u1: %v", err)
	}
	if err := st.InsertUser(ctx, u1); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate InsertUser = %v, want ErrAlreadyExists", err)
	}

	// Cannot disable or delete the sole active password user.
	dis1 := u1
	dis1.Disabled = true
	if err := st.PutUser(ctx, dis1); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("disable last user = %v, want ErrConflict", err)
	}
	if err := st.DeleteUser(ctx, "admin"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete last user = %v, want ErrConflict", err)
	}

	u2 := domain.User{
		ID:         "user:alice@example.com",
		Email:      "alice@example.com",
		Name:       "alice@example.com",
		Salt:       []byte("s2"),
		Hash:       []byte("h2"),
		MustChange: true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := st.InsertUser(ctx, u2); err != nil {
		t.Fatalf("InsertUser u2: %v", err)
	}
	list, err := st.ListUsers(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "admin" || list[1].ID != "user:alice@example.com" {
		t.Fatalf("ListUsers = %+v, err=%v", list, err)
	}
	got, err := st.GetUser(ctx, "user:alice@example.com")
	if err != nil || !got.MustChange || got.Email != "alice@example.com" {
		t.Fatalf("GetUser = %+v, err=%v", got, err)
	}
	if _, err := st.GetUser(ctx, "user:missing@example.com"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetUser missing = %v, want ErrNotFound", err)
	}

	// Now disabling u1 succeeds because u2 remains active.
	if err := st.PutUser(ctx, dis1); err != nil {
		t.Fatalf("disable u1 when u2 active = %v", err)
	}
	// Now disabling u2 must fail because u1 is disabled.
	dis2 := u2
	dis2.Disabled = true
	if err := st.PutUser(ctx, dis2); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("disable u2 when u1 disabled = %v, want ErrConflict", err)
	}
	// Delete u1 (disabled) succeeds, then deleting u2 fails.
	if err := st.DeleteUser(ctx, "admin"); err != nil {
		t.Fatalf("delete disabled admin = %v", err)
	}
	if err := st.DeleteUser(ctx, "user:missing@example.com"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
	if err := st.DeleteUser(ctx, "user:alice@example.com"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete remaining user = %v, want ErrConflict", err)
	}

	snap, err := st.Snapshot()
	if err != nil || len(snap.Users) != 1 || snap.Users[0].ID != "user:alice@example.com" {
		t.Fatalf("Snapshot.Users = %+v, err=%v", snap.Users, err)
	}
}
