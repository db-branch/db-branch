package domain_test

import (
	"errors"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestCanPause_準備完了のブランチだけを一時停止できる(t *testing.T) {
	for _, st := range []domain.BranchStatus{domain.BranchProvisioning, domain.BranchResetting, domain.BranchRestoring, domain.BranchDeleting, domain.BranchFailed, domain.BranchLost, domain.BranchUpgrading, domain.BranchPausing, domain.BranchPaused, domain.BranchResuming} {
		if err := (domain.Branch{Name: nm("x"), Status: st}).CanPause(); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("%s: %v", st, err)
		}
	}
	if err := (domain.Branch{Name: nm("x"), Status: domain.BranchReady}).CanPause(); err != nil {
		t.Errorf("ready: %v", err)
	}
}

func TestCanResume_一時停止中のブランチだけを再開できる(t *testing.T) {
	for _, st := range []domain.BranchStatus{domain.BranchReady, domain.BranchProvisioning, domain.BranchFailed, domain.BranchLost, domain.BranchPausing, domain.BranchResuming} {
		if err := (domain.Branch{Name: nm("x"), Status: st}).CanResume(); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("%s: %v", st, err)
		}
	}
	if err := (domain.Branch{Name: nm("x"), Status: domain.BranchPaused}).CanResume(); err != nil {
		t.Errorf("paused: %v", err)
	}
}

func TestCanDelete_一時停止中のブランチも消せる_停止や再開の途中は消せない(t *testing.T) {
	if err := (domain.Branch{Name: nm("x"), Status: domain.BranchPaused}).CanDelete(); err != nil {
		t.Errorf("paused: %v", err)
	}
	for _, st := range []domain.BranchStatus{domain.BranchPausing, domain.BranchResuming} {
		if err := (domain.Branch{Name: nm("x"), Status: st}).CanDelete(); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("%s: %v", st, err)
		}
	}
}

func TestCanReset_一時停止中は再開してからにする(t *testing.T) {
	if err := (domain.Branch{Name: nm("x"), Status: domain.BranchPaused}).CanReset(); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("paused reset: %v", err)
	}
}

func TestProtocolBranchPause_一時停止は全ノードがプロトコル18になってから(t *testing.T) {
	if domain.ProtocolBranchPause != 18 {
		t.Errorf("ProtocolBranchPause = %d", domain.ProtocolBranchPause)
	}
}

func nm(s string) domain.Name {
	n, err := domain.ParseName(s)
	if err != nil {
		panic(err)
	}
	return n
}
