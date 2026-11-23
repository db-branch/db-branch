package sqlquery_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
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

func TestQueryHook_RuntimeProviderのQueryとして従来と同じ結果を返す(t *testing.T) {
	r, dsns := runner(2)
	hook := r.QueryHook()
	srv := domain.Server{ID: "branch-x", Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}, Port: 40001}

	res, err := hook(context.Background(), srv, domain.QueryRequest{Host: "192.0.2.10", User: "u", Password: "pw", SQL: "SELECT id, name FROM t", MaxRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || !res.Truncated || res.Columns[0] != "id" {
		t.Errorf("result = %+v", res)
	}
	if len(*dsns) != 1 || !strings.Contains((*dsns)[0], "192.0.2.10:40001") || !strings.Contains((*dsns)[0], "u:pw@") {
		t.Errorf("dsn = %q", *dsns)
	}
	aff, err := hook(context.Background(), srv, domain.QueryRequest{Host: "h", User: "u", Password: "p", SQL: "DELETE FROM t"})
	if err != nil || aff.Affected == nil || *aff.Affected != 3 {
		t.Errorf("update = %+v, %v", aff, err)
	}
	if _, err := hook(context.Background(), srv, domain.QueryRequest{SQL: "fail"}); err == nil {
		t.Error("a failing statement succeeded")
	}
}

func TestQueryHook_ホストが空ならこのノードのループバックに繋ぐ(t *testing.T) {
	r, dsns := runner(1)
	srv := domain.Server{Runtime: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}, Port: 40002}
	if _, err := r.QueryHook()(context.Background(), srv, domain.QueryRequest{User: "u", Password: "p", SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*dsns)[0], "127.0.0.1:40002") {
		t.Errorf("dsn = %q", *dsns)
	}
}

func TestSQLQuery_空文字およびコメントのみ入力でpanicせずCRLFと行コメントとRETURNINGを正しく判定する(t *testing.T) {
	r, _ := runner(2)
	pg := sqlquery.Target{Engine: "postgres", Host: "127.0.0.1", Port: 40002, User: "u", Password: "p"}
	my := sqlquery.Target{Engine: "mysql", Host: "127.0.0.1", Port: 40001, User: "u", Password: "p"}

	// Empty, whitespace, and comment-only inputs must not panic.
	for _, tc := range []struct {
		target sqlquery.Target
		q      string
	}{
		{pg, ""},
		{pg, "   \r\n\t \v\f "},
		{pg, "-- only line comment"},
		{my, "# mysql comment"},
		{pg, "/* only block */"},
		{pg, "/* outer /* inner */ outer */"},
		{pg, "/* unclosed"},
		{my, "/* unclosed"},
	} {
		res, err := r.Run(ctx, tc.target, tc.q, 10)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", tc.q, err)
		}
		if res.Affected == nil || len(res.Rows) != 0 {
			t.Errorf("%q: expected Exec path (affected, no rows), got %+v", tc.q, res)
		}
	}

	// CRLF, line comments, nested block comments, and RETURNING must take the Query path.
	for _, tc := range []struct {
		target sqlquery.Target
		q      string
	}{
		{pg, "SELECT\r\n1"},
		{pg, "SELECT\r1"},
		{pg, "-- leading comment\r\nSELECT 1"},
		{my, "# mysql line comment\nSHOW TABLES"},
		{pg, "/* outer /* inner */ */ SELECT 1"},
		{my, "/* a /* b */ SELECT 1"},
		{my, "-- c1\n/* c2 */\r\n# c3\nSELECT 1"},
		{pg, "INSERT INTO t (a) VALUES (1) RETURNING id"},
		{pg, "UPDATE t SET a = 1 RETURNING *"},
		{pg, "DELETE FROM t WHERE id = 1 RETURNING id, name"},
		{pg, "UPDATE t SET a = (SELECT 1) RETURNING id"},
		{pg, "INSERT INTO t (a) VALUES ('C:\\') RETURNING id"},
		{pg, "INSERT INTO t (a) VALUES (E'it\\'s ''ok'' RETURNING') RETURNING id"},
		{my, "INSERT INTO t (a) VALUES ('it\\'s ''ok'' RETURNING') RETURNING id"},
		{my, "INSERT INTO t (a) VALUES ($$) RETURNING id"},
		{pg, "(SELECT 1)"},
		{pg, "(INSERT INTO t (a) VALUES (1) RETURNING id)"},
	} {
		res, err := r.Run(ctx, tc.target, tc.q, 10)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", tc.q, err)
		}
		if len(res.Rows) != 2 || res.Affected != nil {
			t.Errorf("%q: expected Query path (rows, nil affected), got %+v", tc.q, res)
		}
	}

	// RETURNING inside string literals, comments, or column lists must NOT trigger Query path.
	for _, tc := range []struct {
		target sqlquery.Target
		q      string
	}{
		{pg, "INSERT INTO t (returning) VALUES (1)"},
		{pg, "INSERT INTO t (a) VALUES ('RETURNING id')"},
		{pg, "INSERT INTO t (a) VALUES (1) -- RETURNING id"},
		{pg, "INSERT INTO t (a) VALUES (1) /* RETURNING id */"},
		{pg, "UPDATE t SET a = $$RETURNING id$$"},
		{pg, "UPDATE t SET a = $q$RETURNING$q$, b = $1"},
		{pg, "UPDATE t SET a = $q$unclosed"},
		{pg, "UPDATE t SET a = 'unclosed"},
		{pg, "UPDATE t SET a = $unclosed"},
		{pg, "UPDATE t SET a = foo$e'RETURNING'"},
		{pg, "INSERT INTO t (a) VALUES (E'x')"},
	} {
		res, err := r.Run(ctx, tc.target, tc.q, 10)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", tc.q, err)
		}
		if res.Affected == nil || len(res.Rows) != 0 {
			t.Errorf("%q: expected Exec path (affected, no rows), got %+v", tc.q, res)
		}
	}
}

