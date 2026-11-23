package sqlmerge

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// Merger implements domain.LogicalMerger for MySQL and PostgreSQL.
type Merger struct {
	// Open defaults to sql.Open.
	Open func(driver, dsn string) (*sql.DB, error)
}

var _ domain.LogicalMerger = (*Merger)(nil)

// New returns a new Merger using sql.Open.
func New() *Merger { return &Merger{} }

func (m *Merger) openDB(acc domain.AdminAccess, dbName ...string) (*sql.DB, error) {
	open := m.Open
	if open == nil {
		open = sql.Open
	}
	host := acc.Host
	if host == "" {
		host = "127.0.0.1"
	}
	switch acc.Engine {
	case domain.MySQL:
		c := mysql.NewConfig()
		c.User = acc.User
		if c.User == "" {
			c.User = "root"
		}
		if acc.Socket != "" {
			c.Net = "unix"
			c.Addr = acc.Socket
		} else {
			c.Net = "tcp"
			c.Addr = net.JoinHostPort(host, strconv.Itoa(acc.Port))
		}
		c.Timeout = 10 * time.Second
		return open("mysql", c.FormatDSN())
	case domain.Postgres:
		user := acc.User
		if user == "" {
			user = "postgres"
		}
		pgDB := "postgres"
		if len(dbName) > 0 && dbName[0] != "" {
			pgDB = dbName[0]
		}
		if acc.Socket != "" {
			dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable connect_timeout=10",
				quoteLibpqKV(acc.Socket), acc.Port, quoteLibpqKV(user), quoteLibpqKV(pgDB))
			return open("pgx", dsn)
		}
		u := url.URL{
			Scheme:   "postgres",
			User:     url.User(user),
			Host:     net.JoinHostPort(host, strconv.Itoa(acc.Port)),
			Path:     "/" + pgDB,
			RawQuery: "sslmode=disable&connect_timeout=10",
		}
		return open("pgx", u.String())
	default:
		return nil, fmt.Errorf("unsupported engine %q", acc.Engine)
	}
}

