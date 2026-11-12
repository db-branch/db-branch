package domain_test

import (
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestTrialStartedIn_記録がなければまだ始まっていない(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if got := domain.TrialStartedIn(map[string]string{domain.TrialKey: "2026-11-10T12:00:00Z"}); !got.Equal(at) {
		t.Errorf("recorded: %s", got)
	}
	if !domain.TrialStartedIn(nil).IsZero() || !domain.TrialStartedIn(map[string]string{domain.TrialKey: "soon"}).IsZero() {
		t.Error("nothing recorded: not begun")
	}
}
