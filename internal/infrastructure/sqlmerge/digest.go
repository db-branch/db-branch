package sqlmerge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
)

type tableDigest struct {
	Valid bool
	Value string
}

func schemasIdentical(a, b, c tableMeta) bool {
	return tableSchemasEqual(a, b) && tableSchemasEqual(a, c)
}

func tableSchemasEqual(a, b tableMeta) bool {
	if len(a.Columns) != len(b.Columns) || a.ExplicitPK != b.ExplicitPK ||
		len(a.PKColumns) != len(b.PKColumns) || len(a.UniqueConstraints) != len(b.UniqueConstraints) {
		return false
	}
	for i := range a.Columns {
		ca, cb := a.Columns[i], b.Columns[i]
		if ca.Name != cb.Name || ca.Ordinal != cb.Ordinal ||
			!strings.EqualFold(ca.DataType, cb.DataType) ||
			!strings.EqualFold(strings.TrimSpace(ca.ColumnType), strings.TrimSpace(cb.ColumnType)) ||
			ca.Nullable != cb.Nullable || ca.HasDefault != cb.HasDefault || ca.Default != cb.Default ||
			ca.Extra != cb.Extra || ca.IsGenerated != cb.IsGenerated || ca.IsStoredGen != cb.IsStoredGen ||
			ca.GenerationExpr != cb.GenerationExpr || ca.IsAutoInc != cb.IsAutoInc ||
			ca.IsIdentity != cb.IsIdentity || ca.IdentityAlways != cb.IdentityAlways || ca.Kind != cb.Kind {
			return false
		}
	}
	for i := range a.PKColumns {
		if a.PKColumns[i] != b.PKColumns[i] {
			return false
		}
	}
	for i := range a.UniqueConstraints {
		ua, ub := a.UniqueConstraints[i], b.UniqueConstraints[i]
		if ua.Name != ub.Name || ua.NullsNotDistinct != ub.NullsNotDistinct || !slices.Equal(ua.Columns, ub.Columns) {
			return false
		}
	}
	return true
}

func (m *Merger) digestsMatch(ctx context.Context, dbLeft, dbRight *sql.DB, engine domain.Engine, tmLeft, tmRight tableMeta) (bool, error) {
	var (
		dL, dR tableDigest
		errL   error
		errR   error
		wg     sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		dL, errL = m.computeTableDigest(ctx, dbLeft, engine, tmLeft)
	}()
	go func() {
		defer wg.Done()
		dR, errR = m.computeTableDigest(ctx, dbRight, engine, tmRight)
	}()
	wg.Wait()
	if errL != nil {
		return false, errL
	}
	if errR != nil {
		return false, errR
	}
	if !dL.Valid || !dR.Valid {
		return false, nil
	}
	return dL.Value == dR.Value, nil
}

func (m *Merger) computeTableDigest(ctx context.Context, db *sql.DB, engine domain.Engine, tm tableMeta) (tableDigest, error) {
	if db == nil {
		return tableDigest{}, nil
	}
	switch engine {
	case domain.MySQL:
		return m.computeMySQLDigest(ctx, db, tm)
	case domain.Postgres:
		return m.computePostgresDigest(ctx, db, tm)
	default:
		return tableDigest{}, nil
	}
}

func digestQueryErr(ctx context.Context, err error) (tableDigest, error) {
	if ctx.Err() != nil {
		return tableDigest{}, ctx.Err()
	}
	_ = err
	return tableDigest{}, nil
}

func (m *Merger) computeMySQLDigest(ctx context.Context, db *sql.DB, tm tableMeta) (tableDigest, error) {
	qDB, qTbl := quoteIdent(domain.MySQL, tm.Schema), quoteIdent(domain.MySQL, tm.Name)
	q := fmt.Sprintf("/* db-branch:checksum-table %s.%s */ CHECKSUM TABLE %s.%s",
		sanitizeCommentLabel(tm.Database), sanitizeCommentLabel(tm.Name), qDB, qTbl)
	var tblName string
	var sum sql.NullString
	if err := db.QueryRowContext(ctx, q).Scan(&tblName, &sum); err != nil {
		return digestQueryErr(ctx, err)
	}
	if !sum.Valid {
		return tableDigest{}, nil
	}
	if sum.String == "0" {
		// Distinguish an empty table from a rare CRC=0 non-empty table.
		hasRowsQ := fmt.Sprintf("/* db-branch:has-rows %s.%s */ SELECT 1 FROM %s.%s LIMIT 1",
			sanitizeCommentLabel(tm.Database), sanitizeCommentLabel(tm.Name), qDB, qTbl)
		var one int
		err := db.QueryRowContext(ctx, hasRowsQ).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return tableDigest{Valid: true, Value: "0:empty"}, nil
		}
		if err != nil {
			return digestQueryErr(ctx, err)
		}
		return tableDigest{Valid: true, Value: "0:nonempty"}, nil
	}
	return tableDigest{Valid: true, Value: sum.String}, nil
}

