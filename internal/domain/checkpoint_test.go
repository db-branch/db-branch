package domain_test

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

func snap(name string, props map[string]string, clones ...string) domain.SnapshotInfo {
	return domain.SnapshotInfo{Name: name, Created: now, Used: 10, Props: props, Clones: clones}
}

func line(n int) map[string]string { return map[string]string{domain.PropLine: itoa(n)} }

func itoa(n int) string { return string(rune('0' + n)) }

func TestParseCheckpointName_小文字英数字とハイフンだけ受け付ける(t *testing.T) {
	for _, ok := range []string{"a", "before-migration", "20261112-153000", "v1-2"} {
		if got, err := domain.ParseCheckpointName(ok); err != nil || got != ok {
			t.Errorf("%q: %q %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a_b", "a b", "a/b", "a@b",
		"0123456789012345678901234567890123456789x"} {
		if _, err := domain.ParseCheckpointName(bad); !errors.Is(err, domain.ErrInvalidName) {
			t.Errorf("%q: %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestCheckpointNames_既定名は時刻から作り自動分は接頭辞が付く(t *testing.T) {
	at := time.Date(2026, 11, 12, 15, 30, 5, 0, time.FixedZone("JST", 9*3600))
	if got := domain.DefaultCheckpointName(at); got != "20261112-063005" {
		t.Errorf("default = %q", got)
	}
	if got := domain.AutoCheckpointName("restore", at); got != "before-restore-20261112-063005" {
		t.Errorf("auto = %q", got)
	}
	if _, err := domain.ParseCheckpointName(domain.AutoCheckpointName("restore", at)); err != nil {
		t.Errorf("auto names must be valid names: %v", err)
	}
}

func TestCheckpointSnapshot_スナップショット名と相互に変換する(t *testing.T) {
	if got := domain.CheckpointSnapshot("c1"); got != "ckpt-c1" {
		t.Errorf("got %q", got)
	}
	if n, ok := domain.CheckpointOf("ckpt-c1"); !ok || n != "c1" {
		t.Errorf("CheckpointOf = %q %v", n, ok)
	}
	for _, other := range []string{"reset", "gone-1", "ckpt-", "s20261112"} {
		if _, ok := domain.CheckpointOf(other); ok {
			t.Errorf("%q is not a checkpoint", other)
		}
	}
}

func lines() []domain.Line {
	return []domain.Line{
		{Dataset: "p/branches/x", Live: true, Props: line(2), Snapshots: []domain.SnapshotInfo{
			snap("reset", nil),
			{Name: "ckpt-c1", Created: now.Add(1 * time.Minute), Used: 5, Props: map[string]string{domain.PropLine: "0", domain.PropAuto: "off"}, Clones: []string{"p/history/x/1"}},
		}},
		{Dataset: "p/history/x/1", Props: line(1), Snapshots: []domain.SnapshotInfo{
			{Name: "ckpt-c3", Created: now.Add(3 * time.Minute), Used: 7, Props: map[string]string{domain.PropLine: "1", domain.PropAuto: "off"}},
			{Name: "ckpt-before-restore-x", Created: now.Add(2 * time.Minute), Used: 9, Props: map[string]string{domain.PropLine: "1", domain.PropAuto: "on"}},
			snap("gone-1", nil),
		}},
	}
}

func TestListCheckpoints_全ての線のチェックポイントを作成順に並べ現在地に印を付ける(t *testing.T) {
	got := domain.ListCheckpoints(lines(), "c1")
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if len(names) != 3 || names[0] != "c1" || names[1] != "before-restore-x" || names[2] != "c3" {
		t.Fatalf("names = %v", names)
	}
	if !got[0].Current || got[1].Current || got[2].Current {
		t.Errorf("current marks = %+v", got)
	}
	if !got[1].Auto || got[0].Auto || got[1].Line != 1 || got[1].Size != 9 || got[0].Line != 0 {
		t.Errorf("fields = %+v", got)
	}
}

func TestNextLine_使われている最大の線番号の次を返す(t *testing.T) {
	if got := domain.NextLine(lines()); got != 3 {
		t.Errorf("NextLine = %d, want 3", got)
	}
	if got := domain.NextLine(nil); got != 1 {
		t.Errorf("NextLine(nil) = %d, want 1", got)
	}
}

func TestFindCheckpoint_名前から所属するデータセットを探す(t *testing.T) {
	ds, s, ok := domain.FindCheckpoint(lines(), "c3")
	if !ok || ds != "p/history/x/1" || s.Name != "ckpt-c3" {
		t.Errorf("%q %+v %v", ds, s, ok)
	}
	if _, _, ok := domain.FindCheckpoint(lines(), "nope"); ok {
		t.Error("unknown checkpoint found")
	}
	if _, _, ok := domain.FindCheckpoint(lines(), "gone-1"); ok {
		t.Error("tombstone must not be found")
	}
}

func TestUniqueCheckpointName_重なる名前には番号を付ける(t *testing.T) {
	if got := domain.UniqueCheckpointName(lines(), "fresh"); got != "fresh" {
		t.Errorf("got %q", got)
	}
	if got := domain.UniqueCheckpointName(lines(), "c1"); got != "c1-2" {
		t.Errorf("got %q", got)
	}
}

func TestCheckLimit_手動の数だけが上限に当たる(t *testing.T) {
	if err := domain.CheckLimit("x", 19, 20); err != nil {
		t.Errorf("19 of 20: %v", err)
	}
	err := domain.CheckLimit("x", 20, 20)
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "limit 20") {
		t.Errorf("20 of 20: %v", err)
	}
}

func cps(spec string) []domain.Checkpoint {
	// "m1 a2 a3": m = manual, a = automatic; the number is the minute taken.
	var out []domain.Checkpoint
	for _, f := range strings.Fields(spec) {
		min, _ := strconv.Atoi(f[1:])
		out = append(out, domain.Checkpoint{Name: f, CreatedAt: now.Add(time.Duration(min) * time.Minute), Auto: f[0] == 'a'})
	}
	return out
}

func TestCountManual_自動のチェックポイントは数えない(t *testing.T) {
	if got := domain.CountManual(cps("m1 a2 m3 a4 a5")); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

func names(list []domain.Checkpoint) []string {
	var out []string
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}

func TestAutoToRotate_新しい5件を残し古い自動から順に返す(t *testing.T) {
	got := domain.AutoToRotate(cps("a7 m2 a1 a3 a5 a6 a4 a8 m9"), domain.MaxAutoCheckpoints)
	if want := []string{"a1", "a3"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("rotate = %v, want %v", names(got), want)
	}
	if got := domain.AutoToRotate(cps("a1 a2 a3 a4 a5 m6"), 5); len(got) != 0 {
		t.Errorf("five automatic checkpoints are kept: %v", names(got))
	}
}

func TestSpaceWarning_使用量がプール空きの半分を超えたら警告する(t *testing.T) {
	if got := domain.SpaceWarning(50, 100); got != "" {
		t.Errorf("exactly half: %q", got)
	}
	if got := domain.SpaceWarning(0, 0); got != "" {
		t.Errorf("nothing known: %q", got)
	}
	got := domain.SpaceWarning(51, 100)
	if !strings.Contains(got, "half") || !strings.Contains(got, "free space") {
		t.Errorf("warning = %q", got)
	}
}

func TestGarbage_チェックポイントも子もない線と子のない墓標だけ捨てる(t *testing.T) {
	ls := []domain.Line{
		{Dataset: "live", Live: true, Snapshots: []domain.SnapshotInfo{snap("reset", nil), snap("gone-5", nil), snap("gone-6", nil, "other")}},
		{Dataset: "h/empty", Snapshots: []domain.SnapshotInfo{snap("gone-1", nil)}},
		{Dataset: "h/kept", Snapshots: []domain.SnapshotInfo{snap("ckpt-a", nil)}},
		{Dataset: "h/origin", Snapshots: []domain.SnapshotInfo{snap("gone-2", nil, "h/kept")}},
		{Dataset: "h/reset", Snapshots: []domain.SnapshotInfo{snap("reset", nil)}},
		{Dataset: "h/new", Pending: true},
	}
	g := domain.Garbage(ls)
	if len(g.Datasets) != 1 || g.Datasets[0] != "h/empty" {
		t.Errorf("datasets = %v", g.Datasets)
	}
	var snaps []string
	for _, s := range g.Snapshots {
		snaps = append(snaps, s.String())
	}
	if len(snaps) != 1 || snaps[0] != "live@gone-5" {
		t.Errorf("snapshots = %v", snaps)
	}
}

func TestDestroyOrder_子のクローンを先に消す(t *testing.T) {
	ls := []domain.Line{
		{Dataset: "live", Live: true, Snapshots: []domain.SnapshotInfo{snap("ckpt-a", nil, "h/1")}},
		{Dataset: "h/1", Snapshots: []domain.SnapshotInfo{snap("ckpt-b", nil, "h/2")}},
		{Dataset: "h/2"},
		{Dataset: "h/3"},
	}
	order, err := domain.DestroyOrder(ls)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, d := range order {
		pos[d] = i
	}
	if len(order) != 4 || pos["h/2"] > pos["h/1"] || pos["h/1"] > pos["live"] {
		t.Errorf("order = %v", order)
	}
}

func TestDestroyOrder_外のクローンが依存していたら消さない(t *testing.T) {
	ls := []domain.Line{{Dataset: "live", Live: true, Snapshots: []domain.SnapshotInfo{snap("ckpt-a", nil, "tank/db-branch/branches/other")}}}
	if _, err := domain.DestroyOrder(ls); !errors.Is(err, domain.ErrInUse) {
		t.Errorf("error = %v, want ErrInUse", err)
	}
}

func TestBranch_チェックポイント操作は準備完了のときだけ受け付ける(t *testing.T) {
	for _, s := range []domain.BranchStatus{domain.BranchReady} {
		if err := (domain.Branch{Name: mustName("x"), Status: s}).CanCheckpoint(); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []domain.BranchStatus{domain.BranchProvisioning, domain.BranchResetting, domain.BranchRestoring, domain.BranchDeleting, domain.BranchFailed, domain.BranchLost} {
		if err := (domain.Branch{Name: mustName("x"), Status: s}).CanCheckpoint(); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("%s: %v, want ErrConflict", s, err)
		}
	}
}

func mustName(s string) domain.Name {
	n, err := domain.ParseName(s)
	if err != nil {
		panic(err)
	}
	return n
}

func TestLineViews_線を番号順に並べ復元元を読み取る(t *testing.T) {
	lines := []domain.Line{
		{Dataset: "h/2", Live: true, Props: map[string]string{domain.PropLine: "2", domain.PropFrom: "ckpt-c1", domain.PropParent: "0"}},
		{Dataset: "h/new", Pending: true, Props: map[string]string{domain.PropLine: "3"}},
		{Dataset: "h/1", Props: map[string]string{domain.PropLine: "1", domain.PropFrom: "reset", domain.PropParent: "0"}},
		{Dataset: "h/0", Props: map[string]string{}},
		{Dataset: "h/old", Props: map[string]string{domain.PropLine: "4", domain.PropFrom: "ckpt-x"}}, // made before parents were recorded
	}
	got := domain.LineViews(lines)
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	zero := 0
	want := []domain.LineView{{Line: 0}, {Line: 1, From: "reset", Parent: &zero}, {Line: 2, Live: true, From: "c1", Parent: &zero}, {Line: 4, From: "x"}}
	for i, w := range want {
		g := got[i]
		if g.Line != w.Line || g.Live != w.Live || g.From != w.From || (g.Parent == nil) != (w.Parent == nil) || (w.Parent != nil && *g.Parent != *w.Parent) {
			t.Errorf("line %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestSnapshotLine_チェックポイントは取った線をリセットは最初の線を指す(t *testing.T) {
	lines := []domain.Line{{Dataset: "l", Snapshots: []domain.SnapshotInfo{
		{Name: "reset"}, {Name: "ckpt-a", Props: map[string]string{domain.PropLine: "3"}}}}}
	if domain.SnapshotLine(lines, "ckpt-a") != 3 || domain.SnapshotLine(lines, "reset") != 0 || domain.SnapshotLine(lines, "missing") != 0 {
		t.Error("lines of snapshots")
	}
}

func TestLineViews_データセットを失った線もチェックポイントから描く(t *testing.T) {
	// Line 0's dataset was promoted away; its checkpoints now sit on line 2's dataset,
	// and line 1 (a restore from line 0's c1) remembers where it came from on them.
	lines := []domain.Line{{Dataset: "b/x", Live: true, Props: map[string]string{domain.PropLine: "2", domain.PropFrom: "ckpt-auto", domain.PropParent: "1"},
		Snapshots: []domain.SnapshotInfo{
			{Name: "reset"},
			{Name: "ckpt-c1", Props: map[string]string{domain.PropLine: "0"}},
			{Name: "ckpt-auto", Props: map[string]string{domain.PropLine: "1", domain.PropFrom: "ckpt-c1", domain.PropParent: "0"}},
		}}}
	got := domain.LineViews(lines)
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Line != 0 || got[0].Live || got[0].Parent != nil || got[0].From != "" {
		t.Errorf("line 0 = %+v", got[0])
	}
	if got[1].Line != 1 || got[1].Parent == nil || *got[1].Parent != 0 || got[1].From != "c1" {
		t.Errorf("line 1 = %+v", got[1])
	}
	if got[2].Line != 2 || !got[2].Live || got[2].Parent == nil || *got[2].Parent != 1 || got[2].From != "auto" {
		t.Errorf("line 2 = %+v", got[2])
	}
}

func TestNewestCheckpointOfLine_線の最後のチェックポイントとそれを持つデータセットを返す(t *testing.T) {
	lines := []domain.Line{
		{Dataset: "a", Snapshots: []domain.SnapshotInfo{{Name: "reset"}, {Name: "ckpt-old", Created: now, Props: line(0)}}},
		{Dataset: "b", Snapshots: []domain.SnapshotInfo{{Name: "ckpt-new", Created: now.Add(time.Hour), Props: line(0)}, {Name: "ckpt-other", Created: now.Add(2 * time.Hour), Props: line(1)}}},
	}
	ds, s, ok := domain.NewestCheckpointOfLine(lines, 0)
	if !ok || ds != "b" || s.Name != "ckpt-new" {
		t.Errorf("got %s %+v %v", ds, s, ok)
	}
	if _, _, ok := domain.NewestCheckpointOfLine(lines, 7); ok {
		t.Error("a line without checkpoints")
	}
}

func TestErrCheckpointsUnavailable_競合の一種で理由を伝える(t *testing.T) {
	if !errors.Is(domain.ErrCheckpointsUnavailable, domain.ErrConflict) {
		t.Error("must be a conflict")
	}
	if !strings.Contains(domain.ErrCheckpointsUnavailable.Error(), "not available on this storage") {
		t.Errorf("message = %q", domain.ErrCheckpointsUnavailable)
	}
}
