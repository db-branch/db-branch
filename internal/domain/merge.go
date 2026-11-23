package domain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
)

var (
	// ErrNoCommonAncestor is returned when two history lines have no known or surviving common ancestor snapshot.
	ErrNoCommonAncestor = errors.New("no common ancestor")
	// ErrInvalidMergeSource is returned when merging a line into itself or when the source state is identical to the common ancestor.
	ErrInvalidMergeSource = errors.New("invalid merge source")
	// ErrSchemaNoPrimaryKey is returned when a table with changes has neither a primary key nor a NOT NULL unique constraint.
	ErrSchemaNoPrimaryKey = errors.New("table has no primary key")
	// ErrTriggersNotSupported is returned when a MySQL table with changes has triggers and --ignore-triggers was not set.
	ErrTriggersNotSupported = errors.New("triggers not supported")
	// ErrMergeConflict is returned when data, schema, unique or foreign-key conflicts are detected.
	ErrMergeConflict = errors.New("merge conflict")
)

// MaxMergeConflicts is the maximum number of conflicts collected in MergeResult.Conflicts.
const MaxMergeConflicts = 50

// MergeStatus is how a branch merge operation ended.
type MergeStatus string

const (
	MergeMerged     MergeStatus = "merged"
	MergePreview    MergeStatus = "dry_run"
	MergeConflicted MergeStatus = "conflicted"
	MergeRolledBack MergeStatus = "rolled_back"
	MergeFailed     MergeStatus = "failed"
)

// MergeResult is the result of a branch.merge operation (stored in Operation.Result).
type MergeResult struct {
	Branch           string              `json:"branch"`
	SourceLine       int                 `json:"source_line"`
	SourceCheckpoint string              `json:"source_checkpoint,omitempty"`
	TargetLine       int                 `json:"target_line"`
	Ancestor         string              `json:"ancestor,omitempty"`
	Checkpoint       string              `json:"checkpoint,omitempty"`
	DryRun           bool                `json:"dry_run,omitempty"`
	Status           MergeStatus         `json:"status"`
	Code             string              `json:"code,omitempty"`
	Inserted         int                 `json:"inserted"`
	Updated          int                 `json:"updated"`
	Deleted          int                 `json:"deleted"`
	DDL              int                 `json:"ddl,omitempty"`
	Tables           []MergeTableSummary `json:"tables,omitempty"`
	TotalConflicts   int                 `json:"total_conflicts,omitempty"`
	Truncated        bool                `json:"truncated,omitempty"`
	Conflicts        []MergeConflict     `json:"conflicts,omitempty"`
	Error            string              `json:"error,omitempty"`
}

