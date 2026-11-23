package sqlmerge

import (
	"context"
	"database/sql"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestDigestHelpers_テーブル比較とダイジェストの境界条件(t *testing.T) {
	base := tableMeta{
		Schema:      "shop",
		Database:    "shop",
		Name:        "t",
		DataColumns: []string{"id", "name"},
		Columns: []columnMeta{
			{Name: "id", Ordinal: 1, DataType: "int", ColumnType: "int", Kind: domain.PKInteger},
			{Name: "name", Ordinal: 2, DataType: "varchar", ColumnType: "varchar(50)", Kind: domain.PKText},
		},
		ExplicitPK: true,
		PKColumns:  []domain.PKColumnSpec{{Name: "id", Kind: domain.PKInteger}},
		UniqueConstraints: []uniqueMeta{
			{Name: "uq_name", Columns: []string{"name"}},
		},
	}

	// Column count mismatch
	diffLen := base
	diffLen.Columns = base.Columns[:1]
	if tableSchemasEqual(base, diffLen) {
		t.Error("expected column length mismatch")
	}

	// Column property mismatch
	diffCol := base
	diffCol.Columns = append([]columnMeta(nil), base.Columns...)
	diffCol.Columns[1].Nullable = true
	if tableSchemasEqual(base, diffCol) {
		t.Error("expected column nullability mismatch")
	}

	// PK column mismatch
	diffPK := base
	diffPK.PKColumns = []domain.PKColumnSpec{{Name: "name", Kind: domain.PKText}}
	if tableSchemasEqual(base, diffPK) {
		t.Error("expected PK column mismatch")
	}

	// Unique constraint mismatch
	diffUQ := base
	diffUQ.UniqueConstraints = []uniqueMeta{{Name: "uq_other", Columns: []string{"name"}}}
	if tableSchemasEqual(base, diffUQ) {
		t.Error("expected unique constraint mismatch")
	}

	// Nil DB and unknown engine fall back safely
	m := New()
	if d, err := m.computeTableDigest(context.Background(), nil, domain.MySQL, base); err != nil || d.Valid {
		t.Errorf("nil db digest = %+v, err=%v", d, err)
	}
	// >1600 DataColumns in Postgres falls back safely
	wide := base
	wide.DataColumns = make([]string, 1601)
	for i := range wide.DataColumns {
		wide.DataColumns[i] = "c"
	}
	if d, err := m.computePostgresDigest(context.Background(), nil, wide); err != nil || d.Valid {
		t.Errorf("wide PG digest = %+v, err=%v", d, err)
	}

	// Unsupported engine and PK-less queryChildOrphansInTarget
	if d, err := m.computeTableDigest(context.Background(), openFakeDB(t), domain.Engine("unknown"), base); err != nil || d.Valid {
		t.Errorf("unknown engine digest = %+v, err=%v", d, err)
	}
	pkless := base
	pkless.PKColumns = nil
	if orphans, err := m.queryChildOrphansInTarget(context.Background(), nil, domain.MySQL, pkless, fkMeta{}, nil); err != nil || len(orphans) != 0 {
		t.Errorf("pkless orphans = %+v, err=%v", orphans, err)
	}

	// Canceled context propagates through digestsMatch and digestQueryErr
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := openFakeDB(t)
	if _, err := m.digestsMatch(cctx, db, db, domain.MySQL, base, base); err == nil {
		t.Error("expected canceled context error from digestsMatch")
	}
	if _, err := m.computePostgresDigest(cctx, db, base); err == nil {
		t.Error("expected canceled context error from computePostgresDigest")
	}
	if d, err := digestQueryErr(context.Background(), context.Canceled); err != nil || d.Valid {
		t.Errorf("non-canceled ctx digestQueryErr = %+v, err=%v", d, err)
	}
}

func openFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlmerge-mock", "/run/fast-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