func (m *Merger) computePostgresDigest(ctx context.Context, db *sql.DB, tm tableMeta) (tableDigest, error) {
	qSchema, qTbl := quoteIdent(domain.Postgres, tm.Schema), quoteIdent(domain.Postgres, tm.Name)
	label := sanitizeCommentLabel(tm.Database) + "." + sanitizeCommentLabel(tm.Name)
	if len(tm.DataColumns) == 0 {
		q := fmt.Sprintf("/* db-branch:table-digest %s */ SELECT COUNT(*) FROM %s.%s", label, qSchema, qTbl)
		var cnt int64
		if err := db.QueryRowContext(ctx, q).Scan(&cnt); err != nil {
			return digestQueryErr(ctx, err)
		}
		return tableDigest{Valid: true, Value: fmt.Sprintf("emptycols:%d", cnt)}, nil
	}
	// ROW(...) supports up to 1664 arguments in PostgreSQL; fall back to full scan if exceeded.
	if len(tm.DataColumns) > 1600 {
		return tableDigest{}, nil
	}
	qCols := make([]string, len(tm.DataColumns))
	for i, c := range tm.DataColumns {
		qCols[i] = quoteIdent(domain.Postgres, c)
	}
	q := fmt.Sprintf(
		"/* db-branch:table-digest %s */ "+
			"SELECT COUNT(*), "+
			"COALESCE(SUM(h1), 0)::text, "+
			"COALESCE(SUM(h2), 0)::text, "+
			"COALESCE(BIT_XOR(h3), 0), "+
			"COALESCE(BIT_XOR(h4), 0) "+
			"FROM ("+
			"SELECT ('x' || encode(substring(h FROM 1 FOR 8), 'hex'))::bit(64)::bigint AS h1, "+
			"('x' || encode(substring(h FROM 9 FOR 8), 'hex'))::bit(64)::bigint AS h2, "+
			"('x' || encode(substring(h FROM 17 FOR 8), 'hex'))::bit(64)::bigint AS h3, "+
			"('x' || encode(substring(h FROM 25 FOR 8), 'hex'))::bit(64)::bigint AS h4 "+
			"FROM (SELECT sha256(textsend(ROW(%s)::text)) AS h FROM %s.%s) raw_h"+
			") parts",
		label, strings.Join(qCols, ", "), qSchema, qTbl,
	)
	var cnt, x3, x4 int64
	var s1, s2 string
	if err := db.QueryRowContext(ctx, q).Scan(&cnt, &s1, &s2, &x3, &x4); err != nil {
		return digestQueryErr(ctx, err)
	}
	return tableDigest{Valid: true, Value: fmt.Sprintf("%d|%s|%s|%d|%d", cnt, s1, s2, x3, x4)}, nil
}

const fkCheckBatchSize = 200

func buildTuplePredicates(engine domain.Engine, cols []string, tuples [][]domain.RowValue) (string, []any) {
	qCols := make([]string, len(cols))
	for i, c := range cols {
		qCols[i] = quoteIdent(engine, c)
	}
	var orParts []string
	var args []any
	argIdx := 1
	for _, tuple := range tuples {
		andParts := make([]string, len(cols))
		for i, v := range tuple {
			andParts[i] = fmt.Sprintf("%s = %s", qCols[i], placeholder(engine, argIdx))
			args = append(args, bindValue(v))
			argIdx++
		}
		orParts = append(orParts, "("+strings.Join(andParts, " AND ")+")")
	}
	return strings.Join(orParts, " OR "), args
}

