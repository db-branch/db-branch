package sqlmerge_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/sqlmerge"
)

func TestSEC22_識別子クォートとDEFAULT式および生成列式のSQLインジェクション防止(t *testing.T) {
	// Identifier quoting
	qMy, err := sqlmerge.QuoteIdent(domain.MySQL, "orders`--; DROP TABLE x")
	if err != nil || qMy != "`orders``--; DROP TABLE x`" {
		t.Errorf("MySQL QuoteIdent = %q, err=%v", qMy, err)
	}
	qPG, err := sqlmerge.QuoteIdent(domain.Postgres, `users"; COPY x TO PROGRAM 'id'`)
	if err != nil || qPG != `"users""; COPY x TO PROGRAM 'id'"` {
		t.Errorf("PG QuoteIdent = %q, err=%v", qPG, err)
	}
	for _, bad := range []string{"", "a\x00b"} {
		if _, err := sqlmerge.QuoteIdent(domain.MySQL, bad); err == nil {
			t.Errorf("expected QuoteIdent error for %q", bad)
		}
		if _, err := sqlmerge.QuoteIdent(domain.Postgres, bad); err == nil {
			t.Errorf("expected QuoteIdent error for %q", bad)
		}
	}
	if _, err := sqlmerge.QuoteStringLiteral(domain.MySQL, "bad\x00"); err == nil {
		t.Error("expected QuoteStringLiteral NUL error")
	}
	if lit, err := sqlmerge.QuoteStringLiteral(domain.Postgres, "line1\nline2'\\"); err != nil || !strings.HasPrefix(lit, "E'") {
		t.Errorf("PG escaped literal = %q, err=%v", lit, err)
	}
	if lit, err := sqlmerge.QuoteStringLiteral(domain.Postgres, "plain'text"); err != nil || lit != "'plain''text'" {
		t.Errorf("PG plain literal = %q, err=%v", lit, err)
	}
	if lit, err := sqlmerge.QuoteStringLiteral(domain.MySQL, "a'b\\c\n"); err != nil || lit != `'a''b\\c\n'` {
		t.Errorf("MySQL literal = %q, err=%v", lit, err)
	}

	// Safe column types
	for _, okType := range []string{
		"int", "bigint unsigned", "varchar(255)", "decimal(10,2)", "numeric(18,4)",
		"timestamp with time zone", "character varying(100)", "jsonb", "text[]",
		"enum('pending','shipped','cancelled')", `"public"."order_status"`,
	} {
		if err := sqlmerge.ValidateColumnType(okType); err != nil {
			t.Errorf("ValidateColumnType(%q) unexpected error: %v", okType, err)
		}
	}
	// Malicious column types
	for _, badType := range []string{
		"", "int\x00", "int; DROP TABLE users", "int -- comment", "int /* comment */",
		"varchar(20) DEFAULT 1; CREATE USER evil", "enum('a'); COPY t TO PROGRAM 'sh'",
		"int)", "(int", "enum('unclosed)", "int`bad`", `"unclosed`, "int]", "[int", "int */",
	} {
		if err := sqlmerge.ValidateColumnType(badType); err == nil {
			t.Errorf("ValidateColumnType(%q) should have failed", badType)
		}
	}

	// Safe DEFAULT and generated column expressions
	for _, okExpr := range []string{
		"0", "-42", "3.14", "'pending'", "'it''s; -- safe inside string'",
		"NULL", "TRUE", "FALSE", "CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP(6)",
		"now()", "gen_random_uuid()", "(price * qty)", "COALESCE(discount, 0) + 10",
		"'pending'::character varying", "'{}'::jsonb", "CASE WHEN qty > 0 THEN price ELSE 0 END",
		"nextval('orders_id_seq'::regclass)", "(`qty` * 2)", `("qty" * 2)`, "ARRAY[1, 2]",
		"`col``1` + 1", `"col""1" + 1`,
	} {
		if err := sqlmerge.ValidateSQLExpression(okExpr); err != nil {
			t.Errorf("ValidateSQLExpression(%q) unexpected error: %v", okExpr, err)
		}
	}

	// Malicious DEFAULT and generated column expressions (SEC-22)
	for _, badExpr := range []string{
		"", "1\x00",
		"1; DROP TABLE users",
		"1 -- comment",
		"1 /* block comment */",
		"1 # hash comment",
		"$$dollar quoted$$",
		"$tag$evil$tag$",
		`\! id`,
		"(SELECT password FROM mysql.user LIMIT 1)",
		"LOAD_FILE('/etc/passwd')",
		"pg_read_file('/etc/passwd')",
		"pg_read_binary_file('/etc/shadow')",
		"pg_ls_dir('/')",
		"pg_execute_server_program('id')",
		"lo_import('/etc/passwd')",
		"lo_export(1, '/tmp/x')",
		"dblink('host=evil', 'SELECT 1')",
		"SLEEP(10)",
		"pg_sleep(10)",
		"BENCHMARK(1000000, MD5('a'))",
		"set_config('log_statement', 'none', false)",
		"current_setting('data_directory')",
		"(1 + 2",
		"1 + 2)",
		"'unclosed string",
		"`unclosed backtick",
	} {
		if err := sqlmerge.ValidateSQLExpression(badExpr); err == nil {
			t.Errorf("ValidateSQLExpression(%q) should have been rejected (SEC-22)", badExpr)
		}
	}
}

// mockInstance holds catalog and table data for one mocked database server (A, S, or T).
type mockInstance struct {
	pgDBs       [][]any
	tables      [][]any
	columns     [][]any
	indexes     [][]any
	fks         [][]any
	triggers    [][]any
	enums       [][]any
	sequences   [][]any
	tableRows   map[string][][]any // "db.table" -> rows
	maxValues   map[string]int64   // "db.table" -> MAX(col)
	seqValues   map[string]int64   // "schema.seq" -> last_value
	orphanCount int64
	execErr     string
	execErrOn   string
	queryErr    string

	mu    sync.Mutex
	execs []string
}

func (m *mockInstance) recordExec(q string, args []driver.NamedValue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(args) == 0 {
		m.execs = append(m.execs, q)
		return
	}
	vals := make([]string, len(args))
	for i, a := range args {
		vals[i] = fmt.Sprintf("%v", a.Value)
	}
	m.execs = append(m.execs, fmt.Sprintf("%s [%s]", q, strings.Join(vals, ", ")))
}

type mockRegistry struct {
	mu        sync.Mutex
	instances map[string]*mockInstance
	dsns      []string
}

var globalMock = &mockRegistry{instances: map[string]*mockInstance{}}

func init() {
	sql.Register("sqlmerge-mock", globalMock)
}

func (r *mockRegistry) register(key string, inst *mockInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instances[key] = inst
}

func (r *mockRegistry) Open(dsn string) (driver.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dsns = append(r.dsns, dsn)
	bestKey := ""
	var bestInst *mockInstance
	for k, inst := range r.instances {
		if strings.Contains(dsn, k) && len(k) > len(bestKey) {
			bestKey, bestInst = k, inst
		}
	}
	if bestInst != nil {
		return &mockConn{inst: bestInst}, nil
	}
	return nil, fmt.Errorf("unknown mock dsn: %s", dsn)
}

type mockConn struct{ inst *mockInstance }

func (c *mockConn) Prepare(q string) (driver.Stmt, error) { return &mockStmt{c: c, q: q}, nil }
func (c *mockConn) Close() error                          { return nil }
func (c *mockConn) Begin() (driver.Tx, error)             { return &mockTx{}, nil }

func (c *mockConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.inst.recordExec(q, args)
	if c.inst.execErr != "" && (c.inst.execErrOn != "" && strings.Contains(q, c.inst.execErrOn) ||
		c.inst.execErrOn == "" && (strings.Contains(q, "INSERT") || strings.Contains(q, "UPDATE") || strings.Contains(q, "DELETE") || strings.Contains(q, "CREATE") || strings.Contains(q, "ALTER"))) {
		return nil, errors.New(c.inst.execErr)
	}
	return driver.RowsAffected(1), nil
}

func (c *mockConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.inst.queryErr != "" && strings.Contains(q, c.inst.queryErr) {
		return nil, errors.New("simulated query error")
	}
	switch {
	case strings.Contains(q, "/* db-branch:pg-databases */"):
		return newMockRows([]string{"datname"}, c.inst.pgDBs), nil
	case strings.Contains(q, "/* db-branch:tables */"):
		return newMockRows([]string{"s", "t", "auto"}, c.inst.tables), nil
	case strings.Contains(q, "/* db-branch:columns */"):
		return newMockRows([]string{"s", "t", "c", "ord", "dt", "ct", "null", "def", "extra", "gen"}, c.inst.columns), nil
	case strings.Contains(q, "/* db-branch:indexes */"):
		return newMockRows([]string{"s", "t", "idx", "p1", "p2", "c", "ord"}, c.inst.indexes), nil
	case strings.Contains(q, "/* db-branch:fks */"):
		return newMockRows([]string{"cs", "ct", "con", "cc", "ps", "pt", "pc", "ord"}, c.inst.fks), nil
	case strings.Contains(q, "/* db-branch:triggers */"):
		return newMockRows([]string{"s", "t", "tr"}, c.inst.triggers), nil
	case strings.Contains(q, "/* db-branch:enums */"):
		return newMockRows([]string{"s", "t", "lbl", "ord"}, c.inst.enums), nil
	case strings.Contains(q, "/* db-branch:sequences */"):
		return newMockRows([]string{"ts", "tt", "tc", "ss", "sn"}, c.inst.sequences), nil
	case strings.Contains(q, "/* db-branch:seq-val */"):
		for k, v := range c.inst.seqValues {
			if strings.Contains(q, k) || len(c.inst.seqValues) == 1 {
				return newMockRows([]string{"v"}, [][]any{{v}}), nil
			}
		}
		return newMockRows([]string{"v"}, [][]any{{int64(0)}}), nil
	case strings.Contains(q, "/* db-branch:max-col */"):
		for k, v := range c.inst.maxValues {
			parts := strings.SplitN(k, ".", 2)
			if len(parts) == 2 && strings.Contains(q, parts[1]) {
				return newMockRows([]string{"m"}, [][]any{{v}}), nil
			}
		}
		return newMockRows([]string{"m"}, [][]any{{int64(0)}}), nil
	case strings.Contains(q, "/* db-branch:set-seq */"):
		c.inst.recordExec(q, args)
		return newMockRows([]string{"v"}, [][]any{{int64(1)}}), nil
	case strings.Contains(q, "/* db-branch:verify-fk */"):
		return newMockRows([]string{"cnt"}, [][]any{{c.inst.orphanCount}}), nil
	case strings.Contains(q, "/* db-branch:scan "):
		// Extract table key from comment /* db-branch:scan db.table */
		start := strings.Index(q, "/* db-branch:scan ") + len("/* db-branch:scan ")
		end := strings.Index(q[start:], " */")
		key := q[start : start+end]
		rows := c.inst.tableRows[key]
		cols := []string{"c1", "c2", "c3", "c4"}
		if len(rows) > 0 {
			cols = make([]string, len(rows[0]))
			for i := range cols {
				cols[i] = fmt.Sprintf("c%d", i)
			}
		}
		return newMockRows(cols, rows), nil
	default:
		return newMockRows([]string{"ok"}, nil), nil
	}
}

