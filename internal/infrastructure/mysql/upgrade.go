package mysql

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

var _ domain.Upgrader = (*Server)(nil)

// optionRule says that a my.cnf option does not exist from series on.
type optionRule struct {
	name  string
	since string
	// to rewrites "name=value" for the new series; "" drops the line.
	to func(value string) string
	// note says what db-branch did.
	note string
}

func seriesAtLeast(series, since string) bool {
	a, b := parseSeries(series), parseSeries(since)
	return a[0] > b[0] || (a[0] == b[0] && a[1] >= b[1])
}

func parseSeries(s string) [2]int {
	major, minor, _ := strings.Cut(s, ".")
	a, _ := strconv.Atoi(major)
	b, _ := strconv.Atoi(minor)
	return [2]int{a, b}
}

// optionRules are the options this code knows to be gone. Each was checked
// with `mysqld --validate-config` of the versions named in since.
var optionRules = []optionRule{
	{name: "default_authentication_plugin", since: "8.4", note: "default_authentication_plugin was removed in 8.4 (authentication_policy replaces it); dropped"},
	{name: "expire_logs_days", since: "8.4", note: "expire_logs_days was removed in 8.4; written as binlog_expire_logs_seconds",
		to: func(v string) string {
			days, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return ""
			}
			return fmt.Sprintf("binlog_expire_logs_seconds=%d", int64(days*86400))
		}},
	{name: "master_info_repository", since: "8.4", note: "master_info_repository was removed in 8.4; dropped"},
	{name: "relay_log_info_repository", since: "8.4", note: "relay_log_info_repository was removed in 8.4; dropped"},
	{name: "transaction_write_set_extraction", since: "8.4", note: "transaction_write_set_extraction was removed in 8.4; dropped"},
	{name: "innodb_log_file_size", since: "9.6", note: "innodb_log_file_size was removed in 9.6 (innodb_redo_log_capacity replaces it); dropped"},
	{name: "innodb_log_files_in_group", since: "9.6", note: "innodb_log_files_in_group was removed in 9.6; dropped"},
}

func optionName(line string) string {
	name, _, _ := strings.Cut(line, "=")
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "loose-")
	return strings.ReplaceAll(name, "-", "_")
}

// adaptOptions rewrites extra option lines for a runtime of series.
func adaptOptions(series string, extra []string) (out, notes []string) {
	have := map[string]bool{}
	for _, l := range extra {
		have[optionName(l)] = true
	}
	for _, l := range extra {
		rule, found := optionRule{}, false
		for _, r := range optionRules {
			if optionName(l) == r.name && seriesAtLeast(series, r.since) {
				rule, found = r, true
			}
		}
		if !found {
			out = append(out, l)
			continue
		}
		notes = append(notes, rule.note)
		if rule.to == nil {
			continue
		}
		_, value, _ := strings.Cut(l, "=")
		if line := rule.to(value); line != "" && !have[optionName(line)] {
			out = append(out, line)
		}
	}
	return out, notes
}

// query runs sql as root and returns the unescaped tab separated rows.
func (m *Server) query(ctx context.Context, s domain.Server, sql string) ([][]string, error) {
	out, err := m.Run.RunInput(ctx, strings.NewReader(sql), m.bin(s, "mysql"), "--socket="+m.socket(s), "-uroot", "--batch", "--skip-column-names")
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		for i, f := range fields {
			fields[i] = unescapeBatch(f)
		}
		rows = append(rows, fields)
	}
	return rows, nil
}

// unescapeBatch reverses mysql --batch (non-raw) output escaping (\\, \t, \n, \0).
func unescapeBatch(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '0':
				b.WriteByte(0)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

const oldPasswordUsers = "SELECT user, host FROM mysql.user WHERE plugin = 'mysql_old_password' ORDER BY user, host"

// partitionedNotInnoDB lists the tables 8.0 cannot take: it has no partitioning outside InnoDB.
const partitionedNotInnoDB = "SELECT DISTINCT p.table_schema, p.table_name FROM information_schema.partitions p " +
	"JOIN information_schema.tables t ON t.table_schema = p.table_schema AND t.table_name = p.table_name " +
	"WHERE p.partition_name IS NOT NULL AND t.engine <> 'InnoDB' ORDER BY 1, 2"

const nativeUsers = "SELECT user, host FROM mysql.user WHERE plugin = 'mysql_native_password' AND account_locked = 'N' AND user NOT LIKE 'mysql.%' ORDER BY user, host"

func accountList(rows [][]string) string {
	var names []string
	for _, r := range rows {
		names = append(names, fmt.Sprintf("'%s'@'%s'", r[0], r[1]))
	}
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + fmt.Sprintf(" and %d more", len(names)-5)
	}
	return strings.Join(names, ", ")
}