func (m *Merger) lookupParentTuplesInTarget(
	ctx context.Context, dbT *sql.DB, engine domain.Engine, tmP tableMeta, refCols []string, tuples [][]domain.RowValue,
) (map[string]bool, error) {
	found := make(map[string]bool, len(tuples))
	qDB, qTbl := quoteIdent(engine, tmP.Schema), quoteIdent(engine, tmP.Name)
	qCols := make([]string, len(refCols))
	kinds := make([]domain.PKColumnKind, len(refCols))
	for i, c := range refCols {
		qCols[i] = quoteIdent(engine, c)
		kinds[i] = tmP.ColByName[c].Kind
	}
	label := sanitizeCommentLabel(tmP.Database) + "." + sanitizeCommentLabel(tmP.Name)
	for start := 0; start < len(tuples); start += fkCheckBatchSize {
		end := min(start+fkCheckBatchSize, len(tuples))
		whereClause, args := buildTuplePredicates(engine, refCols, tuples[start:end])
		q := fmt.Sprintf("/* db-branch:fk-lookup %s */ SELECT %s FROM %s.%s WHERE %s",
			label, strings.Join(qCols, ", "), qDB, qTbl, whereClause)
		rows, err := dbT.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("lookup foreign key target %s.%s: %w", tmP.Database, tmP.Name, err)
		}
		for rows.Next() {
			raw := make([]any, len(refCols))
			ptrs := make([]any, len(refCols))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				_ = rows.Close()
				return nil, err
			}
			vals := make(map[string]domain.RowValue, len(refCols))
			for i, col := range refCols {
				vals[col] = convertScannedValue(raw[i], kinds[i])
			}
			found[rowTupleSignature(vals, refCols)] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return found, nil
}

func (m *Merger) queryChildOrphansInTarget(
	ctx context.Context, dbT *sql.DB, engine domain.Engine, tmC tableMeta, fk fkMeta, removedTuples [][]domain.RowValue,
) ([]mergedRowInfo, error) {
	if len(tmC.PKColumns) == 0 {
		return nil, nil
	}
	qDB, qTbl := quoteIdent(engine, tmC.Schema), quoteIdent(engine, tmC.Name)
	var selectCols []string
	var selectKinds []domain.PKColumnKind
	for _, pk := range tmC.PKColumns {
		selectCols = append(selectCols, quoteIdent(engine, pk.Name))
		selectKinds = append(selectKinds, pk.Kind)
	}
	for _, fc := range fk.Columns {
		selectCols = append(selectCols, quoteIdent(engine, fc))
		selectKinds = append(selectKinds, tmC.ColByName[fc].Kind)
	}
	label := sanitizeCommentLabel(tmC.Database) + "." + sanitizeCommentLabel(tmC.Name)

	scanOrphans := func(whereClause string, args []any) ([]mergedRowInfo, error) {
		q := fmt.Sprintf("/* db-branch:fk-orphans %s */ SELECT %s FROM %s.%s WHERE %s LIMIT %d",
			label, strings.Join(selectCols, ", "), qDB, qTbl, whereClause, domain.MaxMergeConflicts)
		rows, err := dbT.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("check orphan foreign keys in %s.%s: %w", tmC.Database, tmC.Name, err)
		}
		defer rows.Close()
		var out []mergedRowInfo
		for rows.Next() {
			raw := make([]any, len(selectCols))
			ptrs := make([]any, len(selectCols))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			kDisp := make(map[string]any, len(tmC.PKColumns))
			kRaw := make(map[string]domain.RowValue, len(tmC.PKColumns))
			for i, pk := range tmC.PKColumns {
				v := convertScannedValue(raw[i], selectKinds[i])
				kDisp[pk.Name] = v.DisplayValue()
				kRaw[pk.Name] = v
			}
			out = append(out, mergedRowInfo{Key: kDisp, KeyValues: kRaw})
		}
		return out, rows.Err()
	}

	if removedTuples == nil {
		// Parent table was dropped: any non-null FK row in child is orphaned.
		notNullParts := make([]string, len(fk.Columns))
		for i, c := range fk.Columns {
			notNullParts[i] = quoteIdent(engine, c) + " IS NOT NULL"
		}
		return scanOrphans(strings.Join(notNullParts, " AND "), nil)
	}

	var all []mergedRowInfo
	for start := 0; start < len(removedTuples) && len(all) < domain.MaxMergeConflicts; start += fkCheckBatchSize {
		end := min(start+fkCheckBatchSize, len(removedTuples))
		whereClause, args := buildTuplePredicates(engine, fk.Columns, removedTuples[start:end])
		batch, err := scanOrphans(whereClause, args)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}
	return all, nil
}