type mockTx struct{}

func (t *mockTx) Commit() error   { return nil }
func (t *mockTx) Rollback() error { return nil }

type mockStmt struct {
	c *mockConn
	q string
}

func (s *mockStmt) Close() error                                    { return nil }
func (s *mockStmt) NumInput() int                                   { return -1 }
func (s *mockStmt) Exec(args []driver.Value) (driver.Result, error) { return driver.RowsAffected(1), nil }
func (s *mockStmt) Query(args []driver.Value) (driver.Rows, error)  { return newMockRows(nil, nil), nil }

type mockRows struct {
	cols []string
	rows [][]any
	idx  int
}

func newMockRows(cols []string, rows [][]any) *mockRows {
	if len(rows) > 0 && len(rows[0]) < len(cols) {
		cols = cols[:len(rows[0])]
	}
	return &mockRows{cols: cols, rows: rows}
}

func (r *mockRows) Columns() []string { return r.cols }
func (r *mockRows) Close() error      { return nil }
func (r *mockRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	for i := range dest {
		if i < len(row) {
			dest[i] = row[i]
		} else {
			dest[i] = nil
		}
	}
	return nil
}

func newTestMerger() *sqlmerge.Merger {
	return &sqlmerge.Merger{
		Open: func(_, dsn string) (*sql.DB, error) {
			return sql.Open("sqlmerge-mock", dsn)
		},
	}
}

func TestDiffAndApply_MySQL_3Way行マージとユニークスワップとAUTO_INCREMENT同期(t *testing.T) {
	// Schema: shop.users (id PK auto_increment, email UNIQUE, name, tag GENERATED)
	// and shop.orders (id PK, user_id FK -> shop.users.id, amount DECIMAL)
	myTables := func(usersAuto int64) [][]any {
		return [][]any{
			{"shop", "orders", int64(100)},
			{"shop", "users", usersAuto},
		}
	}
	myCols := [][]any{
		{"shop", "orders", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "orders", "user_id", 2, "int", "int", "NO", nil, "", ""},
		{"shop", "orders", "amount", 3, "decimal", "decimal(10,2)", "NO", "0.00", "", ""},
		{"shop", "users", "id", 1, "int", "int", "NO", nil, "auto_increment", ""},
		{"shop", "users", "email", 2, "varchar", "varchar(255)", "NO", nil, "", ""},
		{"shop", "users", "name", 3, "varchar", "varchar(100)", "YES", nil, "", ""},
		{"shop", "users", "tag", 4, "varchar", "varchar(100)", "YES", nil, "VIRTUAL GENERATED", "CONCAT(name, '!')"},
	}
	myIndexes := [][]any{
		{"shop", "orders", "PRIMARY", 0, 1, "id", 1},
		{"shop", "users", "PRIMARY", 0, 1, "id", 1},
		{"shop", "users", "idx_users_email", 0, 1, "email", 1},
	}
	myFKs := [][]any{
		{"shop", "orders", "fk_orders_user", "user_id", "shop", "users", "id", 1},
	}

	instA := &mockInstance{
		tables: myTables(3), columns: myCols, indexes: myIndexes, fks: myFKs,
		tableRows: map[string][][]any{
			"shop.users": {
				{int64(1), "a@example.com", "Alice"},
				{int64(2), "b@example.com", "Bob"},
			},
			"shop.orders": {
				{int64(10), int64(1), "19.99"},
				{int64(20), int64(2), "50.00"},
			},
		},
	}
	// Source swaps email of user 1 and 2, inserts user 3, updates order 10, deletes order 20, inserts order 30 for user 3.
	instS := &mockInstance{
		tables: myTables(4), columns: myCols, indexes: myIndexes, fks: myFKs,
		tableRows: map[string][][]any{
			"shop.users": {
				{int64(1), "b@example.com", "Alice"},
				{int64(2), "a@example.com", "Bob"},
				{int64(3), "c@example.com", "Carol"},
			},
			"shop.orders": {
				{int64(10), int64(1), "25.00"},
				{int64(30), int64(3), "99.95"},
			},
		},
	}
	// Target keeps users 1 and 2 untouched, adds user 4 and order 40.
	instT := &mockInstance{
		tables: myTables(5), columns: myCols, indexes: myIndexes, fks: myFKs,
		tableRows: map[string][][]any{
			"shop.users": {
				{int64(1), "a@example.com", "Alice"},
				{int64(2), "b@example.com", "Bob"},
				{int64(4), "d@example.com", "Dave"},
			},
			"shop.orders": {
				{int64(10), int64(1), "19.99"},
				{int64(20), int64(2), "50.00"},
				{int64(40), int64(4), "10.00"},
			},
		},
		maxValues: map[string]int64{"shop.users": 4},
	}
	globalMock.register("my-a.sock", instA)
	globalMock.register("my-s.sock", instS)
	globalMock.register("127.0.0.1:41003", instT)

	m := newTestMerger()
	plan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/my-a.sock", User: "root"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/my-s.sock", User: "root"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Host: "127.0.0.1", Port: 41003, User: "root"},
	})
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if plan.TotalConflicts != 0 {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}
	// users: 1 inserted (id=3), 2 updated (id=1,2 email swap); orders: 1 inserted (30), 1 updated (10), 1 deleted (20)
	if plan.Inserted != 2 || plan.Updated != 3 || plan.Deleted != 1 {
		t.Errorf("counts = +%d ~%d -%d, want +2 ~3 -1", plan.Inserted, plan.Updated, plan.Deleted)
	}

	var phases []string
	err = m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Host: "127.0.0.1", Port: 41003, User: "root"}, plan, func(p string) {
		phases = append(phases, p)
	})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	joined := strings.Join(instT.execs, "\n")
	for _, want := range []string{
		"SET FOREIGN_KEY_CHECKS = 0",
		"DELETE FROM `shop`.`orders` WHERE `id` = ?",
		// Unique column swap on users must expand to DELETE + INSERT
		"DELETE FROM `shop`.`users` WHERE `id` = ?",
		"INSERT INTO `shop`.`users` (`id`, `email`, `name`) VALUES (?, ?, ?)",
		"UPDATE `shop`.`orders` SET `amount` = ? WHERE `id` = ?",
		"SET FOREIGN_KEY_CHECKS = 1",
		"ALTER TABLE `shop`.`users` AUTO_INCREMENT = 5",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected SQL %q in executed statements:\n%s", want, joined)
		}
	}
	if len(phases) == 0 || phases[len(phases)-1] != "verifying" {
		t.Errorf("phases = %v, want verifying at end", phases)
	}
}

