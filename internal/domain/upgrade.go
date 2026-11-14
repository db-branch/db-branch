package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ProtocolBranchRuntime is the cluster protocol from which a branch may run
// another runtime than its base (branch upgrade). Older nodes would rebuild or
// reset such a branch on the base runtime's binaries without noticing.
const ProtocolBranchRuntime = 16

// ErrUpgradeRefused is returned for an upgrade db-branch does not do.
var ErrUpgradeRefused = errors.New("upgrade not allowed")

// FindingLevel says how serious a pre-check or log finding is.
type FindingLevel string

const (
	LevelError   FindingLevel = "error"
	LevelWarning FindingLevel = "warning"
	LevelInfo    FindingLevel = "info"
)

// Finding is one line of an upgrade pre-check or of the server log read
// during the upgrade.
type Finding struct {
	Level   FindingLevel `json:"level"`
	Code    string       `json:"code"`
	Message string       `json:"message"`
}

// Finding codes.
const (
	CodeUpgradePath        = "upgrade_path"
	CodeRuntimeMissing     = "runtime_not_installed"
	CodeDiskSpace          = "disk_space"
	CodeNativePassword     = "native_password_users"
	CodeNativePasswordGone = "native_password_removed"
	CodeBranchUser         = "branch_user_converted"
	CodeOldPassword        = "old_password_users"
	CodePartitionEngine    = "partitions_not_innodb"
	CodeConfigInvalid      = "config_invalid"
	CodeConfigAdapted      = "config_adapted"
	CodeLogWarning         = "log_warning"
	CodeLogError           = "log_error"
	CodeLogProgress        = "log_upgrade"
)

// HasErrors reports whether any finding is an error.
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Level == LevelError {
			return true
		}
	}
	return false
}

// UpgradeStatus is how an upgrade request ended.
type UpgradeStatus string

const (
	UpgradeChecked    UpgradeStatus = "checked"     // check only: nothing changed
	UpgradeUpgraded   UpgradeStatus = "upgraded"    // the branch runs the new runtime
	UpgradeRolledBack UpgradeStatus = "rolled_back" // failed; the branch is back on the old runtime
	UpgradeFailed     UpgradeStatus = "failed"      // failed; kept as it is (no rollback)
	UpgradeRefused    UpgradeStatus = "refused"     // the pre-check found errors; nothing changed
	UpgradeAborted    UpgradeStatus = "aborted"     // failed before anything changed; the branch is as it was
)

// UpgradeResult is the result of a branch upgrade operation.
type UpgradeResult struct {
	Branch     string        `json:"branch"`
	From       RuntimeRef    `json:"from"`
	To         RuntimeRef    `json:"to"`
	Status     UpgradeStatus `json:"status"`
	CheckOnly  bool          `json:"check_only"`
	Checkpoint string        `json:"checkpoint,omitempty"`
	PreCheck   []Finding     `json:"pre_check"`
	Log        []Finding     `json:"log"`
	Error      string        `json:"error,omitempty"`
	LogExcerpt string        `json:"log_excerpt,omitempty"`
}

// UpgradeFailedPrefix starts Branch.Error of a branch left failed by an
// upgrade that was not rolled back.
const UpgradeFailedPrefix = "upgrade failed: "

// UpgradePlan is what an engine found out about moving a running server to
// another runtime.
type UpgradePlan struct {
	Findings []Finding
	// LegacyAuth: accounts use MySQL's mysql_native_password, so the new
	// server must keep that plugin loaded (8.4 disables it by default).
	LegacyAuth bool
}

// Login is a database account of a branch.
type Login struct{ User, Password string }

// UpgradeLog is what the server logged while it started on the new runtime.
type UpgradeLog struct {
	Findings []Finding
	// Excerpt is the end of the error log (also when the start failed).
	Excerpt string
}

