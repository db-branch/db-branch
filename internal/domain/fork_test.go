package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestForkSnapshotName_フォーク用スナップショット名を組み立てて判定できる(t *testing.T) {
	child, err := domain.ParseName("feature-x-try")
	if err != nil {
		t.Fatal(err)
	}
	got := domain.ForkSnapshotName(child)
	if got != "fork-feature-x-try" {
		t.Fatalf("ForkSnapshotName = %q, want %q", got, "fork-feature-x-try")
	}
	if !domain.IsForkSnapshot(got) {
		t.Errorf("IsForkSnapshot(%q) = false, want true", got)
	}
	for _, other := range []string{"", "fork-", "ckpt-a", "gone-123", "reset", "s20261109"} {
		if domain.IsForkSnapshot(other) {
			t.Errorf("IsForkSnapshot(%q) = true, want false", other)
		}
	}
}

func TestBranchCanFork_readyとpausedだけフォークできる(t *testing.T) {
	n, _ := domain.ParseName("parent")
	for _, st := range []domain.BranchStatus{domain.BranchReady, domain.BranchPaused} {
		if err := (domain.Branch{Name: n, Status: st}).CanFork(); err != nil {
			t.Errorf("status %s: CanFork() = %v, want nil", st, err)
		}
	}
	for _, st := range []domain.BranchStatus{
		domain.BranchProvisioning, domain.BranchResetting, domain.BranchRestoring,
		domain.BranchUpgrading, domain.BranchPausing, domain.BranchResuming,
		domain.BranchForking, domain.BranchDeleting, domain.BranchFailed, domain.BranchLost,
	} {
		err := (domain.Branch{Name: n, Status: st}).CanFork()
		if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), string(st)) {
			t.Errorf("status %s: CanFork() = %v, want ErrConflict naming status", st, err)
		}
	}
}

func TestBranchCanRecreate_フォークした子ブランチは別ノードで作り直せない(t *testing.T) {
	n, _ := domain.ParseName("child")
	p, _ := domain.ParseName("parent")
	b := domain.Branch{Name: n, Status: domain.BranchLost, ParentBranch: p}
	err := b.CanRecreate()
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CanRecreate() = %v, want ErrConflict", err)
	}
}

func TestBranch_フォーク関連フィールドがJSONで往復し空のときは省略される(t *testing.T) {
	if domain.BranchForking != "forking" {
		t.Fatalf("BranchForking = %q", domain.BranchForking)
	}
	if domain.ProtocolBranchFork != 25 {
		t.Fatalf("ProtocolBranchFork = %d, want 25", domain.ProtocolBranchFork)
	}
	n, _ := domain.ParseName("child")
	p, _ := domain.ParseName("parent")
	rt := domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}
	b := domain.Branch{
		Name: n, ParentBranch: p, ParentSnapshot: "fork-child",
		ForkRuntime: rt, ForkLegacyAuth: true,
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var got domain.Branch
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ParentBranch != p || got.ParentSnapshot != "fork-child" || got.ForkRuntime != rt || !got.ForkLegacyAuth {
		t.Fatalf("round-trip = %+v", got)
	}

	plain, err := json.Marshal(domain.Branch{Name: n})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"parent_branch", "parent_snapshot", "fork_runtime", "fork_legacy_auth"} {
		if strings.Contains(string(plain), key) {
			t.Errorf("plain branch JSON contains %q: %s", key, plain)
		}
	}
}