func quoteLibpqKV(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

func queryEach(ctx context.Context, db *sql.DB, query string, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

type columnMeta struct {
	Name           string
	Ordinal        int
	DataType       string
	ColumnType     string
	Nullable       bool
	HasDefault     bool
	Default        string
	Extra          string
	IsGenerated    bool
	IsStoredGen    bool
	GenerationExpr string
	IsAutoInc      bool
	IsIdentity     bool
	IdentityAlways bool
	Kind           domain.PKColumnKind
}

type uniqueMeta struct {
	Name             string
	Columns          []string
	NullsNotDistinct bool
}

type fkMeta struct {
	Name        string
	PGDatabase  string
	Database    string
	Table       string
	Columns     []string
	RefDatabase string
	RefTable    string
	RefColumns  []string
}

type seqMeta struct {
	PGDatabase  string
	TableSchema string
	TableName   string
	ColumnName  string
	SeqSchema   string
	SeqName     string
	LastValue   int64
}

type enumMeta struct {
	PGDatabase string
	Schema     string
	Name       string
	Labels     []string
}

type tableMeta struct {
	PGDatabase        string // Logical DB for PostgreSQL ("postgres" by default); "" for MySQL
	Schema            string // Schema name inside PGDatabase, or DB name in MySQL
	Database          string // Qualified display DB name (for PG != "postgres": pgDB.schema, else schema)
	Name              string
	AutoIncrement     int64
	Columns           []columnMeta
	ColByName         map[string]columnMeta
	DataColumns       []string // non-generated columns
	PKColumns         []domain.PKColumnSpec
	ExplicitPK        bool
	UniqueConstraints []uniqueMeta
	ForeignKeys       []fkMeta
	Triggers          []string
	HasIdentityAlways bool
	AutoIncColumn     string
}

func (t tableMeta) key() string { return t.Database + "." + t.Name }

func qualifyPGNamespace(pgDB, schema string) string {
	if pgDB == "" || pgDB == "postgres" {
		return schema
	}
	return pgDB + "." + schema
}

type instanceSchema struct {
	Engine    domain.Engine
	PGDBs     []string
	Tables    map[string]tableMeta
	Enums     map[string]enumMeta
	Sequences map[string]seqMeta // "qualDB.table.col" -> seqMeta
}

func classifyColumnKind(dataType, colType string) domain.PKColumnKind {
	dt := strings.ToLower(strings.TrimSpace(dataType))
	if dt == "" {
		dt = strings.ToLower(strings.TrimSpace(colType))
	}
	switch {
	case strings.HasPrefix(dt, "int"), strings.HasPrefix(dt, "bigint"),
		strings.HasPrefix(dt, "smallint"), strings.HasPrefix(dt, "tinyint"),
		strings.HasPrefix(dt, "mediumint"), strings.HasPrefix(dt, "serial"),
		strings.HasPrefix(dt, "bigserial"), strings.HasPrefix(dt, "smallserial"):
		return domain.PKInteger
	case strings.HasPrefix(dt, "decimal"), strings.HasPrefix(dt, "numeric"), strings.HasPrefix(dt, "dec"):
		return domain.PKDecimal
	case strings.HasPrefix(dt, "float"), strings.HasPrefix(dt, "double"), strings.HasPrefix(dt, "real"):
		return domain.PKFloat
	default:
		return domain.PKText
	}
}

func (m *Merger) listPGDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	var dbs []string
	err := queryEach(ctx, db, `/* db-branch:pg-databases */
SELECT datname FROM pg_catalog.pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`, func(r *sql.Rows) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		if name != "" {
			dbs = append(dbs, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query pg databases: %w", err)
	}
	if len(dbs) == 0 {
		return []string{"postgres"}, nil
	}
	return dbs, nil
}

func (m *Merger) inspectInstance(ctx context.Context, acc domain.AdminAccess, engine domain.Engine) (instanceSchema, error) {
	sch := instanceSchema{
		Engine:    engine,
		Tables:    map[string]tableMeta{},
		Enums:     map[string]enumMeta{},
		Sequences: map[string]seqMeta{},
	}
	if engine == domain.MySQL {
		db, err := m.openDB(acc)
		if err != nil {
			return sch, err
		}
		defer db.Close()
		_, _ = db.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
		if err := m.inspectSingleDB(ctx, db, engine, "", &sch); err != nil {
			return sch, err
		}
		return sch, nil
	}

	rootDB, err := m.openDB(acc, "postgres")
	if err != nil {
		return sch, err
	}
	pgDBs, err := m.listPGDatabases(ctx, rootDB)
	rootDB.Close()
	if err != nil {
		return sch, err
	}
	sch.PGDBs = pgDBs
	for _, pgDB := range pgDBs {
		db, err := m.openDB(acc, pgDB)
		if err != nil {
			return sch, fmt.Errorf("open database %s: %w", pgDB, err)
		}
		err = m.inspectSingleDB(ctx, db, engine, pgDB, &sch)
		db.Close()
		if err != nil {
			return sch, err
		}
	}
	return sch, nil
}

func (m *Merger) inspectSingleDB(ctx context.Context, db *sql.DB, engine domain.Engine, pgDB string, sch *instanceSchema) error {
	if err := m.inspectTables(ctx, db, engine, pgDB, sch); err != nil {
		return err
	}
	if err := m.inspectColumns(ctx, db, engine, pgDB, sch); err != nil {
		return err
	}
	if err := m.inspectIndexes(ctx, db, engine, pgDB, sch); err != nil {
		return err
	}
	if err := m.inspectForeignKeys(ctx, db, engine, pgDB, sch); err != nil {
		return err
	}
	if engine == domain.MySQL {
		return m.inspectTriggers(ctx, db, sch)
	}
	if err := m.inspectPGEnums(ctx, db, pgDB, sch); err != nil {
		return err
	}
	return m.inspectPGSequences(ctx, db, pgDB, sch)
}

func (m *Merger) inspectTables(ctx context.Context, db *sql.DB, engine domain.Engine, pgDB string, sch *instanceSchema) error {
	q := `/* db-branch:tables */
SELECT TABLE_SCHEMA, TABLE_NAME, COALESCE(AUTO_INCREMENT, 0)
FROM information_schema.TABLES
WHERE TABLE_TYPE = 'BASE TABLE'
  AND TABLE_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY TABLE_SCHEMA, TABLE_NAME`
	if engine == domain.Postgres {
		q = `/* db-branch:tables */
SELECT n.nspname, c.relname
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND NOT c.relispartition
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname`
	}
	if err := queryEach(ctx, db, q, func(r *sql.Rows) error {
		var schemaName, tblName string
		var autoInc sql.NullInt64
		if engine == domain.MySQL {
			if err := r.Scan(&schemaName, &tblName, &autoInc); err != nil {
				return err
			}
		} else {
			if err := r.Scan(&schemaName, &tblName); err != nil {
				return err
			}
		}
		tm := tableMeta{
			PGDatabase:    pgDB,
			Schema:        schemaName,
			Database:      qualifyPGNamespace(pgDB, schemaName),
			Name:          tblName,
			AutoIncrement: autoInc.Int64,
			ColByName:     map[string]columnMeta{},
		}
		sch.Tables[tm.key()] = tm
		return nil
	}); err != nil {
		return fmt.Errorf("query tables: %w", err)
	}
	return nil
}

func (m *Merger) inspectColumns(ctx context.Context, db *sql.DB, engine domain.Engine, pgDB string, sch *instanceSchema) error {
	q := `/* db-branch:columns */
SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, ORDINAL_POSITION,
       DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT,
       EXTRA, COALESCE(GENERATION_EXPRESSION, '')
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION`
	if engine == domain.Postgres {
		q = `/* db-branch:columns */
SELECT n.nspname, c.relname, a.attname, a.attnum,
       pg_catalog.format_type(a.atttypid, a.atttypmod) AS col_type,
       NOT a.attnotnull AS is_nullable,
       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '') AS col_default,
       a.attidentity::text AS attidentity,
       a.attgenerated::text AS attgenerated
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE c.relkind IN ('r', 'p')
  AND NOT c.relispartition
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY n.nspname, c.relname, a.attnum`
	}
	if err := queryEach(ctx, db, q, func(r *sql.Rows) error {
		var schemaName, tblName, colName string
		var ord int
		var cm columnMeta
		if engine == domain.MySQL {
			var dt, ct, isNull, extra, genExpr string
			var defVal sql.NullString
			if err := r.Scan(&schemaName, &tblName, &colName, &ord, &dt, &ct, &isNull, &defVal, &extra, &genExpr); err != nil {
				return err
			}
			upperExtra := strings.ToUpper(extra)
			isGen := (strings.Contains(upperExtra, "GENERATED") && !strings.Contains(upperExtra, "DEFAULT_GENERATED")) || genExpr != ""
			cm = columnMeta{
				Name:           colName,
				Ordinal:        ord,
				DataType:       dt,
				ColumnType:     ct,
				Nullable:       strings.EqualFold(isNull, "YES") || strings.EqualFold(isNull, "true"),
				HasDefault:     defVal.Valid,
				Default:        defVal.String,
				Extra:          extra,
				IsGenerated:    isGen,
				IsStoredGen:    strings.Contains(upperExtra, "STORED"),
				GenerationExpr: genExpr,
				IsAutoInc:      strings.Contains(strings.ToLower(extra), "auto_increment"),
				Kind:           classifyColumnKind(dt, ct),
			}
		} else {
			var ct, defExpr, attIdent, attGen string
			var isNull bool
			if err := r.Scan(&schemaName, &tblName, &colName, &ord, &ct, &isNull, &defExpr, &attIdent, &attGen); err != nil {
				return err
			}
			attIdent = strings.TrimSpace(attIdent)
			attGen = strings.TrimSpace(attGen)
			isGen := attGen == "s" || attGen == "v"
			hasDef := !isGen && defExpr != ""
			var genExpr string
			if isGen {
				genExpr, defExpr = defExpr, ""
			}
			cm = columnMeta{
				Name:           colName,
				Ordinal:        ord,
				DataType:       ct,
				ColumnType:     ct,
				Nullable:       isNull,
				HasDefault:     hasDef,
				Default:        defExpr,
				IsGenerated:    isGen,
				IsStoredGen:    attGen == "s",
				GenerationExpr: genExpr,
				IsIdentity:     attIdent == "a" || attIdent == "d",
				IdentityAlways: attIdent == "a",
				Kind:           classifyColumnKind(ct, ct),
			}
		}
		key := qualifyPGNamespace(pgDB, schemaName) + "." + tblName
		tm, ok := sch.Tables[key]
		if !ok {
			return nil
		}
		tm.Columns = append(tm.Columns, cm)
		tm.ColByName[cm.Name] = cm
		if !cm.IsGenerated {
			tm.DataColumns = append(tm.DataColumns, cm.Name)
		}
		if cm.IdentityAlways {
			tm.HasIdentityAlways = true
		}
		if cm.IsAutoInc {
			tm.AutoIncColumn = cm.Name
		}
		sch.Tables[key] = tm
		return nil
	}); err != nil {
		return fmt.Errorf("query columns: %w", err)
	}
	return nil
}

func (m *Merger) inspectIndexes(ctx context.Context, db *sql.DB, engine domain.Engine, pgDB string, sch *instanceSchema) error {
	q := `/* db-branch:indexes */
SELECT TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, SEQ_IN_INDEX
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
  AND NON_UNIQUE = 0
ORDER BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`
	if engine == domain.Postgres {
		q = `/* db-branch:indexes */
SELECT n.nspname, t.relname, ic.relname AS index_name,
       i.indisprimary, i.indnullsnotdistinct, a.attname, k.ord
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
JOIN pg_catalog.pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
WHERE t.relkind IN ('r', 'p')
  AND NOT t.relispartition
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
  AND i.indisunique
  AND i.indisvalid
  AND i.indpred IS NULL
ORDER BY n.nspname, t.relname, ic.relname, k.ord`
	}
	type rawIndex struct {
		name             string
		primary          bool
		nullsNotDistinct bool
		cols             []string
	}
	byTableIdx := map[string][]rawIndex{}
	if err := queryEach(ctx, db, q, func(r *sql.Rows) error {
		var schemaName, tblName, idxName, colName string
		var p1, p2 any
		var ord int
		if err := r.Scan(&schemaName, &tblName, &idxName, &p1, &p2, &colName, &ord); err != nil {
			return err
		}
		isPrimary := idxName == "PRIMARY"
		if b, ok := p1.(bool); ok && b {
			isPrimary = true
		}
		nullsNotDist := false
		if b, ok := p2.(bool); ok && b {
			nullsNotDist = true
		}
		tKey := qualifyPGNamespace(pgDB, schemaName) + "." + tblName
		list := byTableIdx[tKey]
		found := false
		for i := range list {
			if list[i].name == idxName {
				list[i].cols = append(list[i].cols, colName)
				found = true
				break
			}
		}
		if !found {
			byTableIdx[tKey] = append(list, rawIndex{
				name: idxName, primary: isPrimary, nullsNotDistinct: nullsNotDist, cols: []string{colName},
			})
		}
		return nil
	}); err != nil {
		return fmt.Errorf("query indexes: %w", err)
	}

	for tKey, tm := range sch.Tables {
		if tm.PGDatabase != pgDB {
			continue
		}
		idxs := byTableIdx[tKey]
		var promotedIdx string
		for _, idx := range idxs {
			if idx.primary {
				tm.ExplicitPK = true
				for _, c := range idx.cols {
					tm.PKColumns = append(tm.PKColumns, domain.PKColumnSpec{Name: c, Kind: tm.ColByName[c].Kind})
				}
				break
			}
		}
		if len(tm.PKColumns) == 0 {
			// Promote first all-NOT NULL unique index as effective primary key
			for _, idx := range idxs {
				allNotNull := len(idx.cols) > 0
				for _, c := range idx.cols {
					if cm, ok := tm.ColByName[c]; !ok || cm.Nullable || cm.IsGenerated {
						allNotNull = false
						break
					}
				}
				if allNotNull {
					promotedIdx = idx.name
					for _, c := range idx.cols {
						tm.PKColumns = append(tm.PKColumns, domain.PKColumnSpec{Name: c, Kind: tm.ColByName[c].Kind})
					}
					break
				}
			}
		}
		for _, idx := range idxs {
			if idx.primary || idx.name == promotedIdx {
				continue
			}
			tm.UniqueConstraints = append(tm.UniqueConstraints, uniqueMeta{
				Name: idx.name, Columns: idx.cols, NullsNotDistinct: idx.nullsNotDistinct,
			})
		}
		sch.Tables[tKey] = tm
	}
	return nil
}

func (m *Merger) inspectForeignKeys(ctx context.Context, db *sql.DB, engine domain.Engine, pgDB string, sch *instanceSchema) error {
	q := `/* db-branch:fks */
SELECT kcu.CONSTRAINT_SCHEMA, kcu.TABLE_NAME, kcu.CONSTRAINT_NAME,
       kcu.COLUMN_NAME, kcu.REFERENCED_TABLE_SCHEMA, kcu.REFERENCED_TABLE_NAME,
       kcu.REFERENCED_COLUMN_NAME, kcu.ORDINAL_POSITION
FROM information_schema.KEY_COLUMN_USAGE kcu
WHERE kcu.REFERENCED_TABLE_NAME IS NOT NULL
  AND kcu.CONSTRAINT_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY kcu.CONSTRAINT_SCHEMA, kcu.TABLE_NAME, kcu.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`
	if engine == domain.Postgres {
		q = `/* db-branch:fks */
SELECT ns1.nspname AS child_schema, cl1.relname AS child_table,
       con.conname AS constraint_name, a1.attname AS child_col,
       ns2.nspname AS parent_schema, cl2.relname AS parent_table,
       a2.attname AS parent_col, k.ord
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class cl1 ON cl1.oid = con.conrelid
JOIN pg_catalog.pg_namespace ns1 ON ns1.oid = cl1.relnamespace
JOIN pg_catalog.pg_class cl2 ON cl2.oid = con.confrelid
JOIN pg_catalog.pg_namespace ns2 ON ns2.oid = cl2.relnamespace
JOIN LATERAL unnest(con.conkey, con.confkey) WITH ORDINALITY AS k(c_attnum, p_attnum, ord) ON TRUE
JOIN pg_catalog.pg_attribute a1 ON a1.attrelid = cl1.oid AND a1.attnum = k.c_attnum
JOIN pg_catalog.pg_attribute a2 ON a2.attrelid = cl2.oid AND a2.attnum = k.p_attnum
WHERE con.contype = 'f'
  AND NOT cl1.relispartition
  AND ns1.nspname !~ '^pg_'
  AND ns1.nspname <> 'information_schema'
ORDER BY ns1.nspname, cl1.relname, con.conname, k.ord`
	}
	if err := queryEach(ctx, db, q, func(r *sql.Rows) error {
		var cs, ct, con, cc, ps, pt, pc string
		var ord int
		if err := r.Scan(&cs, &ct, &con, &cc, &ps, &pt, &pc, &ord); err != nil {
			return err
		}
		qualChild := qualifyPGNamespace(pgDB, cs)
		qualParent := qualifyPGNamespace(pgDB, ps)
		tKey := qualChild + "." + ct
		tm, ok := sch.Tables[tKey]
		if !ok {
			return nil
		}
		found := false
		for i := range tm.ForeignKeys {
			if tm.ForeignKeys[i].Name == con {
				tm.ForeignKeys[i].Columns = append(tm.ForeignKeys[i].Columns, cc)
				tm.ForeignKeys[i].RefColumns = append(tm.ForeignKeys[i].RefColumns, pc)
				found = true
				break
			}
		}
		if !found {
			tm.ForeignKeys = append(tm.ForeignKeys, fkMeta{
				Name:        con,
				PGDatabase:  pgDB,
				Database:    qualChild,
				Table:       ct,
				Columns:     []string{cc},
				RefDatabase: qualParent,
				RefTable:    pt,
				RefColumns:  []string{pc},
			})
		}
		sch.Tables[tKey] = tm
		return nil
	}); err != nil {
		return fmt.Errorf("query foreign keys: %w", err)
	}
	return nil
}

func (m *Merger) inspectTriggers(ctx context.Context, db *sql.DB, sch *instanceSchema) error {
	if err := queryEach(ctx, db, `/* db-branch:triggers */
SELECT EVENT_OBJECT_SCHEMA, EVENT_OBJECT_TABLE, TRIGGER_NAME
FROM information_schema.TRIGGERS
WHERE EVENT_OBJECT_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY EVENT_OBJECT_SCHEMA, EVENT_OBJECT_TABLE, TRIGGER_NAME`, func(r *sql.Rows) error {
		var s, t, tr string
		if err := r.Scan(&s, &t, &tr); err != nil {
			return err
		}
		tKey := s + "." + t
		if tm, ok := sch.Tables[tKey]; ok {
			tm.Triggers = append(tm.Triggers, tr)
			sch.Tables[tKey] = tm
		}
		return nil
	}); err != nil {
		return fmt.Errorf("query triggers: %w", err)
	}
	return nil
}

func (m *Merger) inspectPGEnums(ctx context.Context, db *sql.DB, pgDB string, sch *instanceSchema) error {
	if err := queryEach(ctx, db, `/* db-branch:enums */
SELECT n.nspname, t.typname, e.enumlabel, e.enumsortorder
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
JOIN pg_catalog.pg_enum e ON e.enumtypid = t.oid
WHERE t.typtype = 'e'
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
ORDER BY n.nspname, t.typname, e.enumsortorder`, func(r *sql.Rows) error {
		var s, t, lbl string
		var ord float64
		if err := r.Scan(&s, &t, &lbl, &ord); err != nil {
			return err
		}
		eKey := qualifyPGNamespace(pgDB, s) + "." + t
		em := sch.Enums[eKey]
		em.PGDatabase = pgDB
		em.Schema = s
		em.Name = t
		em.Labels = append(em.Labels, lbl)
		sch.Enums[eKey] = em
		return nil
	}); err != nil {
		return fmt.Errorf("query enums: %w", err)
	}
	return nil
}

func (m *Merger) inspectPGSequences(ctx context.Context, db *sql.DB, pgDB string, sch *instanceSchema) error {
	var seqList []seqMeta
	if err := queryEach(ctx, db, `/* db-branch:sequences */
SELECT ns.nspname AS table_schema, t.relname AS table_name, a.attname AS col_name,
       sns.nspname AS seq_schema, s.relname AS seq_name
FROM pg_catalog.pg_depend d
JOIN pg_catalog.pg_class s ON s.oid = d.objid AND s.relkind = 'S'
JOIN pg_catalog.pg_namespace sns ON sns.oid = s.relnamespace
JOIN pg_catalog.pg_class t ON t.oid = d.refobjid AND t.relkind IN ('r', 'p')
JOIN pg_catalog.pg_namespace ns ON ns.oid = t.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
WHERE d.classid = 'pg_class'::regclass
  AND d.refclassid = 'pg_class'::regclass
  AND d.deptype IN ('a', 'i')
  AND NOT t.relispartition
  AND ns.nspname !~ '^pg_'
  AND ns.nspname <> 'information_schema'
ORDER BY ns.nspname, t.relname, a.attnum`, func(r *sql.Rows) error {
		var sm seqMeta
		if err := r.Scan(&sm.TableSchema, &sm.TableName, &sm.ColumnName, &sm.SeqSchema, &sm.SeqName); err != nil {
			return err
		}
		sm.PGDatabase = pgDB
		seqList = append(seqList, sm)
		return nil
	}); err != nil {
		return fmt.Errorf("query sequences: %w", err)
	}
	for _, sm := range seqList {
		q := fmt.Sprintf("/* db-branch:seq-val %s.%s */ SELECT COALESCE(last_value, 0) FROM pg_catalog.pg_sequences WHERE schemaname = %s AND sequencename = %s",
			sm.SeqSchema, sm.SeqName, mustLiteral(domain.Postgres, sm.SeqSchema), mustLiteral(domain.Postgres, sm.SeqName))
		var lastVal sql.NullInt64
		_ = db.QueryRowContext(ctx, q).Scan(&lastVal)
		sm.LastValue = lastVal.Int64
		key := qualifyPGNamespace(pgDB, sm.TableSchema) + "." + sm.TableName + "." + sm.ColumnName
		sch.Sequences[key] = sm
	}
	return nil
}

func quoteIdent(engine domain.Engine, s string) string {
	q, _ := QuoteIdent(engine, s)
	return q
}

func validateIdents(engine domain.Engine, names ...string) error {
	for _, n := range names {
		if _, err := QuoteIdent(engine, n); err != nil {
			return err
		}
	}
	return nil
}

func mustLiteral(engine domain.Engine, s string) string {
	lit, _ := QuoteStringLiteral(engine, s)
	return lit
}

type sqlStmt struct {
	Query string
	Args  []any
}

type ddlStmt struct {
	PGDatabase string
	Query      string
}

type tableApplyPlan struct {
	PGDatabase string
	Ref        domain.TableRef
	Deletes    []sqlStmt
	Updates    []sqlStmt
	Inserts    []sqlStmt
}

type autoIncSync struct {
	Database string
	Table    string
	Column   string
	MinVal   int64
}

type seqSync struct {
	PGDatabase  string
	TableSchema string
	TableName   string
	ColumnName  string
	SeqSchema   string
	SeqName     string
	MinVal      int64
}

type opaquePlan struct {
	Engine       domain.Engine
	CreateTypes  []ddlStmt
	CreateTables map[domain.TableRef]ddlStmt
	AlterTables  []ddlStmt
	DropTables   map[domain.TableRef]ddlStmt
	TablePlans   map[domain.TableRef]tableApplyPlan
	InsertOrder  []domain.TableRef
	DeleteOrder  []domain.TableRef
	VerifyFKs    []fkMeta
	AutoIncs     []autoIncSync
	Sequences    []seqSync
}

type mergedRowInfo struct {
	Key        map[string]any
	KeyValues  map[string]domain.RowValue
	AllValues  map[string]domain.RowValue
	FromSource bool
}

type dbConnPool struct {
	acc    domain.AdminAccess
	merger *Merger
	conns  map[string]*sql.DB
}

func newDBConnPool(m *Merger, acc domain.AdminAccess) *dbConnPool {
	return &dbConnPool{acc: acc, merger: m, conns: map[string]*sql.DB{}}
}

func (p *dbConnPool) get(pgDB string) (*sql.DB, error) {
	key := ""
	if p.acc.Engine == domain.Postgres {
		key = pgDB
		if key == "" {
			key = "postgres"
		}
	}
	if db, ok := p.conns[key]; ok {
		return db, nil
	}
	db, err := p.merger.openDB(p.acc, key)
	if err != nil {
		return nil, err
	}
	p.conns[key] = db
	return db, nil
}

func (p *dbConnPool) close() {
	for _, db := range p.conns {
		_ = db.Close()
	}
}

// Diff computes a 3-way schema and row diff across Ancestor, Source and Target.
func (m *Merger) Diff(ctx context.Context, req domain.MergeDiffRequest) (domain.MergePlan, error) {
	engine := req.Target.Engine
	if engine == "" {
		engine = req.Source.Engine
	}
	req.Ancestor.Engine = engine
	req.Source.Engine = engine
	req.Target.Engine = engine

	schA, err := m.inspectInstance(ctx, req.Ancestor, engine)
	if err != nil {
		return domain.MergePlan{}, fmt.Errorf("inspect ancestor: %w", err)
	}
	schS, err := m.inspectInstance(ctx, req.Source, engine)
	if err != nil {
		return domain.MergePlan{}, fmt.Errorf("inspect source: %w", err)
	}
	schT, err := m.inspectInstance(ctx, req.Target, engine)
	if err != nil {
		return domain.MergePlan{}, fmt.Errorf("inspect target: %w", err)
	}

	poolA := newDBConnPool(m, req.Ancestor)
	defer poolA.close()
	poolS := newDBConnPool(m, req.Source)
	defer poolS.close()
	poolT := newDBConnPool(m, req.Target)
	defer poolT.close()

	sortedKeys, tableKeySet, allFKs, allFKMetas, filterSet, insertOrder, deleteOrder, err := prepareTablesAndOrder(req.Tables, schA, schS, schT)
	if err != nil {
		return domain.MergePlan{}, err
	}

	var plan domain.MergePlan
	addConflict := func(c domain.MergeConflict) {
		plan.TotalConflicts++
		if len(plan.Conflicts) < domain.MaxMergeConflicts {
			plan.Conflicts = append(plan.Conflicts, c)
		} else {
			plan.Truncated = true
		}
	}

	op := opaquePlan{
		Engine:       engine,
		CreateTables: map[domain.TableRef]ddlStmt{},
		DropTables:   map[domain.TableRef]ddlStmt{},
		TablePlans:   map[domain.TableRef]tableApplyPlan{},
		InsertOrder:  insertOrder,
		DeleteOrder:  deleteOrder,
	}

	if engine == domain.Postgres && req.AllowDDL {
		collectNewPGEnums(&op, schS, schT, addConflict)
	}

	mergedRowsByTable := map[string][]mergedRowInfo{}
	droppedTables := map[string]bool{}

	getDB := func(p *dbConnPool, pgDB string) *sql.DB {
		db, gerr := p.get(pgDB)
		if gerr != nil && err == nil {
			err = gerr
		}
		return db
	}

	for _, tKey := range sortedKeys {
		tmA, inA := schA.Tables[tKey]
		tmS, inS := schS.Tables[tKey]
		tmT, inT := schT.Tables[tKey]
		ref := tableKeySet[tKey]

		switch {
		case !inA && !inS && inT:
			dbT := getDB(poolT, tmT.PGDatabase)
			if err != nil {
				return domain.MergePlan{}, err
			}
			rows, rerr := m.loadTableRowsForCheck(ctx, dbT, engine, tmT)
			if rerr != nil {
				return domain.MergePlan{}, rerr
			}
			mergedRowsByTable[tKey] = rows

		case !inA && inS:
			dbS := getDB(poolS, tmS.PGDatabase)
			if err != nil {
				return domain.MergePlan{}, err
			}
			if abortPlan, serr := m.diffNewSourceTable(ctx, dbS, engine, req.AllowDDL, ref, inT, tmS, schS, schT, &plan, &op, mergedRowsByTable, addConflict); serr != nil {
				return abortPlan, serr
			}

		case inA && !inS && inT:
			if !req.AllowDDL {
				addConflict(domain.MergeConflict{
					Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table,
					Message: fmt.Sprintf("table %s.%s was dropped in source (use --allow-ddl to apply schema changes)", ref.Database, ref.Table),
				})
				continue
			}
			dbA, dbT := getDB(poolA, tmA.PGDatabase), getDB(poolT, tmT.PGDatabase)
			if err != nil {
				return domain.MergePlan{}, err
			}
			modifiedInT, merr := m.tableModifiedBetween(ctx, dbA, dbT, engine, tmA, tmT)
			if merr != nil {
				return domain.MergePlan{}, merr
			}
			if modifiedInT {
				addConflict(domain.MergeConflict{
					Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table,
					Message: fmt.Sprintf("table %s.%s was dropped in source but modified in target", ref.Database, ref.Table),
				})
				continue
			}
			dropSQL, derr := buildDropTableSQL(engine, tmT.Schema, tmT.Name)
			if derr != nil {
				addConflict(domain.MergeConflict{Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table, Message: derr.Error()})
				continue
			}
			op.DropTables[ref] = ddlStmt{PGDatabase: tmT.PGDatabase, Query: dropSQL}
			droppedTables[tKey] = true
			plan.DDL++
			plan.Tables = append(plan.Tables, domain.MergeTableSummary{Database: ref.Database, Table: ref.Table, DDL: "DROP TABLE"})

		case inA && inS && !inT:
			dbA, dbS := getDB(poolA, tmA.PGDatabase), getDB(poolS, tmS.PGDatabase)
			if err != nil {
				return domain.MergePlan{}, err
			}
			modifiedInS, merr := m.tableModifiedBetween(ctx, dbA, dbS, engine, tmA, tmS)
			if merr != nil {
				return domain.MergePlan{}, merr
			}
			if !req.AllowDDL || modifiedInS {
				addConflict(domain.MergeConflict{
					Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table,
					Message: fmt.Sprintf("table %s.%s was dropped in target", ref.Database, ref.Table),
				})
			}

		case inS && inT:
			dbA, dbS, dbT := getDB(poolA, tmA.PGDatabase), getDB(poolS, tmS.PGDatabase), getDB(poolT, tmT.PGDatabase)
			if err != nil {
				return domain.MergePlan{}, err
			}
			if abortPlan, derr := m.diffExistingTable(ctx, dbA, dbS, dbT, engine, req, ref, tmA, tmS, tmT, schS, schT, &plan, &op, mergedRowsByTable, addConflict); derr != nil {
				return abortPlan, derr
			}
		}
	}

	_ = allFKs
	precheckForeignKeys(allFKMetas, filterSet, droppedTables, mergedRowsByTable, &op, addConflict)

	if plan.TotalConflicts > 0 {
		plan.Code = "merge_conflict"
		plan.Message = fmt.Sprintf("merge blocked by %d conflict(s)", plan.TotalConflicts)
	}
	plan.Opaque = op
	return plan, nil
}

func prepareTablesAndOrder(filterTables []string, schA, schS, schT instanceSchema) (
	sortedKeys []string,
	tableKeySet map[string]domain.TableRef,
	allFKs []domain.ForeignKey,
	allFKMetas []fkMeta,
	filterSet map[string]bool,
	insertOrder []domain.TableRef,
	deleteOrder []domain.TableRef,
	err error,
) {
	tableKeySet = map[string]domain.TableRef{}
	for _, sch := range []instanceSchema{schA, schS, schT} {
		for k, tm := range sch.Tables {
			tableKeySet[k] = domain.TableRef{Database: tm.Database, Table: tm.Name}
		}
	}
	seenFK := map[string]bool{}
	for _, sch := range []instanceSchema{schT, schS} {
		for _, tm := range sch.Tables {
			for _, fk := range tm.ForeignKeys {
				sig := fmt.Sprintf("%s.%s.%s->%s.%s", fk.Database, fk.Table, fk.Name, fk.RefDatabase, fk.RefTable)
				if seenFK[sig] {
					continue
				}
				seenFK[sig] = true
				allFKMetas = append(allFKMetas, fk)
				allFKs = append(allFKs, domain.ForeignKey{
					Name:          fk.Name,
					Child:         domain.TableRef{Database: fk.Database, Table: fk.Table},
					ChildColumns:  fk.Columns,
					Parent:        domain.TableRef{Database: fk.RefDatabase, Table: fk.RefTable},
					ParentColumns: fk.RefColumns,
				})
			}
		}
	}
	allTableList := make([]domain.TableRef, 0, len(tableKeySet))
	for _, ref := range tableKeySet {
		allTableList = append(allTableList, ref)
	}
	sort.Slice(allTableList, func(i, j int) bool { return allTableList[i].String() < allTableList[j].String() })

	if len(filterTables) > 0 {
		filteredRefs, err := domain.ValidateTableFilter(filterTables, allTableList, allFKs)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, err
		}
		filterSet = make(map[string]bool, len(filteredRefs))
		for _, t := range filteredRefs {
			filterSet[t.String()] = true
		}
	}
	var allRefs []domain.TableRef
	for k, ref := range tableKeySet {
		if filterSet != nil && !filterSet[k] {
			continue
		}
		sortedKeys = append(sortedKeys, k)
		allRefs = append(allRefs, ref)
	}
	sort.Strings(sortedKeys)
	insertOrder, deleteOrder, _ = domain.TopoSortTables(allRefs, allFKs)
	return
}

func collectNewPGEnums(op *opaquePlan, schS, schT instanceSchema, addConflict func(domain.MergeConflict)) {
	var enumKeys []string
	for k := range schS.Enums {
		if _, exists := schT.Enums[k]; !exists {
			enumKeys = append(enumKeys, k)
		}
	}
	sort.Strings(enumKeys)
	for _, k := range enumKeys {
		em := schS.Enums[k]
		ddl, err := buildCreateEnumSQL(em)
		if err != nil {
			addConflict(domain.MergeConflict{Type: domain.ConflictSchema, Database: em.Schema, Table: em.Name, Message: err.Error()})
			continue
		}
		op.CreateTypes = append(op.CreateTypes, ddlStmt{PGDatabase: em.PGDatabase, Query: ddl})
	}
}

func (m *Merger) diffNewSourceTable(
	ctx context.Context, dbS *sql.DB, engine domain.Engine, allowDDL bool,
	ref domain.TableRef, inT bool, tmS tableMeta, schS, schT instanceSchema,
	plan *domain.MergePlan, op *opaquePlan, mergedRowsByTable map[string][]mergedRowInfo,
	addConflict func(domain.MergeConflict),
) (domain.MergePlan, error) {
	if inT {
		addConflict(domain.MergeConflict{
			Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table,
			Message: fmt.Sprintf("table %s.%s was created in both source and target", ref.Database, ref.Table),
		})
		return domain.MergePlan{}, nil
	}
	if !allowDDL {
		addConflict(domain.MergeConflict{
			Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table,
			Message: fmt.Sprintf("table %s.%s was created in source (use --allow-ddl to apply schema changes)", ref.Database, ref.Table),
		})
		return domain.MergePlan{}, nil
	}
	createSQL, err := buildCreateTableSQL(engine, tmS)
	if err != nil {
		addConflict(domain.MergeConflict{Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table, Message: err.Error()})
		return domain.MergePlan{}, nil
	}
	op.CreateTables[ref] = ddlStmt{PGDatabase: tmS.PGDatabase, Query: createSQL}
	plan.DDL++

	var inserted int
	tap := tableApplyPlan{PGDatabase: tmS.PGDatabase, Ref: ref}
	tKey := tmS.key()
	if len(tmS.PKColumns) > 0 {
		curS, closeS, err := m.openTableCursor(ctx, dbS, engine, tmS, tmS.DataColumns)
		if err != nil {
			return domain.MergePlan{}, err
		}
		diffRes, err := domain.DiffTableRows(
			domain.TableMergeSpec{Database: ref.Database, Table: ref.Table, PKColumns: tmS.PKColumns, Columns: tmS.DataColumns},
			domain.NewSliceRowCursor(nil), curS, domain.NewSliceRowCursor(nil), domain.MaxMergeConflicts,
		)
		closeS()
		if err != nil {
			return domain.MergePlan{}, err
		}
		for _, act := range diffRes.Actions {
			if act.Kind == domain.ActionInsert {
				tap.Inserts = append(tap.Inserts, buildInsertStmt(engine, tmS, act))
				mergedRowsByTable[tKey] = append(mergedRowsByTable[tKey], mergedRowInfo{
					Key: act.Key, KeyValues: act.KeyValues, AllValues: act.AllValues, FromSource: true,
				})
			}
		}
		inserted = diffRes.Inserted
	} else {
		changed, err := m.tableHasAnyRows(ctx, dbS, engine, tmS)
		if err != nil {
			return domain.MergePlan{}, err
		}
		if changed {
			plan.Code = "schema_no_primary_key"
			plan.Message = fmt.Sprintf("table %s.%s has rows but no primary key", ref.Database, ref.Table)
			return *plan, fmt.Errorf("%w: %s.%s", domain.ErrSchemaNoPrimaryKey, ref.Database, ref.Table)
		}
	}
	op.TablePlans[ref] = tap
	plan.Inserted += inserted
	plan.Tables = append(plan.Tables, domain.MergeTableSummary{
		Database: ref.Database, Table: ref.Table, Inserted: inserted, DDL: "CREATE TABLE",
	})
	m.recordSequenceSync(op, engine, tmS, tmS, schS, schT)
	return domain.MergePlan{}, nil
}

func (m *Merger) diffExistingTable(
	ctx context.Context, dbA, dbS, dbT *sql.DB, engine domain.Engine, req domain.MergeDiffRequest,
	ref domain.TableRef, tmA, tmS, tmT tableMeta, schS, schT instanceSchema,
	plan *domain.MergePlan, op *opaquePlan, mergedRowsByTable map[string][]mergedRowInfo,
	addConflict func(domain.MergeConflict),
) (domain.MergePlan, error) {
	alterStmts, addedCols, schemaConflictMsg := compareTableSchema(engine, tmA, tmS, tmT, req.AllowDDL)
	if schemaConflictMsg != "" {
		addConflict(domain.MergeConflict{Type: domain.ConflictSchema, Database: ref.Database, Table: ref.Table, Message: schemaConflictMsg})
		return domain.MergePlan{}, nil
	}
	var ddlLabel string
	if len(alterStmts) > 0 {
		for _, stmt := range alterStmts {
			op.AlterTables = append(op.AlterTables, ddlStmt{PGDatabase: tmS.PGDatabase, Query: stmt})
		}
		plan.DDL += len(alterStmts)
		ddlLabel = "ALTER TABLE ADD COLUMN"
	}

	if len(tmS.PKColumns) == 0 {
		changed, err := m.pklessTableChanged(ctx, dbA, dbS, engine, tmA, tmS)
		if err != nil {
			return domain.MergePlan{}, err
		}
		if changed {
			plan.Code = "schema_no_primary_key"
			plan.Message = fmt.Sprintf("table %s.%s has changes but no primary key or NOT NULL unique key", ref.Database, ref.Table)
			return *plan, fmt.Errorf("%w: %s.%s", domain.ErrSchemaNoPrimaryKey, ref.Database, ref.Table)
		}
		return domain.MergePlan{}, nil
	}

	curA, closeA, err := m.openTableCursorWithAdded(ctx, dbA, engine, tmA, tmS.DataColumns, addedCols)
	if err != nil {
		return domain.MergePlan{}, err
	}
	defer closeA()
	curS, closeS, err := m.openTableCursor(ctx, dbS, engine, tmS, tmS.DataColumns)
	if err != nil {
		return domain.MergePlan{}, err
	}
	defer closeS()
	curT, closeT, err := m.openTableCursorWithAdded(ctx, dbT, engine, tmT, tmS.DataColumns, addedCols)
	if err != nil {
		return domain.MergePlan{}, err
	}
	defer closeT()

	trackT := &trackingCursor{inner: curT, spec: domain.TableMergeSpec{
		Database: ref.Database, Table: ref.Table, PKColumns: tmS.PKColumns, Columns: tmS.DataColumns,
	}}
	diffRes, err := domain.DiffTableRows(trackT.spec, curA, curS, trackT, domain.MaxMergeConflicts)
	if err != nil {
		return domain.MergePlan{}, err
	}
	for _, c := range diffRes.Conflicts {
		addConflict(c)
	}

	hasChanges := diffRes.Inserted > 0 || diffRes.Updated > 0 || diffRes.Deleted > 0 || ddlLabel != ""
	if hasChanges && engine == domain.MySQL && !req.IgnoreTriggers && (len(tmT.Triggers) > 0 || len(tmS.Triggers) > 0) {
		plan.Code = "triggers_not_supported"
		plan.Message = fmt.Sprintf("table %s.%s has triggers; pass --ignore-triggers to allow trigger execution during merge", ref.Database, ref.Table)
		return *plan, fmt.Errorf("%w: %s.%s", domain.ErrTriggersNotSupported, ref.Database, ref.Table)
	}

	mergedRows := buildMergedTableRows(trackT.rows, diffRes.Actions, tmS.PKColumns)
	mergedRowsByTable[tmS.key()] = mergedRows

	for _, u := range tmS.UniqueConstraints {
		for _, c := range checkUniqueConstraint(ref, u, mergedRows, tmS.PKColumns) {
			addConflict(c)
		}
	}

	if hasChanges {
		op.TablePlans[ref] = buildTableApplyPlan(engine, tmS, diffRes.Actions)
		plan.Inserted += diffRes.Inserted
		plan.Updated += diffRes.Updated
		plan.Deleted += diffRes.Deleted
		plan.Tables = append(plan.Tables, domain.MergeTableSummary{
			Database: ref.Database, Table: ref.Table,
			Inserted: diffRes.Inserted, Updated: diffRes.Updated, Deleted: diffRes.Deleted, DDL: ddlLabel,
		})
		m.recordSequenceSync(op, engine, tmS, tmT, schS, schT)
	}
	return domain.MergePlan{}, nil
}

func buildTableApplyPlan(engine domain.Engine, tmS tableMeta, actions []domain.MergeAction) tableApplyPlan {
	uniqueCols := map[string]bool{}
	for _, u := range tmS.UniqueConstraints {
		for _, col := range u.Columns {
			uniqueCols[col] = true
		}
	}
	tap := tableApplyPlan{
		PGDatabase: tmS.PGDatabase,
		Ref:        domain.TableRef{Database: tmS.Database, Table: tmS.Name},
	}
	for _, act := range actions {
		switch act.Kind {
		case domain.ActionDelete:
			tap.Deletes = append(tap.Deletes, buildDeleteStmt(engine, tmS, act))
		case domain.ActionUpdate:
			touchesUnique := false
			for col := range act.Values {
				if uniqueCols[col] {
					touchesUnique = true
					break
				}
			}
			if touchesUnique {
				tap.Deletes = append(tap.Deletes, buildDeleteStmt(engine, tmS, act))
				tap.Inserts = append(tap.Inserts, buildInsertStmt(engine, tmS, act))
			} else {
				tap.Updates = append(tap.Updates, buildUpdateStmt(engine, tmS, act))
			}
		case domain.ActionInsert:
			tap.Inserts = append(tap.Inserts, buildInsertStmt(engine, tmS, act))
		}
	}
	return tap
}

func rowTupleSignature(vals map[string]domain.RowValue, cols []string) string {
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteByte('|')
		}
		v := vals[c]
		if v.Null {
			b.WriteString("<NULL>")
		} else {
			fmt.Fprintf(&b, "%d:%s", len(v.Text), v.Text)
		}
	}
	return b.String()
}

