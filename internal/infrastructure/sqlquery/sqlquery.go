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

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// Target is where and as whom to run.
type Target struct {
	Engine   string // "mysql" or "postgres"
	Host     string
	Port     int
	Database string
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
	if err := domain.ValidateDatabaseName(t.Database); err != nil {
		return nil, err
	}
	open := r.Open
	if open == nil {
		open = sql.Open
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	switch t.Engine {
	case "mysql":
		c := mysql.NewConfig()
		c.User, c.Passwd, c.Net, c.Addr = t.User, t.Password, "tcp", addr
		c.DBName = t.Database
		c.Timeout = 10 * time.Second
		return open("mysql", c.FormatDSN())
	case "postgres":
		db := t.Database
		if db == "" {
			db = "postgres"
		}
		u := url.URL{Scheme: "postgres", User: url.UserPassword(t.User, t.Password), Host: addr, Path: "/" + db, RawQuery: "sslmode=disable&connect_timeout=10"}
		return open("pgx", u.String())
	}
	return nil, fmt.Errorf("unsupported engine %q", t.Engine)
}

// returnsRows guesses from the statement whether it has a result set
// (otherwise the affected row count is reported).
func returnsRows(q, engine string) bool {
	tokens := scanSQLTokens(q, engine)
	baseDepth := 0
	for len(tokens) > 0 && tokens[0].text == "(" {
		tokens = tokens[1:]
		baseDepth++
	}
	if len(tokens) == 0 {
		return false
	}
	switch strings.ToLower(tokens[0].text) {
	case "select", "show", "with", "explain", "describe", "desc", "values", "table", "pragma":
		return true
	case "insert", "update", "delete", "merge":
		for _, tok := range tokens[1:] {
			if tok.depth == baseDepth && strings.EqualFold(tok.text, "returning") {
				return true
			}
		}
	}
	return false
}

type sqlToken struct {
	text  string
	depth int
}

// scanSQLTokens extracts unquoted SQL words and parentheses with their nesting
// depth, skipping whitespace, comments, and quoted/dollar-quoted literals.
func scanSQLTokens(q, engine string) []sqlToken {
	var out []sqlToken
	depth := 0
	inCondComment := false
	i := 0
	for i < len(q) {
		c := q[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			i++
			continue
		}
		if inCondComment && strings.HasPrefix(q[i:], "*/") {
			inCondComment = false
			i += 2
			continue
		}
		if (strings.HasPrefix(q[i:], "--") && isLineCommentDash(q, i, engine)) || (c == '#' && engine != "postgres") {
			if nl := strings.IndexByte(q[i:], '\n'); nl >= 0 {
				i += nl + 1
			} else {
				break
			}
			continue
		}
		if !inCondComment && strings.HasPrefix(q[i:], "/*") {
			if engine == "mysql" && strings.HasPrefix(q[i:], "/*!") {
				inCondComment = true
				i += 3
				for d := 0; d < 5 && i < len(q) && q[i] >= '0' && q[i] <= '9'; d++ {
					i++
				}
				continue
			}
			i = skipBlockComment(q, i, engine != "mysql")
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			backslashEscape := c != '`' && (engine != "postgres" || (c == '\'' && isPGCEscape(q, i)))
			i = skipQuoted(q, i, c, backslashEscape)
			continue
		}
		if c == '$' && engine == "postgres" {
			if next, ok := skipDollarQuoted(q, i); ok {
				i = next
				continue
			}
		}
		if c == '(' {
			out = append(out, sqlToken{text: "(", depth: depth})
			depth++
			i++
			continue
		}
		if c == ')' {
			if depth > 0 {
				depth--
			}
			i++
			continue
		}
		if isWordChar(c) {
			start := i
			for i < len(q) && isWordChar(q[i]) {
				i++
			}
			out = append(out, sqlToken{text: q[start:i], depth: depth})
			continue
		}
		i++
	}
	return out
}

func isLineCommentDash(q string, i int, engine string) bool {
	if engine != "mysql" {
		return true
	}
	return i+2 >= len(q) || q[i+2] <= ' '
}

func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func isPGCEscape(q string, quoteIdx int) bool {
	if quoteIdx == 0 || (q[quoteIdx-1] != 'e' && q[quoteIdx-1] != 'E') {
		return false
	}
	if quoteIdx == 1 {
		return true
	}
	prev := q[quoteIdx-2]
	return !isWordChar(prev) && prev != '$'
}

func skipBlockComment(q string, start int, nested bool) int {
	if !nested {
		if end := strings.Index(q[start+2:], "*/"); end >= 0 {
			return start + 2 + end + 2
		}
		return len(q)
	}
	depth := 1
	i := start + 2
	for i < len(q) && depth > 0 {
		if strings.HasPrefix(q[i:], "/*") {
			depth++
			i += 2
		} else if strings.HasPrefix(q[i:], "*/") {
			depth--
			i += 2
		} else {
			i++
		}
	}
	return i
}

func skipQuoted(q string, start int, quote byte, backslashEscape bool) int {
	i := start + 1
	for i < len(q) {
		if backslashEscape && q[i] == '\\' {
			i += 2
			continue
		}
		if q[i] == quote {
			if i+1 < len(q) && q[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(q)
}

func skipDollarQuoted(q string, start int) (int, bool) {
	endTag := strings.IndexByte(q[start+1:], '$')
	if endTag < 0 {
		return 0, false
	}
	tagName := q[start+1 : start+1+endTag]
	for i := 0; i < len(tagName); i++ {
		if !isWordChar(tagName[i]) || (i == 0 && tagName[i] >= '0' && tagName[i] <= '9') {
			return 0, false
		}
	}
	tag := q[start : start+1+endTag+1]
	if end := strings.Index(q[start+len(tag):], tag); end >= 0 {
		return start + len(tag) + end + len(tag), true
	}
	return len(q), true
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
	if !returnsRows(q, t.Engine) {
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

// QueryHook is the Query of a runtime provider for the built-in engines: it
// runs one statement as the request's login on the instance's port.
func (r Runner) QueryHook() func(context.Context, domain.Server, domain.QueryRequest) (domain.QueryResult, error) {
	return func(ctx context.Context, s domain.Server, q domain.QueryRequest) (domain.QueryResult, error) {
		host := q.Host
		if host == "" {
			host = "127.0.0.1"
		}
		res, err := r.Run(ctx, Target{Engine: string(s.Runtime.Engine), Host: host, Port: s.Port, Database: q.Database, User: q.User, Password: q.Password}, q.SQL, q.MaxRows)
		if err != nil {
			return domain.QueryResult{}, err
		}
		return domain.QueryResult(res), nil
	}
}