// CheckUpgrade looks at the running server for what the new runtime would break.
func (m *Server) CheckUpgrade(ctx context.Context, s domain.Server, to domain.RuntimeRef, own domain.Login) (domain.UpgradePlan, error) {
	var plan domain.UpgradePlan
	add := func(level domain.FindingLevel, code, msg string) {
		plan.Findings = append(plan.Findings, domain.Finding{Level: level, Code: code, Message: msg})
	}
	series := domain.MySQLSeries(to.Version)
	from := domain.MySQLSeries(s.Runtime.Version)
	if !seriesAtLeast(from, "5.7") && seriesAtLeast(series, "5.7") {
		// 5.7 dropped the pre-4.1 password hashes (mysql_old_password).
		rows, err := m.query(ctx, s, oldPasswordUsers)
		if err != nil {
			return plan, fmt.Errorf("list accounts with old passwords: %w", err)
		}
		if len(rows) > 0 {
			add(domain.LevelError, domain.CodeOldPassword, fmt.Sprintf("%d account(s) use the old password hash (mysql_old_password), which MySQL 5.7 removed: %s. Give them a new password first: SET PASSWORD FOR ... = PASSWORD('...').", len(rows), accountList(rows)))
		}
	}
	if !seriesAtLeast(from, "8.0") && seriesAtLeast(series, "8.0") {
		rows, err := m.query(ctx, s, partitionedNotInnoDB)
		if err != nil {
			return plan, fmt.Errorf("list partitioned tables: %w", err)
		}
		if len(rows) > 0 {
			var names []string
			for _, r := range rows {
				names = append(names, fmt.Sprintf("`%s`.`%s`", r[0], r[1]))
			}
			add(domain.LevelError, domain.CodePartitionEngine, fmt.Sprintf("%d table(s) are partitioned in an engine other than InnoDB, which MySQL 8.0 does not support: %s. Convert them first: ALTER TABLE ... ENGINE = InnoDB.", len(rows), strings.Join(names, ", ")))
		}
	}
	if seriesAtLeast(series, "8.4") {
		rows, err := m.query(ctx, s, nativeUsers)
		if err != nil {
			return plan, fmt.Errorf("list accounts: %w", err)
		}
		var others [][]string
		for _, r := range rows {
			if own.User == "" || r[0] != own.User {
				others = append(others, r)
			}
		}
		switch {
		case len(rows) == 0:
		case seriesAtLeast(series, "9.0"):
			if len(others) < len(rows) {
				add(domain.LevelInfo, domain.CodeBranchUser, fmt.Sprintf("The branch's own account '%s' uses mysql_native_password; it is switched to caching_sha2_password (same password) after the upgrade.", own.User))
			}
			if len(others) > 0 {
				add(domain.LevelError, domain.CodeNativePasswordGone, fmt.Sprintf("%d account(s) use mysql_native_password, which MySQL %s removed: %s. Switch them first: ALTER USER ... IDENTIFIED WITH caching_sha2_password BY '...'.", len(others), series, accountList(others)))
			}
		default:
			plan.LegacyAuth = true
			add(domain.LevelWarning, domain.CodeNativePassword, fmt.Sprintf("%d account(s) use mysql_native_password, which MySQL %s disables by default: %s. db-branch keeps the plugin loaded on this branch so they still work; switch them to caching_sha2_password before moving to MySQL 9.0, which removes it.", len(rows), series, accountList(rows)))
		}
	}
	plan.Findings = append(plan.Findings, m.validateConfig(ctx, s, to, plan.LegacyAuth)...)
	return plan, nil
}

// validateConfig has the new mysqld read the configuration it would be started with.
func (m *Server) validateConfig(ctx context.Context, s domain.Server, to domain.RuntimeRef, legacy bool) []domain.Finding {
	next := s
	next.Runtime, next.LegacyAuth = to, legacy
	cnf, notes := m.config(next)
	var out []domain.Finding
	for _, n := range notes {
		out = append(out, domain.Finding{Level: domain.LevelInfo, Code: domain.CodeConfigAdapted, Message: "my.cnf: " + n + "."})
	}
	var kept []string
	for _, l := range strings.Split(cnf, "\n") {
		if !strings.HasPrefix(l, "log-error=") { // keep the check out of the instance's log
			kept = append(kept, l)
		}
	}
	_ = os.MkdirAll(m.dir(s), 0o750)
	file := filepath.Join(m.dir(s), "my.cnf.check")
	if err := os.WriteFile(file, []byte(strings.Join(kept, "\n")), 0o640); err != nil {
		return append(out, domain.Finding{Level: domain.LevelWarning, Code: domain.CodeConfigInvalid, Message: "Could not check the configuration: " + err.Error()})
	}
	defer os.Remove(file)
	// --validate-config exists from 8.0.16; an older mysqld would start the server. Its
	// --help --verbose reads the options and stops, failing on one it does not know.
	check := []string{"--defaults-file=" + file, "--validate-config", "--user=db-branch"}
	if !seriesAtLeast(domain.MySQLSeries(to.Version), "8.0") {
		check = []string{"--defaults-file=" + file, "--user=db-branch", "--help", "--verbose"}
	}
	if _, err := m.Run.Run(ctx, m.bin(next, "mysqld"), check...); err != nil {
		out = append(out, domain.Finding{Level: domain.LevelError, Code: domain.CodeConfigInvalid,
			Message: fmt.Sprintf("mysqld %s does not accept the configuration db-branch generates: %s", to.Version, strings.TrimSpace(err.Error()))})
	}
	return out
}