func precheckForeignKeys(
	allFKMetas []fkMeta, filterSet map[string]bool, droppedTables map[string]bool,
	mergedRowsByTable map[string][]mergedRowInfo, op *opaquePlan, addConflict func(domain.MergeConflict),
) {
	parentIndexCache := map[string]map[string]bool{}
	for _, fk := range allFKMetas {
		cKey := fk.Database + "." + fk.Table
		pKey := fk.RefDatabase + "." + fk.RefTable
		if (filterSet != nil && !filterSet[cKey]) || droppedTables[cKey] {
			continue
		}
		op.VerifyFKs = append(op.VerifyFKs, fk)

		cacheKey := pKey + "\x00" + strings.Join(fk.RefColumns, ",")
		idx, ok := parentIndexCache[cacheKey]
		if !ok {
			pRows := mergedRowsByTable[pKey]
			idx = make(map[string]bool, len(pRows))
			for _, pr := range pRows {
				idx[rowTupleSignature(pr.AllValues, fk.RefColumns)] = true
			}
			parentIndexCache[cacheKey] = idx
		}
		for _, cr := range mergedRowsByTable[cKey] {
			anyNull := false
			for _, col := range fk.Columns {
				if v, ok := cr.AllValues[col]; !ok || v.Null {
					anyNull = true
					break
				}
			}
			if anyNull {
				continue
			}
			if !idx[rowTupleSignature(cr.AllValues, fk.Columns)] {
				addConflict(domain.MergeConflict{
					Type: domain.ConflictForeignKey, Database: fk.Database, Table: fk.Table,
					Constraint: fk.Name, Key: cr.Key, Columns: fk.Columns,
					Message: fmt.Sprintf("foreign key %s references non-existent row in %s.%s", fk.Name, fk.RefDatabase, fk.RefTable),
				})
			}
		}
	}
}