func TestDiffAndApply_Postgres_ENUMとDDL追加とIDENTITYとSEQUENCE同期(t *testing.T) {
	// Ancestor has public.users (id IDENTITY PK, name) and public.legacy (id PK).
	// Source adds ENUM public.order_status ('pending', 'paid'), adds column status to public.users,
	// creates table public.orders (id PK, user_id FK -> public.users.id, status order_status),
	// drops table public.legacy (which is unchanged in Target), and inserts rows.
	instA := &mockInstance{
		tables: [][]any{
			{"public", "legacy"},
			{"public", "users"},
		},
		columns: [][]any{
			{"public", "legacy", "id", 1, "integer", false, "", "", ""},
			{"public", "users", "id", 1, "bigint", false, "", "a", ""},
			{"public", "users", "name", 2, "text", false, "", "", ""},
		},
		indexes: [][]any{
			{"public", "legacy", "legacy_pkey", true, false, "id", 1},
			{"public", "users", "users_pkey", true, false, "id", 1},
		},
		sequences: [][]any{
			{"public", "users", "id", "public", "users_id_seq"},
		},
		seqValues: map[string]int64{"users_id_seq": 1},
		tableRows: map[string][][]any{
			"public.legacy": {{int64(1)}},
			"public.users":  {{int64(1), "Alice"}},
		},
	}

	instS := &mockInstance{
		tables: [][]any{
			{"public", "orders"},
			{"public", "users"},
		},
		columns: [][]any{
			{"public", "orders", "id", 1, "bigint", false, "", "a", ""},
			{"public", "orders", "user_id", 2, "bigint", false, "", "", ""},
			{"public", "orders", "status", 3, `"public"."order_status"`, false, "'pending'::order_status", "", ""},
			{"public", "users", "id", 1, "bigint", false, "", "a", ""},
			{"public", "users", "name", 2, "text", false, "", "", ""},
			{"public", "users", "role", 3, "text", true, "'member'::text", "", ""},
		},
		indexes: [][]any{
			{"public", "orders", "orders_pkey", true, false, "id", 1},
			{"public", "users", "users_pkey", true, false, "id", 1},
		},
		fks: [][]any{
			{"public", "orders", "orders_user_id_fkey", "user_id", "public", "users", "id", 1},
		},
		enums: [][]any{
			{"public", "order_status", "pending", 1},
			{"public", "order_status", "paid", 2},
		},
		sequences: [][]any{
			{"public", "users", "id", "public", "users_id_seq"},
		},
		seqValues: map[string]int64{"users_id_seq": 5},
		tableRows: map[string][][]any{
			"public.users": {
				{int64(1), "Alice", "admin"},
				{int64(2), "Bob", "member"},
			},
			"public.orders": {
				{int64(100), int64(2), "paid"},
			},
		},
	}

	instT := &mockInstance{
		tables:    instA.tables,
		columns:   instA.columns,
		indexes:   instA.indexes,
		sequences: instA.sequences,
		seqValues: map[string]int64{"users_id_seq": 3},
		tableRows: map[string][][]any{
			"public.legacy": {{int64(1)}},
			"public.users":  {{int64(1), "Alice"}},
		},
	}

	globalMock.register("pg-a", instA)
	globalMock.register("pg-s", instS)
	globalMock.register("pg-t", instT)

	m := newTestMerger()
	plan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg-a", Port: 5432, User: "postgres"},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg-s", Port: 5433, User: "postgres"},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Host: "pg-t", Port: 5434, User: "postgres"},
		AllowDDL: true,
	})
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if plan.TotalConflicts != 0 {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}
	if plan.DDL != 3 { // ALTER TABLE users ADD COLUMN role, CREATE TABLE orders, DROP TABLE legacy
		t.Errorf("DDL count = %d, want 3 (tables=%+v)", plan.DDL, plan.Tables)
	}

	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.Postgres, Host: "pg-t", Port: 5434, User: "postgres"}, plan, nil); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	joined := strings.Join(instT.execs, "\n")
	for _, want := range []string{
		`CREATE TYPE "public"."order_status" AS ENUM ('pending', 'paid')`,
		`ALTER TABLE "public"."users" ADD COLUMN "role" text DEFAULT 'member'::text`,
		`CREATE TABLE "public"."orders"`,
		`SET LOCAL session_replication_role = 'replica'`,
		`OVERRIDING SYSTEM VALUE VALUES`,
		`DROP TABLE "public"."legacy"`,
		`pg_catalog.setval`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected SQL containing %q in:\n%s", want, joined)
		}
	}
}

func TestDiff_コンフリクト検出_データ競合_ユニーク違反_NULLS_NOT_DISTINCT_外部キー_DDL拒否(t *testing.T) {
	m := newTestMerger()

	// 1. Unique violation with NULLS NOT DISTINCT vs standard nullable unique
	pgTables := [][]any{{"public", "items"}}
	pgCols := [][]any{
		{"public", "items", "id", 1, "integer", false, "", "", ""},
		{"public", "items", "code", 2, "text", true, "", "", ""},
		{"public", "items", "token", 3, "text", true, "", "", ""},
	}
	// idx_code is standard unique (nulls distinct); idx_token is NULLS NOT DISTINCT
	pgIdx := [][]any{
		{"public", "items", "items_pkey", true, false, "id", 1},
		{"public", "items", "idx_code", false, false, "code", 1},
		{"public", "items", "idx_token", false, true, "token", 1},
	}
	uA := &mockInstance{
		tables: pgTables, columns: pgCols, indexes: pgIdx,
		tableRows: map[string][][]any{"public.items": {{int64(1), nil, "t1"}}},
	}
	// Source inserts id=2 with code=NULL (ok for idx_code) and token=NULL (conflicts with Target id=3 token=NULL on idx_token!)
	// and inserts id=4 with code="DUP" (conflicts with Target id=5 code="DUP" on idx_code)
	uS := &mockInstance{
		tables: pgTables, columns: pgCols, indexes: pgIdx,
		tableRows: map[string][][]any{"public.items": {
			{int64(1), nil, "t1"},
			{int64(2), nil, nil},
			{int64(4), "DUP", "t4"},
		}},
	}
	uT := &mockInstance{
		tables: pgTables, columns: pgCols, indexes: pgIdx,
		tableRows: map[string][][]any{"public.items": {
			{int64(1), nil, "t1"},
			{int64(3), nil, nil},
			{int64(5), "DUP", "t5"},
		}},
	}
	globalMock.register("u-a", uA)
	globalMock.register("u-s", uS)
	globalMock.register("u-t", uT)

	plan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/u-a", Port: 5432, User: "postgres"},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/u-s", Port: 5432, User: "postgres"},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/u-t", Port: 5432, User: "postgres"},
	})
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if plan.TotalConflicts != 2 {
		t.Fatalf("expected 2 unique conflicts (1 NULLS NOT DISTINCT + 1 DUP), got %d: %+v", plan.TotalConflicts, plan.Conflicts)
	}

	// 2. FK orphan conflict vs MATCH SIMPLE (NULL FK column ignored)
	fkTables := [][]any{{"shop", "orders", int64(1)}, {"shop", "users", int64(1)}}
	fkCols := [][]any{
		{"shop", "orders", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "orders", "user_id", 2, "int", "int", "YES", nil, "", ""},
		{"shop", "users", "id", 1, "int", "int", "NO", nil, "", ""},
	}
	fkIdx := [][]any{
		{"shop", "orders", "PRIMARY", 0, 1, "id", 1},
		{"shop", "users", "PRIMARY", 0, 1, "id", 1},
	}
	fkList := [][]any{
		{"shop", "orders", "fk_orders_user", "user_id", "shop", "users", "id", 1},
	}
	fkA := &mockInstance{
		tables: fkTables, columns: fkCols, indexes: fkIdx, fks: fkList,
		tableRows: map[string][][]any{
			"shop.users":  {{int64(1)}, {int64(2)}},
			"shop.orders": {},
		},
	}
	// Source deletes user 2
	fkS := &mockInstance{
		tables: fkTables, columns: fkCols, indexes: fkIdx, fks: fkList,
		tableRows: map[string][][]any{
			"shop.users":  {{int64(1)}},
			"shop.orders": {},
		},
	}
	// Target inserts order 100 referencing deleted user 2, and order 101 with user_id = NULL (MATCH SIMPLE -> allowed)
	fkT := &mockInstance{
		tables: fkTables, columns: fkCols, indexes: fkIdx, fks: fkList,
		tableRows: map[string][][]any{
			"shop.users":  {{int64(1)}, {int64(2)}},
			"shop.orders": {{int64(100), int64(2)}, {int64(101), nil}},
		},
	}
	globalMock.register("fk-a", fkA)
	globalMock.register("fk-s", fkS)
	globalMock.register("fk-t", fkT)

	fkPlan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fk-a", User: "root"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fk-s", User: "root"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fk-t", User: "root"},
	})
	if err != nil {
		t.Fatalf("FK Diff failed: %v", err)
	}
	if fkPlan.TotalConflicts != 1 || fkPlan.Conflicts[0].Type != domain.ConflictForeignKey {
		t.Fatalf("expected 1 foreign_key_conflict, got %+v", fkPlan.Conflicts)
	}
	// Also test --tables filter rejecting child table without parent table
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fk-a", User: "root"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fk-s", User: "root"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fk-t", User: "root"},
		Tables:   []string{"shop.orders"},
	}); err == nil {
		t.Error("expected --tables FK validation error when shop.users is omitted")
	}

	// 3. PK-less table with changes -> ErrSchemaNoPrimaryKey; without changes -> skipped
	noPKTables := [][]any{{"shop", "logs", int64(0)}}
	noPKCols := [][]any{{"shop", "logs", "msg", 1, "varchar", "varchar(100)", "YES", nil, "", ""}}
	npkA := &mockInstance{tables: noPKTables, columns: noPKCols, tableRows: map[string][][]any{"shop.logs": {{"a"}}}}
	npkS := &mockInstance{tables: noPKTables, columns: noPKCols, tableRows: map[string][][]any{"shop.logs": {{"a"}, {"b"}}}}
	npkT := &mockInstance{tables: noPKTables, columns: noPKCols, tableRows: map[string][][]any{"shop.logs": {{"a"}}}}
	globalMock.register("npk-a", npkA)
	globalMock.register("npk-s", npkS)
	globalMock.register("npk-t", npkT)

	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/npk-a", User: "root"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/npk-s", User: "root"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/npk-t", User: "root"},
	}); !errors.Is(err, domain.ErrSchemaNoPrimaryKey) {
		t.Errorf("expected ErrSchemaNoPrimaryKey, got %v", err)
	}
	// When A, S, T have identical rows on PK-less table, no error!
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/npk-a", User: "root"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/npk-a", User: "root"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/npk-t", User: "root"},
	}); err != nil {
		t.Errorf("unchanged PK-less table should succeed, got %v", err)
	}

	// 4. MySQL trigger on modified table -> ErrTriggersNotSupported unless IgnoreTriggers: true
	trA := &mockInstance{
		tables: [][]any{{"shop", "users", int64(1)}},
		columns: [][]any{
			{"shop", "users", "id", 1, "int", "int", "NO", nil, "", ""},
		},
		indexes:  [][]any{{"shop", "users", "PRIMARY", 0, 1, "id", 1}},
		triggers: [][]any{{"shop", "users", "trg_audit"}},
		tableRows: map[string][][]any{
			"shop.users": {{int64(1)}},
		},
	}
	trS := &mockInstance{
		tables: trA.tables, columns: trA.columns, indexes: trA.indexes, triggers: trA.triggers,
		tableRows: map[string][][]any{
			"shop.users": {{int64(1)}, {int64(2)}},
		},
	}
	globalMock.register("tr-a", trA)
	globalMock.register("tr-s", trS)

	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/tr-a", User: "root"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/tr-s", User: "root"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/tr-a", User: "root"},
	}); !errors.Is(err, domain.ErrTriggersNotSupported) {
		t.Errorf("expected ErrTriggersNotSupported, got %v", err)
	}
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor:       domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/tr-a", User: "root"},
		Source:         domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/tr-s", User: "root"},
		Target:         domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/tr-a", User: "root"},
		IgnoreTriggers: true,
	}); err != nil {
		t.Errorf("IgnoreTriggers=true should succeed, got %v", err)
	}
}

