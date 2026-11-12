package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrInvalidRefresh is returned for a refresh schedule that cannot run.
var ErrInvalidRefresh = errors.New("invalid refresh")

const (
	// MinRefreshEvery is the shortest refresh interval.
	MinRefreshEvery = time.Hour
	// MaxRefreshDatabases bounds the databases of one source.
	MaxRefreshDatabases = 20
)

var (
	refreshHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
	refreshUser = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@$+-]{0,127}$`)
	refreshDB   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.$-]{0,62}$`)
)

// RefreshSource is the production database a base is refreshed from.
type RefreshSource struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// Password is a secret: it is passed to the dump tool by environment
	// and never shown by the API.
	Password  string   `json:"password,omitempty"`
	Databases []string `json:"databases"`
}

// RefreshRun is the outcome of one refresh.
type RefreshRun struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
}

// BaseRefresh is the schedule that reloads a base from its source.
type BaseRefresh struct {
	Base   Name          `json:"base"`
	Source RefreshSource `json:"source"`
	// Every is the interval between runs.
	Every     time.Duration `json:"every"`
	UpdatedAt time.Time     `json:"updated_at"`
	LastRun   *RefreshRun   `json:"last_run,omitempty"`
}

// Validate checks the schedule against the base's engine.
func (r BaseRefresh) Validate(engine Engine) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidRefresh, fmt.Sprintf(format, args...))
	}
	if engine != Postgres && engine != MySQL {
		return bad("engine %q cannot be refreshed", engine)
	}
	if r.Every < MinRefreshEvery {
		return bad("every %s is too short; at least %s", r.Every, MinRefreshEvery)
	}
	s := r.Source
	switch {
	case !refreshHost.MatchString(s.Host):
		return bad("host %q is not a host name or address", s.Host)
	case s.Port < 1 || s.Port > 65535:
		return bad("port %d is out of range", s.Port)
	case !refreshUser.MatchString(s.User):
		return bad("user %q is not valid", s.User)
	case len(s.Databases) == 0:
		return bad("list the databases to copy")
	case len(s.Databases) > MaxRefreshDatabases:
		return bad("at most %d databases", MaxRefreshDatabases)
	}
	for i := 0; i < len(s.Password); i++ {
		if s.Password[i] == 0 {
			return bad("the password contains a NUL byte")
		}
	}
	seen := map[string]bool{}
	for _, d := range s.Databases {
		if !refreshDB.MatchString(d) {
			return bad("database %q is not valid", d)
		}
		if seen[d] {
			return bad("database %q is listed twice", d)
		}
		seen[d] = true
	}
	return nil
}

// NextRun is when the next run is due: one interval after the later of the
// last run's end and the last change to the schedule.
func (r BaseRefresh) NextRun() time.Time {
	from := r.UpdatedAt
	if r.LastRun != nil && r.LastRun.FinishedAt.After(from) {
		from = r.LastRun.FinishedAt
	}
	return from.Add(r.Every)
}

// Due reports whether a run should start at now.
func (r BaseRefresh) Due(now time.Time) bool { return !now.Before(r.NextRun()) }

// BaseRefreshStore keeps the refresh schedules, apart from the bases so that
// updating a base never touches a schedule.
type BaseRefreshStore interface {
	// GetBaseRefresh fails with ErrNotFound when the base has no schedule.
	GetBaseRefresh(ctx context.Context, base Name) (BaseRefresh, error)
	ListBaseRefreshes(ctx context.Context) ([]BaseRefresh, error)
	// SetBaseRefresh stores the schedule; the last run is kept from the stored one.
	SetBaseRefresh(ctx context.Context, r BaseRefresh) error
	// RecordBaseRefreshRun stores the outcome of a run; a missing schedule is ErrNotFound.
	RecordBaseRefreshRun(ctx context.Context, base Name, run RefreshRun) error
	// ClearBaseRefresh removes the schedule; a missing one is ErrNotFound.
	ClearBaseRefresh(ctx context.Context, base Name) error
}

// SourceDumpJob asks a SourceDumper to dump the source databases.
type SourceDumpJob struct {
	// Tools is the base runtime's client tools (BinDir, LibDir, Engine).
	Tools  AdminAccess
	Source RefreshSource
}

// SourceDumper dumps production databases with the runtime's own client tools.
type SourceDumper interface {
	// Dump writes one SQL script (readable only by its owner) that recreates the
	// databases and returns its path. Nothing stays behind on failure.
	Dump(ctx context.Context, job SourceDumpJob) (path string, err error)
	// Discard deletes a dump. It never fails the caller.
	Discard(path string)
}
