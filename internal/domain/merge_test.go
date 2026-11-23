package domain

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestDiffTableRows_片側のみの挿入更新削除を3wayで正しく判定できる(t *testing.T) {
	schema := TableMergeSpec{
		Database:  "shop",
		Table:     "items",
		PKColumns: []PKColumnSpec{{Name: "id", Kind: PKInteger}},
		Columns:   []string{"id", "name"},
	}
	ancestor := []TableRow{
		{Values: []RowValue{IntValue(1), TextValue("a")}},
		{Values: []RowValue{IntValue(2), TextValue("b")}},
	}
	source := []TableRow{
		{Values: []RowValue{IntValue(1), TextValue("a2")}},
		{Values: []RowValue{IntValue(3), TextValue("c")}},
	}
	target := []TableRow{
		{Values: []RowValue{IntValue(1), TextValue("a")}},
		{Values: []RowValue{IntValue(2), TextValue("b")}},
	}

	diff, err := DiffTableRows(schema, NewSliceRowCursor(ancestor), NewSliceRowCursor(source), NewSliceRowCursor(target), MaxMergeConflicts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diff.Conflicts) != 0 {
		t.Fatalf("expected 0 conflicts, got %+v", diff.Conflicts)
	}
	if len(diff.Actions) != 3 {
		t.Fatalf("expected 3 actions, got %+v", diff.Actions)
	}
	if diff.Actions[0].Kind != ActionUpdate || diff.Actions[0].Key["id"] != int64(1) || diff.Actions[0].Values["name"].Text != "a2" {
		t.Errorf("unexpected action[0]: %+v", diff.Actions[0])
	}
	if diff.Actions[1].Kind != ActionDelete || diff.Actions[1].Key["id"] != int64(2) {
		t.Errorf("unexpected action[1]: %+v", diff.Actions[1])
	}
	if diff.Actions[2].Kind != ActionInsert || diff.Actions[2].Key["id"] != int64(3) || diff.Actions[2].Values["name"].Text != "c" {
		t.Errorf("unexpected action[2]: %+v", diff.Actions[2])
	}
}

