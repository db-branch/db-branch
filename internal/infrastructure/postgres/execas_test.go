package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

// fakeDB plays the connections init SQL goes through.
type fakeDB struct {
	dsns, queries []string
	failOn        string
	openErr       error
}

type fakeConn struct{ db *fakeDB }

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }
func (c fakeConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.db.queries = append(c.db.queries, q)
	if c.db.failOn != "" && strings.Contains(q, c.db.failOn) {
		return nil, errors.New("injected failure")
	}
	return driver.RowsAffected(0), nil
}

type fakeDriver struct{ db *fakeDB }

func (d fakeDriver) Open(dsn string) (driver.Conn, error) {
	d.db.dsns = append(d.db.dsns, dsn)
	return fakeConn{d.db}, nil
}

var drivers atomic.Int32

// useFakeDB makes the server's connections go to a fake database.
func useFakeDB(f fixture) *fakeDB {
	db := &fakeDB{}
	name := fmt.Sprintf("fakepg%d", drivers.Add(1))
	sql.Register(name, fakeDriver{db})
	f.srv.Open = func(_, dsn string) (*sql.DB, error) {
		if db.openErr != nil {
			return nil, db.openErr
		}
		return sql.Open(name, dsn)
	}
	return db
}

func TestExecAs_ブランチのログインでソケットから入り_connectでデータベースを替える(t *testing.T) {
	f := newFixture(t)
	db := useFakeDB(f)
	if err := f.srv.ExecAs(ctx, f.spec, "feature-x", "pw1", "UPDATE a SET b = 1;\n\\c shop\nDELETE FROM c;\n"); err != nil {
		t.Fatal(err)
	}
	if len(db.dsns) != 2 || len(db.queries) != 2 {
		t.Fatalf("dsns %q queries %q", db.dsns, db.queries)
	}
	for i, want := range []string{"dbname='postgres'", "dbname='shop'"} {
		if !strings.Contains(db.dsns[i], want) || !strings.Contains(db.dsns[i], "user='feature-x'") || !strings.Contains(db.dsns[i], "host='/run/db-branch-postgres/base-prod'") || !strings.Contains(db.dsns[i], "port=40001") {
			t.Errorf("dsn %d = %s", i, db.dsns[i])
		}
	}
	if !strings.Contains(db.queries[0], "UPDATE a") || !strings.Contains(db.queries[1], "DELETE FROM c") {
		t.Errorf("queries = %q", db.queries)
	}
	// The admin is never used, and psql never runs the text.
	if f.sys.called("psql") {
		t.Errorf("psql ran: %q", f.sys.calls)
	}
}

func TestExecAs_空のところは流さず_値の引用符は退避する(t *testing.T) {
	f := newFixture(t)
	db := useFakeDB(f)
	if err := f.srv.ExecAs(ctx, f.spec, "o'x", "", "  \n\\c other\n\n"); err != nil {
		t.Fatal(err)
	}
	if len(db.dsns) != 0 {
		t.Errorf("a blank script connects: %q", db.dsns)
	}
	if err := f.srv.ExecAs(ctx, f.spec, "o'x", "", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if len(db.dsns) != 1 || !strings.Contains(db.dsns[0], `user='o\'x'`) {
		t.Errorf("dsns = %q", db.dsns)
	}
}

func TestExecAs_失敗はデータベース名を付けて返す(t *testing.T) {
	f := newFixture(t)
	db := useFakeDB(f)
	db.failOn = "DELETE"
	err := f.srv.ExecAs(ctx, f.spec, "feature-x", "pw1", "SELECT 1;\n\\c shop\nDELETE FROM c;")
	if err == nil || !strings.Contains(err.Error(), "database shop") {
		t.Errorf("err = %v", err)
	}
	db.openErr = errors.New("no connection")
	if err := f.srv.ExecAs(ctx, f.spec, "feature-x", "pw1", "SELECT 1"); err == nil || !strings.Contains(err.Error(), "no connection") {
		t.Errorf("open: %v", err)
	}
}

func TestCreateUser_データベースごとに持ち主を渡す(t *testing.T) {
	f := newFixture(t)
	f.sys.outputs = map[string]string{"SELECT datname FROM pg_database": "postgres\nshop\n\n"}
	if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err != nil {
		t.Fatal(err)
	}
	if !f.sys.called("-d postgres -X -q") || !f.sys.called("-d shop -X -q") {
		t.Errorf("calls = %q", f.sys.calls)
	}
	own := 0
	for _, in := range f.sys.stdin {
		if strings.Contains(in, "ALTER ROUTINE") && strings.Contains(in, "db_branch_owner") {
			own++
		}
	}
	if own != 2 {
		t.Errorf("the objects of %d databases were handed over, want 2", own)
	}
}

func TestCreateUser_失敗は返す(t *testing.T) {
	for name, fail := range map[string]string{"login": "-d postgres -X -q -v ON_ERROR_STOP=1 -f -", "list": "SELECT datname"} {
		f := newFixture(t)
		f.sys.failOn = fail
		if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	f := newFixture(t)
	f.sys.outputs = map[string]string{"SELECT datname FROM pg_database": "shop\n"}
	f.sys.failOn = "-d shop"
	if err := f.srv.CreateUser(ctx, f.spec, "feature-x", "pw1"); err == nil || !strings.Contains(err.Error(), "database shop") {
		t.Errorf("per database: %v", err)
	}
}

func TestUpdateStats_持ち主のロールを作りオブジェクトを渡してからVACUUMする(t *testing.T) {
	f := newFixture(t)
	f.sys.outputs = map[string]string{"SELECT datname FROM pg_database": "postgres\n"}
	if err := f.srv.UpdateStats(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	if len(f.sys.stdin) < 2 || !strings.Contains(f.sys.stdin[0], "CREATE ROLE db_branch_owner NOLOGIN NOSUPERUSER") || !strings.Contains(f.sys.stdin[1], "ALTER ROUTINE") {
		t.Errorf("stdin = %q", f.sys.stdin)
	}
	if !f.sys.called("vacuumdb") {
		t.Errorf("calls = %q", f.sys.calls)
	}
	for name, fail := range map[string]string{"role": "-f -", "own": "SELECT datname"} {
		g := newFixture(t)
		g.sys.failOn = fail
		if err := g.srv.UpdateStats(ctx, g.spec); err == nil || g.sys.called("vacuumdb") {
			t.Errorf("%s: err = %v, calls = %q", name, err, g.sys.calls)
		}
	}
}

var _ = domain.Postgres
