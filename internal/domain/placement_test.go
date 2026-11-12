package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func up(name string, memMB uint64) domain.Node {
	return domain.Node{Name: name, LastSeen: now, MemoryMB: memMB}
}

func on(node string, n int) []domain.Branch {
	out := make([]domain.Branch, n)
	for i := range out {
		out[i] = domain.Branch{Node: node}
	}
	return out
}

func place(t *testing.T, p domain.Placement) string {
	t.Helper()
	got, err := p.Choose()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPlacement_スナップショットを持つノードのうちブランチの少ないノードを選ぶ(t *testing.T) {
	p := domain.Placement{
		Nodes:      []domain.Node{up("a", 8192), up("b", 8192), up("c", 8192)},
		Branches:   append(on("a", 1), on("b", 3)...),
		Candidates: []string{"a", "b"},
		BranchMB:   384, Now: now, DownAfter: 30 * time.Second,
	}
	if got := place(t, p); got != "a" {
		t.Errorf("chose %s, want a (c has no snapshot, b is busier)", got)
	}
}

func TestPlacement_同数なら名前順で決まる(t *testing.T) {
	p := domain.Placement{
		Nodes: []domain.Node{up("b", 0), up("a", 0)}, Candidates: []string{"b", "a"},
		BranchMB: 384, Now: now, DownAfter: 30 * time.Second,
	}
	if got := place(t, p); got != "a" {
		t.Errorf("chose %s", got)
	}
}

func TestPlacement_停止中とメモリ予算を超えるノードは選ばない(t *testing.T) {
	down := up("a", 8192)
	down.LastSeen = now.Add(-time.Minute)
	// 4096MB * 3/4 = 3072MB = 8 branches of 384MB.
	p := domain.Placement{
		Nodes:      []domain.Node{down, up("b", 4096), up("c", 4096)},
		Branches:   append(on("b", 8), on("c", 7)...),
		Candidates: []string{"a", "b", "c"},
		BranchMB:   384, Now: now, DownAfter: 30 * time.Second,
	}
	if got := place(t, p); got != "c" {
		t.Errorf("chose %s, want c", got)
	}
	p.Branches = append(p.Branches, domain.Branch{Node: "c"})
	if _, err := p.Choose(); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("all full: %v", err)
	}
}

func TestPlacement_メモリ量が未登録のノードは数だけで比べる(t *testing.T) {
	p := domain.Placement{Nodes: []domain.Node{up("a", 0)}, Branches: on("a", 100), Candidates: []string{"a"}, BranchMB: 384, Now: now, DownAfter: time.Minute}
	if got := place(t, p); got != "a" {
		t.Errorf("chose %s", got)
	}
}

func TestPlacement_候補が一つもなければ理由を返す(t *testing.T) {
	p := domain.Placement{Nodes: []domain.Node{up("a", 0)}, Candidates: []string{"gone"}, Now: now, DownAfter: time.Minute}
	if _, err := p.Choose(); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("err = %v", err)
	}
}

func TestPlacement_指定されたノードは候補にあり使えるときだけ受け付ける(t *testing.T) {
	p := domain.Placement{
		Nodes: []domain.Node{up("a", 0), up("b", 0)}, Candidates: []string{"a", "b"},
		Branches: on("a", 5), Now: now, DownAfter: time.Minute, Want: "a",
	}
	if got := place(t, p); got != "a" {
		t.Errorf("chose %s, want the requested a", got)
	}
	p.Want = "c"
	if _, err := p.Choose(); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("node without the snapshot: %v", err)
	}
}

func TestPlacement_ARCの上限を上げたノードにはブランチを詰め込まない(t *testing.T) {
	// 8192MB with a 4096MB ARC leaves 4096MB, below the 6144MB of MemoryShare: 10 branches of 384MB.
	big := up("a", 8192)
	big.ArcMaxMB = 4096
	plain := up("b", 8192)
	p := domain.Placement{
		Nodes: []domain.Node{big, plain}, Branches: on("a", 10), Candidates: []string{"a"},
		BranchMB: 384, Now: now, DownAfter: time.Minute,
	}
	if _, err := p.Choose(); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("11th branch on the node with the raised ARC: %v", err)
	}
	p.Branches = on("a", 9)
	if got := place(t, p); got != "a" {
		t.Errorf("10th branch: chose %s", got)
	}
	// The same node without the cap reported keeps the old 75% budget (16 branches).
	p.Nodes, p.Candidates, p.Branches = []domain.Node{plain}, []string{"b"}, on("b", 15)
	if got := place(t, p); got != "b" {
		t.Errorf("chose %s", got)
	}
}

func TestBranchBudgetMB_ARCが小さくてもOSの取り分は削らない(t *testing.T) {
	n := up("a", 8192)
	n.ArcMaxMB = 1024
	if got := n.BranchBudgetMB(); got != 6144 {
		t.Errorf("small ARC: %v", got)
	}
	n.ArcMaxMB = 9000
	if got := n.BranchBudgetMB(); got != 0 {
		t.Errorf("ARC larger than memory: %v", got)
	}
}