func TestApply_検証フェーズでのFK孤児検出とDB重複エラー変換(t *testing.T) {
	m := newTestMerger()
	myTables := [][]any{{"shop", "orders", int64(1)}, {"shop", "users", int64(1)}}
	myCols := [][]any{
		{"shop", "orders", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "orders", "user_id", 2, "int", "int", "NO", nil, "", ""},
		{"shop", "users", "id", 1, "int", "int", "NO", nil, "", ""},
	}
	myIdx := [][]any{
		{"shop", "orders", "PRIMARY", 0, 1, "id", 1},
		{"shop", "users", "PRIMARY", 0, 1, "id", 1},
	}
	myFKs := [][]any{
		{"shop", "orders", "fk_orders_user", "user_id", "shop", "users", "id", 1},
	}
	instA := &mockInstance{
		tables: myTables, columns: myCols, indexes: myIdx, fks: myFKs,
		tableRows: map[string][][]any{"shop.users": {{int64(1)}}, "shop.orders": {}},
	}
	instS := &mockInstance{
		tables: myTables, columns: myCols, indexes: myIdx, fks: myFKs,
		tableRows: map[string][][]any{"shop.users": {{int64(1)}}, "shop.orders": {{int64(10), int64(1)}}},
	}
	instT := &mockInstance{
		tables: myTables, columns: myCols, indexes: myIdx, fks: myFKs,
		tableRows:   map[string][][]any{"shop.users": {{int64(1)}}, "shop.orders": {}},
		orphanCount: 1, // Simulate post-apply orphan detection
	}
	globalMock.register("ap-a", instA)
	globalMock.register("ap-s", instS)
	globalMock.register("ap-t", instT)

	plan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap-a", User: "root"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap-s", User: "root"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap-t", User: "root"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap-t", User: "root"}, plan, nil); !errors.Is(err, domain.ErrMergeConflict) {
		t.Errorf("expected post-apply FK orphan to fail with ErrMergeConflict, got %v", err)
	}

	// Simulate Error 1062 Duplicate entry during Apply
	instT.orphanCount = 0
	instT.execErr = "Error 1062 (23000): Duplicate entry '10' for key 'PRIMARY'"
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap-t", User: "root"}, plan, nil); !errors.Is(err, domain.ErrMergeConflict) {
		t.Errorf("expected duplicate key error to wrap ErrMergeConflict, got %v", err)
	}
}


func TestDiff_スキーマ差分とDDL構築およびデータ型変換の網羅検証(t *testing.T) {
	if sqlmerge.New() == nil {
		t.Fatal("New() returned nil")
	}
	m := newTestMerger()

	// Unsupported engine
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: "oracle"},
		Source:   domain.AdminAccess{Engine: "oracle"},
		Target:   domain.AdminAccess{Engine: "oracle"},
	}); err == nil {
		t.Error("expected error for unsupported engine")
	}

	// Test various schema conflicts:
	// - t_both: created in both S and T
	// - t_new_noddl: created in S without AllowDDL
	// - t_drop_noddl: dropped in S without AllowDDL
	// - t_drop_mod: dropped in S, modified in T (with AllowDDL)
	// - t_drop_in_t: dropped in T, modified in S
	// - t_pkmismatch: PK changed between S and T
	// - t_coldrop: column dropped in S
	// - t_coltype: column type changed in S
	// - t_colnotnull: NOT NULL column without DEFAULT added in S
	// - t_colunsafe: column added in S with unsafe SEC-22 DEFAULT
	// - t_tonly: Target-only table (preserved without conflict)
	// - t_unqpk: Table with no PRIMARY key but a NOT NULL UNIQUE index (promoted to PK) + various data types
	tablesA := [][]any{
		{"shop", "t_drop_in_t", int64(0)},
		{"shop", "t_drop_mod", int64(0)},
		{"shop", "t_pkmismatch", int64(0)},
		{"shop", "t_coldrop", int64(0)},
		{"shop", "t_coltype", int64(0)},
		{"shop", "t_colnotnull", int64(0)},
		{"shop", "t_colunsafe", int64(0)},
		{"shop", "t_unqpk", int64(0)},
	}
	colsA := [][]any{
		{"shop", "t_drop_in_t", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_drop_mod", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_pkmismatch", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coldrop", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coldrop", "old", 2, "int", "int", "YES", nil, "", ""},
		{"shop", "t_coltype", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coltype", "v", 2, "int", "int", "YES", nil, "", ""},
		{"shop", "t_colnotnull", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_colunsafe", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_unqpk", "code", 1, "varchar", "varchar(32)", "NO", nil, "", ""},
		{"shop", "t_unqpk", "fval", 2, "double", "double", "YES", "0.0", "", ""},
		{"shop", "t_unqpk", "dval", 3, "decimal", "decimal(10,2)", "YES", "1.50", "", ""},
		{"shop", "t_unqpk", "bval", 4, "tinyint", "tinyint(1)", "YES", "1", "", ""},
	}
	idxA := [][]any{
		{"shop", "t_drop_in_t", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_drop_mod", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_pkmismatch", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_coldrop", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_coltype", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_colnotnull", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_colunsafe", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_unqpk", "uk_code", 0, 1, "code", 1},
	}

	tablesS := [][]any{
		{"shop", "t_both", int64(0)},
		{"shop", "t_drop_in_t", int64(0)},
		{"shop", "t_pkmismatch", int64(0)},
		{"shop", "t_coldrop", int64(0)},
		{"shop", "t_coltype", int64(0)},
		{"shop", "t_colnotnull", int64(0)},
		{"shop", "t_colunsafe", int64(0)},
		{"shop", "t_unqpk", int64(0)},
		{"shop", "t_new_my", int64(0)},
	}
	colsS := [][]any{
		{"shop", "t_both", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_drop_in_t", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_pkmismatch", "id2", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coldrop", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coltype", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coltype", "v", 2, "bigint", "bigint", "YES", nil, "", ""},
		{"shop", "t_colnotnull", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_colnotnull", "req", 2, "varchar", "varchar(10)", "NO", nil, "", ""},
		{"shop", "t_colunsafe", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_colunsafe", "bad", 2, "int", "int", "YES", "1; DROP TABLE x", "DEFAULT_GENERATED", ""},
		{"shop", "t_unqpk", "code", 1, "varchar", "varchar(32)", "NO", nil, "", ""},
		{"shop", "t_unqpk", "fval", 2, "double", "double", "YES", "0.0", "", ""},
		{"shop", "t_unqpk", "dval", 3, "decimal", "decimal(10,2)", "YES", "1.50", "", ""},
		{"shop", "t_unqpk", "bval", 4, "tinyint", "tinyint(1)", "YES", "1", "", ""},
		{"shop", "t_unqpk", "added_str", 5, "varchar", "varchar(20)", "YES", "hello", "", ""},
		{"shop", "t_unqpk", "added_num", 6, "int", "int", "YES", "42", "", ""},
		{"shop", "t_unqpk", "added_ts", 7, "timestamp", "timestamp", "YES", "CURRENT_TIMESTAMP", "", ""},
		{"shop", "t_unqpk", "added_null", 8, "varchar", "varchar(10)", "YES", "NULL", "", ""},
		{"shop", "t_new_my", "id", 1, "int", "int", "NO", nil, "auto_increment", ""},
		{"shop", "t_new_my", "code", 2, "varchar", "varchar(20)", "NO", "abc", "", ""},
		{"shop", "t_new_my", "gen_s", 3, "int", "int", "YES", nil, "STORED GENERATED", "id + 1"},
	}
	idxS := [][]any{
		{"shop", "t_both", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_drop_in_t", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_pkmismatch", "PRIMARY", 0, 1, "id2", 1},
		{"shop", "t_coldrop", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_coltype", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_colnotnull", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_colunsafe", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_unqpk", "uk_code", 0, 1, "code", 1},
		{"shop", "t_new_my", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_new_my", "uk_new_code", 0, 1, "code", 1},
	}

	tablesT := [][]any{
		{"shop", "t_both", int64(0)},
		{"shop", "t_drop_mod", int64(0)},
		{"shop", "t_pkmismatch", int64(0)},
		{"shop", "t_coldrop", int64(0)},
		{"shop", "t_coltype", int64(0)},
		{"shop", "t_colnotnull", int64(0)},
		{"shop", "t_colunsafe", int64(0)},
		{"shop", "t_unqpk", int64(0)},
		{"shop", "t_tonly", int64(0)},
	}
	colsT := [][]any{
		{"shop", "t_both", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_drop_mod", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_pkmismatch", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coldrop", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coldrop", "old", 2, "int", "int", "YES", nil, "", ""},
		{"shop", "t_coltype", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_coltype", "v", 2, "int", "int", "YES", nil, "", ""},
		{"shop", "t_colnotnull", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_colunsafe", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_unqpk", "code", 1, "varchar", "varchar(32)", "NO", nil, "", ""},
		{"shop", "t_unqpk", "fval", 2, "double", "double", "YES", "0.0", "", ""},
		{"shop", "t_unqpk", "dval", 3, "decimal", "decimal(10,2)", "YES", "1.50", "", ""},
		{"shop", "t_unqpk", "bval", 4, "tinyint", "tinyint(1)", "YES", "1", "", ""},
		{"shop", "t_tonly", "id", 1, "int", "int", "NO", nil, "", ""},
	}
	idxT := [][]any{
		{"shop", "t_both", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_drop_mod", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_pkmismatch", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_coldrop", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_coltype", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_colnotnull", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_colunsafe", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_unqpk", "uk_code", 0, 1, "code", 1},
		{"shop", "t_tonly", "PRIMARY", 0, 1, "id", 1},
	}

	scA := &mockInstance{
		tables: tablesA, columns: colsA, indexes: idxA,
		tableRows: map[string][][]any{
			"shop.t_drop_in_t": {{int64(1)}},
			"shop.t_drop_mod":  {{int64(1)}},
			"shop.t_unqpk": {
				{[]byte("A1"), float64(1.25), float64(9.99), true},
			},
		},
	}
	scS := &mockInstance{
		tables: tablesS, columns: colsS, indexes: idxS,
		tableRows: map[string][][]any{
			"shop.t_drop_in_t": {{int64(1)}, {int64(2)}},
			"shop.t_unqpk": {
				{"A1", "NaN", "NaN", int32(1), "hello", int(42), "2026-01-01", nil},
				{"B2", float64(2.5), "10.50", uint64(0), "world", int64(99), "2026-01-02", "x"},
			},
			"shop.t_new_my": {{int64(1), "abc"}},
		},
	}
	scT := &mockInstance{
		tables: tablesT, columns: colsT, indexes: idxT,
		tableRows: map[string][][]any{
			"shop.t_drop_mod": {{int64(1)}, {int64(2)}}, // Modified in T!
			"shop.t_tonly":    {{int64(10)}},
			"shop.t_unqpk": {
				{"A1", float64(1.25), "9.99", true},
			},
		},
	}
	globalMock.register("sc-a", scA)
	globalMock.register("sc-s", scS)
	globalMock.register("sc-t", scT)

	// 1. Without AllowDDL
	planNoDDL, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-t"},
		AllowDDL: false,
	})
	if err != nil {
		t.Fatalf("Diff (AllowDDL=false) failed: %v", err)
	}
	if planNoDDL.TotalConflicts < 7 {
		t.Errorf("expected >= 7 schema conflicts with AllowDDL=false, got %d: %+v", planNoDDL.TotalConflicts, planNoDDL.Conflicts)
	}

	// 2. With AllowDDL
	planDDL, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-t"},
		AllowDDL: true,
	})
	if err != nil {
		t.Fatalf("Diff (AllowDDL=true) failed: %v", err)
	}
	if planDDL.TotalConflicts < 7 {
		t.Errorf("expected schema conflicts for t_both, t_drop_mod, t_drop_in_t, t_pkmismatch, t_coldrop, t_coltype, t_colnotnull, t_colunsafe; got %d", planDDL.TotalConflicts)
	}
	// Apply must refuse a plan that has conflicts
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-t"}, planDDL, nil); !errors.Is(err, domain.ErrMergeConflict) {
		t.Errorf("expected Apply on conflicted plan to return ErrMergeConflict, got %v", err)
	}
	// Apply with nil Opaque is a no-op
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/sc-t"}, domain.MergePlan{}, nil); err != nil {
		t.Errorf("expected Apply with nil Opaque to return nil, got %v", err)
	}
}


type customStringer struct{ v string }

func (c customStringer) String() string { return c.v }

func TestDiff_Postgres新規テーブルとPKなし新規テーブルとエラー系(t *testing.T) {
	m := newTestMerger()

	// PG: New table with GENERATED BY DEFAULT AS IDENTITY, generated column, UNIQUE NULLS NOT DISTINCT, text PK
	pgA := &mockInstance{
		tables: [][]any{{"public", "t1"}},
		columns: [][]any{
			{"public", "t1", "k", 1, "text", false, "", "", ""},
			{"public", "t1", "ts", 2, "timestamp with time zone", true, "", "", ""},
		},
		indexes: [][]any{
			{"public", "t1", "t1_pkey", true, false, "k", 1},
		},
		tableRows: map[string][][]any{
			"public.t1": {{"a", customStringer{"2026-01-01"}}},
		},
	}
	pgS := &mockInstance{
		tables: [][]any{{"public", "t1"}, {"public", "t_new"}, {"public", "t_empty_nopk"}},
		columns: [][]any{
			{"public", "t1", "k", 1, "text", false, "", "", ""},
			{"public", "t1", "ts", 2, "timestamp with time zone", true, "", "", ""},
			{"public", "t_new", "id", 1, "integer", false, "", "d", ""},
			{"public", "t_new", "code", 2, "text", true, "''", "", ""},
			{"public", "t_new", "gen", 3, "text", true, "code || '!'", "", "s"},
			{"public", "t_empty_nopk", "note", 1, "text", true, "", "", ""},
		},
		indexes: [][]any{
			{"public", "t1", "t1_pkey", true, false, "k", 1},
			{"public", "t_new", "t_new_pkey", true, false, "id", 1},
			{"public", "t_new", "uk_t_new_code", false, true, "code", 1},
		},
		tableRows: map[string][][]any{
			"public.t1": {{"a", customStringer{"2026-01-02"}}},
			"public.t_new": {
				{int64(1), "c1"},
			},
			"public.t_empty_nopk": {},
		},
	}
	globalMock.register("pg2-a", pgA)
	globalMock.register("pg2-s", pgS)
	globalMock.register("pg2-t", pgA)

	plan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-a", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-s", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-t", Port: 5432},
		AllowDDL: true,
	})
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if plan.TotalConflicts != 0 {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}

	// Simulate FK constraint error 23503 during Apply
	pgFK := &mockInstance{execErr: "ERROR: insert or update on table violates foreign key constraint (SQLSTATE 23503)"}
	globalMock.register("pg2-fkerr", pgFK)
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-fkerr", Port: 5432}, plan, nil); !errors.Is(err, domain.ErrMergeConflict) {
		t.Errorf("expected FK error 23503 to wrap ErrMergeConflict, got %v", err)
	}

	// Now populate t_empty_nopk in Source -> should fail with ErrSchemaNoPrimaryKey
	pgS.tableRows["public.t_empty_nopk"] = [][]any{{"has-row"}}
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-a", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-s", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-t", Port: 5432},
		AllowDDL: true,
	}); !errors.Is(err, domain.ErrSchemaNoPrimaryKey) {
		t.Errorf("expected ErrSchemaNoPrimaryKey for new PK-less table with rows, got %v", err)
	}

	// Query error during inspectInstance
	errInst := &mockInstance{queryErr: "/* db-branch:tables */"}
	globalMock.register("pg2-qerr", errInst)
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-qerr", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-s", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg2-t", Port: 5432},
	}); err == nil {
		t.Error("expected inspectInstance error")
	}
}


