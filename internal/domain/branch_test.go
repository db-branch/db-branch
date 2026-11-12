package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func TestBranch_有効期限を過ぎたら失効とみなす(t *testing.T) {
	cases := []struct {
		label   string
		expires *time.Time
		want    bool
	}{
		{"no ttl", nil, false},
		{"future", ptr(now.Add(time.Minute)), false},
		{"exactly now", ptr(now), true},
		{"past", ptr(now.Add(-time.Minute)), true},
	}
	for _, c := range cases {
		b := domain.Branch{ExpiresAt: c.expires}
		if got := b.Expired(now); got != c.want {
			t.Errorf("%s: Expired = %v, want %v", c.label, got, c.want)
		}
	}
}

func TestBranch_延長すると今からTTL分先が新しい期限になる(t *testing.T) {
	b := domain.Branch{ExpiresAt: ptr(now.Add(time.Hour))}

	if err := b.Extend(now, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !b.ExpiresAt.Equal(now.Add(72 * time.Hour)) {
		t.Errorf("ExpiresAt = %v", b.ExpiresAt)
	}
}

func TestBranch_延長は正のTTLだけ受け付ける(t *testing.T) {
	b := domain.Branch{}
	if err := b.Extend(now, 0); !errors.Is(err, domain.ErrInvalidTTL) {
		t.Errorf("error = %v, want ErrInvalidTTL", err)
	}
}

func TestBranch_TTLゼロは期限なしにする(t *testing.T) {
	b := domain.Branch{ExpiresAt: ptr(now)}
	b.SetTTL(now, 0)
	if b.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", b.ExpiresAt)
	}
	b.SetTTL(now, time.Hour)
	if b.ExpiresAt == nil || !b.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("ExpiresAt = %v", b.ExpiresAt)
	}
}

func TestBranch_準備完了のときだけリセットと削除ができる(t *testing.T) {
	allowed := map[domain.BranchStatus][2]bool{ // reset, delete
		domain.BranchProvisioning: {false, false},
		domain.BranchReady:        {true, true},
		domain.BranchResetting:    {false, false},
		domain.BranchRestoring:    {false, false},
		domain.BranchDeleting:     {false, false},
		domain.BranchFailed:       {false, true},
		domain.BranchLost:         {false, true},
	}
	for status, want := range allowed {
		b := domain.Branch{Status: status}
		if got := b.CanReset() == nil; got != want[0] {
			t.Errorf("%s: CanReset ok = %v, want %v", status, got, want[0])
		}
		if got := b.CanDelete() == nil; got != want[1] {
			t.Errorf("%s: CanDelete ok = %v, want %v", status, got, want[1])
		}
		if err := b.CanReset(); err != nil && !errors.Is(err, domain.ErrConflict) {
			t.Errorf("%s: CanReset error should wrap ErrConflict: %v", status, err)
		}
	}
}

func TestBranch_接続文字列を組み立てる(t *testing.T) {
	b := domain.Branch{Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}, Port: 40001, User: "branch", Password: "s3cret"}
	if got := b.DSN("192.0.2.10"); got != "mysql://branch:s3cret@192.0.2.10:40001/" {
		t.Errorf("DSN = %q", got)
	}
	b.Runtime.Engine = domain.Postgres
	if got := b.DSN("db.local"); got != "postgres://branch:s3cret@db.local:40001/postgres" {
		t.Errorf("DSN = %q", got)
	}
}

func TestBase_スナップショットがあれば最新を返す(t *testing.T) {
	b := domain.Base{}
	if _, ok := b.LatestSnapshot(); ok {
		t.Error("no snapshots yet")
	}
	b.Snapshots = []domain.BaseSnapshot{{Name: "s1", CreatedAt: now}, {Name: "s2", CreatedAt: now.Add(time.Hour)}}
	s, ok := b.LatestSnapshot()
	if !ok || s.Name != "s2" {
		t.Errorf("LatestSnapshot = %+v, %v", s, ok)
	}
	if _, ok := b.Snapshot("s1"); !ok {
		t.Error("Snapshot(s1) should exist")
	}
	if _, ok := b.Snapshot("nope"); ok {
		t.Error("Snapshot(nope) should not exist")
	}
}

func TestBase_レプリカの記録は重複せず保持ノードはベースのノードが先頭(t *testing.T) {
	b := domain.Base{Node: "a"}
	b.AddReplica("s1", "b")
	b.AddReplica("s1", "b")
	b.AddReplica("s1", "c")
	if got := b.Holders("s1"); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("holders = %v", got)
	}
	if got := b.Holders("s2"); len(got) != 1 {
		t.Errorf("unreplicated holders = %v", got)
	}
	if !b.HasReplica("s1", "c") || b.HasReplica("s2", "c") {
		t.Error("HasReplica")
	}
}

func TestBranch_作り直せるのはノードごと失われたものだけ(t *testing.T) {
	if err := (domain.Branch{Status: domain.BranchLost}).CanRecreate(); err != nil {
		t.Error(err)
	}
	if err := (domain.Branch{Status: domain.BranchReady}).CanRecreate(); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("ready: %v", err)
	}
}

func TestAccessとPublic(t *testing.T) {
	for a, ok := range map[string]bool{"": true, domain.AccessPrivate: true, domain.AccessOpen: true, "public": false} {
		if domain.ValidAccess(a) != ok {
			t.Errorf("%q", a)
		}
	}
	if (domain.Branch{Access: domain.AccessOpen}).Open() == false || (domain.Branch{}).Open() {
		t.Error("Open")
	}
	b := domain.Base{Snapshots: []domain.BaseSnapshot{{Name: "s1"}, {Name: "s2"}}}
	if !b.SetPublic("s2", true) || !b.Snapshots[1].Public || b.Snapshots[0].Public || b.SetPublic("nope", true) {
		t.Errorf("SetPublic: %+v", b.Snapshots)
	}
}

func TestBase_レプリカ先を限ると起点と指定ノードだけが欲しがり撤去中のノードは配置先から外れる(t *testing.T) {
	b := domain.Base{Node: "a"}
	b.AddReplica("s1", "b")
	b.AddReplica("s1", "c")
	if !b.Wants("zzz") || b.Draining("c") {
		t.Fatal("unset ReplicaNodes means every node wants a copy")
	}
	only := []string{"b"}
	b.ReplicaNodes = &only
	if !b.Wants("a") || !b.Wants("b") || b.Wants("c") {
		t.Errorf("Wants: a=%v b=%v c=%v", b.Wants("a"), b.Wants("b"), b.Wants("c"))
	}
	if !b.Draining("c") || b.Draining("b") || b.Draining("a") || b.Draining("d") {
		t.Error("only c holds a copy it no longer wants")
	}
	if got := b.Placeable("s1"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Placeable = %v", got)
	}
	b.RemoveReplica("c")
	if b.HoldsAny("c") || !b.HoldsAny("b") {
		t.Errorf("RemoveReplica: %v", b.Replicas)
	}
}
