package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// OwnerRole owns every user object in the databases of a base and its
// branches. A branch login is a member of it: that is enough to create, alter
// and drop objects, and the role is no superuser, so a client cannot reach the
// server's files or run programs.
const OwnerRole = "db_branch_owner"

// ownSQL hands the user objects of the current database to OwnerRole (the
// objects it owns already are left alone, so a second run does almost
// nothing). It runs as the local superuser. An object that cannot change
// hands (say, one an extension owns) is reported and skipped.
const ownSQL = `DO $$
DECLARE
  r record;
  o text := 'db_branch_owner';
  oid_o oid := (SELECT oid FROM pg_roles WHERE rolname = 'db_branch_owner');
BEGIN
  IF (SELECT datdba FROM pg_database WHERE datname = current_database()) <> oid_o THEN
    EXECUTE format('ALTER DATABASE %I OWNER TO %I', current_database(), o);
  END IF;
  FOR r IN
    SELECT n.nspname FROM pg_namespace n
    WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND n.nspowner <> oid_o
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
  LOOP
    BEGIN
      EXECUTE format('ALTER SCHEMA %I OWNER TO %I', r.nspname, o);
    EXCEPTION WHEN others THEN RAISE NOTICE 'schema %: %', r.nspname, SQLERRM;
    END;
  END LOOP;
  FOR r IN
    SELECT n.nspname, c.relname, c.relkind
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND c.relowner <> oid_o
      AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f', 'c')
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
      AND NOT (c.relkind = 'S' AND EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('a', 'i')))
  LOOP
    BEGIN
      EXECUTE format('ALTER %s %I.%I OWNER TO %I',
        CASE r.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'S' THEN 'SEQUENCE'
                       WHEN 'f' THEN 'FOREIGN TABLE' WHEN 'c' THEN 'TYPE' ELSE 'TABLE' END,
        r.nspname, r.relname, o);
    EXCEPTION WHEN others THEN RAISE NOTICE '%.%: %', r.nspname, r.relname, SQLERRM;
    END;
  END LOOP;
  FOR r IN
    SELECT p.oid::regprocedure::text AS sig
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND p.proowner <> oid_o
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
  LOOP
    BEGIN
      EXECUTE format('ALTER ROUTINE %s OWNER TO %I', r.sig, o);
    EXCEPTION WHEN others THEN RAISE NOTICE '%: %', r.sig, SQLERRM;
    END;
  END LOOP;
  FOR r IN
    SELECT t.oid::regtype::text AS typ, t.typtype
    FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
    WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND t.typowner <> oid_o
      AND t.typtype IN ('e', 'd', 'r', 'm')
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
  LOOP
    BEGIN
      EXECUTE format('ALTER %s %s OWNER TO %I', CASE r.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END, r.typ, o);
    EXCEPTION WHEN others THEN RAISE NOTICE '%: %', r.typ, SQLERRM;
    END;
  END LOOP;
END
$$;`

// ownObjects gives OwnerRole every user object in every database of the
// instance.
func (p *Server) ownObjects(ctx context.Context, s domain.Server) error {
	name, args := p.tool(s, "psql", "-h", p.sockDir(s), "-p", fmt.Sprint(s.Port), "-U", Superuser, "-d", "postgres",
		"-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate")
	out, err := p.Run.Run(ctx, name, args...)
	if err != nil {
		return err
	}
	for _, db := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		db = strings.TrimSpace(db)
		if db == "" {
			continue
		}
		name, args := p.tool(s, "psql", "-h", p.sockDir(s), "-p", fmt.Sprint(s.Port), "-U", Superuser, "-d", db,
			"-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-")
		if _, err := p.Run.RunInput(ctx, strings.NewReader(ownSQL), name, args...); err != nil {
			return fmt.Errorf("database %s: %w", db, err)
		}
	}
	return nil
}

type scriptPart struct{ DB, SQL string }

// splitScript cuts an init script at its \connect lines. Everything else
// is plain SQL for the server: psql's other commands (\! runs a program, \i
// reads a file) are refused, because the script comes from the caller who
// asked for the branch.
func splitScript(script string) ([]scriptPart, error) {
	parts := []scriptPart{{DB: "postgres"}}
	var cur strings.Builder
	for n, line := range strings.Split(script, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, `\`) {
			cur.WriteString(line)
			cur.WriteByte('\n')
			continue
		}
		f := strings.Fields(t)
		if f[0] != `\c` && f[0] != `\connect` {
			return nil, fmt.Errorf("line %d: %s is a psql command; init SQL takes plain SQL and \\connect only", n+1, f[0])
		}
		db := strings.TrimSpace(strings.TrimPrefix(t, f[0]))
		if len(db) >= 2 && (db[0] == '"' || db[0] == '\'') && db[len(db)-1] == db[0] {
			db = db[1 : len(db)-1]
		} else if strings.ContainsAny(db, " \t") {
			db = ""
		}
		if db == "" {
			return nil, fmt.Errorf(`line %d: \connect takes a database name and nothing else`, n+1)
		}
		parts[len(parts)-1].SQL = cur.String()
		cur.Reset()
		parts = append(parts, scriptPart{DB: db})
	}
	parts[len(parts)-1].SQL = cur.String()
	return parts, nil
}

// kv quotes a value of a libpq key/value connection string.
func kv(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// ExecAs runs the script as the branch login over the instance's socket
// (local connections are trusted, so no password is sent). The server
// reads the SQL itself: no psql runs the caller's text, so no psql command
// can run a program on the node.
func (p *Server) ExecAs(ctx context.Context, s domain.Server, user, _ string, script string) error {
	parts, err := splitScript(script)
	if err != nil {
		return fmt.Errorf("init sql: %w", err)
	}
	for _, part := range parts {
		if strings.TrimSpace(part.SQL) == "" {
			continue
		}
		if err := p.execIn(ctx, s, user, part); err != nil {
			return err
		}
	}
	return nil
}

func (p *Server) execIn(ctx context.Context, s domain.Server, user string, part scriptPart) error {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable connect_timeout=10",
		kv(p.sockDir(s)), s.Port, kv(user), kv(part.DB))
	open := p.Open
	if open == nil {
		open = sql.Open
	}
	db, err := open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// No arguments: the simple protocol, which takes several statements at once.
	if _, err := db.ExecContext(ctx, part.SQL); err != nil {
		return fmt.Errorf("database %s: %w", part.DB, err)
	}
	return nil
}