func (m *Merger) recordSequenceSync(op *opaquePlan, engine domain.Engine, tmS, tmT tableMeta, schS, schT instanceSchema) {
	if engine == domain.MySQL && tmS.AutoIncColumn != "" {
		op.AutoIncs = append(op.AutoIncs, autoIncSync{
			Database: tmS.Schema, Table: tmS.Name, Column: tmS.AutoIncColumn,
			MinVal: max(tmS.AutoIncrement, tmT.AutoIncrement),
		})
	}
	if engine == domain.Postgres {
		for _, col := range tmS.Columns {
			sKey := tmS.key() + "." + col.Name
			smS, okS := schS.Sequences[sKey]
			smT, okT := schT.Sequences[sKey]
			if !okS && !okT {
				continue
			}
			sm := smS
			if !okS {
				sm = smT
			}
			op.Sequences = append(op.Sequences, seqSync{
				PGDatabase: sm.PGDatabase, TableSchema: tmS.Schema, TableName: tmS.Name, ColumnName: col.Name,
				SeqSchema: sm.SeqSchema, SeqName: sm.SeqName, MinVal: max(smS.LastValue, smT.LastValue),
			})
		}
	}
}

// Apply executes a conflict-free MergePlan against Target and verifies FK integrity.
func (m *Merger) Apply(ctx context.Context, target domain.AdminAccess, plan domain.MergePlan, report func(phase string)) error {
	if plan.TotalConflicts > 0 {
		return fmt.Errorf("%w: %d conflict(s)", domain.ErrMergeConflict, plan.TotalConflicts)
	}
	op, ok := plan.Opaque.(opaquePlan)
	if !ok {
		return nil
	}
	target.Engine = op.Engine
	notify := func(phase string) {
		if report != nil {
			report(phase)
		}
	}

	// Group work per logical DB (always [""] for MySQL; one entry per touched PG database).
	pgDBs := collectPGDBsInPlan(op)
	for _, pgDB := range pgDBs {
		if err := m.applyToDatabase(ctx, target, op, pgDB, notify); err != nil {
			return err
		}
	}
	return nil
}

