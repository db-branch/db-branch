package domain_test

import (
	"errors"
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

func TestCheckLimit_手動は上限まで自動は上限を一つ超えて許す(t *testing.T) {
	if err := domain.CheckLimit("x", 19, 20, false); err != nil {
		t.Errorf("19 of 20: %v", err)
	}
	if err := domain.CheckLimit("x", 20, 20, false); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("20 of 20: %v", err)
	}
	if err := domain.CheckLimit("x", 20, 20, true); err != nil {
		t.Errorf("auto at the limit: %v", err)
	}
	err := domain.CheckLimit("x", 21, 20, true)
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("auto above the limit: %v", err)
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