func TestDiff_MultiDB_Postgresとコンフリクト件数上限と各種エラー系網羅(t *testing.T) {
	m := newTestMerger()

	// 1. PostgreSQL multi-database (shop != postgres) + generated/empty/ON UPDATE/unsafe table DDL + conflict truncation (>50)
	var rowsA, rowsS, rowsT [][]any
	for i := 1; i <= 55; i++ {
		rowsA = append(rowsA, []any{int64(i), "init", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		rowsS = append(rowsS, []any{int64(i), "from_s", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), ""})
		rowsT = append(rowsT, []any{int64(i), "from_t", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)})
	}
	pgMultiA := &mockInstance{
		pgDBs:  [][]any{{"shop"}},
		tables: [][]any{{"public", "items"}, {"public", "drop_pkless"}},
		columns: [][]any{
			{"public", "items", "id", 1, "integer", false, "", "", ""},
			{"public", "items", "val", 2, "text", true, "", "", ""},
			{"public", "items", "ts", 3, "timestamp with time zone", true, "", "", ""},
			{"public", "drop_pkless", "note", 1, "text", true, "", "", ""},
		},
		indexes: [][]any{
			{"public", "items", "items_pkey", true, false, "id", 1},
		},
		tableRows: map[string][][]any{
			"shop.public.items":       rowsA,
			"shop.public.drop_pkless": {{"x"}},
		},
	}
	pgMultiS := &mockInstance{
		pgDBs: [][]any{{"shop"}},
		tables: [][]any{
			{"public", "items"},
			{"public", "bad_enum_tbl"},
			{"public", "empty_gen"},
			{"public", "no_cols"},
		},
		columns: [][]any{
			{"public", "items", "id", 1, "integer", false, "", "", ""},
			{"public", "items", "val", 2, "text", true, "", "", ""},
			{"public", "items", "ts", 3, "timestamp with time zone", true, "", "", ""},
			{"public", "items", "note", 4, "text", true, "''", "", ""},
			{"public", "bad_enum_tbl", "id", 1, "integer", false, "", "", ""},
			{"public", "empty_gen", "g", 1, "text", true, "1+1", "", "s"},
		},
		indexes: [][]any{
			{"public", "items", "items_pkey", true, false, "id", 1},
			{"public", "bad_enum_tbl", "b_pkey", true, false, "id", 1},
		},
		enums: [][]any{
			{"public", "bad_enum", "ok", 1},
			{"public", "bad_enum", "nul\x00bad", 2}, // invalid enum literal
		},
		tableRows: map[string][][]any{
			"shop.public.items": rowsS,
		},
	}
	pgMultiT := &mockInstance{
		pgDBs:  [][]any{{"shop"}},
		tables: [][]any{{"public", "items"}, {"public", "drop_pkless"}, {"public", "tonly_nopk"}},
		columns: [][]any{
			{"public", "items", "id", 1, "integer", false, "", "", ""},
			{"public", "items", "val", 2, "text", true, "", "", ""},
			{"public", "items", "ts", 3, "timestamp with time zone", true, "", "", ""},
			{"public", "drop_pkless", "note", 1, "text", true, "", "", ""},
			{"public", "tonly_nopk", "msg", 1, "text", true, "", "", ""},
		},
		indexes: [][]any{
			{"public", "items", "items_pkey", true, false, "id", 1},
		},
		tableRows: map[string][][]any{
			"shop.public.items":       rowsT,
			"shop.public.drop_pkless": {{"x"}, {"y"}}, // changed in T -> schema_conflict on drop
			"shop.public.tonly_nopk":  {{"ok"}},
		},
	}
	globalMock.register("pgm-a", pgMultiA)
	globalMock.register("pgm-s", pgMultiS)
	globalMock.register("pgm-t", pgMultiT)

	plan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgm-a", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgm-s", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgm-t", Port: 5432},
		AllowDDL: true,
	})
	if err != nil {
		t.Fatalf("MultiDB Diff failed: %v", err)
	}
	if !plan.Truncated || len(plan.Conflicts) != domain.MaxMergeConflicts || plan.TotalConflicts < domain.MaxMergeConflicts {
		t.Errorf("expected truncated=true len=%d total>=50, got truncated=%v len=%d total=%d",
			domain.MaxMergeConflicts, plan.Truncated, len(plan.Conflicts), plan.TotalConflicts)
	}

	// 2. Inspect error paths across each query phase
	for _, qTag := range []string{
		"/* db-branch:pg-databases */",
		"/* db-branch:columns */",
		"/* db-branch:indexes */",
		"/* db-branch:fks */",
		"/* db-branch:enums */",
		"/* db-branch:sequences */",
	} {
		errMock := &mockInstance{queryErr: qTag}
		globalMock.register("err-q", errMock)
		if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
			Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/err-q", Port: 5432},
			Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgm-s", Port: 5432},
			Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgm-t", Port: 5432},
		}); err == nil {
			t.Errorf("expected error when %s fails", qTag)
		}
	}
	// MySQL triggers error
	trErr := &mockInstance{queryErr: "/* db-branch:triggers */"}
	globalMock.register("my-tr-err", trErr)
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/my-tr-err"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/my-tr-err"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/my-tr-err"},
	}); err == nil {
		t.Error("expected triggers query error")
	}

	// 3. Apply error branches: DROP TABLE error, CREATE TABLE error, Verify FK error, SYNC AUTO_INCREMENT error, SYNC SEQ error
	apA := &mockInstance{
		tables: [][]any{{"shop", "dropme", int64(0)}, {"shop", "t", int64(10)}},
		columns: [][]any{
			{"shop", "dropme", "id", 1, "int", "int", "NO", nil, "", ""},
			{"shop", "t", "id", 1, "int", "int", "NO", nil, "auto_increment", ""},
			{"shop", "t", "ts", 2, "timestamp", "timestamp", "YES", "CURRENT_TIMESTAMP", "on update CURRENT_TIMESTAMP", ""},
		},
		indexes: [][]any{
			{"shop", "dropme", "PRIMARY", 0, 1, "id", 1},
			{"shop", "t", "PRIMARY", 0, 1, "id", 1},
		},
		tableRows: map[string][][]any{"shop.dropme": {{int64(1)}}, "shop.t": {{int64(1), nil}}},
	}
	apS := &mockInstance{
		tables: [][]any{{"shop", "t", int64(10)}, {"shop", "newt", int64(0)}},
		columns: [][]any{
			{"shop", "t", "id", 1, "int", "int", "NO", nil, "auto_increment", ""},
			{"shop", "t", "ts", 2, "timestamp", "timestamp", "YES", "CURRENT_TIMESTAMP", "on update CURRENT_TIMESTAMP", ""},
			{"shop", "newt", "id", 1, "int", "int", "NO", nil, "", ""},
			{"shop", "newt", "ts", 2, "timestamp", "timestamp", "YES", "CURRENT_TIMESTAMP", "on update CURRENT_TIMESTAMP", ""},
		},
		indexes: [][]any{
			{"shop", "t", "PRIMARY", 0, 1, "id", 1},
			{"shop", "newt", "PRIMARY", 0, 1, "id", 1},
		},
		tableRows: map[string][][]any{"shop.t": {{int64(1), nil}, {int64(2), "2026-01-01"}}, "shop.newt": {}},
	}
	globalMock.register("ap2-a", apA)
	globalMock.register("ap2-s", apS)

	apPlan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap2-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap2-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ap2-a"},
		AllowDDL: true,
	})
	if err != nil || apPlan.TotalConflicts != 0 {
		t.Fatalf("apPlan err=%v conflicts=%+v", err, apPlan.Conflicts)
	}
	for _, errTarget := range []struct {
		key       string
		execOn    string
		queryErr  string
	}{
		{"ap2-t-create", "CREATE TABLE", ""},
		{"ap2-t-drop", "DROP TABLE", ""},
		{"ap2-t-ai", "AUTO_INCREMENT =", ""},
		{"ap2-t-fk", "", "/* db-branch:verify-fk */"},
	} {
		if errTarget.queryErr != "" {
			apPlan2 := apPlan
			globalMock.register(errTarget.key, &mockInstance{execErr: "boom", execErrOn: errTarget.execOn, queryErr: errTarget.queryErr})
			_ = apPlan2
		} else {
			globalMock.register(errTarget.key, &mockInstance{execErr: "fail", execErrOn: errTarget.execOn})
			if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/" + errTarget.key}, apPlan, nil); err == nil {
				t.Errorf("expected Apply error for %s", errTarget.key)
			}
		}
	}
	// Connect error in Apply
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/does-not-exist"}, apPlan, nil); err == nil {
		t.Error("expected Apply connect error")
	}
}