func collectPGDBsInPlan(op opaquePlan) []string {
	if op.Engine != domain.Postgres {
		return []string{""}
	}
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" {
			d = "postgres"
		}
		seen[d] = true
	}
	for _, s := range op.CreateTypes {
		add(s.PGDatabase)
	}
	for _, s := range op.CreateTables {
		add(s.PGDatabase)
	}
	for _, s := range op.AlterTables {
		add(s.PGDatabase)
	}
	for _, s := range op.DropTables {
		add(s.PGDatabase)
	}
	for _, t := range op.TablePlans {
		add(t.PGDatabase)
	}
	for _, sq := range op.Sequences {
		add(sq.PGDatabase)
	}
	if len(seen) == 0 {
		return []string{"postgres"}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func (m *Merger) applyToDatabase(ctx context.Context, target domain.AdminAccess, op opaquePlan, pgDB string, notify func(string)) error {
	db, err := m.openDB(target, pgDB)
	if err != nil {
		return fmt.Errorf("connect target for apply: %w", err)
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin target session: %w", err)
	}
	defer conn.Close()

	matchDB := func(stmtDB string) bool {
		if op.Engine != domain.Postgres {
			return true
		}
		if stmtDB == "" {
			stmtDB = "postgres"
		}
		return stmtDB == pgDB
	}

	// 1. Schema DDL phase
	var createTypes, createTables, alterTables []string
	for _, s := range op.CreateTypes {
		if matchDB(s.PGDatabase) {
			createTypes = append(createTypes, s.Query)
		}
	}
	for _, ref := range op.InsertOrder {
		if s, ok := op.CreateTables[ref]; ok && matchDB(s.PGDatabase) {
			createTables = append(createTables, s.Query)
		}
	}
	for _, s := range op.AlterTables {
		if matchDB(s.PGDatabase) {
			alterTables = append(alterTables, s.Query)
		}
	}
	if len(createTypes) > 0 || len(createTables) > 0 || len(alterTables) > 0 {
		notify("applying schema")
		if err := execQueries(ctx, conn, append(append(createTypes, createTables...), alterTables...)...); err != nil {
			return err
		}
	}

	// 2. Data DML phase
	notify("applying data")
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin apply transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	disableFK, enableFK := []string{"SET FOREIGN_KEY_CHECKS = 0"}, []string{"SET FOREIGN_KEY_CHECKS = 1"}
	if op.Engine == domain.Postgres {
		disableFK = []string{"SET LOCAL session_replication_role = 'replica'", "SET CONSTRAINTS ALL DEFERRED"}
		enableFK = []string{"SET LOCAL session_replication_role = 'origin'"}
	}
	if err := execQueries(ctx, tx, disableFK...); err != nil {
		return err
	}
	for _, ref := range op.DeleteOrder {
		if tap, ok := op.TablePlans[ref]; ok && matchDB(tap.PGDatabase) {
			if err := execStmts(ctx, tx, tap.Deletes); err != nil {
				return err
			}
		}
	}
	for _, ref := range op.InsertOrder {
		if tap, ok := op.TablePlans[ref]; ok && matchDB(tap.PGDatabase) {
			if err := execStmts(ctx, tx, tap.Updates); err != nil {
				return err
			}
		}
	}
	for _, ref := range op.InsertOrder {
		if tap, ok := op.TablePlans[ref]; ok && matchDB(tap.PGDatabase) {
			if err := execStmts(ctx, tx, tap.Inserts); err != nil {
				return err
			}
		}
	}
	for _, ref := range op.DeleteOrder {
		if s, ok := op.DropTables[ref]; ok && matchDB(s.PGDatabase) {
			if err := execQueries(ctx, tx, s.Query); err != nil {
				return err
			}
		}
	}
	if err := execQueries(ctx, tx, enableFK...); err != nil {
		return err
	}

	// 3. Verifying phase: check FK integrity with LEFT JOIN (MATCH SIMPLE)
	notify("verifying")
	for _, fk := range op.VerifyFKs {
		if !matchDB(fk.PGDatabase) {
			continue
		}
		var orphans int64
		if err := tx.QueryRowContext(ctx, buildVerifyFKQuery(op.Engine, fk)).Scan(&orphans); err != nil {
			return fmt.Errorf("verify foreign key %s: %w", fk.Name, err)
		}
		if orphans > 0 {
			return fmt.Errorf("%w: foreign key %s on %s.%s has %d orphan row(s)", domain.ErrMergeConflict, fk.Name, fk.Database, fk.Table, orphans)
		}
	}
	if err := tx.Commit(); err != nil {
		return wrapDBError(err)
	}

	// 4. Sync AUTO_INCREMENT / SEQUENCE
	return m.syncCounters(ctx, conn, op, pgDB, matchDB)
}

func (m *Merger) syncCounters(ctx context.Context, conn *sql.Conn, op opaquePlan, _ string, matchDB func(string) bool) error {
	if op.Engine == domain.MySQL && len(op.AutoIncs) > 0 {
		_, _ = conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
		for _, ai := range op.AutoIncs {
			qDB, qTbl, qCol := quoteIdent(domain.MySQL, ai.Database), quoteIdent(domain.MySQL, ai.Table), quoteIdent(domain.MySQL, ai.Column)
			maxQ := fmt.Sprintf("/* db-branch:max-col */ SELECT COALESCE(MAX(%s), 0) FROM %s.%s", qCol, qDB, qTbl)
			var maxCol int64
			_ = conn.QueryRowContext(ctx, maxQ).Scan(&maxCol)
			if nextVal := max(maxCol+1, ai.MinVal); nextVal > 1 {
				if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s.%s AUTO_INCREMENT = %d", qDB, qTbl, nextVal)); err != nil {
					return fmt.Errorf("sync auto_increment on %s.%s: %w", ai.Database, ai.Table, err)
				}
			}
		}
	}
	if op.Engine == domain.Postgres {
		for _, sq := range op.Sequences {
			if !matchDB(sq.PGDatabase) {
				continue
			}
			qTS, qTT, qTC := quoteIdent(domain.Postgres, sq.TableSchema), quoteIdent(domain.Postgres, sq.TableName), quoteIdent(domain.Postgres, sq.ColumnName)
			qSS, qSN := quoteIdent(domain.Postgres, sq.SeqSchema), quoteIdent(domain.Postgres, sq.SeqName)
			seqReg := mustLiteral(domain.Postgres, qSS+"."+qSN)
			setQ := fmt.Sprintf(
				"/* db-branch:set-seq */ SELECT CASE WHEN v > 0 THEN pg_catalog.setval(%s::regclass, v, true) ELSE 0::bigint END FROM (SELECT GREATEST(COALESCE((SELECT MAX(%s) FROM %s.%s), 0), %d::bigint) AS v) sub",
				seqReg, qTC, qTS, qTT, sq.MinVal,
			)
			var dummy int64
			if err := conn.QueryRowContext(ctx, setQ).Scan(&dummy); err != nil {
				return fmt.Errorf("sync sequence %s.%s: %w", sq.SeqSchema, sq.SeqName, err)
			}
		}
	}
	return nil
}