func TestSQLQuery_PostgreSQLおよびMySQLで任意database指定クエリを実行できる(t *testing.T) {
	r, dsns := runner(1)
	if _, err := r.Run(ctx, sqlquery.Target{Engine: "postgres", Host: "192.0.2.10", Port: 5432, Database: "app_db", User: "u", Password: "p"}, "SELECT 1", 10); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix((*dsns)[0], "pgx postgres://u:p@192.0.2.10:5432/app_db?") {
		t.Errorf("postgres custom db dsn = %q", (*dsns)[0])
	}
	if _, err := r.Run(ctx, sqlquery.Target{Engine: "postgres", Host: "192.0.2.10", Port: 5432, Database: "my db", User: "u", Password: "p"}, "SELECT 1", 10); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix((*dsns)[1], "pgx postgres://u:p@192.0.2.10:5432/my%20db?") {
		t.Errorf("postgres spaced db dsn = %q", (*dsns)[1])
	}
	if _, err := r.Run(ctx, sqlquery.Target{Engine: "mysql", Host: "192.0.2.10", Port: 3306, Database: "shop_db", User: "u", Password: "p"}, "SELECT 1", 10); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix((*dsns)[2], "mysql u:p@tcp(192.0.2.10:3306)/shop_db?") {
		t.Errorf("mysql custom db dsn = %q", (*dsns)[2])
	}

	hook := r.QueryHook()
	pgSrv := domain.Server{Runtime: domain.RuntimeRef{Engine: domain.Postgres, Version: "18.6"}, Port: 40002}
	if _, err := hook(ctx, pgSrv, domain.QueryRequest{Host: "h", Database: "custom_pg", User: "u", Password: "p", SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix((*dsns)[3], "pgx postgres://u:p@h:40002/custom_pg?") {
		t.Errorf("hook postgres custom db dsn = %q", (*dsns)[3])
	}
	mySrv := domain.Server{Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}, Port: 40001}
	if _, err := hook(ctx, mySrv, domain.QueryRequest{Host: "h", Database: "custom_my", User: "u", Password: "p", SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix((*dsns)[4], "mysql u:p@tcp(h:40001)/custom_my?") {
		t.Errorf("hook mysql custom db dsn = %q", (*dsns)[4])
	}

	// Unsafe database names (DSN parameter injection, slashes, control chars, >128 bytes) must be rejected.
	for _, bad := range []string{"app?allowAllFiles=true", "app&tls=false", "app/other", "app\\other", "app#x", "app=1", "app;x", "app\n", "app\x00", strings.Repeat("a", 129)} {
		for _, eng := range []string{"mysql", "postgres"} {
			if _, err := r.Run(ctx, sqlquery.Target{Engine: eng, Host: "h", Port: 3306, Database: bad, User: "u", Password: "p"}, "SELECT 1", 10); !errors.Is(err, domain.ErrInvalidDatabase) {
				t.Errorf("%s bad database %q: got %v, want ErrInvalidDatabase", eng, bad, err)
			}
		}
	}
}
