package sqlquery_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/infrastructure/sqlquery"
)

// fakeDriver answers "SELECT ..." with n rows of (id, name) and anything else
// with 3 affected rows; "fail" fails.
type fakeDriver struct{ rows int }

type fconn struct{ d *fakeDriver }
type fstmt struct {
	c *fconn
	q string
}
type frows struct {
	n, i int
}

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fconn{d}, nil }
func (c *fconn) Prepare(q string) (driver.Stmt, error) {
	if strings.Contains(q, "fail") {
		return nil, errors.New("syntax error")
	}
	return &fstmt{c, q}, nil
}
func (c *fconn) Close() error              { return nil }
func (c *fconn) Begin() (driver.Tx, error) { return nil, errors.New("no tx") }
func (s *fstmt) Close() error              { return nil }
func (s *fstmt) NumInput() int             { return 0 }
func (s *fstmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(3), nil
}
func (s *fstmt) Query([]driver.Value) (driver.Rows, error) { return &frows{n: s.c.d.rows}, nil }
func (r *frows) Columns() []string                         { return []string{"id", "name"} }
func (r *frows) Close() error                              { return nil }
func (r *frows) Next(dest []driver.Value) error {
	if r.i == r.n {
		return io.EOF
	}
	dest[0] = int64(r.i)
	if r.i%2 == 0 {
		dest[1] = "x"
	} else {
		dest[1] = nil
	}
	r.i++
	return nil
}

func runner(rows int) (sqlquery.Runner, *[]string) {
	var dsns []string
	name := "fake" + strings.Repeat("x", rows)
	func() {
		defer func() { _ = recover() }() // registered by an earlier test
		sql.Register(name, &fakeDriver{rows: rows})
	}()
	return sqlquery.Runner{Open: func(drv, dsn string) (*sql.DB, error) {
		dsns = append(dsns, drv+" "+dsn)
		return sql.Open(name, dsn)
	}}, &dsns
}

var ctx = context.Background()

func TestRun_行を文字列で返し上限で打ち切る(t *testing.T) {
	r, dsns := runner(5)
	res, err := r.Run(ctx, sqlquery.Target{Engine: "mysql", Host: "192.0.2.10", Port: 40001, User: "x", Password: "p"}, " /* c */ SELECT * FROM t", 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Columns, ",") != "id,name" || len(res.Rows) != 3 || !res.Truncated || *res.Rows[0][1] != "x" || res.Rows[1][1] != nil {
		t.Errorf("res = %+v", res)
	}
	if !strings.HasPrefix((*dsns)[0], "mysql x:p@tcp(192.0.2.10:40001)/") {
		t.Errorf("dsn %q", (*dsns)[0])
	}
}

func TestRun_行を返さない文は影響行数を返す(t *testing.T) {
	r, dsns := runner(0)
	res, err := r.Run(ctx, sqlquery.Target{Engine: "postgres", Host: "h", Port: 1, User: "u", Password: "p"}, "UPDATE t SET a = 1", 0)
	if err != nil || res.Affected == nil || *res.Affected != 3 || len(res.Rows) != 0 {
		t.Errorf("res = %+v %v", res, err)
	}
	if !strings.HasPrefix((*dsns)[0], "pgx postgres://u:p@h:1/postgres?sslmode=disable") {
		t.Errorf("dsn %q", (*dsns)[0])
	}
	if res, _ := r.Run(ctx, sqlquery.Target{Engine: "mysql"}, "WITH x AS (SELECT 1) SELECT * FROM x", 5000); res.Affected != nil {
		t.Error("WITH returns rows")
	}
}

func TestRun_失敗とエンジン違い(t *testing.T) {
	r, _ := runner(1)
	for _, q := range []string{"SELECT fail", "DELETE fail"} {
		if _, err := r.Run(ctx, sqlquery.Target{Engine: "mysql"}, q, 1); err == nil {
			t.Errorf("%s: want error", q)
		}
	}
	if _, err := r.Run(ctx, sqlquery.Target{Engine: "oracle"}, "SELECT 1", 1); err == nil {
		t.Error("engine: want error")
	}
	if _, err := (sqlquery.Runner{}).Run(ctx, sqlquery.Target{Engine: "mysql", Host: "127.0.0.1", Port: 1}, "SELECT 1", 1); err == nil {
		t.Error("unreachable: want error")
	}
	if _, err := r.Run(ctx, sqlquery.Target{Engine: "mysql"}, "/* unclosed", 1); err != nil {
		t.Errorf("odd comment: %v", err)
	}
}
