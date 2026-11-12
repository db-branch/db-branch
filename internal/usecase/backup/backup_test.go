package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/backup"
)

var ctx = context.Background()

type fakeSource struct {
	restored string
	fail     error
}

func (f *fakeSource) WriteBackup(w io.Writer, node string, now time.Time) error {
	if f.fail != nil {
		return f.fail
	}
	_, err := io.WriteString(w, "state of "+node+" at "+now.Format(time.RFC3339))
	return err
}

func (f *fakeSource) RestoreBackup(_ context.Context, r io.Reader) (domain.RestoreSummary, error) {
	b, _ := io.ReadAll(r)
	f.restored = string(b)
	return domain.RestoreSummary{Node: "node1"}, f.fail
}

// memDir is a backup directory in memory.
type memDir struct {
	failCreate, failList, failClose error
	files                           map[string]string
	times                           map[string]time.Time
	now                             func() time.Time
}

type memFile struct {
	bytes.Buffer
	d    *memDir
	name string
}

func (f *memFile) Close() error {
	if f.d.failClose != nil {
		return f.d.failClose
	}
	f.d.files[f.name], f.d.times[f.name] = f.String(), f.d.now()
	return nil
}

func (d *memDir) Create(name string) (io.WriteCloser, error) {
	if d.failCreate != nil {
		return nil, d.failCreate
	}
	return &memFile{d: d, name: name}, nil
}

func (d *memDir) List() ([]domain.BackupFile, error) {
	if d.failList != nil {
		return nil, d.failList
	}
	var out []domain.BackupFile
	for n, c := range d.files {
		out = append(out, domain.BackupFile{Name: n, Size: int64(len(c)), TakenAt: d.times[n]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	return out, nil
}

func (d *memDir) Remove(name string) error { delete(d.files, name); return nil }

func (d *memDir) Open(name string) (io.ReadCloser, error) {
	c, ok := d.files[name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

func setup() (*backup.Service, *memDir, *fakeSource, *time.Time) {
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	dir := &memDir{files: map[string]string{}, times: map[string]time.Time{}, now: clock}
	src := &fakeSource{}
	s := &backup.Service{Source: src, Dir: dir, Node: "node1", Clock: clock, Keep: func() int { return 3 }, Every: func() time.Duration { return 6 * time.Hour }}
	return s, dir, src, &now
}

func TestTake_名前に時刻とノードを入れ古いものを消す(t *testing.T) {
	s, dir, _, now := setup()
	for i := 0; i < 5; i++ {
		if _, err := s.Take(ctx); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}
	list, _ := s.List()
	if len(list) != 3 || list[0].Name != "db-branch-node1-20261110T070000Z.json.gz" {
		t.Errorf("kept %+v", list)
	}
	if dir.files[list[0].Name] != "state of node1 at 2026-11-10T07:00:00Z" {
		t.Errorf("content %q", dir.files[list[0].Name])
	}
}

func TestDue_前回から間隔が過ぎたら取る(t *testing.T) {
	s, _, _, now := setup()
	if !s.Due() {
		t.Error("never taken: due")
	}
	_, _ = s.Take(ctx)
	*now = now.Add(5 * time.Hour)
	if s.Due() {
		t.Error("5h after: not due")
	}
	*now = now.Add(time.Hour)
	if !s.Due() {
		t.Error("6h after: due")
	}
	s.Every = func() time.Duration { return 0 }
	if s.Due() {
		t.Error("interval 0 turns it off")
	}
}

func TestRestore_ファイルの中身を戻す(t *testing.T) {
	s, _, src, _ := setup()
	sum, err := s.Restore(ctx, strings.NewReader("old state"))
	if err != nil || src.restored != "old state" || sum.Node != "node1" {
		t.Errorf("%+v %v %q", sum, err, src.restored)
	}
	f, _ := s.Take(ctx)
	r, err := s.Open(f.Name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	if !strings.HasPrefix(string(b), "state of node1") {
		t.Errorf("open %q", b)
	}
	for _, bad := range []string{"../x", "other.json.gz", ""} {
		if _, err := s.Open(bad); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestTake_書き出しに失敗したら残さない(t *testing.T) {
	s, dir, src, _ := setup()
	src.fail = errors.New("no state")
	if _, err := s.Take(ctx); err == nil {
		t.Error("source failure")
	}
	if len(dir.files) != 0 {
		t.Errorf("left %v", dir.files)
	}
}

func TestTake_保存先の失敗を伝える(t *testing.T) {
	boom := errors.New("disk full")
	for _, set := range []func(*memDir){
		func(d *memDir) { d.failCreate = boom },
		func(d *memDir) { d.failClose = boom },
		func(d *memDir) { d.failList = boom },
	} {
		s, dir, _, _ := setup()
		set(dir)
		if _, err := s.Take(ctx); !errors.Is(err, boom) {
			t.Errorf("%v", err)
		}
	}
	s, dir, _, _ := setup()
	dir.failList = boom
	if s.Due() {
		t.Error("unreadable dir: not due")
	}
}
