// Package sqlmerge implements logical 3-way schema and row comparison and
// SQL application across temporary and maintenance MySQL / PostgreSQL instances.
package sqlmerge

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/db-branch/db-branch/internal/domain"
)

// ErrUnsafeDDL is returned when a column type, DEFAULT expression or generated
// column expression fails SEC-22 structural validation.
var ErrUnsafeDDL = errors.New("unsafe DDL expression rejected (SEC-22)")

// QuoteIdent quotes s as a SQL identifier for engine (backticks for MySQL,
// double quotes for PostgreSQL), rejecting empty strings or NUL bytes.
func QuoteIdent(engine domain.Engine, s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: identifier cannot be empty", ErrUnsafeDDL)
	}
	if strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("%w: identifier cannot contain NUL bytes", ErrUnsafeDDL)
	}
	if engine == domain.Postgres {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, nil
	}
	return "`" + strings.ReplaceAll(s, "`", "``") + "`", nil
}

// QuoteStringLiteral quotes s as a standard single-quoted SQL string literal.
func QuoteStringLiteral(engine domain.Engine, s string) (string, error) {
	if strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("%w: string literal cannot contain NUL bytes", ErrUnsafeDDL)
	}
	if engine == domain.Postgres {
		if strings.ContainsAny(s, "\\\r\n") {
			return "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`, "\n", `\n`, "\r", `\r`).Replace(s) + "'", nil
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
	}
	r := strings.NewReplacer(`\`, `\\`, `'`, `''`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)
	return "'" + r.Replace(s) + "'", nil
}

// forbiddenTokens are SQL statement keywords and dangerous server-side functions
// that must never appear inside a column type, DEFAULT expression or generated
// column expression applied as local database admin (SEC-22).
var forbiddenTokens = map[string]bool{
	"select": true, "insert": true, "update": true, "delete": true,
	"drop": true, "alter": true, "create": true, "truncate": true,
	"grant": true, "revoke": true, "copy": true, "program": true,
	"outfile": true, "dumpfile": true, "load_file": true, "load": true,
	"infile": true, "into": true, "from": true, "where": true,
	"union": true, "join": true, "exec": true, "execute": true,
	"prepare": true, "deallocate": true, "set": true, "reset": true,
	"show": true, "describe": true, "explain": true, "do": true,
	"call": true, "handler": true, "lock": true, "unlock": true,
	"shutdown": true, "kill": true, "flush": true, "install": true,
	"uninstall": true, "plugin": true, "soname": true, "function": true,
	"procedure": true, "trigger": true, "event": true, "user": true,
	"role": true, "database": true, "schema": true, "tablespace": true,
	"server": true, "wrapper": true, "sleep": true, "benchmark": true,
	"pg_sleep": true, "pg_read_file": true, "pg_read_binary_file": true,
	"pg_ls_dir": true, "pg_stat_file": true, "pg_execute_server_program": true,
	"lo_import": true, "lo_export": true, "dblink": true, "dblink_exec": true,
	"dblink_connect": true, "pg_terminate_backend": true, "pg_cancel_backend": true,
	"pg_reload_conf": true, "pg_rotate_logfile": true, "set_config": true,
	"current_setting": true, "query_to_xml": true, "table_to_xml": true,
	"cursor_to_xml": true, "database_to_xml": true, "default": true,
}

// ValidateColumnType checks that a SQL data type definition (from information_schema
// or pg_catalog.format_type) contains only valid type syntax and no statement/comment
// or expression injection (SEC-22).
func ValidateColumnType(colType string) error {
	colType = strings.TrimSpace(colType)
	if colType == "" {
		return fmt.Errorf("%w: empty column type", ErrUnsafeDDL)
	}
	return scanSafeSQL(colType, true)
}

// ValidateSQLExpression checks that a column DEFAULT expression or generated
// column expression contains no multi-statement delimiters, comments, dollar
// quotes, subqueries, or server file/program/sleep functions (SEC-22).
func ValidateSQLExpression(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return fmt.Errorf("%w: empty SQL expression", ErrUnsafeDDL)
	}
	return scanSafeSQL(expr, false)
}

func scanSafeSQL(s string, isType bool) error {
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("%w: NUL byte in SQL fragment", ErrUnsafeDDL)
	}
	parenDepth := 0
	bracketDepth := 0
	n := len(s)
	for i := 0; i < n; {
		ch := s[i]
		switch {
		case ch == '\'':
			// Single-quoted string literal (only allowed in enum(...) types or expressions)
			i++
			closed := false
			for i < n {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '\'' {
					if i+1 < n && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return fmt.Errorf("%w: unclosed string literal in %q", ErrUnsafeDDL, s)
			}
		case ch == '"':
			// Double-quoted identifier (e.g. schema-qualified type in PostgreSQL)
			i++
			closed := false
			for i < n {
				if s[i] == '"' {
					if i+1 < n && s[i+1] == '"' {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return fmt.Errorf("%w: unclosed double-quoted identifier in %q", ErrUnsafeDDL, s)
			}
		case ch == '`':
			if isType {
				return fmt.Errorf("%w: backtick not allowed in column type %q", ErrUnsafeDDL, s)
			}
			i++
			closed := false
			for i < n {
				if s[i] == '`' {
					if i+1 < n && s[i+1] == '`' {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return fmt.Errorf("%w: unclosed backtick identifier in %q", ErrUnsafeDDL, s)
			}
		case ch == ';':
			return fmt.Errorf("%w: semicolon not allowed in %q", ErrUnsafeDDL, s)
		case ch == '#':
			return fmt.Errorf("%w: hash comment not allowed in %q", ErrUnsafeDDL, s)
		case ch == '$':
			return fmt.Errorf("%w: dollar sign / dollar quoting not allowed in %q", ErrUnsafeDDL, s)
		case ch == '\\':
			return fmt.Errorf("%w: backslash command not allowed in %q", ErrUnsafeDDL, s)
		case ch == '-' && i+1 < n && s[i+1] == '-':
			return fmt.Errorf("%w: line comment not allowed in %q", ErrUnsafeDDL, s)
		case ch == '/' && i+1 < n && s[i+1] == '*':
			return fmt.Errorf("%w: block comment not allowed in %q", ErrUnsafeDDL, s)
		case ch == '*' && i+1 < n && s[i+1] == '/':
			return fmt.Errorf("%w: block comment delimiter not allowed in %q", ErrUnsafeDDL, s)
		case ch == '(':
			parenDepth++
			i++
		case ch == ')':
			parenDepth--
			if parenDepth < 0 {
				return fmt.Errorf("%w: unbalanced closing parenthesis in %q", ErrUnsafeDDL, s)
			}
			i++
		case ch == '[':
			bracketDepth++
			i++
		case ch == ']':
			bracketDepth--
			if bracketDepth < 0 {
				return fmt.Errorf("%w: unbalanced closing bracket in %q", ErrUnsafeDDL, s)
			}
			i++
		case isIdentStart(ch):
			start := i
			for i < n && isIdentPart(s[i]) {
				i++
			}
			word := strings.ToLower(s[start:i])
			if forbiddenTokens[word] {
				return fmt.Errorf("%w: forbidden keyword or function %q in %q", ErrUnsafeDDL, word, s)
			}
		default:
			i++
		}
	}
	if parenDepth != 0 || bracketDepth != 0 {
		return fmt.Errorf("%w: unbalanced parentheses or brackets in %q", ErrUnsafeDDL, s)
	}
	return nil
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9') || unicode.IsLetter(rune(b))
}