func TestDiffAndApply_エッジケースとCompositeFKとtableModified網羅(t *testing.T) {
	m := newTestMerger()

	// Composite PK & Composite FK + PG setval error + DDL ident NUL validation + tableModifiedBetween variants
	baseTables := [][]any{
		{"shop", "parent", int64(0)},
		{"shop", "child", int64(0)},
		{"shop", "drop_cols", int64(0)},
		{"shop", "drop_type", int64(0)},
		{"shop", "drop_cnt", int64(0)},
		{"shop", "drop_val", int64(0)},
		{"shop", "drop_pkless_cnt", int64(0)},
		{"shop", "drop_pkless_val", int64(0)},
	}
	baseCols := [][]any{
		{"shop", "parent", "k1", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "parent", "k2", 2, "int", "int", "NO", nil, "", ""},
		{"shop", "child", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "child", "fk1", 2, "int", "int", "NO", nil, "", ""},
		{"shop", "child", "fk2", 3, "int", "int", "NO", nil, "", ""},
		{"shop", "drop_cols", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "drop_type", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "drop_cnt", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "drop_val", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "drop_val", "v", 2, "int", "int", "YES", nil, "", ""},
		{"shop", "drop_pkless_cnt", "v", 1, "int", "int", "YES", nil, "", ""},
		{"shop", "drop_pkless_val", "v", 1, "int", "int", "YES", nil, "", ""},
	}
	baseIdx := [][]any{
		{"shop", "parent", "PRIMARY", 0, 1, "k1", 1},
		{"shop", "parent", "PRIMARY", 0, 2, "k2", 2},
		{"shop", "child", "PRIMARY", 0, 1, "id", 1},
		{"shop", "drop_cols", "PRIMARY", 0, 1, "id", 1},
		{"shop", "drop_type", "PRIMARY", 0, 1, "id", 1},
		{"shop", "drop_cnt", "PRIMARY", 0, 1, "id", 1},
		{"shop", "drop_val", "PRIMARY", 0, 1, "id", 1},
	}
	baseFKs := [][]any{
		{"shop", "child", "fk_comp", "fk1", "shop", "parent", "k1", 1},
		{"shop", "child", "fk_comp", "fk2", "shop", "parent", "k2", 2},
	}

	compA := &mockInstance{
		tables: baseTables, columns: baseCols, indexes: baseIdx, fks: baseFKs,
		tableRows: map[string][][]any{
			"shop.parent":          {{int64(1), int64(10)}},
			"shop.child":           {{int64(100), int64(1), int64(10)}},
			"shop.drop_cnt":        {{int64(1)}},
			"shop.drop_val":        {{int64(1), int64(5)}},
			"shop.drop_pkless_cnt": {{int64(1)}},
			"shop.drop_pkless_val": {{int64(1)}},
		},
	}
	compS := &mockInstance{
		tables: [][]any{
			{"shop", "parent", int64(0)},
			{"shop", "child", int64(0)},
			{"shop", "bad\x00tbl", int64(0)},
		},
		columns: append(append([][]any{}, baseCols[:5]...),
			[]any{"shop", "bad\x00tbl", "id", 1, "int", "int", "NO", nil, "", ""},
		),
		indexes: append(append([][]any{}, baseIdx[:3]...),
			[]any{"shop", "bad\x00tbl", "PRIMARY", 0, 1, "id", 1},
		),
		fks: baseFKs,
		tableRows: map[string][][]any{
			"shop.parent": {{int64(1), int64(10)}, {int64(2), int64(20)}},
			"shop.child":  {{int64(100), int64(1), int64(10)}, {int64(101), int64(2), int64(20)}},
		},
	}
	// Target modifies every dropped table in different ways so every branch of tableModifiedBetween runs
	tCols := append([][]any{}, baseCols...)
	tCols[6] = []any{"shop", "drop_type", "id", 1, "bigint", "bigint", "NO", nil, "", ""} // type mismatch
	tCols = append(tCols, []any{"shop", "drop_cols", "extra", 2, "int", "int", "YES", nil, "", ""})
	compT := &mockInstance{
		tables: baseTables, columns: tCols, indexes: baseIdx, fks: baseFKs,
		tableRows: map[string][][]any{
			"shop.parent":          {{int64(1), int64(10)}},
			"shop.child":           {{int64(100), int64(1), int64(10)}},
			"shop.drop_cnt":        {{int64(1)}, {int64(2)}},
			"shop.drop_val":        {{int64(1), int64(999)}},
			"shop.drop_pkless_cnt": {{int64(1)}, {int64(2)}},
			"shop.drop_pkless_val": {{int64(9)}},
		},
	}
	globalMock.register("cmp-a", compA)
	globalMock.register("cmp-s", compS)
	globalMock.register("cmp-t", compT)

	plan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-t"},
		AllowDDL: true,
	})
	if err != nil {
		t.Fatalf("comp Diff failed: %v", err)
	}
	if plan.TotalConflicts != 7 { // 6 dropped modified tables + 1 unsafe table name bad\x00tbl
		t.Errorf("expected 7 schema conflicts, got %d: %+v", plan.TotalConflicts, plan.Conflicts)
	}

	// Open DB errors on Source and Target + inspect errors on Source and Target
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/missing-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-t"},
	}); err == nil {
		t.Error("expected Source inspect error")
	}
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-a"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/missing-t"},
	}); err == nil {
		t.Error("expected Target inspect error")
	}

	// Apply verify-fk query error and sequence sync error
	fkErrT := &mockInstance{queryErr: "/* db-branch:verify-fk */"}
	globalMock.register("fkq-err", fkErrT)
	validPlan, _ := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmp-t"},
		Tables:   []string{"shop.parent", "shop.child"},
	})
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fkq-err"}, validPlan, nil); err == nil {
		t.Error("expected verify-fk query error")
	}

	// Non-numeric strings in integer/float/decimal columns and delete error in Apply
	nnA := &mockInstance{
		tables: [][]any{{"shop", "nv", int64(0)}},
		columns: [][]any{
			{"shop", "nv", "id", 1, "varchar", "varchar(20)", "NO", nil, "", ""},
			{"shop", "nv", "iv", 2, "int", "int", "YES", nil, "", ""},
			{"shop", "nv", "dv", 3, "decimal", "decimal(10,2)", "YES", nil, "", ""},
			{"shop", "nv", "fv", 4, "float", "float", "YES", nil, "", ""},
		},
		indexes:   [][]any{{"shop", "nv", "PRIMARY", 0, 1, "id", 1}},
		tableRows: map[string][][]any{"shop.nv": {{"k1", "notanint", "NaN", "notfloat"}, {"k2", "1", "2.00", "3.5"}}},
	}
	nnS := &mockInstance{
		tables: nnA.tables, columns: nnA.columns, indexes: nnA.indexes,
		tableRows: map[string][][]any{"shop.nv": {{"k1", "notanint", "NaN", "notfloat"}}},
	}
	globalMock.register("nn-a", nnA)
	globalMock.register("nn-s", nnS)
	nnPlan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/nn-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/nn-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/nn-a"},
	})
	if err != nil || nnPlan.Deleted != 1 {
		t.Fatalf("nnPlan err=%v deleted=%d", err, nnPlan.Deleted)
	}
	delErrT := &mockInstance{execErr: "delete fail", execErrOn: "DELETE"}
	globalMock.register("nn-del-err", delErrT)
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/nn-del-err"}, nnPlan, nil); err == nil {
		t.Error("expected DELETE error in Apply")
	}
}