// MergeTableSummary summarizes the changes for one table in MergeResult.
type MergeTableSummary struct {
	Database string `json:"database"`
	Table    string `json:"table"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Deleted  int    `json:"deleted"`
	DDL      string `json:"ddl,omitempty"`
}

// MergeDiffRequest describes a 3-way logical diff request across three temporary or live database instances.
type MergeDiffRequest struct {
	Ancestor       AdminAccess
	Source         AdminAccess
	Target         AdminAccess
	Tables         []string
	AllowDDL       bool
	IgnoreTriggers bool
}

// MergePlan holds the computed diff, conflicts, summary and opaque apply instructions for LogicalMerger.
type MergePlan struct {
	Code           string
	Message        string
	Inserted       int
	Updated        int
	Deleted        int
	DDL            int
	Tables         []MergeTableSummary
	TotalConflicts int
	Truncated      bool
	Conflicts      []MergeConflict
	// Opaque holds engine-specific apply instructions computed by Diff.
	Opaque any
}

// LogicalMerger compares three database instances (Ancestor, Source, Target) at the
// schema and row level and applies the resulting changes to Target.
type LogicalMerger interface {
	Diff(ctx context.Context, req MergeDiffRequest) (MergePlan, error)
	Apply(ctx context.Context, target AdminAccess, plan MergePlan, report func(phase string)) error
}

// MergeConflictType identifies the category of a merge conflict.
type MergeConflictType string

const (
	ConflictUpdateUpdate    MergeConflictType = "update_update"
	ConflictDeleteUpdate    MergeConflictType = "delete_update"
	ConflictUpdateDelete    MergeConflictType = "update_delete"
	ConflictInsertInsert    MergeConflictType = "insert_insert"
	ConflictUniqueViolation MergeConflictType = "unique_violation"
	ConflictForeignKey      MergeConflictType = "foreign_key_conflict"
	ConflictSchema          MergeConflictType = "schema_conflict"
	ConflictNoPrimaryKey    MergeConflictType = "schema_no_primary_key"
)

// MergeConflict describes one conflict detected during a 3-way merge.
type MergeConflict struct {
	Type                 MergeConflictType `json:"type"`
	Database             string            `json:"database"`
	Table                string            `json:"table"`
	Constraint           string            `json:"constraint,omitempty"`
	Key                  map[string]any    `json:"key,omitempty"`
	ConflictingTargetKey map[string]any    `json:"conflicting_target_key,omitempty"`
	Columns              []string          `json:"columns,omitempty"`
	AncestorValues       map[string]any    `json:"ancestor_values,omitempty"`
	SourceValues         map[string]any    `json:"source_values,omitempty"`
	TargetValues         map[string]any    `json:"target_values,omitempty"`
	Message              string            `json:"message,omitempty"`
}

// MergeActionKind is the DML operation to apply to the target table.
type MergeActionKind string

const (
	ActionInsert MergeActionKind = "insert"
	ActionUpdate MergeActionKind = "update"
	ActionDelete MergeActionKind = "delete"
)

// RowValue is a single cell value in a table row, preserving NULL vs empty string
// and exact textual representation for high-precision decimals and NaN.
type RowValue struct {
	Null    bool
	Text    string
	Display any
}

// NullValue returns a NULL RowValue.
func NullValue() RowValue { return RowValue{Null: true, Display: nil} }

// IntValue returns an integer RowValue.
func IntValue(v int64) RowValue { return RowValue{Text: strconv.FormatInt(v, 10), Display: v} }

// TextValue returns a string/text RowValue.
func TextValue(s string) RowValue { return RowValue{Text: s, Display: s} }

// DecimalValue returns an exact decimal/numeric RowValue (kept as text to avoid float64 rounding).
func DecimalValue(s string) RowValue { return RowValue{Text: s, Display: s} }

// FloatValue returns a floating-point RowValue (preserving NaN == NaN).
func FloatValue(v float64) RowValue {
	if math.IsNaN(v) {
		return RowValue{Text: "NaN", Display: "NaN"}
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	return RowValue{Text: s, Display: v}
}

// Equal reports whether two RowValues represent the same value (NULL-safe, exact precision).
func (v RowValue) Equal(other RowValue) bool {
	if v.Null != other.Null {
		return false
	}
	if v.Null {
		return true
	}
	return v.Text == other.Text
}

// DisplayValue returns the JSON-serializable display representation (nil for NULL).
func (v RowValue) DisplayValue() any {
	if v.Null {
		return nil
	}
	if v.Display != nil {
		return v.Display
	}
	return v.Text
}

// PKColumnKind controls how primary key values are ordered in merge-join cursors.
type PKColumnKind int

const (
	PKText PKColumnKind = iota
	PKInteger
	PKDecimal
	PKFloat
)

// PKColumnSpec describes one primary key column and its ordering kind.
type PKColumnSpec struct {
	Name string
	Kind PKColumnKind
}

// TableMergeSpec describes a table's columns and primary key for 3-way row diffing.
type TableMergeSpec struct {
	Database  string
	Table     string
	PKColumns []PKColumnSpec
	Columns   []string
}

// TableRow holds the ordered column values for one row matching TableMergeSpec.Columns.
type TableRow struct {
	Values []RowValue
}

// RowCursor streams TableRows ordered by primary key ascending.
type RowCursor interface {
	Next() (TableRow, bool, error)
}

// SliceRowCursor is an in-memory RowCursor backed by a slice of TableRow.
type SliceRowCursor struct {
	rows []TableRow
	idx  int
}

// NewSliceRowCursor wraps rows in a SliceRowCursor.
func NewSliceRowCursor(rows []TableRow) *SliceRowCursor {
	return &SliceRowCursor{rows: rows}
}

// Next returns the next row from the slice.
func (c *SliceRowCursor) Next() (TableRow, bool, error) {
	if c.idx >= len(c.rows) {
		return TableRow{}, false, nil
	}
	r := c.rows[c.idx]
	c.idx++
	return r, true, nil
}

// MergeAction is one row-level change to apply to the target table.
type MergeAction struct {
	Kind      MergeActionKind
	Key       map[string]any
	KeyValues map[string]RowValue
	Values    map[string]RowValue
	AllValues map[string]RowValue
}

// TableDiffResult is the result of 3-way row diffing for a single table.
type TableDiffResult struct {
	Actions        []MergeAction
	Conflicts      []MergeConflict
	TotalConflicts int
	Truncated      bool
	Inserted       int
	Updated        int
	Deleted        int
}

// DiffTableRows performs an O(1)-memory streaming 3-way merge-join over ancestor,
// source and target cursors (each ordered by primary key ascending).
func DiffTableRows(spec TableMergeSpec, ancestor, source, target RowCursor, maxConflicts int) (TableDiffResult, error) {
	if maxConflicts <= 0 {
		maxConflicts = MaxMergeConflicts
	}
	colIdx := make(map[string]int, len(spec.Columns))
	for i, c := range spec.Columns {
		colIdx[c] = i
	}
	pkIdx := make([]int, len(spec.PKColumns))
	isPK := make(map[string]bool, len(spec.PKColumns))
	for i, pk := range spec.PKColumns {
		idx, ok := colIdx[pk.Name]
		if !ok {
			return TableDiffResult{}, fmt.Errorf("primary key column %q not in table %s.%s columns", pk.Name, spec.Database, spec.Table)
		}
		pkIdx[i] = idx
		isPK[pk.Name] = true
	}

	extractPK := func(r TableRow) []RowValue {
		out := make([]RowValue, len(pkIdx))
		for i, idx := range pkIdx {
			out[i] = r.Values[idx]
		}
		return out
	}
	keyMap := func(r TableRow) (map[string]any, map[string]RowValue) {
		disp := make(map[string]any, len(spec.PKColumns))
		raw := make(map[string]RowValue, len(spec.PKColumns))
		for i, pk := range spec.PKColumns {
			v := r.Values[pkIdx[i]]
			disp[pk.Name] = v.DisplayValue()
			raw[pk.Name] = v
		}
		return disp, raw
	}
	allMap := func(r TableRow) map[string]RowValue {
		m := make(map[string]RowValue, len(spec.Columns))
		for i, c := range spec.Columns {
			m[c] = r.Values[i]
		}
		return m
	}

	rowA, okA, err := ancestor.Next()
	if err != nil {
		return TableDiffResult{}, err
	}
	rowS, okS, err := source.Next()
	if err != nil {
		return TableDiffResult{}, err
	}
	rowT, okT, err := target.Next()
	if err != nil {
		return TableDiffResult{}, err
	}

	var res TableDiffResult
	addConflict := func(c MergeConflict) {
		res.TotalConflicts++
		if len(res.Conflicts) < maxConflicts {
			res.Conflicts = append(res.Conflicts, c)
		} else {
			res.Truncated = true
		}
	}

	for okA || okS || okT {
		var minPK []RowValue
		if okA {
			minPK = extractPK(rowA)
		}
		if okS {
			pkS := extractPK(rowS)
			if minPK == nil || ComparePK(pkS, minPK, spec.PKColumns) < 0 {
				minPK = pkS
			}
		}
		if okT {
			pkT := extractPK(rowT)
			if minPK == nil || ComparePK(pkT, minPK, spec.PKColumns) < 0 {
				minPK = pkT
			}
		}

		hasA := okA && ComparePK(extractPK(rowA), minPK, spec.PKColumns) == 0
		hasS := okS && ComparePK(extractPK(rowS), minPK, spec.PKColumns) == 0
		hasT := okT && ComparePK(extractPK(rowT), minPK, spec.PKColumns) == 0

		var curA, curS, curT TableRow
		if hasA {
			curA = rowA
			if rowA, okA, err = ancestor.Next(); err != nil {
				return TableDiffResult{}, err
			}
		}
		if hasS {
			curS = rowS
			if rowS, okS, err = source.Next(); err != nil {
				return TableDiffResult{}, err
			}
		}
		if hasT {
			curT = rowT
			if rowT, okT, err = target.Next(); err != nil {
				return TableDiffResult{}, err
			}
		}

		switch {
		case !hasA && !hasS && hasT:
			// Target only insert -> keep
		case !hasA && hasS && !hasT:
			// Source only insert -> INSERT
			kDisp, kRaw := keyMap(curS)
			all := allMap(curS)
			res.Actions = append(res.Actions, MergeAction{
				Kind:      ActionInsert,
				Key:       kDisp,
				KeyValues: kRaw,
				Values:    all,
				AllValues: all,
			})
			res.Inserted++
		case !hasA && hasS && hasT:
			// Inserted on both sides: if identical keep, else insert_insert conflict
			diffCols, sVals, tVals := diffColumns(spec.Columns, isPK, curS, curT)
			if len(diffCols) > 0 {
				kDisp, _ := keyMap(curS)
				addConflict(MergeConflict{
					Type:         ConflictInsertInsert,
					Database:     spec.Database,
					Table:        spec.Table,
					Key:          kDisp,
					Columns:      diffCols,
					SourceValues: sVals,
					TargetValues: tVals,
				})
			}
		case hasA && !hasS && !hasT:
			// Deleted on both sides -> keep
		case hasA && hasS && !hasT:
			// Deleted in Target; check if Source modified it
			diffCols, aVals, sVals := diffColumns(spec.Columns, isPK, curA, curS)
			if len(diffCols) > 0 {
				kDisp, _ := keyMap(curA)
				addConflict(MergeConflict{
					Type:           ConflictUpdateDelete,
					Database:       spec.Database,
					Table:          spec.Table,
					Key:            kDisp,
					Columns:        diffCols,
					AncestorValues: aVals,
					SourceValues:   sVals,
				})
			}
		case hasA && !hasS && hasT:
			// Deleted in Source; check if Target modified it
			diffCols, aVals, tVals := diffColumns(spec.Columns, isPK, curA, curT)
			if len(diffCols) > 0 {
				kDisp, _ := keyMap(curA)
				addConflict(MergeConflict{
					Type:           ConflictDeleteUpdate,
					Database:       spec.Database,
					Table:          spec.Table,
					Key:            kDisp,
					Columns:        diffCols,
					AncestorValues: aVals,
					TargetValues:   tVals,
				})
			} else {
				kDisp, kRaw := keyMap(curA)
				res.Actions = append(res.Actions, MergeAction{
					Kind:      ActionDelete,
					Key:       kDisp,
					KeyValues: kRaw,
					AllValues: allMap(curA),
				})
				res.Deleted++
			}
		case hasA && hasS && hasT:
			// Present in all three: check whether Source and/or Target modified the row.
			sDiffCols, _, _ := diffColumns(spec.Columns, isPK, curA, curS)
			tDiffCols, _, _ := diffColumns(spec.Columns, isPK, curA, curT)
			switch {
			case len(sDiffCols) == 0:
				// Unchanged in Source (Target may or may not have modified it) -> keep Target
			case len(tDiffCols) == 0:
				// Only Source modified this row -> UPDATE
				changedInS := make(map[string]RowValue, len(sDiffCols))
				mergedRow := allMap(curT)
				for _, col := range sDiffCols {
					vS := curS.Values[colIdx[col]]
					changedInS[col] = vS
					mergedRow[col] = vS
				}
				kDisp, kRaw := keyMap(curA)
				res.Actions = append(res.Actions, MergeAction{
					Kind:      ActionUpdate,
					Key:       kDisp,
					KeyValues: kRaw,
					Values:    changedInS,
					AllValues: mergedRow,
				})
				res.Updated++
			default:
				// Both Source and Target modified this same PK row: if final values match, keep Target;
				// otherwise report update_update conflict across all columns that differ between S and T.
				stCols, sVals, tVals := diffColumns(spec.Columns, isPK, curS, curT)
				if len(stCols) > 0 {
					aVals := make(map[string]any, len(stCols))
					for _, col := range stCols {
						aVals[col] = curA.Values[colIdx[col]].DisplayValue()
					}
					kDisp, _ := keyMap(curA)
					addConflict(MergeConflict{
						Type:           ConflictUpdateUpdate,
						Database:       spec.Database,
						Table:          spec.Table,
						Key:            kDisp,
						Columns:        stCols,
						AncestorValues: aVals,
						SourceValues:   sVals,
						TargetValues:   tVals,
					})
				}
			}
		}
	}
	return res, nil
}

func diffColumns(cols []string, isPK map[string]bool, left, right TableRow) ([]string, map[string]any, map[string]any) {
	var diff []string
	lMap := make(map[string]any)
	rMap := make(map[string]any)
	for i, c := range cols {
		if isPK[c] {
			continue
		}
		vL, vR := left.Values[i], right.Values[i]
		if !vL.Equal(vR) {
			diff = append(diff, c)
			lMap[c] = vL.DisplayValue()
			rMap[c] = vR.DisplayValue()
		}
	}
	return diff, lMap, rMap
}

// ComparePK compares two primary key tuples according to specs.
func ComparePK(a, b []RowValue, specs []PKColumnSpec) int {
	for i, spec := range specs {
		va, vb := a[i], b[i]
		if va.Null != vb.Null {
			if va.Null {
				return -1
			}
			return 1
		}
		if va.Null {
			continue
		}
		var c int
		switch spec.Kind {
		case PKInteger:
			c = compareIntText(va.Text, vb.Text)
		case PKDecimal:
			c = compareDecimalText(va.Text, vb.Text)
		case PKFloat:
			c = compareFloatText(va.Text, vb.Text)
		default:
			c = strings.Compare(va.Text, vb.Text)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func compareIntText(a, b string) int {
	ia, errA := strconv.ParseInt(a, 10, 64)
	ib, errB := strconv.ParseInt(b, 10, 64)
	if errA == nil && errB == nil {
		switch {
		case ia < ib:
			return -1
		case ia > ib:
			return 1
		default:
			return 0
		}
	}
	ba, okA := new(big.Int).SetString(a, 10)
	bb, okB := new(big.Int).SetString(b, 10)
	if okA && okB {
		return ba.Cmp(bb)
	}
	return strings.Compare(a, b)
}

func compareDecimalText(a, b string) int {
	ra, okA := new(big.Rat).SetString(a)
	rb, okB := new(big.Rat).SetString(b)
	if okA && okB {
		return ra.Cmp(rb)
	}
	return strings.Compare(a, b)
}

func compareFloatText(a, b string) int {
	nanA := strings.EqualFold(a, "NaN")
	nanB := strings.EqualFold(b, "NaN")
	if nanA || nanB {
		switch {
		case nanA && nanB:
			return 0
		case nanA:
			return 1
		default:
			return -1
		}
	}
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(b, 64)
	if errA == nil && errB == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

// LineMergePlan describes the resolved source, target and common ancestor for a line merge.
type LineMergePlan struct {
	SourceLine       int
	SourceCheckpoint string
	TargetLine       int
	// AncestorSnapshot is the ZFS snapshot of the lowest common ancestor.
	AncestorSnapshot Snapshot
	// AncestorLabel is the human-readable checkpoint name (or "@reset") of the ancestor.
	AncestorLabel string
	// AncestorRuntime is the PropRuntime value on the ancestor snapshot ("" if unset).
	AncestorRuntime string
	// SourceDataset is set when the source is the tip of an existing history/<branch>/<line> dataset.
	SourceDataset string
	// SourceSnapshot is set when the source is an existing snapshot (--checkpoint or promoted-away line).
	SourceSnapshot Snapshot
	// SourceRuntime is the PropRuntime value of the source snapshot or dataset ("" if unset).
	SourceRuntime string
}

type rawLineOrigin struct {
	line    int
	live    bool
	dataset string
	parent  *int
	from    string // raw ZFS snapshot name ("ckpt-x" or "reset")
	runtime string
}

// ResolveLineAncestor resolves the source state and lowest common ancestor snapshot
// for merging sourceLine (or checkpoint) into the live line of lines.
func ResolveLineAncestor(lines []Line, sourceLine *int, checkpoint string) (LineMergePlan, error) {
	var live Line
	haveLive := false
	for _, l := range lines {
		if l.Live && !l.Pending {
			live, haveLive = l, true
			break
		}
	}
	if !haveLive {
		return LineMergePlan{}, fmt.Errorf("live dataset: %w", ErrNotFound)
	}
	targetLine := lineNumber(live.Props)

	// Build raw origin map across datasets and checkpoint snapshots.
	byLine := map[int]rawLineOrigin{}
	applyOrigin := func(o *rawLineOrigin, props map[string]string) {
		if from := props[PropFrom]; from != "" && o.from == "" {
			o.from = from
			if p, err := strconv.Atoi(props[PropParent]); err == nil {
				o.parent = &p
			}
		}
		if rt := props[PropRuntime]; rt != "" && o.runtime == "" {
			o.runtime = rt
		}
	}
	for _, l := range lines {
		if l.Pending {
			continue
		}
		for _, s := range l.Snapshots {
			if _, ok := CheckpointOf(s.Name); !ok {
				continue
			}
			n := lineNumber(s.Props)
			o := byLine[n]
			o.line = n
			applyOrigin(&o, s.Props)
			byLine[n] = o
		}
	}
	for _, l := range lines {
		if l.Pending {
			continue
		}
		n := lineNumber(l.Props)
		o := byLine[n]
		o.line = n
		o.live = l.Live
		o.dataset = l.Dataset
		applyOrigin(&o, l.Props)
		byLine[n] = o
	}

	var plan LineMergePlan
	plan.TargetLine = targetLine

	var srcSnapInfo SnapshotInfo
	var hasSrcSnap bool

	switch {
	case checkpoint != "":
		if checkpoint == ResetPoint {
			ds, info, ok := findSnapshotInfo(lines, ResetSnapshot)
			if !ok {
				return LineMergePlan{}, fmt.Errorf("checkpoint %s: %w", checkpoint, ErrNotFound)
			}
			plan.SourceLine = lineNumber(info.Props)
			plan.SourceCheckpoint = ResetPoint
			plan.SourceSnapshot = Snapshot{Dataset: ds, Name: ResetSnapshot}
			plan.SourceRuntime = info.Props[PropRuntime]
			srcSnapInfo, hasSrcSnap = info, true
		} else {
			ds, info, ok := FindCheckpoint(lines, checkpoint)
			if !ok {
				return LineMergePlan{}, fmt.Errorf("checkpoint %s: %w", checkpoint, ErrNotFound)
			}
			plan.SourceLine = lineNumber(info.Props)
			plan.SourceCheckpoint = checkpoint
			plan.SourceSnapshot = Snapshot{Dataset: ds, Name: info.Name}
			plan.SourceRuntime = info.Props[PropRuntime]
			srcSnapInfo, hasSrcSnap = info, true
		}
	case sourceLine != nil:
		sLine := *sourceLine
		if sLine == targetLine {
			return LineMergePlan{}, fmt.Errorf("%w: source line %d is the current live line", ErrInvalidMergeSource, sLine)
		}
		o, exists := byLine[sLine]
		if !exists {
			return LineMergePlan{}, fmt.Errorf("line %d: %w", sLine, ErrNotFound)
		}
		plan.SourceLine = sLine
		if o.dataset != "" {
			plan.SourceDataset = o.dataset
			plan.SourceRuntime = o.runtime
			if ds, info, ok := NewestCheckpointOfLine(lines, sLine); ok && plan.SourceRuntime == "" {
				_ = ds
				plan.SourceRuntime = info.Props[PropRuntime]
			}
		} else if ds, info, ok := NewestCheckpointOfLine(lines, sLine); ok {
			plan.SourceSnapshot = Snapshot{Dataset: ds, Name: info.Name}
			plan.SourceRuntime = info.Props[PropRuntime]
			srcSnapInfo, hasSrcSnap = info, true
		} else {
			return LineMergePlan{}, fmt.Errorf("line %d: %w", sLine, ErrNotFound)
		}
	default:
		return LineMergePlan{}, fmt.Errorf("%w: specify source_line or checkpoint", ErrInvalidMergeSource)
	}

	// Build ancestry chain from a line up to root: [line, parent, grandparent, ...].
	chainOf := func(start int) ([]int, bool) {
		var chain []int
		seen := map[int]bool{}
		cur := start
		for {
			if seen[cur] {
				return nil, false
			}
			seen[cur] = true
			chain = append(chain, cur)
			o, ok := byLine[cur]
			if !ok || o.parent == nil || o.from == "" {
				break
			}
			cur = *o.parent
		}
		return chain, true
	}

	chainL, okL := chainOf(targetLine)
	chainS, okS := chainOf(plan.SourceLine)
	if !okL || !okS {
		return LineMergePlan{}, ErrNoCommonAncestor
	}

	// Find lowest common line P in chainL and chainS.
	inS := map[int]int{} // line -> index in chainS
	for i, l := range chainS {
		inS[l] = i
	}
	commonLine := -1
	idxL, idxS := -1, -1
	for i, l := range chainL {
		if j, ok := inS[l]; ok {
			commonLine = l
			idxL = i
			idxS = j
			break
		}
	}
	if commonLine < 0 {
		return LineMergePlan{}, fmt.Errorf("%w: lines %d and %d do not share a recorded history root", ErrNoCommonAncestor, plan.SourceLine, targetLine)
	}

	var ancDS string
	var ancInfo SnapshotInfo
	var ancLabel string

	switch {
	case idxL == 0 && idxS == 0:
		// TargetLine == SourceLine (only possible when --checkpoint was given on the live line).
		// Any checkpoint on the live line is already an ancestor of the live line tip.
		return LineMergePlan{}, fmt.Errorf("%w: checkpoint %s is already on the current live line %d", ErrInvalidMergeSource, checkpoint, targetLine)

	case idxS == 0:
		// SourceLine is an ancestor line of TargetLine (commonLine == SourceLine).
		// childL is the direct child of SourceLine on the path to TargetLine.
		childL := chainL[idxL-1]
		origL := byLine[childL]
		ds, info, label, ok := resolveBranchSnapshot(lines, byLine, commonLine, childL, origL.from)
		if !ok {
			return LineMergePlan{}, fmt.Errorf("%w: branch point %s from line %d to line %d is no longer available", ErrNoCommonAncestor, origL.from, commonLine, childL)
		}
		ancDS, ancInfo, ancLabel = ds, info, label
		if hasSrcSnap && !snapshotOlder(ancInfo, srcSnapInfo) {
			// The source snapshot is at or before the branch point, so Target already descends from it.
			return LineMergePlan{}, fmt.Errorf("%w: source snapshot %s has no changes past common ancestor %s (changes after the last checkpoint are discarded on restore; take a checkpoint before restore to keep and merge them)", ErrInvalidMergeSource, srcSnapInfo.Name, ancInfo.Name)
		}

	case idxL == 0:
		// TargetLine is an ancestor line of SourceLine (commonLine == TargetLine).
		childS := chainS[idxS-1]
		origS := byLine[childS]
		ds, info, label, ok := resolveBranchSnapshot(lines, byLine, commonLine, childS, origS.from)
		if !ok {
			return LineMergePlan{}, fmt.Errorf("%w: branch point %s from line %d to line %d is no longer available", ErrNoCommonAncestor, origS.from, commonLine, childS)
		}
		ancDS, ancInfo, ancLabel = ds, info, label

	default:
		// Sibling/cousin lines branching off commonLine via childS and childL.
		childL := chainL[idxL-1]
		childS := chainS[idxS-1]
		origL := byLine[childL]
		origS := byLine[childS]
		dsL, infoL, labelL, ok1 := resolveBranchSnapshot(lines, byLine, commonLine, childL, origL.from)
		dsS, infoS, labelS, ok2 := resolveBranchSnapshot(lines, byLine, commonLine, childS, origS.from)
		if !ok1 || !ok2 {
			return LineMergePlan{}, fmt.Errorf("%w: branch point on line %d is no longer available", ErrNoCommonAncestor, commonLine)
		}
		if infoL.Name == infoS.Name || snapshotOlder(infoL, infoS) {
			ancDS, ancInfo, ancLabel = dsL, infoL, labelL
		} else {
			ancDS, ancInfo, ancLabel = dsS, infoS, labelS
		}
	}

	if plan.SourceSnapshot.Name != "" && plan.SourceSnapshot.Name == ancInfo.Name {
		return LineMergePlan{}, fmt.Errorf("%w: source is identical to common ancestor %s", ErrInvalidMergeSource, ancLabel)
	}

	plan.AncestorSnapshot = Snapshot{Dataset: ancDS, Name: ancInfo.Name}
	plan.AncestorLabel = ancLabel
	plan.AncestorRuntime = ancInfo.Props[PropRuntime]
	return plan, nil
}

func findSnapshotInfo(lines []Line, snapName string) (string, SnapshotInfo, bool) {
	for _, l := range lines {
		if l.Pending {
			continue
		}
		for _, s := range l.Snapshots {
			if s.Name == snapName {
				return l.Dataset, s, true
			}
		}
	}
	return "", SnapshotInfo{}, false
}

func snapshotLabel(rawFrom, actualSnapName string) string {
	if rawFrom == ResetSnapshot || actualSnapName == ResetSnapshot {
		return ResetPoint
	}
	if cp, ok := CheckpointOf(rawFrom); ok {
		return cp
	}
	if cp, ok := CheckpointOf(actualSnapName); ok {
		return cp
	}
	return actualSnapName
}

// resolveBranchSnapshot finds the snapshot on parentLine from which childLine branched.
// If the original checkpoint was deleted, it follows ZFS's gone-* rename via Clones and PropLine.
func resolveBranchSnapshot(lines []Line, byLine map[int]rawLineOrigin, parentLine, childLine int, rawFrom string) (string, SnapshotInfo, string, bool) {
	if rawFrom == "" {
		return "", SnapshotInfo{}, "", false
	}
	if ds, info, ok := findSnapshotInfo(lines, rawFrom); ok {
		return ds, info, snapshotLabel(rawFrom, info.Name), true
	}
	if !strings.HasPrefix(rawFrom, CheckpointPrefix) {
		return "", SnapshotInfo{}, "", false
	}
	parentDS := byLine[parentLine].dataset
	childDS := byLine[childLine].dataset

	type goneCand struct {
		ds    string
		info  SnapshotInfo
		score int
	}
	var cands []goneCand
	for _, l := range lines {
		if l.Pending {
			continue
		}
		for _, s := range l.Snapshots {
			if !strings.HasPrefix(s.Name, GonePrefix) || lineNumber(s.Props) != parentLine {
				continue
			}
			score := 0
			// Pre-promote: gone-* sits on parentLine's dataset and childDS is in Clones.
			if childDS != "" && slices.Contains(s.Clones, childDS) {
				score = 3
			}
			// Post-promote: gone-* moved to childDS (or a descendant) and parentDS is in Clones.
			if score == 0 && parentDS != "" && slices.Contains(s.Clones, parentDS) {
				score = 2
			}
			if score == 0 && len(s.Clones) > 0 {
				score = 1
			}
			if score > 0 {
				cands = append(cands, goneCand{ds: l.Dataset, info: s, score: score})
			}
		}
	}
	if len(cands) == 0 {
		return "", SnapshotInfo{}, "", false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return snapshotOlder(cands[i].info, cands[j].info)
	})
	best := cands[0]
	return best.ds, best.info, snapshotLabel(rawFrom, best.info.Name), true
}

// snapshotOlder reports whether a was created strictly before b.
func snapshotOlder(a, b SnapshotInfo) bool {
	if a.Name == b.Name {
		return false
	}
	if a.Name == ResetSnapshot {
		return true
	}
	if b.Name == ResetSnapshot {
		return false
	}
	if a.CreateTXG > 0 && b.CreateTXG > 0 && a.CreateTXG != b.CreateTXG {
		return a.CreateTXG < b.CreateTXG
	}
	if !a.Created.Equal(b.Created) {
		return a.Created.Before(b.Created)
	}
	return a.Name < b.Name
}

// TableRef uniquely identifies a table by database/schema and table name.
type TableRef struct {
	Database string
	Table    string
}

func (t TableRef) String() string {
	if t.Database == "" {
		return t.Table
	}
	return t.Database + "." + t.Table
}

// ForeignKey describes a foreign-key dependency from Child to Parent.
type ForeignKey struct {
	Name          string
	Child         TableRef
	Parent        TableRef
	ChildColumns  []string
	ParentColumns []string
}

// TopoSortTables sorts tables in parent-before-child order for INSERT (insertOrder)
// and child-before-parent order for DELETE (deleteOrder). Self-referencing FKs are
// ignored for table ordering. If a cycle exists among distinct tables, hasCycle is
// true and a deterministic order covering all tables is still returned.
func TopoSortTables(tables []TableRef, fks []ForeignKey) (insertOrder []TableRef, deleteOrder []TableRef, hasCycle bool) {
	sorted := append([]TableRef(nil), tables...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })

	inSet := make(map[TableRef]bool, len(sorted))
	for _, t := range sorted {
		inSet[t] = true
	}
	inDegree := make(map[TableRef]int, len(sorted))
	childrenOf := make(map[TableRef][]TableRef, len(sorted))
	edgeSeen := make(map[[2]TableRef]bool)

	for _, fk := range fks {
		if fk.Child == fk.Parent || !inSet[fk.Child] || !inSet[fk.Parent] {
			continue
		}
		edge := [2]TableRef{fk.Parent, fk.Child}
		if edgeSeen[edge] {
			continue
		}
		edgeSeen[edge] = true
		childrenOf[fk.Parent] = append(childrenOf[fk.Parent], fk.Child)
		inDegree[fk.Child]++
	}
	for p := range childrenOf {
		sort.Slice(childrenOf[p], func(i, j int) bool { return childrenOf[p][i].String() < childrenOf[p][j].String() })
	}

	done := make(map[TableRef]bool, len(sorted))
	for len(insertOrder) < len(sorted) {
		progressed := false
		for _, t := range sorted {
			if done[t] || inDegree[t] > 0 {
				continue
			}
			done[t] = true
			insertOrder = append(insertOrder, t)
			for _, ch := range childrenOf[t] {
				inDegree[ch]--
			}
			progressed = true
			break
		}
		if !progressed {
			hasCycle = true
			// Break cycle deterministically by picking the first remaining table.
			for _, t := range sorted {
				if !done[t] {
					done[t] = true
					insertOrder = append(insertOrder, t)
					for _, ch := range childrenOf[t] {
						inDegree[ch]--
					}
					break
				}
			}
		}
	}

	deleteOrder = make([]TableRef, len(insertOrder))
	for i, t := range insertOrder {
		deleteOrder[len(insertOrder)-1-i] = t
	}
	return insertOrder, deleteOrder, hasCycle
}

// ValidateTableFilter filters allTables by the user-supplied table names (matching
// either "table" or "db.table") and verifies that no foreign key connects an
// included table to an excluded table.
func ValidateTableFilter(filter []string, allTables []TableRef, fks []ForeignKey) ([]TableRef, error) {
	if len(filter) == 0 {
		return allTables, nil
	}
	want := make(map[string]bool, len(filter))
	for _, f := range filter {
		f = strings.TrimSpace(f)
		if f != "" {
			want[strings.ToLower(f)] = true
		}
	}
	matched := make(map[TableRef]bool)
	seenFilter := make(map[string]bool)
	var out []TableRef
	for _, t := range allTables {
		short := strings.ToLower(t.Table)
		full := strings.ToLower(t.String())
		if want[short] || want[full] {
			matched[t] = true
			out = append(out, t)
			if want[short] {
				seenFilter[short] = true
			}
			if want[full] {
				seenFilter[full] = true
			}
		}
	}
	for w := range want {
		if !seenFilter[w] {
			return nil, fmt.Errorf("%w: table %q does not exist", ErrInvalidMergeSource, w)
		}
	}
	for _, fk := range fks {
		if fk.Child == fk.Parent {
			continue
		}
		cIn, pIn := matched[fk.Child], matched[fk.Parent]
		if cIn && !pIn {
			return nil, fmt.Errorf("%w: table %s references %s (foreign key %s); include %s in --tables",
				ErrInvalidMergeSource, fk.Child, fk.Parent, fk.Name, fk.Parent)
		}
		if pIn && !cIn {
			return nil, fmt.Errorf("%w: table %s is referenced by %s (foreign key %s); include %s in --tables",
				ErrInvalidMergeSource, fk.Parent, fk.Child, fk.Name, fk.Child)
		}
	}
	return out, nil
}
