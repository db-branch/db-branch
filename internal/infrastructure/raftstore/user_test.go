package raftstore_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/raftstore"
	"github.com/db-branch/db-branch/internal/usecase/auth"
)

func TestProtocolUsers_プロトコル30と混在クラスタ拒否(t *testing.T) {
	if raftstore.Protocol != domain.ProtocolUsers {
		t.Fatalf("Protocol = %d, want %d", raftstore.Protocol, domain.ProtocolUsers)
	}
	for _, c := range []raftstore.Command{
		raftstore.InsertUser(domain.User{ID: "user:a@example.com"}),
		raftstore.PutUser(domain.User{ID: "user:a@example.com"}),
		raftstore.DeleteUser("user:a@example.com"),
	} {
		if got := c.Needs(); got != domain.ProtocolUsers {
			t.Errorf("%s.Needs() = %d, want %d", c.Op, got, domain.ProtocolUsers)
		}
	}

	s := newCluster(t, 1).leader(t)
	_ = s.PutNode(ctx, domain.Node{Name: "old-node", Protocol: 29, LastSeen: time.Now()})
	if err := s.InsertUser(ctx, domain.User{ID: "user:a@example.com", Email: "a@example.com", Hash: []byte("h")}); !errors.Is(err, domain.ErrClusterUpgrading) {
		t.Fatalf("InsertUser on protocol 29 cluster = %v, want ErrClusterUpgrading", err)
	}
}

func TestUserStore_クラスタ複製と最後の1人保護とSnapshot(t *testing.T) {
	c := newCluster(t, 3)
	l := c.leader(t)
	var _ domain.UserStore = l

	u1 := domain.User{ID: "admin", Name: "admin", Salt: []byte("s1"), Hash: []byte("h1"), CreatedAt: taken, UpdatedAt: taken}
	if err := l.InsertUser(ctx, u1); err != nil {
		t.Fatalf("InsertUser u1: %v", err)
	}
	if err := l.InsertUser(ctx, u1); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate InsertUser = %v, want ErrAlreadyExists", err)
	}
	if err := l.DeleteUser(ctx, "admin"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete last user = %v, want ErrConflict", err)
	}
	dis := u1
	dis.Disabled = true
	if err := l.PutUser(ctx, dis); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("disable last user = %v, want ErrConflict", err)
	}

	u2 := domain.User{ID: "user:bob@example.com", Email: "bob@example.com", Name: "bob@example.com", Salt: []byte("s2"), Hash: []byte("h2"), CreatedAt: taken, UpdatedAt: taken}
	if err := l.InsertUser(ctx, u2); err != nil {
		t.Fatalf("InsertUser u2: %v", err)
	}
	if err := l.PutUser(ctx, dis); err != nil {
		t.Fatalf("disable admin when bob active = %v", err)
	}
	eventually(t, func() bool {
		list, err := c.stores[1].ListUsers(ctx)
		return err == nil && len(list) == 2 && list[0].Disabled
	})
	if err := l.DeleteUser(ctx, "admin"); err != nil {
		t.Fatalf("delete disabled admin = %v", err)
	}
	if err := l.DeleteUser(ctx, "user:bob@example.com"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete last active bob = %v, want ErrConflict", err)
	}
}

func TestUserStore_Admin削除後は残存ユーザーを無効化も削除もできない(t *testing.T) {
	l := newCluster(t, 1).leader(t)
	_ = raftstore.AdminStore{Store: l}.Save(auth.Credentials{Hash: []byte("pw-hash"), Salt: []byte("s")})
	adm := domain.User{ID: "admin", Name: "admin", Salt: []byte("s"), Hash: []byte("pw-hash"), CreatedAt: taken, UpdatedAt: taken}
	alice := domain.User{ID: "user:a@example.com", Email: "a@example.com", Salt: []byte("s2"), Hash: []byte("h"), CreatedAt: taken, UpdatedAt: taken}
	if err := l.InsertUser(ctx, adm); err != nil {
		t.Fatal(err)
	}
	if err := l.InsertUser(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteUser(ctx, "admin"); err != nil {
		t.Fatalf("delete admin when alice active = %v", err)
	}
	dis := alice
	dis.Disabled = true
	if err := l.PutUser(ctx, dis); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("disable last user after admin deleted = %v, want ErrConflict", err)
	}
	if err := l.DeleteUser(ctx, "user:a@example.com"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delete last user after admin deleted = %v, want ErrConflict", err)
	}
}

func TestBackup_ユーザー一覧と旧Adminバックアップからの同期(t *testing.T) {
	src := newCluster(t, 1).leader(t)
	_ = src.InsertUser(ctx, domain.User{ID: "admin", Name: "admin", Salt: []byte("s1"), Hash: []byte("h1"), CreatedAt: taken, UpdatedAt: taken})
	_ = src.InsertUser(ctx, domain.User{ID: "user:alice@example.com", Email: "alice@example.com", Name: "Alice", Salt: []byte("s2"), Hash: []byte("h2"), MustChange: true, CreatedAt: taken, UpdatedAt: taken})

	var buf bytes.Buffer
	if err := src.WriteBackup(&buf, "node1", taken); err != nil {
		t.Fatal(err)
	}
	dst := newCluster(t, 1).leader(t)
	sum, err := dst.RestoreBackup(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Users != 2 {
		t.Fatalf("sum.Users = %d, want 2", sum.Users)
	}
	got, err := dst.GetUser(ctx, "user:alice@example.com")
	if err != nil || !got.MustChange || got.Name != "Alice" {
		t.Fatalf("restored user = %+v, err=%v", got, err)
	}

	// Restoring an old backup (admin only, no users field) updates existing Users["admin"].
	oldDoc, _ := json.Marshal(map[string]any{
		"format":   1,
		"taken_at": taken,
		"node":     "node1",
		"admin":    auth.Credentials{Salt: []byte("new-salt"), Hash: []byte("new-hash"), Generation: 3, UpdatedAt: taken.Add(time.Hour)},
	})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(oldDoc)
	_ = zw.Close()
	if _, err := dst.RestoreBackup(ctx, bytes.NewReader(gz.Bytes())); err != nil {
		t.Fatalf("restore legacy backup: %v", err)
	}
	adm, err := dst.GetUser(ctx, "admin")
	if err != nil || string(adm.Hash) != "new-hash" || adm.SessionGen < 3 {
		t.Fatalf("restored legacy admin into Users[admin] = %+v, err=%v", adm, err)
	}
}