func TestDiffAndApply_追加パス_PGマルチDBFKとDropNULとSetval失敗とNULLPKLess(t *testing.T) {
	m := newTestMerger()

	// PG multi-db FK (unqualify "shop.public" -> "public") + seq setval error + drop_bad\x00 + pkless NULL rows
	pgA := &mockInstance{
		pgDBs: [][]any{{"shop"}},
		tables: [][]any{
			{"public", "p"},
			{"public", "c"},
			{"public", "drop\x00bad"},
			{"public", "pkless_null"},
		},
		columns: [][]any{
			{"public", "p", "id", 1, "integer", false, "", "d", ""},
			{"public", "c", "id", 1, "integer", false, "", "", ""},
			{"public", "c", "pid", 2, "integer", false, "", "", ""},
			{"public", "drop\x00bad", "id", 1, "integer", false, "", "", ""},
			{"public", "pkless_null", "v", 1, "text", true, "", "", ""},
		},
		indexes: [][]any{
			{"public", "p", "p_pkey", true, false, "id", 1},
			{"public", "c", "c_pkey", true, false, "id", 1},
			{"public", "drop\x00bad", "d_pkey", true, false, "id", 1},
		},
		fks: [][]any{
			{"public", "c", "fk_cp", "pid", "public", "p", "id", 1},
		},
		sequences: [][]any{{"public", "p", "id", "public", "p_id_seq"}},
		seqValues: map[string]int64{"p_id_seq": 1},
		tableRows: map[string][][]any{
			"shop.public.p":           {{int64(1)}},
			"shop.public.c":           {{int64(10), int64(1)}},
			"shop.public.drop\x00bad": {{int64(1)}},
			"shop.public.pkless_null": {{nil}},
		},
	}
	pgS := &mockInstance{
		pgDBs: [][]any{{"shop"}},
		tables: [][]any{
			{"public", "p"},
			{"public", "c"},
			{"public", "pkless_null"},
		},
		columns: [][]any{
			{"public", "p", "id", 1, "integer", false, "", "d", ""},
			{"public", "c", "id", 1, "integer", false, "", "", ""},
			{"public", "c", "pid", 2, "integer", false, "", "", ""},
			{"public", "pkless_null", "v", 1, "text", true, "", "", ""},
		},
		indexes: [][]any{
			{"public", "p", "p_pkey", true, false, "id", 1},
			{"public", "c", "c_pkey", true, false, "id", 1},
		},
		fks: [][]any{
			{"public", "c", "fk_cp", "pid", "public", "p", "id", 1},
		},
		sequences: [][]any{{"public", "p", "id", "public", "p_id_seq"}},
		seqValues: map[string]int64{"p_id_seq": 2},
		tableRows: map[string][][]any{
			"shop.public.p":           {{int64(1)}, {int64(2)}},
			"shop.public.c":           {{int64(10), int64(1)}, {int64(20), int64(2)}},
			"shop.public.pkless_null": {{nil}},
		},
	}
	pgT := &mockInstance{
		pgDBs:     pgA.pgDBs,
		tables:    pgA.tables,
		columns:   pgA.columns,
		indexes:   pgA.indexes,
		fks:       pgA.fks,
		sequences: pgA.sequences,
		seqValues: pgA.seqValues,
		tableRows: pgA.tableRows,
	}
	globalMock.register("pg3-a", pgA)
	globalMock.register("pg3-s", pgS)
	globalMock.register("pg3-t", pgT)

	// Diff includes unsafe drop\x00bad -> conflict (keeps plan usable for clean subset)
	planBad, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-a", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-s", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-t", Port: 5432},
		AllowDDL: true,
	})
	if err != nil || planBad.TotalConflicts != 1 {
		t.Fatalf("planBad err=%v conflicts=%+v", err, planBad.Conflicts)
	}

	// Clean subset of p + c
	planOK, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-a", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-s", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-t", Port: 5432},
		Tables:   []string{"shop.public.p", "shop.public.c"},
	})
	if err != nil || planOK.TotalConflicts != 0 {
		t.Fatalf("planOK err=%v conflicts=%+v", err, planOK.Conflicts)
	}
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-t", Port: 5432}, planOK, nil); err != nil {
		t.Fatalf("pg3 Apply failed: %v", err)
	}
	// setval failure during syncCounters
	pgSeqErr := &mockInstance{queryErr: "/* db-branch:set-seq */"}
	globalMock.register("pg3-seqerr", pgSeqErr)
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-seqerr", Port: 5432}, planOK, nil); err == nil {
		t.Error("expected set-seq error in Apply")
	}
	// session_replication_role error
	pgRoleErr := &mockInstance{execErr: "role err", execErrOn: "session_replication_role"}
	globalMock.register("pg3-roleerr", pgRoleErr)
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pg3-roleerr", Port: 5432}, planOK, nil); err == nil {
		t.Error("expected session_replication_role error in Apply")
	}
}