type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func execQueries(ctx context.Context, ex sqlExecutor, queries ...string) error {
	for _, q := range queries {
		if _, err := ex.ExecContext(ctx, q); err != nil {
			return wrapDBError(err)
		}
	}
	return nil
}

func execStmts(ctx context.Context, ex sqlExecutor, stmts []sqlStmt) error {
	for _, s := range stmts {
		if _, err := ex.ExecContext(ctx, s.Query, s.Args...); err != nil {
			return wrapDBError(err)
		}
	}
	return nil
}

func wrapDBError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if strings.Contains(msg, "1062") || strings.Contains(msg, "23505") ||
		strings.Contains(lower, "duplicate entry") || strings.Contains(lower, "duplicate key") ||
		strings.Contains(lower, "unique constraint") {
		return fmt.Errorf("%w: unique_violation: %v", domain.ErrMergeConflict, err)
	}
	if strings.Contains(msg, "1451") || strings.Contains(msg, "1452") || strings.Contains(msg, "23503") ||
		strings.Contains(lower, "foreign key constraint") {
		return fmt.Errorf("%w: foreign_key_conflict: %v", domain.ErrMergeConflict, err)
	}
	return err
}

func pgUnqualifySchema(dbField string) string {
	if _, after, ok := strings.Cut(dbField, "."); ok {
		return after
	}
	return dbField
}

func buildVerifyFKQuery(engine domain.Engine, fk fkMeta) string {
	cSchema := fk.Database
	pSchema := fk.RefDatabase
	if engine == domain.Postgres {
		cSchema = pgUnqualifySchema(cSchema)
		pSchema = pgUnqualifySchema(pSchema)
	}
	cDB, cTbl := quoteIdent(engine, cSchema), quoteIdent(engine, fk.Table)
	pDB, pTbl := quoteIdent(engine, pSchema), quoteIdent(engine, fk.RefTable)
	var joinConds, notNullConds []string
	var firstParentCol string
	for i, cCol := range fk.Columns {
		pCol := fk.RefColumns[i]
		qc, qp := quoteIdent(engine, cCol), quoteIdent(engine, pCol)
		if i == 0 {
			firstParentCol = qp
		}
		joinConds = append(joinConds, fmt.Sprintf("c.%s = p.%s", qc, qp))
		notNullConds = append(notNullConds, fmt.Sprintf("c.%s IS NOT NULL", qc))
	}
	return fmt.Sprintf("/* db-branch:verify-fk */ SELECT COUNT(*) FROM %s.%s c LEFT JOIN %s.%s p ON %s WHERE %s AND p.%s IS NULL",
		cDB, cTbl, pDB, pTbl, strings.Join(joinConds, " AND "), strings.Join(notNullConds, " AND "), firstParentCol)
}

func buildCreateEnumSQL(em enumMeta) (string, error) {
	if err := validateIdents(domain.Postgres, em.Schema, em.Name); err != nil {
		return "", err
	}
	if len(em.Labels) == 0 {
		return "", fmt.Errorf("%w: enum %s.%s has no labels", ErrUnsafeDDL, em.Schema, em.Name)
	}
	lits := make([]string, len(em.Labels))
	for i, l := range em.Labels {
		lit, err := QuoteStringLiteral(domain.Postgres, l)
		if err != nil {
			return "", err
		}
		lits[i] = lit
	}
	return fmt.Sprintf("CREATE TYPE %s.%s AS ENUM (%s)",
		quoteIdent(domain.Postgres, em.Schema), quoteIdent(domain.Postgres, em.Name), strings.Join(lits, ", ")), nil
}

func formatColumnDef(engine domain.Engine, col columnMeta) (string, error) {
	qName, err := QuoteIdent(engine, col.Name)
	if err != nil {
		return "", err
	}
	if err := ValidateColumnType(col.ColumnType); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(qName)
	b.WriteByte(' ')
	b.WriteString(strings.TrimSpace(col.ColumnType))

	if col.IsGenerated {
		if err := ValidateSQLExpression(col.GenerationExpr); err != nil {
			return "", err
		}
		mode := "VIRTUAL"
		if col.IsStoredGen || engine == domain.Postgres {
			mode = "STORED"
		}
		fmt.Fprintf(&b, " GENERATED ALWAYS AS (%s) %s", strings.TrimSpace(col.GenerationExpr), mode)
		return b.String(), nil
	}
	if !col.Nullable {
		b.WriteString(" NOT NULL")
	}
	if col.IsIdentity {
		if col.IdentityAlways {
			b.WriteString(" GENERATED ALWAYS AS IDENTITY")
		} else {
			b.WriteString(" GENERATED BY DEFAULT AS IDENTITY")
		}
		return b.String(), nil
	}
	if col.IsAutoInc {
		b.WriteString(" AUTO_INCREMENT")
		return b.String(), nil
	}
	if col.HasDefault {
		defClause, err := formatDefaultClause(engine, col)
		if err != nil {
			return "", err
		}
		b.WriteString(" DEFAULT ")
		b.WriteString(defClause)
	}
	if engine == domain.MySQL && strings.Contains(strings.ToUpper(col.Extra), "ON UPDATE CURRENT_TIMESTAMP") {
		b.WriteString(" ON UPDATE CURRENT_TIMESTAMP")
	}
	return b.String(), nil
}

func formatDefaultClause(engine domain.Engine, col columnMeta) (string, error) {
	raw := strings.TrimSpace(col.Default)
	if raw == "" {
		return "''", nil
	}
	if err := ValidateSQLExpression(raw); err != nil {
		return "", err
	}
	if engine == domain.Postgres || strings.Contains(strings.ToUpper(col.Extra), "DEFAULT_GENERATED") {
		return raw, nil
	}
	upper := strings.ToUpper(raw)
	if upper == "NULL" || upper == "TRUE" || upper == "FALSE" ||
		upper == "CURRENT_TIMESTAMP" || strings.HasPrefix(upper, "CURRENT_TIMESTAMP(") ||
		(strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'")) {
		return raw, nil
	}
	if col.Kind == domain.PKInteger || col.Kind == domain.PKDecimal || col.Kind == domain.PKFloat {
		if _, err := strconv.ParseFloat(raw, 64); err == nil {
			return raw, nil
		}
	}
	return QuoteStringLiteral(engine, raw)
}

