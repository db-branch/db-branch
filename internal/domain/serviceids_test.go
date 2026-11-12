package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestIDProblem_同じidか_クラスタ未記録なら問題なし(t *testing.T) {
	a := domain.ServiceIDs{UID: 109, GID: 112}
	if p := domain.IDProblem(a, a); p != "" {
		t.Errorf("same ids: %q", p)
	}
	if p := domain.IDProblem(domain.ServiceIDs{UID: 1, GID: 2}, domain.ServiceIDs{}); p != "" {
		t.Errorf("cluster without record: %q", p)
	}
}

func TestIDProblem_違うidは両方の値を含む説明を返す(t *testing.T) {
	p := domain.IDProblem(domain.ServiceIDs{UID: 103, GID: 109}, domain.ServiceIDs{UID: 109, GID: 112})
	for _, want := range []string{"uid 103 / gid 109", "uid 109 / gid 112", "align-ids"} {
		if !strings.Contains(p, want) {
			t.Errorf("%q lacks %q", p, want)
		}
	}
}

func TestIDProblem_ローカルにユーザーがなければそう伝える(t *testing.T) {
	p := domain.IDProblem(domain.ServiceIDs{}, domain.ServiceIDs{UID: 109, GID: 112})
	if !strings.Contains(p, "missing") {
		t.Errorf("%q", p)
	}
}

func TestServiceIDs_Valid(t *testing.T) {
	if (domain.ServiceIDs{UID: 1}).Valid() || !(domain.ServiceIDs{UID: 1, GID: 2}).Valid() {
		t.Error("Valid needs both ids")
	}
}

func TestPlacement_idが合わないノードには置かない(t *testing.T) {
	bad := up("a", 0)
	bad.IDProblem = "ids differ"
	p := domain.Placement{Nodes: []domain.Node{bad, up("b", 0)}, Candidates: []string{"a", "b"},
		BranchMB: 384, Now: now, DownAfter: 30 * time.Second}
	if got := place(t, p); got != "b" {
		t.Errorf("chose %s", got)
	}
}

func TestPlacement_指定ノードのidが合わなければ409相当で理由を返す(t *testing.T) {
	bad := up("a", 0)
	bad.IDProblem = "ids differ"
	p := domain.Placement{Nodes: []domain.Node{bad}, Candidates: []string{"a"}, Want: "a",
		BranchMB: 384, Now: now, DownAfter: 30 * time.Second}
	_, err := p.Choose()
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "ids differ") {
		t.Errorf("err = %v", err)
	}
}

func TestStartFailure_権限エラーはErrOwnershipとして区別する(t *testing.T) {
	log := "[ERROR] File './binlog.index' not found (OS errno 13 - Permission denied)"
	err := domain.StartFailure("branch-x", log)
	if !errors.Is(err, domain.ErrOwnership) || !strings.Contains(err.Error(), "cluster status") || !strings.Contains(err.Error(), "binlog.index") {
		t.Errorf("err = %v", err)
	}
	other := domain.StartFailure("branch-x", "[ERROR] bad config")
	if errors.Is(other, domain.ErrOwnership) || !strings.Contains(other.Error(), "bad config") {
		t.Errorf("other = %v", other)
	}
}