// Upgrader is implemented by a DBServer that can move an instance to another
// runtime of its engine.
type Upgrader interface {
	// CheckUpgrade inspects the running server s (still on its old runtime).
	CheckUpgrade(ctx context.Context, s Server, to RuntimeRef, own Login) (UpgradePlan, error)
	// StartUpgraded starts s, whose Runtime is the new one, over data a
	// clean shutdown of the old runtime left, and reads its log. login is the
	// branch's own account.
	StartUpgraded(ctx context.Context, s Server, login Login) (UpgradeLog, error)
}

type version struct{ major, minor, patch int }

func (v version) less(o version) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

func parseVersion(s string) version {
	s, _, _ = strings.Cut(s, "-")
	var n [3]int
	for i, p := range strings.SplitN(s, ".", 3) {
		n[i], _ = strconv.Atoi(p)
	}
	return version{n[0], n[1], n[2]}
}

// mysqlLTS are the long-term-support series; an upgrade may not jump over one.
var mysqlLTS = []version{{5, 7, 0}, {8, 0, 0}, {8, 4, 0}, {9, 7, 0}}

// CheckUpgradePath decides whether a branch on from may move to to.
func CheckUpgradePath(from, to RuntimeRef) error {
	if from.Engine != to.Engine {
		return fmt.Errorf("%w: %s cannot become %s: an upgrade stays within one engine", ErrUpgradeRefused, from, to)
	}
	a, b := parseVersion(from.Version), parseVersion(to.Version)
	switch {
	case a == b:
		return fmt.Errorf("%w: the branch already runs %s", ErrUpgradeRefused, from)
	case b.less(a):
		return fmt.Errorf("%w: %s to %s is a downgrade, and downgrades are not supported; restore a checkpoint taken on the older version instead", ErrUpgradeRefused, from, to)
	}
	if to.Engine == Postgres {
		return checkPostgresPath(from, to, a, b)
	}
	return checkMySQLPath(from, to, a, b)
}

func pgMajor(v version) version {
	if v.major >= 10 {
		return version{v.major, 0, 0}
	}
	return version{v.major, v.minor, 0}
}

func checkPostgresPath(from, to RuntimeRef, a, b version) error {
	if pgMajor(a) != pgMajor(b) {
		return fmt.Errorf("%w: PostgreSQL major version upgrades (%s to %s, pg_upgrade) are not supported yet; minor upgrades within one major version, such as 17.2 to 17.5, are", ErrUpgradeRefused, from, to)
	}
	return nil
}

func checkMySQLPath(from, to RuntimeRef, a, b version) error {
	if a.major < 5 || (a.major == 5 && a.minor < 6) {
		return fmt.Errorf("%w: %s is too old; upgrades start from MySQL 5.6", ErrUpgradeRefused, from)
	}
	sa, sb := version{a.major, a.minor, 0}, version{b.major, b.minor, 0}
	for _, l := range mysqlLTS {
		if sa.less(l) && l.less(sb) {
			return fmt.Errorf("%w: %s to %s would skip the long-term-support series %d.%d; upgrade to mysql@%d.%d.x first", ErrUpgradeRefused, from, to, l.major, l.minor, l.major, l.minor)
		}
	}
	return nil
}

// MySQLSeries returns the "8.4" of a MySQL version.
func MySQLSeries(v string) string {
	p := parseVersion(v)
	return fmt.Sprintf("%d.%d", p.major, p.minor)
}

// RuntimeProp writes a runtime as a ZFS property value (which cannot hold "@").
func RuntimeProp(r RuntimeRef) string {
	if r == (RuntimeRef{}) {
		return ""
	}
	return string(r.Engine) + ":" + r.Version
}

// ParseRuntimeProp is the inverse of RuntimeProp; ok is false for "" and anything unreadable.
func ParseRuntimeProp(s string) (RuntimeRef, bool) {
	e, v, found := strings.Cut(s, ":")
	if !found {
		return RuntimeRef{}, false
	}
	r, err := ParseRuntimeRef(e + "@" + v)
	return r, err == nil
}
