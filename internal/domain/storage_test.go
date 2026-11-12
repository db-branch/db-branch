package domain_test

import (
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestSnapshot_ZFSの表記で表す(t *testing.T) {
	s := domain.Snapshot{Dataset: "tank/branches/x", Name: "reset"}
	if got := s.String(); got != "tank/branches/x@reset" {
		t.Errorf("String() = %q", got)
	}
}
