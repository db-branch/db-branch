// Package sqlquery runs one SQL statement on a branch for API clients and
// LLM agents (MCP), with a row limit and a timeout.
package sqlquery

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// Target is where and as whom to run.
type Target struct {
	Engine   string // "mysql" or "postgres"
	Host     string
	Port     int
	User     string
	Password string
}

// Result is what a statement returned.
type Result struct {
	Columns []string `json:"columns,omitempty"`
	// Rows hold values as text (NULL is nil).
	Rows      [][]*string `json:"rows,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	// Affected is set for statements that return no rows.
	Affected *int64 `json:"affected,omitempty"`
	Millis   int64  `json:"millis"`
}

// Runner runs statements.
type Runner struct {
	// Open defaults to sql.Open.
	Open    func(driver, dsn string) (*sql.DB, error)
	Timeout time.Duration
}

// MaxRows bounds what one call can return.
const MaxRows = 1000

func (r Runner) open(t Target) (*sql.DB, error) {
	open := r.Open
	if open == nil {
		open = sql.Open
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	switch t.Engine {
	case "mysql":
		c := mysql.NewConfig()
		c.User, c.Passwd, c.Net, c.Addr = t.User, t.Password, "tcp", addr
		c.Timeout = 10 * time.Second
		return open("mysql", c.FormatDSN())
	case "postgres":
		u := url.URL{Scheme: "postgres", User: url.UserPassword(t.User, t.Password), Host: addr, Path: "/postgres", RawQuery: "sslmode=disable&connect_timeout=10"}
		return open("pgx", u.String())
	}
	return nil, fmt.Errorf("unsupported engine %q", t.Engine)
}

// returnsRows guesses from the first keyword whether a statement has a
// result set (otherwise the affected row count is reported).
func returnsRows(q string) bool {
	q = strings.TrimSpace(q)
	for strings.HasPrefix(q, "/*") {
		if end := strings.Index(q, "*/"); end >= 0 {
			q = strings.TrimSpace(q[end+2:])
		} else {
			break
		}
	}
	word := strings.ToLower(strings.FieldsFunc(q+" ", func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' || r == '(' })[0])
	switch word {
	case "select", "show", "with", "explain", "describe", "desc", "values", "table", "pragma":
		return true
	}
	return false
}

// Run executes q (one statement) and returns at most maxRows rows.
func (r Runner) Run(ctx context.Context, t Target, q string, maxRows int) (Result, error) {
	if maxRows <= 0 || maxRows > MaxRows {
		maxRows = MaxRows
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	db, err := r.open(t)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	start := time.Now()
	var res Result
	if !returnsRows(q) {
		out, err := db.ExecContext(ctx, q)
		if err != nil {
			return Result{}, err
		}
		n, _ := out.RowsAffected()
		res.Affected = &n
		res.Millis = time.Since(start).Milliseconds()
		return res, nil
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	if res.Columns, err = rows.Columns(); err != nil {
		return Result{}, err
	}
	for rows.Next() {
		if len(res.Rows) == maxRows {
			res.Truncated = true
			break
		}
		raw := make([]sql.NullString, len(res.Columns))
		ptrs := make([]any, len(raw))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Result{}, err
		}
		row := make([]*string, len(raw))
		for i, v := range raw {
			if v.Valid {
				s := v.String
				row[i] = &s
			}
		}
		res.Rows = append(res.Rows, row)
	}
	res.Millis = time.Since(start).Milliseconds()
	return res, rows.Err()
}
