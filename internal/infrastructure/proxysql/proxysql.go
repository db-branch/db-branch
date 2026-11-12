// Package proxysql configures the local ProxySQL through its admin interface.
package proxysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/db-branch/db-branch/internal/usecase/routing"
	"github.com/db-branch/db-branch/pkg/ext"
)

// DB is the part of *sql.DB the admin uses.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Close() error
}

// Admin applies routing configs to ProxySQL on this node.
type Admin struct {
	// Addr is the admin interface, normally 127.0.0.1:6032.
	Addr string
	// CredFile keeps the admin credentials db-branch set (0600).
	CredFile string
	// Open connects; defaults to the MySQL driver.
	Open func(user, password, addr string) (DB, error)
	// Options are the module's settings.
	Options Options
}

// Options are what the console can change about the proxy.
type Options struct {
	// NoCommentRouting drops the /*db_branch='x'*/ rules: connections reach
	// their user's branch only.
	NoCommentRouting bool
	// ServerVersion is the MySQL version the proxy reports (default 8.4.0).
	ServerVersion string
}

var (
	_ routing.Admin = (*Admin)(nil)
	_ ext.Router    = (*Admin)(nil)
)

// Route implements ext.Router: the db-branch-proxysql module serves it.
func (a *Admin) Route(ctx context.Context, c ext.RouteConfig) error { return a.Apply(ctx, c) }

// DefaultUser and DefaultPassword are what a fresh ProxySQL accepts.
const (
	DefaultUser     = "admin"
	DefaultPassword = "admin"
	adminUser       = "dbbranch"
)

type creds struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func open(user, password, addr string) (DB, error) {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = user, password, "tcp", addr
	// The admin interface has no server-side prepared statements.
	cfg.InterpolateParams = true
	cfg.AllowNativePasswords = true
	return sql.Open("mysql", cfg.FormatDSN())
}

func (a *Admin) open(user, password string) (DB, error) {
	if a.Open != nil {
		return a.Open(user, password, a.Addr)
	}
	return open(user, password, a.Addr)
}

func (a *Admin) loadCreds() (creds, error) {
	var c creds
	b, err := os.ReadFile(a.CredFile)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// connect logs in with db-branch's own credentials, taking over a fresh
// ProxySQL (default credentials, admin open on every interface) first.
func (a *Admin) connect(ctx context.Context) (DB, error) {
	c, err := a.loadCreds()
	if err == nil {
		return a.open(c.User, c.Password)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return a.takeOver(ctx)
}

func randomPassword() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 32)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

// takeOver replaces the default admin credentials with a random password
// (saved before it is set, so it cannot be lost), keeps the admin interfaces
// on localhost, and turns off the backend monitor (it needs its own user on
// every branch; db-branch knows which servers are up).
func (a *Admin) takeOver(ctx context.Context) (DB, error) {
	db, err := a.open(DefaultUser, DefaultPassword)
	if err != nil {
		return nil, err
	}
	c := creds{User: adminUser, Password: randomPassword()}
	b, _ := json.Marshal(c)
	if err := os.MkdirAll(filepath.Dir(a.CredFile), 0o755); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.WriteFile(a.CredFile, b, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	stmts := []string{
		fmt.Sprintf("UPDATE global_variables SET variable_value='%s:%s' WHERE variable_name='admin-admin_credentials'", c.User, c.Password),
		"UPDATE global_variables SET variable_value='127.0.0.1:6032' WHERE variable_name='admin-mysql_ifaces'",
		"UPDATE global_variables SET variable_value='127.0.0.1:6132' WHERE variable_name='admin-pgsql_ifaces'",
		"LOAD ADMIN VARIABLES TO RUNTIME", "SAVE ADMIN VARIABLES TO DISK",
	}
	if err := execAll(ctx, db, stmts); err != nil {
		_ = os.Remove(a.CredFile)
		db.Close()
		return nil, fmt.Errorf("take over proxysql admin: %w", err)
	}
	db.Close()
	return a.open(c.User, c.Password)
}

type stmt struct {
	query string
	args  []any
}

// Query and Args expose a statement (for tests and logs).
func (s stmt) Query() string { return s.query }
func (s stmt) Args() []any   { return s.args }

func execAll(ctx context.Context, db DB, queries []string) error {
	for _, q := range queries {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(q)[0], err)
		}
	}
	return nil
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Statements returns what Apply runs for c, in order. The backend monitors
// stay off: they need a user of their own on every branch, and db-branch
// knows which servers are up.
func Statements(c routing.Config, o Options) []stmt {
	version := o.ServerVersion
	if version == "" {
		version = "8.4.0"
	}
	if o.NoCommentRouting {
		c.MySQL.Rules, c.Postgres.Rules = nil, nil
	}
	out := []stmt{
		{query: "UPDATE global_variables SET variable_value='false' WHERE variable_name IN ('mysql-monitor_enabled', 'pgsql-monitor_enabled')"},
		{query: "UPDATE global_variables SET variable_value=? WHERE variable_name='mysql-server_version'", args: []any{version}},
	}
	out = append(out, tableStatements("mysql", c.MySQL)...)
	out = append(out, tableStatements("pgsql", c.Postgres)...)
	for _, p := range []string{"MYSQL", "PGSQL"} {
		for _, what := range []string{"VARIABLES", "SERVERS", "USERS", "QUERY RULES"} {
			out = append(out, stmt{query: "LOAD " + p + " " + what + " TO RUNTIME"}, stmt{query: "SAVE " + p + " " + what + " TO DISK"})
		}
	}
	return out
}

// tableStatements replaces one protocol's servers, users and rules
// (prefix "mysql" or "pgsql": the tables have the same columns).
func tableStatements(prefix string, t routing.Tables) []stmt {
	out := []stmt{{query: "DELETE FROM " + prefix + "_servers"}}
	for _, s := range t.Servers {
		out = append(out, stmt{"INSERT INTO " + prefix + "_servers (hostgroup_id, hostname, port) VALUES (?, ?, ?)", []any{s.Hostgroup, s.Host, s.Port}})
	}
	out = append(out, stmt{query: "DELETE FROM " + prefix + "_users"})
	for _, u := range t.Users {
		out = append(out, stmt{"INSERT INTO " + prefix + "_users (username, password, default_hostgroup, transaction_persistent, active) VALUES (?, ?, ?, 1, 1)", []any{u.Name, u.Password, u.Hostgroup}})
	}
	out = append(out, stmt{query: "DELETE FROM " + prefix + "_query_rules"})
	for _, r := range t.Rules {
		apply := 0
		if r.Apply {
			apply = 1
		}
		out = append(out, stmt{"INSERT INTO " + prefix + "_query_rules (rule_id, active, flagIN, flagOUT, match_pattern, destination_hostgroup, error_msg, apply) VALUES (?, 1, ?, ?, ?, ?, ?, ?)",
			[]any{r.ID, r.FlagIn, nullInt(r.FlagOut), r.Pattern, nullInt(r.Hostgroup), nullString(r.Error), apply}})
	}
	return out
}

// Apply replaces ProxySQL's servers, users and query rules with c and loads
// them into the running proxy.
func (a *Admin) Apply(ctx context.Context, c routing.Config) error {
	db, err := a.connect(ctx)
	if err != nil {
		return fmt.Errorf("proxysql admin %s: %w", a.Addr, err)
	}
	defer db.Close()
	for _, s := range Statements(c, a.Options) {
		if _, err := db.ExecContext(ctx, s.query, s.args...); err != nil {
			return fmt.Errorf("proxysql: %s: %w", s.query, err)
		}
	}
	return nil
}