func extractNonNullTuple(vals map[string]domain.RowValue, cols []string) ([]domain.RowValue, bool) {
	tuple := make([]domain.RowValue, len(cols))
	for i, col := range cols {
		v, ok := vals[col]
		if !ok || v.Null {
			return nil, false
		}
		tuple[i] = v
	}
	return tuple, true
}

func (m *Merger) checkSkippedChildFK(
	ctx context.Context, poolT *dbConnPool, engine domain.Engine, fk fkMeta, tmC tableMeta,
	pKey string, droppedTables map[string]bool, targetRowsByTable, mergedRowsByTable map[string][]mergedRowInfo,
	addConflict func(domain.MergeConflict),
) error {
	var removed [][]domain.RowValue
	if !droppedTables[pKey] {
		tRows := targetRowsByTable[pKey]
		if len(tRows) == 0 {
			return nil
		}
		surviving := make(map[string]bool, len(mergedRowsByTable[pKey]))
		for _, pr := range mergedRowsByTable[pKey] {
			surviving[rowTupleSignature(pr.AllValues, fk.RefColumns)] = true
		}
		seenRemoved := map[string]bool{}
		for _, tr := range tRows {
			tuple, ok := extractNonNullTuple(tr.AllValues, fk.RefColumns)
			if !ok {
				continue
			}
			sig := rowTupleSignature(tr.AllValues, fk.RefColumns)
			if !surviving[sig] && !seenRemoved[sig] {
				seenRemoved[sig] = true
				removed = append(removed, tuple)
			}
		}
		if len(removed) == 0 {
			return nil
		}
	}
	dbT, err := poolT.get(tmC.PGDatabase)
	if err != nil {
		return err
	}
	orphans, err := m.queryChildOrphansInTarget(ctx, dbT, engine, tmC, fk, removed)
	if err != nil {
		return err
	}
	for _, cr := range orphans {
		addConflict(domain.MergeConflict{
			Type: domain.ConflictForeignKey, Database: fk.Database, Table: fk.Table,
			Constraint: fk.Name, Key: cr.Key, Columns: fk.Columns,
			Message: fmt.Sprintf("foreign key %s references non-existent row in %s.%s", fk.Name, fk.RefDatabase, fk.RefTable),
		})
	}
	return nil
}

func (m *Merger) checkSkippedParentFK(
	ctx context.Context, poolT *dbConnPool, engine domain.Engine, fk fkMeta, tmP tableMeta,
	cKey, pKey string, skippedTables map[string]tableMeta, mergedRowsByTable map[string][]mergedRowInfo,
	addConflict func(domain.MergeConflict),
) (fallback bool, err error) {
	type fkRefGroup struct {
		tuple []domain.RowValue
		rows  []mergedRowInfo
	}
	groups := map[string]*fkRefGroup{}
	var order []string
	for _, cr := range mergedRowsByTable[cKey] {
		tuple, ok := extractNonNullTuple(cr.AllValues, fk.Columns)
		if !ok {
			continue
		}
		sig := rowTupleSignature(cr.AllValues, fk.Columns)
		g, exists := groups[sig]
		if !exists {
			g = &fkRefGroup{tuple: tuple}
			groups[sig] = g
			order = append(order, sig)
		}
		g.rows = append(g.rows, cr)
	}
	if len(order) == 0 {
		return false, nil
	}
	dbT, err := poolT.get(tmP.PGDatabase)
	if err != nil {
		return false, err
	}
	if len(order) > 5000 {
		rows, err := m.loadTableRowsForCheck(ctx, dbT, engine, tmP)
		if err != nil {
			return false, err
		}
		mergedRowsByTable[pKey] = rows
		delete(skippedTables, pKey)
		return true, nil
	}
	tuples := make([][]domain.RowValue, len(order))
	for i, sig := range order {
		tuples[i] = groups[sig].tuple
	}
	found, err := m.lookupParentTuplesInTarget(ctx, dbT, engine, tmP, fk.RefColumns, tuples)
	if err != nil {
		return false, err
	}
	for _, sig := range order {
		if found[sig] {
			continue
		}
		for _, cr := range groups[sig].rows {
			addConflict(domain.MergeConflict{
				Type: domain.ConflictForeignKey, Database: fk.Database, Table: fk.Table,
				Constraint: fk.Name, Key: cr.Key, Columns: fk.Columns,
				Message: fmt.Sprintf("foreign key %s references non-existent row in %s.%s", fk.Name, fk.RefDatabase, fk.RefTable),
			})
		}
	}
	return false, nil
}