func buildCreateTableSQL(engine domain.Engine, tm tableMeta) (string, error) {
	if err := validateIdents(engine, tm.Schema, tm.Name); err != nil {
		return "", err
	}
	if len(tm.Columns) == 0 {
		return "", fmt.Errorf("%w: table %s.%s has no columns", ErrUnsafeDDL, tm.Database, tm.Name)
	}
	var parts []string
	for _, col := range tm.Columns {
		def, err := formatColumnDef(engine, col)
		if err != nil {
			return "", err
		}
		parts = append(parts, def)
	}
	if tm.ExplicitPK && len(tm.PKColumns) > 0 {
		pkCols := make([]string, len(tm.PKColumns))
		for i, pk := range tm.PKColumns {
			pkCols[i] = quoteIdent(engine, pk.Name)
		}
		parts = append(parts, fmt.Sprintf("PRIMARY KEY (%s)", strings.Join(pkCols, ", ")))
	}
	for _, u := range tm.UniqueConstraints {
		if err := validateIdents(engine, append([]string{u.Name}, u.Columns...)...); err != nil {
			return "", err
		}
		uCols := make([]string, len(u.Columns))
		for i, c := range u.Columns {
			uCols[i] = quoteIdent(engine, c)
		}
		nullsClause := ""
		if engine == domain.Postgres && u.NullsNotDistinct {
			nullsClause = " NULLS NOT DISTINCT"
		}
		parts = append(parts, fmt.Sprintf("CONSTRAINT %s UNIQUE%s (%s)", quoteIdent(engine, u.Name), nullsClause, strings.Join(uCols, ", ")))
	}
	return fmt.Sprintf("CREATE TABLE %s.%s (%s)", quoteIdent(engine, tm.Schema), quoteIdent(engine, tm.Name), strings.Join(parts, ", ")), nil
}

func buildAddColumnSQL(engine domain.Engine, schemaName, tblName string, col columnMeta) (string, error) {
	if err := validateIdents(engine, schemaName, tblName); err != nil {
		return "", err
	}
	def, err := formatColumnDef(engine, col)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ALTER TABLE %s.%s ADD COLUMN %s", quoteIdent(engine, schemaName), quoteIdent(engine, tblName), def), nil
}

func buildDropTableSQL(engine domain.Engine, schemaName, tblName string) (string, error) {
	if err := validateIdents(engine, schemaName, tblName); err != nil {
		return "", err
	}
	return fmt.Sprintf("DROP TABLE %s.%s", quoteIdent(engine, schemaName), quoteIdent(engine, tblName)), nil
}

func compareTableSchema(engine domain.Engine, tmA, tmS, tmT tableMeta, allowDDL bool) (alterStmts []string, addedCols map[string]columnMeta, conflictMsg string) {
	if len(tmS.PKColumns) != len(tmT.PKColumns) {
		return nil, nil, fmt.Sprintf("primary key mismatch on %s.%s between source and target", tmS.Database, tmS.Name)
	}
	for i := range tmS.PKColumns {
		if tmS.PKColumns[i].Name != tmT.PKColumns[i].Name {
			return nil, nil, fmt.Sprintf("primary key column mismatch on %s.%s (%s vs %s)", tmS.Database, tmS.Name, tmS.PKColumns[i].Name, tmT.PKColumns[i].Name)
		}
	}
	for _, colT := range tmT.Columns {
		if _, inS := tmS.ColByName[colT.Name]; !inS {
			return nil, nil, fmt.Sprintf("column %s on %s.%s exists in target but not in source", colT.Name, tmS.Database, tmS.Name)
		}
	}
	for _, colA := range tmA.Columns {
		if _, inS := tmS.ColByName[colA.Name]; !inS {
			return nil, nil, fmt.Sprintf("column %s on %s.%s was dropped in source", colA.Name, tmS.Database, tmS.Name)
		}
	}

	addedCols = map[string]columnMeta{}
	for _, colS := range tmS.Columns {
		colT, inT := tmT.ColByName[colS.Name]
		if inT {
			if !strings.EqualFold(strings.TrimSpace(colS.ColumnType), strings.TrimSpace(colT.ColumnType)) {
				return nil, nil, fmt.Sprintf("column %s on %s.%s changed type (%s vs %s)", colS.Name, tmS.Database, tmS.Name, colS.ColumnType, colT.ColumnType)
			}
			continue
		}
		if !allowDDL {
			return nil, nil, fmt.Sprintf("column %s was added to %s.%s in source (use --allow-ddl to apply schema changes)", colS.Name, tmS.Database, tmS.Name)
		}
		if !colS.Nullable && !colS.HasDefault && !colS.IsGenerated {
			return nil, nil, fmt.Sprintf("NOT NULL column %s without DEFAULT on %s.%s cannot be added automatically", colS.Name, tmS.Database, tmS.Name)
		}
		stmt, err := buildAddColumnSQL(engine, tmS.Schema, tmS.Name, colS)
		if err != nil {
			return nil, nil, err.Error()
		}
		alterStmts = append(alterStmts, stmt)
		addedCols[colS.Name] = colS
	}
	return alterStmts, addedCols, ""
}

func (m *Merger) tableModifiedBetween(ctx context.Context, dbLeft, dbRight *sql.DB, engine domain.Engine, tmLeft, tmRight tableMeta) (bool, error) {
	if len(tmLeft.Columns) != len(tmRight.Columns) {
		return true, nil
	}
	for i := range tmLeft.Columns {
		if tmLeft.Columns[i].Name != tmRight.Columns[i].Name ||
			!strings.EqualFold(tmLeft.Columns[i].ColumnType, tmRight.Columns[i].ColumnType) {
			return true, nil
		}
	}
	if len(tmLeft.PKColumns) == 0 {
		return m.pklessTableChanged(ctx, dbLeft, dbRight, engine, tmLeft, tmRight)
	}
	curL, closeL, err := m.openTableCursor(ctx, dbLeft, engine, tmLeft, tmLeft.DataColumns)
	if err != nil {
		return false, err
	}
	defer closeL()
	curR, closeR, err := m.openTableCursor(ctx, dbRight, engine, tmRight, tmRight.DataColumns)
	if err != nil {
		return false, err
	}
	defer closeR()
	for {
		rL, okL, err := curL.Next()
		if err != nil {
			return false, err
		}
		rR, okR, err := curR.Next()
		if err != nil {
			return false, err
		}
		if okL != okR {
			return true, nil
		}
		if !okL {
			return false, nil
		}
		for i := range rL.Values {
			if !rL.Values[i].Equal(rR.Values[i]) {
				return true, nil
			}
		}
	}
}

func (m *Merger) tableHasAnyRows(ctx context.Context, db *sql.DB, engine domain.Engine, tm tableMeta) (bool, error) {
	cur, closeFn, err := m.openTableCursor(ctx, db, engine, tm, tm.DataColumns)
	if err != nil {
		return false, err
	}
	defer closeFn()
	_, ok, err := cur.Next()
	return ok, err
}

func (m *Merger) pklessTableChanged(ctx context.Context, dbA, dbS *sql.DB, engine domain.Engine, tmA, tmS tableMeta) (bool, error) {
	rowsA, err := m.readAllRawRows(ctx, dbA, engine, tmA)
	if err != nil {
		return false, err
	}
	rowsS, err := m.readAllRawRows(ctx, dbS, engine, tmS)
	if err != nil {
		return false, err
	}
	if len(rowsA) != len(rowsS) {
		return true, nil
	}
	for i := range rowsA {
		if rowsA[i] != rowsS[i] {
			return true, nil
		}
	}
	return false, nil
}

func (m *Merger) readAllRawRows(ctx context.Context, db *sql.DB, engine domain.Engine, tm tableMeta) ([]string, error) {
	cur, closeFn, err := m.openTableCursor(ctx, db, engine, tm, tm.DataColumns)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	var out []string
	for {
		r, ok, err := cur.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		var b strings.Builder
		for i, v := range r.Values {
			if i > 0 {
				b.WriteByte('|')
			}
			if v.Null {
				b.WriteString("<NULL>")
			} else {
				fmt.Fprintf(&b, "%d:%s", len(v.Text), v.Text)
			}
		}
		out = append(out, b.String())
	}
	sort.Strings(out)
	return out, nil
}

func (m *Merger) loadTableRowsForCheck(ctx context.Context, db *sql.DB, engine domain.Engine, tm tableMeta) ([]mergedRowInfo, error) {
	if len(tm.PKColumns) == 0 || len(tm.DataColumns) == 0 {
		return nil, nil
	}
	cur, closeFn, err := m.openTableCursor(ctx, db, engine, tm, tm.DataColumns)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	var out []mergedRowInfo
	for {
		r, ok, err := cur.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		all := make(map[string]domain.RowValue, len(tm.DataColumns))
		for i, c := range tm.DataColumns {
			all[c] = r.Values[i]
		}
		kDisp := make(map[string]any, len(tm.PKColumns))
		kRaw := make(map[string]domain.RowValue, len(tm.PKColumns))
		for _, pk := range tm.PKColumns {
			v := all[pk.Name]
			kDisp[pk.Name] = v.DisplayValue()
			kRaw[pk.Name] = v
		}
		out = append(out, mergedRowInfo{Key: kDisp, KeyValues: kRaw, AllValues: all})
	}
	return out, nil
}

func (m *Merger) openTableCursor(ctx context.Context, db *sql.DB, engine domain.Engine, tm tableMeta, targetCols []string) (domain.RowCursor, func(), error) {
	return m.openTableCursorWithAdded(ctx, db, engine, tm, targetCols, nil)
}

func (m *Merger) openTableCursorWithAdded(ctx context.Context, db *sql.DB, engine domain.Engine, tm tableMeta, targetCols []string, addedCols map[string]columnMeta) (domain.RowCursor, func(), error) {
	qDB, qTbl := quoteIdent(engine, tm.Schema), quoteIdent(engine, tm.Name)
	var selectCols []string
	var selectKinds []domain.PKColumnKind
	for _, c := range targetCols {
		if _, isAdded := addedCols[c]; isAdded {
			continue
		}
		selectCols = append(selectCols, quoteIdent(engine, c))
		selectKinds = append(selectKinds, tm.ColByName[c].Kind)
	}
	if len(selectCols) == 0 {
		selectCols = []string{"1"}
	}
	var orderParts []string
	for _, pk := range tm.PKColumns {
		qpk := quoteIdent(engine, pk.Name)
		if pk.Kind == domain.PKText {
			if engine == domain.MySQL {
				orderParts = append(orderParts, fmt.Sprintf("CAST(%s AS BINARY) ASC", qpk))
			} else {
				orderParts = append(orderParts, fmt.Sprintf(`%s COLLATE "C" ASC`, qpk))
			}
		} else {
			orderParts = append(orderParts, qpk+" ASC")
		}
	}
	orderClause := ""
	if len(orderParts) > 0 {
		orderClause = " ORDER BY " + strings.Join(orderParts, ", ")
	}
	q := fmt.Sprintf("/* db-branch:scan %s.%s */ SELECT %s FROM %s.%s%s",
		tm.Database, tm.Name, strings.Join(selectCols, ", "), qDB, qTbl, orderClause)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, func() {}, fmt.Errorf("select %s.%s: %w", tm.Database, tm.Name, err)
	}
	cur := &sqlRowCursor{rows: rows, targetCols: targetCols, addedCols: addedCols, kinds: selectKinds}
	return cur, func() { _ = rows.Close() }, nil
}

type sqlRowCursor struct {
	rows       *sql.Rows
	targetCols []string
	addedCols  map[string]columnMeta
	kinds      []domain.PKColumnKind
}