func TestDiffAndApply_スキャンエラーとDDLバリデーションとUpdate失敗網羅(t *testing.T) {
	m := newTestMerger()

	// 1. scan error on /* db-branch:scan ... */ during diffNewSourceTable / diffExistingTable / tableModifiedBetween / pkless
	sTbls := [][]any{{"shop", "t_scan", int64(0)}, {"shop", "t_new_scan", int64(0)}}
	sCols := [][]any{
		{"shop", "t_scan", "id", 1, "int", "int", "NO", nil, "", ""},
		{"shop", "t_scan", "v", 2, "", "", "YES", nil, "", ""},
		{"shop", "t_new_scan", "id", 1, "int", "int", "NO", nil, "", ""},
	}
	sIdx := [][]any{
		{"shop", "t_scan", "PRIMARY", 0, 1, "id", 1},
		{"shop", "t_new_scan", "PRIMARY", 0, 1, "id", 1},
	}
	aScan := &mockInstance{
		tables: [][]any{{"shop", "t_scan", int64(0)}},
		columns: sCols[:2], indexes: sIdx[:1],
		tableRows: map[string][][]any{"shop.t_scan": {{int64(1), "a"}}},
	}
	sScan := &mockInstance{
		tables: sTbls, columns: sCols, indexes: sIdx,
		queryErr: "/* db-branch:scan shop.t_scan */",
	}
	globalMock.register("scan-a", aScan)
	globalMock.register("scan-s", sScan)
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-a"},
		AllowDDL: true,
	}); err == nil {
		t.Error("expected scan error on Source")
	}

	sScan2 := &mockInstance{
		tables: sTbls, columns: sCols, indexes: sIdx,
		tableRows: map[string][][]any{"shop.t_scan": {{int64(1), "a"}}},
		queryErr:  "/* db-branch:scan shop.t_new_scan */",
	}
	globalMock.register("scan-s2", sScan2)
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-s2"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-a"},
		AllowDDL: true,
	}); err == nil {
		t.Error("expected scan error on new table")
	}

	// Scan error on t_only (loadTableRowsForCheck)
	tScanErr := &mockInstance{
		tables:    [][]any{{"shop", "t_scan", int64(0)}},
		columns:   sCols[:2],
		indexes:   sIdx[:1],
		queryErr:  "/* db-branch:scan shop.t_scan */",
	}
	emptyInst := &mockInstance{}
	globalMock.register("scan-empty", emptyInst)
	globalMock.register("scan-t-only", tScanErr)
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-empty"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-empty"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-t-only"},
	}); err == nil {
		t.Error("expected scan error on Target-only table")
	}

	// 2. Bad PG enum identifier + bad column identifier in ADD COLUMN
	pgBadA := &mockInstance{
		tables:  [][]any{{"public", "t1"}},
		columns: [][]any{{"public", "t1", "id", 1, "integer", false, "", "", ""}},
		indexes: [][]any{{"public", "t1", "t1_pkey", true, false, "id", 1}},
		tableRows: map[string][][]any{"public.t1": {{int64(1)}}},
	}
	pgBadS := &mockInstance{
		tables: [][]any{{"public", "t1"}},
		columns: [][]any{
			{"public", "t1", "id", 1, "integer", false, "", "", ""},
			{"public", "t1", "bad\x00col", 2, "text", true, "", "", ""},
		},
		indexes:   pgBadA.indexes,
		enums:     [][]any{{"public", "bad\x00enum", "a", 1}},
		tableRows: map[string][][]any{"public.t1": {{int64(1), "x"}}},
	}
	globalMock.register("pgbad-a", pgBadA)
	globalMock.register("pgbad-s", pgBadS)
	planBad, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgbad-a", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgbad-s", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgbad-a", Port: 5432},
		AllowDDL: true,
	})
	if err != nil || planBad.TotalConflicts < 2 {
		t.Fatalf("expected 2 schema conflicts (bad enum + bad col), got err=%v total=%d", err, planBad.TotalConflicts)
	}

	// 3. Apply UPDATE error
	updA := &mockInstance{
		tables: [][]any{{"shop", "u", int64(0)}},
		columns: [][]any{
			{"shop", "u", "id", 1, "int", "int", "NO", nil, "", ""},
			{"shop", "u", "v", 2, "varchar", "varchar(20)", "YES", nil, "", ""},
		},
		indexes:   [][]any{{"shop", "u", "PRIMARY", 0, 1, "id", 1}},
		tableRows: map[string][][]any{"shop.u": {{int64(1), "old"}}},
	}
	updS := &mockInstance{
		tables: updA.tables, columns: updA.columns, indexes: updA.indexes,
		tableRows: map[string][][]any{"shop.u": {{int64(1), "new"}}},
	}
	globalMock.register("upd-a", updA)
	globalMock.register("upd-s", updS)
	updPlan, _ := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/upd-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/upd-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/upd-a"},
	})
	updErr := &mockInstance{execErr: "update failed", execErrOn: "UPDATE"}
	globalMock.register("upd-err-t", updErr)
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/upd-err-t"}, updPlan, nil); err == nil {
		t.Error("expected UPDATE error in Apply")
	}

	// 4. Empty PG plan Apply (exercises collectPGDBsInPlan empty fallback)
	pgEmptyPlan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgbad-a", Port: 5432},
		Source:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgbad-a", Port: 5432},
		Target:   domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgbad-a", Port: 5432},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.Postgres, Socket: "/run/pgbad-a", Port: 5432}, pgEmptyPlan, nil); err != nil {
		t.Errorf("empty PG Apply failed: %v", err)
	}

	// 5. Row scan error (type conversion failure via short row from mock or queryEach scan error)
	badTableScan := &mockInstance{tables: [][]any{{"only_one_col"}}}
	globalMock.register("scan-short", badTableScan)
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-short"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-short"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-short"},
	}); err == nil {
		t.Error("expected queryEach Scan error when tables row has too few columns")
	}
}

func TestDiffAndApply_追加分岐_PKLessエラーとColDroppedInBothとEmptyDefault(t *testing.T) {
	m := newTestMerger()

	// colA dropped in both S and T (hits colA loop in compareTableSchema) + empty string default
	ca := &mockInstance{
		tables: [][]any{{"shop", "dcol", int64(0)}},
		columns: [][]any{
			{"shop", "dcol", "id", 1, "int", "int", "NO", nil, "", ""},
			{"shop", "dcol", "gone", 2, "int", "int", "YES", nil, "", ""},
		},
		indexes: [][]any{{"shop", "dcol", "PRIMARY", 0, 1, "id", 1}},
	}
	cs := &mockInstance{
		tables: [][]any{{"shop", "dcol", int64(0)}, {"shop", "empty_def", int64(0)}},
		columns: [][]any{
			{"shop", "dcol", "id", 1, "int", "int", "NO", nil, "", ""},
			{"shop", "empty_def", "id", 1, "int", "int", "NO", nil, "", ""},
			{"shop", "empty_def", "s", 2, "varchar", "varchar(10)", "NO", "", "", ""},
		},
		indexes: [][]any{
			{"shop", "dcol", "PRIMARY", 0, 1, "id", 1},
			{"shop", "empty_def", "PRIMARY", 0, 1, "id", 1},
		},
	}
	ct := &mockInstance{
		tables:  [][]any{{"shop", "dcol", int64(0)}},
		columns: [][]any{{"shop", "dcol", "id", 1, "int", "int", "NO", nil, "", ""}},
		indexes: [][]any{{"shop", "dcol", "PRIMARY", 0, 1, "id", 1}},
	}
	globalMock.register("ca-1", ca)
	globalMock.register("cs-1", cs)
	globalMock.register("ct-1", ct)

	p, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ca-1"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cs-1"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/ct-1"},
		AllowDDL: true,
	})
	if err != nil || p.TotalConflicts != 1 {
		t.Fatalf("expected 1 conflict for dropped col in S/T, got err=%v total=%d", err, p.TotalConflicts)
	}

	// Scan error on pkless table and new pkless table
	pkErrA := &mockInstance{
		tables:  [][]any{{"shop", "pk1", int64(0)}},
		columns: [][]any{{"shop", "pk1", "v", 1, "int", "int", "YES", nil, "", ""}},
	}
	pkErrS := &mockInstance{
		tables:   [][]any{{"shop", "pk1", int64(0)}},
		columns:  pkErrA.columns,
		queryErr: "/* db-branch:scan shop.pk1 */",
	}
	globalMock.register("pke-a", pkErrA)
	globalMock.register("pke-s", pkErrS)
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/pke-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/pke-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/pke-a"},
	}); err == nil {
		t.Error("expected pkless scan error")
	}
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-empty"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/pke-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/scan-empty"},
		AllowDDL: true,
	}); err == nil {
		t.Error("expected new pkless scan error")
	}

	// MySQL FOREIGN_KEY_CHECKS error in Apply
	apPlan, _ := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/upd-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/upd-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/upd-a"},
	})
	fkChkErr := &mockInstance{execErr: "fk check err", execErrOn: "FOREIGN_KEY_CHECKS"}
	globalMock.register("fkc-err", fkChkErr)
	if err := m.Apply(context.Background(), domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/fkc-err"}, apPlan, nil); err == nil {
		t.Error("expected FOREIGN_KEY_CHECKS error")
	}

	// Finding 1 & 2: SQL comment label sanitization & backslash inside single-quoted literals rejected
	if err := sqlmerge.ValidateSQLExpression(`'foo\'; DROP TABLE users; --'`); !errors.Is(err, sqlmerge.ErrUnsafeDDL) {
		t.Errorf("ValidateSQLExpression backslash escape should be rejected as ErrUnsafeDDL, got %v", err)
	}
	cmtS := &mockInstance{
		tables:  [][]any{{"shop", "t*/; DROP TABLE x; --", int64(0)}},
		columns: [][]any{{"shop", "t*/; DROP TABLE x; --", "id", 1, "int", "int", "NO", nil, "", ""}},
		indexes: [][]any{{"shop", "t*/; DROP TABLE x; --", "PRIMARY", 0, 1, "id", 1}},
	}
	globalMock.register("cmt-a", &mockInstance{tables: cmtS.tables, columns: cmtS.columns, indexes: cmtS.indexes})
	if _, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmt-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmt-a"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/cmt-a"},
	}); err != nil {
		t.Errorf("table name with comment meta-chars should succeed safely: %v", err)
	}

	// Finding 5: Empty PK-less table with AllowDDL adding a column keeps MergeTableSummary in plan.Tables
	pkeA := &mockInstance{
		tables:  [][]any{{"shop", "events", int64(0)}},
		columns: [][]any{{"shop", "events", "id", 1, "int", "int", "NO", nil, "", ""}},
	}
	pkeS2 := &mockInstance{
		tables: [][]any{{"shop", "events", int64(0)}},
		columns: [][]any{
			{"shop", "events", "id", 1, "int", "int", "NO", nil, "", ""},
			{"shop", "events", "note", 2, "varchar", "varchar(64)", "YES", nil, "", ""},
		},
	}
	globalMock.register("pkemp-a", pkeA)
	globalMock.register("pkemp-s", pkeS2)
	ddlEmptyPlan, err := m.Diff(context.Background(), domain.MergeDiffRequest{
		Ancestor: domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/pkemp-a"},
		Source:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/pkemp-s"},
		Target:   domain.AdminAccess{Engine: domain.MySQL, Socket: "/run/pkemp-a"},
		AllowDDL: true,
	})
	if err != nil || ddlEmptyPlan.DDL != 1 || len(ddlEmptyPlan.Tables) != 1 || ddlEmptyPlan.Tables[0].DDL != "ALTER TABLE ADD COLUMN" {
		t.Errorf("empty pk-less DDL plan = %+v err=%v", ddlEmptyPlan, err)
	}
}
