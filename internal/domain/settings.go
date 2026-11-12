package domain

import (
	"context"
	"time"
)

// SettingsStore keeps cluster-wide settings by scope ("core", or a module's
// name). Values are strings, checked against the scope's definitions.
type SettingsStore interface {
	GetSettings(ctx context.Context, scope string) (map[string]string, error)
	PutSettings(ctx context.Context, scope string, values map[string]string) error
}

// TrialStore records when the cluster began its trial of the paid modules.
// The start is recorded once: a later StartTrial keeps the first.
type TrialStore interface {
	// TrialStarted is the zero time before the trial began.
	TrialStarted(ctx context.Context) (time.Time, error)
	// StartTrial records at unless a start is recorded, and returns the
	// recorded one.
	StartTrial(ctx context.Context, at time.Time) (time.Time, error)
}

// Where the stores keep the trial's start: a settings scope no module or
// user can have, so the settings API never reaches it.
const (
	TrialScope = "_license"
	TrialKey   = "trial_started"
)

// TrialStartedIn reads the trial's start from the settings of TrialScope.
func TrialStartedIn(v map[string]string) time.Time {
	t, _ := time.Parse(time.RFC3339, v[TrialKey])
	return t
}