var logLine = regexp.MustCompile(`^\S+\s+\d+\s+\[(\w+)\]\s+\[(MY-\d+)\]\s+\[\w+\]\s+(.*)$`)

const maxLogFindings = 40

// upgradeFindings picks the lines of an error log that matter to an upgrade:
// warnings, errors and the server's own upgrade progress.
func upgradeFindings(log string) []domain.Finding {
	var out []domain.Finding
	seen := map[string]bool{}
	more := 0
	for _, line := range strings.Split(log, "\n") {
		m := logLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		level, code := domain.FindingLevel(""), ""
		switch {
		case m[1] == "ERROR":
			level, code = domain.LevelError, domain.CodeLogError
		case m[1] == "Warning":
			level, code = domain.LevelWarning, domain.CodeLogWarning
		case strings.Contains(strings.ToLower(m[3]), "upgrade"):
			level, code = domain.LevelInfo, domain.CodeLogProgress
		default:
			continue
		}
		msg := m[2] + ": " + strings.TrimSpace(m[3])
		if seen[msg] {
			continue
		}
		seen[msg] = true
		if len(out) >= maxLogFindings {
			more++
			continue
		}
		out = append(out, domain.Finding{Level: level, Code: code, Message: msg})
	}
	if more > 0 {
		out = append(out, domain.Finding{Level: domain.LevelInfo, Code: domain.CodeLogProgress, Message: fmt.Sprintf("%d more log lines not shown", more)})
	}
	return out
}

// StartUpgraded starts the server on the new runtime, waits for the data
// dictionary upgrade to finish and reports what the log said.
func (m *Server) StartUpgraded(ctx context.Context, s domain.Server, own domain.Login) (domain.UpgradeLog, error) {
	offset := fileSize(m.errLog(s))
	timeout := m.UpgradeTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	err := m.start(ctx, s, max(timeout, m.StartTimeout))
	logged := m.tailLogN(s, offset, 1<<20)
	res := domain.UpgradeLog{Findings: upgradeFindings(logged), Excerpt: lastBytes(logged, 4096)}
	if err != nil && logged == "" {
		logCtx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		res.Excerpt = m.startOutput(logCtx, s, offset, 4096)
		lcancel()
	}
	if err != nil {
		return res, err
	}
	if !seriesAtLeast(domain.MySQLSeries(s.Runtime.Version), "8.0") {
		// 5.x leaves the system tables as they were: mysql_upgrade brings them up to date, and
		// the server is restarted so that it runs on the new ones.
		if err := m.systemUpgrade(ctx, s); err != nil {
			return res, fmt.Errorf("mysql_upgrade: %w", err)
		}
	}
	if seriesAtLeast(domain.MySQLSeries(s.Runtime.Version), "9.0") {
		if err := m.convertOwn(ctx, s, own); err != nil {
			return res, fmt.Errorf("switch account %s to caching_sha2_password: %w", own.User, err)
		}
	}
	return res, nil
}

// systemUpgrade runs mysql_upgrade of the running server's runtime and restarts the server.
func (m *Server) systemUpgrade(ctx context.Context, s domain.Server) error {
	if _, err := m.Run.Run(ctx, m.bin(s, "mysql_upgrade"), "--socket="+m.socket(s), "-uroot"); err != nil {
		return err
	}
	if err := m.Stop(ctx, s, domain.StopClean); err != nil {
		return err
	}
	return m.Start(ctx, s)
}

// convertOwn moves the branch's own account off mysql_native_password, which
// the new server no longer has.
func (m *Server) convertOwn(ctx context.Context, s domain.Server, own domain.Login) error {
	if own.User == "" {
		return nil
	}
	if err := validateLogin(own.User, own.Password); err != nil {
		return err
	}
	rows, err := m.query(ctx, s, fmt.Sprintf(allowBackslashEscapes+"SELECT host FROM mysql.user WHERE user = %s AND plugin = 'mysql_native_password'", quoteMySQL(own.User)))
	if err != nil {
		return err
	}
	for _, r := range rows {
		sql := fmt.Sprintf(allowBackslashEscapes+"ALTER USER %s@%s IDENTIFIED WITH caching_sha2_password BY %s", quoteMySQL(own.User), quoteMySQL(r[0]), quoteMySQL(own.Password))
		if err := m.Exec(ctx, s, sql); err != nil {
			return err
		}
	}
	return nil
}

func lastBytes(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