func (c *sqlRowCursor) Next() (domain.TableRow, bool, error) {
	if !c.rows.Next() {
		return domain.TableRow{}, false, c.rows.Err()
	}
	raw := make([]any, max(len(c.kinds), 1))
	ptrs := make([]any, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := c.rows.Scan(ptrs...); err != nil {
		return domain.TableRow{}, false, err
	}
	vals := make([]domain.RowValue, len(c.targetCols))
	rawIdx := 0
	for i, colName := range c.targetCols {
		if added, isAdded := c.addedCols[colName]; isAdded {
			vals[i] = defaultRowValueForAddedColumn(added)
			continue
		}
		vals[i] = convertScannedValue(raw[rawIdx], c.kinds[rawIdx])
		rawIdx++
	}
	return domain.TableRow{Values: vals}, true, nil
}

func defaultRowValueForAddedColumn(col columnMeta) domain.RowValue {
	if !col.HasDefault {
		return domain.NullValue()
	}
	d := strings.TrimSpace(col.Default)
	if idx := strings.Index(d, "::"); idx > 0 {
		d = strings.TrimSpace(d[:idx])
	}
	if len(d) >= 2 && d[0] == '\'' && d[len(d)-1] == '\'' {
		return domain.TextValue(strings.ReplaceAll(d[1:len(d)-1], "''", "'"))
	}
	if strings.EqualFold(d, "NULL") {
		return domain.NullValue()
	}
	return convertScannedValue(d, col.Kind)
}

func convertScannedValue(v any, kind domain.PKColumnKind) domain.RowValue {
	if v == nil {
		return domain.NullValue()
	}
	switch val := v.(type) {
	case int64:
		return domain.IntValue(val)
	case int:
		return domain.IntValue(int64(val))
	case int32:
		return domain.IntValue(int64(val))
	case uint64:
		return domain.RowValue{Text: strconv.FormatUint(val, 10), Display: val}
	case float64:
		if kind == domain.PKDecimal {
			return domain.DecimalValue(strconv.FormatFloat(val, 'f', -1, 64))
		}
		return domain.FloatValue(val)
	case bool:
		return domain.RowValue{Text: strconv.FormatBool(val), Display: val}
	case time.Time:
		return domain.TextValue(val.UTC().Format(time.RFC3339Nano))
	case []byte:
		return parseStringByKind(string(val), kind)
	case string:
		return parseStringByKind(val, kind)
	default:
		return parseStringByKind(fmt.Sprint(val), kind)
	}
}

func parseStringByKind(s string, kind domain.PKColumnKind) domain.RowValue {
	switch kind {
	case domain.PKInteger:
		if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
			return domain.IntValue(iv)
		}
		return domain.TextValue(s)
	case domain.PKDecimal:
		if strings.EqualFold(s, "NaN") {
			return domain.RowValue{Text: "NaN", Display: "NaN"}
		}
		return domain.DecimalValue(s)
	case domain.PKFloat:
		if strings.EqualFold(s, "NaN") {
			return domain.FloatValue(math.NaN())
		}
		if fv, err := strconv.ParseFloat(s, 64); err == nil {
			return domain.FloatValue(fv)
		}
		return domain.TextValue(s)
	default:
		return domain.TextValue(s)
	}
}

type trackingCursor struct {
	inner domain.RowCursor
	spec  domain.TableMergeSpec
	rows  []mergedRowInfo
}

func (c *trackingCursor) Next() (domain.TableRow, bool, error) {
	r, ok, err := c.inner.Next()
	if !ok || err != nil {
		return r, ok, err
	}
	all := make(map[string]domain.RowValue, len(c.spec.Columns))
	for i, col := range c.spec.Columns {
		all[col] = r.Values[i]
	}
	kDisp := make(map[string]any, len(c.spec.PKColumns))
	kRaw := make(map[string]domain.RowValue, len(c.spec.PKColumns))
	for _, pk := range c.spec.PKColumns {
		v := all[pk.Name]
		kDisp[pk.Name] = v.DisplayValue()
		kRaw[pk.Name] = v
	}
	c.rows = append(c.rows, mergedRowInfo{Key: kDisp, KeyValues: kRaw, AllValues: all})
	return r, true, nil
}

func pkTupleSignature(vals map[string]domain.RowValue, pks []domain.PKColumnSpec) string {
	cols := make([]string, len(pks))
	for i, pk := range pks {
		cols[i] = pk.Name
	}
	return rowTupleSignature(vals, cols)
}

func buildMergedTableRows(targetRows []mergedRowInfo, actions []domain.MergeAction, pks []domain.PKColumnSpec) []mergedRowInfo {
	deleted := map[string]bool{}
	updated := map[string]domain.MergeAction{}
	var inserted []domain.MergeAction
	for _, act := range actions {
		sig := pkTupleSignature(act.KeyValues, pks)
		switch act.Kind {
		case domain.ActionDelete:
			deleted[sig] = true
		case domain.ActionUpdate:
			updated[sig] = act
		case domain.ActionInsert:
			inserted = append(inserted, act)
		}
	}
	var out []mergedRowInfo
	for _, tr := range targetRows {
		sig := pkTupleSignature(tr.KeyValues, pks)
		if deleted[sig] {
			continue
		}
		if upd, ok := updated[sig]; ok {
			out = append(out, mergedRowInfo{Key: upd.Key, KeyValues: upd.KeyValues, AllValues: upd.AllValues, FromSource: true})
			continue
		}
		out = append(out, tr)
	}
	for _, ins := range inserted {
		out = append(out, mergedRowInfo{Key: ins.Key, KeyValues: ins.KeyValues, AllValues: ins.AllValues, FromSource: true})
	}
	return out
}

func checkUniqueConstraint(ref domain.TableRef, u uniqueMeta, rows []mergedRowInfo, pks []domain.PKColumnSpec) []domain.MergeConflict {
	seen := map[string]mergedRowInfo{}
	var conflicts []domain.MergeConflict
	for _, passSource := range []bool{false, true} {
		for _, r := range rows {
			if r.FromSource != passSource {
				continue
			}
			var b strings.Builder
			skipNull := false
			for i, col := range u.Columns {
				if i > 0 {
					b.WriteByte('|')
				}
				v := r.AllValues[col]
				if v.Null {
					if !u.NullsNotDistinct {
						skipNull = true
						break
					}
					b.WriteString("<NULL>")
				} else {
					fmt.Fprintf(&b, "%d:%s", len(v.Text), v.Text)
				}
			}
			if skipNull {
				continue
			}
			uSig := b.String()
			if prev, exists := seen[uSig]; exists {
				if pkTupleSignature(prev.KeyValues, pks) != pkTupleSignature(r.KeyValues, pks) && (r.FromSource || prev.FromSource) {
					sRow, tRow := r, prev
					if !r.FromSource {
						sRow, tRow = prev, r
					}
					sVals := make(map[string]any, len(u.Columns))
					tVals := make(map[string]any, len(u.Columns))
					for _, col := range u.Columns {
						sVals[col] = sRow.AllValues[col].DisplayValue()
						tVals[col] = tRow.AllValues[col].DisplayValue()
					}
					conflicts = append(conflicts, domain.MergeConflict{
						Type: domain.ConflictUniqueViolation, Database: ref.Database, Table: ref.Table,
						Constraint: u.Name, Key: sRow.Key, ConflictingTargetKey: tRow.Key,
						Columns: u.Columns, SourceValues: sVals, TargetValues: tVals,
					})
				}
				continue
			}
			seen[uSig] = r
		}
	}
	return conflicts
}

func placeholder(engine domain.Engine, idx int) string {
	if engine == domain.Postgres {
		return fmt.Sprintf("$%d", idx)
	}
	return "?"
}

func bindValue(v domain.RowValue) any {
	if v.Null {
		return nil
	}
	if v.Display != nil {
		return v.Display
	}
	return v.Text
}

func buildInsertStmt(engine domain.Engine, tm tableMeta, act domain.MergeAction) sqlStmt {
	qDB, qTbl := quoteIdent(engine, tm.Schema), quoteIdent(engine, tm.Name)
	qCols := make([]string, len(tm.DataColumns))
	ph := make([]string, len(tm.DataColumns))
	args := make([]any, len(tm.DataColumns))
	for i, col := range tm.DataColumns {
		qCols[i] = quoteIdent(engine, col)
		ph[i] = placeholder(engine, i+1)
		args[i] = bindValue(act.AllValues[col])
	}
	overriding := ""
	if engine == domain.Postgres && tm.HasIdentityAlways {
		overriding = " OVERRIDING SYSTEM VALUE"
	}
	q := fmt.Sprintf("INSERT INTO %s.%s (%s)%s VALUES (%s)",
		qDB, qTbl, strings.Join(qCols, ", "), overriding, strings.Join(ph, ", "))
	return sqlStmt{Query: q, Args: args}
}

func buildUpdateStmt(engine domain.Engine, tm tableMeta, act domain.MergeAction) sqlStmt {
	qDB, qTbl := quoteIdent(engine, tm.Schema), quoteIdent(engine, tm.Name)
	var setParts []string
	var args []any
	argIdx := 1
	for _, col := range tm.DataColumns {
		val, changed := act.Values[col]
		if !changed {
			continue
		}
		setParts = append(setParts, fmt.Sprintf("%s = %s", quoteIdent(engine, col), placeholder(engine, argIdx)))
		args = append(args, bindValue(val))
		argIdx++
	}
	var whereParts []string
	for _, pk := range tm.PKColumns {
		whereParts = append(whereParts, fmt.Sprintf("%s = %s", quoteIdent(engine, pk.Name), placeholder(engine, argIdx)))
		args = append(args, bindValue(act.KeyValues[pk.Name]))
		argIdx++
	}
	q := fmt.Sprintf("UPDATE %s.%s SET %s WHERE %s",
		qDB, qTbl, strings.Join(setParts, ", "), strings.Join(whereParts, " AND "))
	return sqlStmt{Query: q, Args: args}
}

func buildDeleteStmt(engine domain.Engine, tm tableMeta, act domain.MergeAction) sqlStmt {
	qDB, qTbl := quoteIdent(engine, tm.Schema), quoteIdent(engine, tm.Name)
	var whereParts []string
	var args []any
	for i, pk := range tm.PKColumns {
		whereParts = append(whereParts, fmt.Sprintf("%s = %s", quoteIdent(engine, pk.Name), placeholder(engine, i+1)))
		args = append(args, bindValue(act.KeyValues[pk.Name]))
	}
	q := fmt.Sprintf("DELETE FROM %s.%s WHERE %s",
		qDB, qTbl, strings.Join(whereParts, " AND "))
	return sqlStmt{Query: q, Args: args}
}