func TestDiffTableRows_全11パターンのコンフリクトと維持を正しく判定する(t *testing.T) {
	schema := TableMergeSpec{
		Database:  "shop",
		Table:     "orders",
		PKColumns: []PKColumnSpec{{Name: "id", Kind: PKInteger}},
		Columns:   []string{"id", "status", "note"},
	}
	ancestor := []TableRow{
		// id=1: unchanged in both -> no-op
		{Values: []RowValue{IntValue(1), TextValue("open"), NullValue()}},
		// id=2: Target only updated -> keep Target
		{Values: []RowValue{IntValue(2), TextValue("open"), NullValue()}},
		// id=3: Target only deleted -> keep Target
		{Values: []RowValue{IntValue(3), TextValue("open"), NullValue()}},
		// id=4: both updated to identical value -> keep Target
		{Values: []RowValue{IntValue(4), TextValue("open"), NullValue()}},
		// id=5: both deleted -> keep Target
		{Values: []RowValue{IntValue(5), TextValue("open"), NullValue()}},
		// id=6: update_update conflict (same column)
		{Values: []RowValue{IntValue(6), TextValue("pending"), TextValue("n")}},
		// id=7: update_update conflict (different columns on same row)
		{Values: []RowValue{IntValue(7), TextValue("pending"), TextValue("n")}},
		// id=8: delete_update conflict (Source deleted, Target updated)
		{Values: []RowValue{IntValue(8), TextValue("pending"), NullValue()}},
		// id=9: update_delete conflict (Source updated, Target deleted)
		{Values: []RowValue{IntValue(9), TextValue("pending"), NullValue()}},
	}
	source := []TableRow{
		{Values: []RowValue{IntValue(1), TextValue("open"), NullValue()}},
		{Values: []RowValue{IntValue(2), TextValue("open"), NullValue()}},
		{Values: []RowValue{IntValue(3), TextValue("open"), NullValue()}},
		{Values: []RowValue{IntValue(4), TextValue("done"), NullValue()}},
		// id=5 deleted
		{Values: []RowValue{IntValue(6), TextValue("shipped"), TextValue("n")}},
		{Values: []RowValue{IntValue(7), TextValue("shipped"), TextValue("n")}},
		// id=8 deleted in Source
		{Values: []RowValue{IntValue(9), TextValue("shipped"), NullValue()}},
		// id=11: both inserted identical -> keep
		{Values: []RowValue{IntValue(11), TextValue("new"), NullValue()}},
		// id=12: insert_insert conflict
		{Values: []RowValue{IntValue(12), TextValue("from-s"), NullValue()}},
	}
	target := []TableRow{
		{Values: []RowValue{IntValue(1), TextValue("open"), NullValue()}},
		{Values: []RowValue{IntValue(2), TextValue("paid"), NullValue()}},
		// id=3 deleted in Target
		{Values: []RowValue{IntValue(4), TextValue("done"), NullValue()}},
		// id=5 deleted
		{Values: []RowValue{IntValue(6), TextValue("cancelled"), TextValue("n")}},
		{Values: []RowValue{IntValue(7), TextValue("pending"), TextValue("t-note")}},
		{Values: []RowValue{IntValue(8), TextValue("paid"), NullValue()}},
		// id=9 deleted in Target
		// id=10: Target only insert -> keep
		{Values: []RowValue{IntValue(10), TextValue("t-only"), NullValue()}},
		{Values: []RowValue{IntValue(11), TextValue("new"), NullValue()}},
		{Values: []RowValue{IntValue(12), TextValue("from-t"), NullValue()}},
	}

	diff, err := DiffTableRows(schema, NewSliceRowCursor(ancestor), NewSliceRowCursor(source), NewSliceRowCursor(target), MaxMergeConflicts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(diff.Actions) != 0 {
		t.Errorf("expected 0 actions when only conflicts or target-kept rows exist, got %+v", diff.Actions)
	}
	if len(diff.Conflicts) != 5 {
		t.Fatalf("expected 5 conflicts, got %d: %+v", len(diff.Conflicts), diff.Conflicts)
	}
	wantTypes := []MergeConflictType{
		ConflictUpdateUpdate,
		ConflictUpdateUpdate,
		ConflictDeleteUpdate,
		ConflictUpdateDelete,
		ConflictInsertInsert,
	}
	for i, w := range wantTypes {
		if diff.Conflicts[i].Type != w {
			t.Errorf("conflict[%d]: got type %s, want %s (%+v)", i, diff.Conflicts[i].Type, w, diff.Conflicts[i])
		}
	}
}

func TestDiffTableRows_NULLと空文字と高精度数値とNaNを厳密に区別し50件超を切り詰める(t *testing.T) {
	// NULL == NULL, NULL != "", NULL != "0", Decimal "10.00" != "10.0", NaN == NaN
	if !NullValue().Equal(NullValue()) {
		t.Error("NULL should equal NULL")
	}
	if NullValue().Equal(TextValue("")) || NullValue().Equal(TextValue("0")) {
		t.Error("NULL should not equal empty string or zero")
	}
	if !FloatValue(math.NaN()).Equal(FloatValue(math.NaN())) {
		t.Error("NaN should equal NaN")
	}
	if DecimalValue("12345678901234567890.01").Equal(DecimalValue("12345678901234567890.02")) {
		t.Error("high-precision decimals should not round to equal")
	}

	// Test truncation at maxConflicts
	schema := TableMergeSpec{
		Database:  "shop",
		Table:     "t",
		PKColumns: []PKColumnSpec{{Name: "id", Kind: PKInteger}},
		Columns:   []string{"id", "v"},
	}
	var sRows, tRows []TableRow
	for i := 1; i <= 55; i++ {
		sRows = append(sRows, TableRow{Values: []RowValue{IntValue(int64(i)), TextValue("s")}})
		tRows = append(tRows, TableRow{Values: []RowValue{IntValue(int64(i)), TextValue("t")}})
	}
	diff, err := DiffTableRows(schema, NewSliceRowCursor(nil), NewSliceRowCursor(sRows), NewSliceRowCursor(tRows), 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff.TotalConflicts != 55 || len(diff.Conflicts) != 50 || !diff.Truncated {
		t.Errorf("expected total=55, len=50, truncated=true; got total=%d, len=%d, truncated=%v", diff.TotalConflicts, len(diff.Conflicts), diff.Truncated)
	}
}

func TestComparePK_型別の順序と複合主キーを比較できる(t *testing.T) {
	specs := []PKColumnSpec{
		{Name: "d", Kind: PKDecimal},
		{Name: "f", Kind: PKFloat},
		{Name: "s", Kind: PKText},
	}
	a := []RowValue{DecimalValue("2.5"), FloatValue(1.5), TextValue("a")}
	b := []RowValue{DecimalValue("10.0"), FloatValue(1.5), TextValue("a")}
	if ComparePK(a, b, specs) >= 0 || ComparePK(b, a, specs) <= 0 {
		t.Error("expected 2.5 < 10.0")
	}
	if ComparePK([]RowValue{DecimalValue("bad"), FloatValue(2.5), TextValue("a")}, []RowValue{DecimalValue("bad2"), FloatValue(1.5), TextValue("a")}, specs) >= 0 {
		t.Error("expected fallback decimal text comparison")
	}
	if ComparePK([]RowValue{DecimalValue("2.5"), FloatValue(2.5), TextValue("a")}, []RowValue{DecimalValue("2.5"), FloatValue(1.5), TextValue("a")}, specs) <= 0 {
		t.Error("expected 2.5 > 1.5")
	}
	if ComparePK([]RowValue{DecimalValue("2.5"), TextValue("bad-float"), TextValue("a")}, []RowValue{DecimalValue("2.5"), TextValue("z-float"), TextValue("a")}, specs) >= 0 {
		t.Error("expected fallback float text comparison")
	}
	c1 := []RowValue{DecimalValue("2.5"), FloatValue(1.5), TextValue("a")}
	c2 := []RowValue{DecimalValue("2.5"), FloatValue(math.NaN()), TextValue("a")}
	if ComparePK(c1, c2, specs) >= 0 || ComparePK(c2, c1, specs) <= 0 || ComparePK(c2, c2, specs) != 0 {
		t.Error("expected finite < NaN and NaN == NaN")
	}
	n1 := []RowValue{NullValue(), FloatValue(1), TextValue("a")}
	if ComparePK(n1, a, specs) >= 0 || ComparePK(a, n1, specs) <= 0 || ComparePK(n1, n1, specs) != 0 {
		t.Error("expected NULL < non-NULL")
	}
	intSpecs := []PKColumnSpec{{Name: "id", Kind: PKInteger}}
	big1 := []RowValue{{Text: "99999999999999999999999999999"}}
	big2 := []RowValue{{Text: "100000000000000000000000000000"}}
	if ComparePK(big1, big2, intSpecs) >= 0 || ComparePK(big2, big1, intSpecs) <= 0 {
		t.Error("expected big.Int comparison to work")
	}
	if ComparePK([]RowValue{{Text: "not-int-a"}}, []RowValue{{Text: "not-int-b"}}, intSpecs) >= 0 {
		t.Error("expected fallback string compare for non-integers")
	}
	if (RowValue{Text: "plain"}).DisplayValue() != "plain" {
		t.Error("expected fallback DisplayValue to return Text")
	}
}

type errCursor struct{ err error }

func (e errCursor) Next() (TableRow, bool, error) { return TableRow{}, false, e.err }

func TestDiffTableRows_異常系と境界値(t *testing.T) {
	badSpec := TableMergeSpec{Database: "db", Table: "t", PKColumns: []PKColumnSpec{{Name: "missing"}}, Columns: []string{"id"}}
	if _, err := DiffTableRows(badSpec, NewSliceRowCursor(nil), NewSliceRowCursor(nil), NewSliceRowCursor(nil), 0); err == nil {
		t.Error("expected error for missing PK column")
	}
	goodSpec := TableMergeSpec{Database: "db", Table: "t", PKColumns: []PKColumnSpec{{Name: "id"}}, Columns: []string{"id"}}
	boom := errors.New("cursor boom")
	if _, err := DiffTableRows(goodSpec, errCursor{boom}, NewSliceRowCursor(nil), NewSliceRowCursor(nil), 0); !errors.Is(err, boom) {
		t.Errorf("expected ancestor cursor error, got %v", err)
	}
	if _, err := DiffTableRows(goodSpec, NewSliceRowCursor(nil), errCursor{boom}, NewSliceRowCursor(nil), 0); !errors.Is(err, boom) {
		t.Errorf("expected source cursor error, got %v", err)
	}
	if _, err := DiffTableRows(goodSpec, NewSliceRowCursor(nil), NewSliceRowCursor(nil), errCursor{boom}, 0); !errors.Is(err, boom) {
		t.Errorf("expected target cursor error, got %v", err)
	}
}

func TestBranchCanMerge_ReadyとPausedのみ許可する(t *testing.T) {
	if ProtocolBranchMerge != 26 {
		t.Errorf("ProtocolBranchMerge = %d, want 26", ProtocolBranchMerge)
	}
	if PropMerge != "db-branch:merge" {
		t.Errorf("PropMerge = %q, want db-branch:merge", PropMerge)
	}
	n, _ := ParseName("b")
	for _, st := range []BranchStatus{BranchReady, BranchPaused} {
		b := Branch{Name: n, Status: st}
		if err := b.CanMerge(); err != nil {
			t.Errorf("status %s should allow merge, got %v", st, err)
		}
	}
	for _, st := range []BranchStatus{BranchProvisioning, BranchResetting, BranchRestoring, BranchUpgrading, BranchPausing, BranchResuming, BranchForking, BranchMerging, BranchDeleting, BranchFailed, BranchLost} {
		b := Branch{Name: n, Status: st}
		if err := b.CanMerge(); !errors.Is(err, ErrConflict) {
			t.Errorf("status %s should reject merge with ErrConflict, got %v", st, err)
		}
	}
}

func TestResolveLineAncestor_親子_兄弟_削除済みgoneチェックポイント_エラーを解決できる(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	// Case 1: Live line 1 branched from line 0 at ckpt-c1
	lines := []Line{
		{
			Dataset: "tank/db-branch/history/x/0",
			Props:   map[string]string{PropLine: "0"},
			Snapshots: []SnapshotInfo{
				{Name: "ckpt-c2", Created: t0.Add(2 * time.Minute), CreateTXG: 20, Props: map[string]string{PropLine: "0", PropRuntime: "mysql:8.4.13"}},
			},
		},
		{
			Dataset: "tank/db-branch/branches/x",
			Live:    true,
			Props:   map[string]string{PropLine: "1", PropParent: "0", PropFrom: "ckpt-c1"},
			Snapshots: []SnapshotInfo{
				{Name: ResetSnapshot, Created: t0, CreateTXG: 5, Props: map[string]string{PropLine: "0"}},
				{Name: "ckpt-c1", Created: t0.Add(time.Minute), CreateTXG: 10, Clones: []string{"tank/db-branch/history/x/0"}, Props: map[string]string{PropLine: "0", PropRuntime: "mysql:8.0.46"}},
			},
		},
	}
	src0 := 0
	plan, err := ResolveLineAncestor(lines, &src0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.SourceLine != 0 || plan.TargetLine != 1 || plan.AncestorLabel != "c1" || plan.AncestorSnapshot.Name != "ckpt-c1" || plan.SourceDataset != "tank/db-branch/history/x/0" {
		t.Errorf("unexpected plan: %+v", plan)
	}

	// Case 2: ckpt-c1 was deleted and renamed to gone-12345 (post-promote: resides on live dataset, Clones has history/x/0)
	linesGone := []Line{
		{
			Dataset: "tank/db-branch/history/x/0",
			Props:   map[string]string{PropLine: "0"},
		},
		{
			Dataset: "tank/db-branch/branches/x",
			Live:    true,
			Props:   map[string]string{PropLine: "1", PropParent: "0", PropFrom: "ckpt-c1"},
			Snapshots: []SnapshotInfo{
				{Name: "gone-12345", Created: t0.Add(time.Minute), CreateTXG: 10, Clones: []string{"tank/db-branch/history/x/0"}, Props: map[string]string{PropLine: "0"}},
			},
		},
	}
	planGone, err := ResolveLineAncestor(linesGone, &src0, "")
	if err != nil {
		t.Fatalf("gone-* resolution failed: %v", err)
	}
	if planGone.AncestorSnapshot.Name != "gone-12345" || planGone.AncestorLabel != "c1" {
		t.Errorf("unexpected gone plan: %+v", planGone)
	}

	// Case 3: Sibling lines (line 1 from line 0 at ckpt-c1 [txg=10], live line 2 from line 0 at ckpt-c2 [txg=20], same Created timestamp!)
	linesSib := []Line{
		{
			Dataset: "tank/db-branch/history/x/1",
			Props:   map[string]string{PropLine: "1", PropParent: "0", PropFrom: "ckpt-c1"},
		},
		{
			Dataset: "tank/db-branch/branches/x",
			Live:    true,
			Props:   map[string]string{PropLine: "2", PropParent: "0", PropFrom: "ckpt-c2"},
			Snapshots: []SnapshotInfo{
				{Name: "ckpt-c1", Created: t0, CreateTXG: 10, Props: map[string]string{PropLine: "0"}},
				{Name: "ckpt-c2", Created: t0, CreateTXG: 20, Props: map[string]string{PropLine: "0"}},
			},
		},
	}
	src1 := 1
	planSib, err := ResolveLineAncestor(linesSib, &src1, "")
	if err != nil {
		t.Fatalf("sibling resolution failed: %v", err)
	}
	if planSib.AncestorSnapshot.Name != "ckpt-c1" || planSib.AncestorLabel != "c1" {
		t.Errorf("expected older sibling snapshot ckpt-c1 (txg=10), got %+v", planSib)
	}

	// Case 4: Source line == live line -> ErrInvalidMergeSource
	live1 := 1
	if _, err := ResolveLineAncestor(lines, &live1, ""); !errors.Is(err, ErrInvalidMergeSource) {
		t.Errorf("expected ErrInvalidMergeSource for sourceLine == liveLine, got %v", err)
	}

	// Case 5: Checkpoint equals ancestor -> ErrInvalidMergeSource
	if _, err := ResolveLineAncestor(lines, nil, "c1"); !errors.Is(err, ErrInvalidMergeSource) {
		t.Errorf("expected ErrInvalidMergeSource when checkpoint is the ancestor itself, got %v", err)
	}

	// Case 6: No common ancestor (missing PropParent / PropFrom)
	linesNoAnc := []Line{
		{Dataset: "tank/db-branch/history/x/0", Props: map[string]string{PropLine: "0"}},
		{Dataset: "tank/db-branch/branches/x", Live: true, Props: map[string]string{PropLine: "1"}},
	}
	if _, err := ResolveLineAncestor(linesNoAnc, &src0, ""); !errors.Is(err, ErrNoCommonAncestor) {
		t.Errorf("expected ErrNoCommonAncestor, got %v", err)
	}

	// Case 7: Child line merged into parent live line (live=0, source=1) & promoted-away source line (dataset="")
	linesChildIntoParent := []Line{
		{
			Dataset: "tank/db-branch/branches/x",
			Live:    true,
			Props:   map[string]string{PropLine: "0"},
			Snapshots: []SnapshotInfo{
				{Name: ResetSnapshot, Created: t0, Props: map[string]string{PropLine: "0"}},
				{Name: "ckpt-c1", Created: t0.Add(time.Minute), Props: map[string]string{PropLine: "0"}},
				{Name: "ckpt-s1", Created: t0.Add(2 * time.Minute), Props: map[string]string{PropLine: "1", PropParent: "0", PropFrom: ResetSnapshot, PropRuntime: "postgres:18.6"}},
			},
		},
	}
	planCIP, err := ResolveLineAncestor(linesChildIntoParent, &src1, "")
	if err != nil {
		t.Fatalf("child into parent failed: %v", err)
	}
	if planCIP.AncestorLabel != ResetPoint || planCIP.SourceSnapshot.Name != "ckpt-s1" || planCIP.SourceRuntime != "postgres:18.6" {
		t.Errorf("unexpected planCIP: %+v", planCIP)
	}

	// Case 8: Explicit --checkpoint on another line & @reset
	planCP, err := ResolveLineAncestor(lines, nil, "c2")
	if err != nil || planCP.SourceCheckpoint != "c2" || planCP.SourceLine != 0 || planCP.AncestorLabel != "c1" {
		t.Errorf("unexpected planCP: %+v, err=%v", planCP, err)
	}
	// @reset when live line branched from c1 on line 0: @reset is older than c1, so @reset has no changes past c1 -> ErrInvalidMergeSource
	if _, err := ResolveLineAncestor(lines, nil, ResetPoint); !errors.Is(err, ErrInvalidMergeSource) {
		t.Errorf("expected ErrInvalidMergeSource for @reset older than branch point, got %v", err)
	}
	// Checkpoint on the live line itself -> ErrInvalidMergeSource
	linesWithLiveCP := append([]Line{}, lines...)
	linesWithLiveCP[1].Snapshots = append(linesWithLiveCP[1].Snapshots, SnapshotInfo{Name: "ckpt-livecp", Created: t0.Add(3 * time.Minute), Props: map[string]string{PropLine: "1", PropParent: "0", PropFrom: "ckpt-c1"}})
	if _, err := ResolveLineAncestor(linesWithLiveCP, nil, "livecp"); !errors.Is(err, ErrInvalidMergeSource) {
		t.Errorf("expected ErrInvalidMergeSource for checkpoint on live line, got %v", err)
	}

	// Case 9: Pre-promote gone-* (resides on parent dataset, Clones contains child dataset) and fallback timestamp comparison
	linesPreGone := []Line{
		{
			Dataset: "tank/db-branch/history/x/0",
			Props:   map[string]string{PropLine: "0"},
			Snapshots: []SnapshotInfo{
				{Name: "gone-111", Created: t0, Clones: []string{"other"}, Props: map[string]string{PropLine: "0"}},
				{Name: "gone-222", Created: t0.Add(time.Minute), Clones: []string{"tank/db-branch/branches/x"}, Props: map[string]string{PropLine: "0"}},
			},
		},
		{
			Dataset: "tank/db-branch/branches/x",
			Live:    true,
			Props:   map[string]string{PropLine: "1", PropParent: "0", PropFrom: "ckpt-c1"},
		},
	}
	planPreGone, err := ResolveLineAncestor(linesPreGone, &src0, "")
	if err != nil || planPreGone.AncestorSnapshot.Name != "gone-222" {
		t.Errorf("unexpected planPreGone: %+v, err=%v", planPreGone, err)
	}

	// Case 10: Error branches (no live dataset, missing source line, missing checkpoint, neither sourceLine nor checkpoint)
	if _, err := ResolveLineAncestor(nil, &src0, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound when no live dataset, got %v", err)
	}
	unknownLine := 99
	if _, err := ResolveLineAncestor(lines, &unknownLine, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown line, got %v", err)
	}
	if _, err := ResolveLineAncestor(lines, nil, "no-such-cp"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown checkpoint, got %v", err)
	}
	if _, err := ResolveLineAncestor(lines, nil, ""); !errors.Is(err, ErrInvalidMergeSource) {
		t.Errorf("expected ErrInvalidMergeSource when neither sourceLine nor checkpoint specified, got %v", err)
	}
	if !IsMergeSnapshot("merge-live-123") || IsMergeSnapshot("ckpt-a") {
		t.Error("unexpected IsMergeSnapshot result")
	}
	if ds, ok := FindSnapshot(lines, ResetSnapshot); !ok || ds == "" {
		t.Error("expected FindSnapshot to find reset")
	}
	if _, ok := FindSnapshot(lines, "missing-snap"); ok {
		t.Error("expected FindSnapshot to return false for missing snapshot")
	}
}

func TestTopoSortTables_FK依存順序と循環参照とテーブルフィルタ検証ができる(t *testing.T) {
	users := TableRef{Database: "shop", Table: "users"}
	orders := TableRef{Database: "shop", Table: "orders"}
	items := TableRef{Database: "shop", Table: "order_items"}
	tables := []TableRef{items, orders, users}
	fks := []ForeignKey{
		{Name: "fk_orders_user", Child: orders, Parent: users},
		{Name: "fk_items_order", Child: items, Parent: orders},
		{Name: "fk_users_self", Child: users, Parent: users}, // self-reference should not be a cycle
	}
	ins, del, cycle := TopoSortTables(tables, fks)
	if cycle {
		t.Error("expected no cycle for DAG with self-reference")
	}
	if fmt.Sprint(ins) != fmt.Sprint([]TableRef{users, orders, items}) {
		t.Errorf("unexpected insertOrder: %+v", ins)
	}
	if fmt.Sprint(del) != fmt.Sprint([]TableRef{items, orders, users}) {
		t.Errorf("unexpected deleteOrder: %+v", del)
	}

	// Cycle detection
	a := TableRef{Database: "shop", Table: "a"}
	b := TableRef{Database: "shop", Table: "b"}
	_, _, cycle = TopoSortTables([]TableRef{a, b}, []ForeignKey{
		{Name: "fk_ab", Child: a, Parent: b},
		{Name: "fk_ba", Child: b, Parent: a},
	})
	if !cycle {
		t.Error("expected cycle=true for mutual FK")
	}

	// Table filter validation
	if _, err := ValidateTableFilter(nil, tables, fks); err != nil {
		t.Errorf("expected empty filter to return all tables, got %v", err)
	}
	if _, err := ValidateTableFilter([]string{"nonexistent"}, tables, fks); !errors.Is(err, ErrInvalidMergeSource) {
		t.Errorf("expected error for nonexistent table, got %v", err)
	}
	if _, err := ValidateTableFilter([]string{"orders"}, tables, fks); err == nil {
		t.Error("expected error when FK-related parent/child tables are omitted from --tables")
	}
	if _, err := ValidateTableFilter([]string{"users"}, tables, fks); err == nil {
		t.Error("expected error when FK child table is omitted from --tables")
	}
	filtered, err := ValidateTableFilter([]string{"shop.users", "orders", "order_items"}, tables, fks)
	if err != nil || len(filtered) != 3 {
		t.Errorf("expected valid filter for complete FK component, got %v, err=%v", filtered, err)
	}
	if (TableRef{Table: "bare"}).String() != "bare" {
		t.Error("expected bare table name when Database is empty")
	}
}
