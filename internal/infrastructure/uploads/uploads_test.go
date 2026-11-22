package uploads_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/uploads"
)

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func newStore(t *testing.T, free uint64) *uploads.Store {
	t.Helper()
	return &uploads.Store{
		Dir:      filepath.Join(t.TempDir(), "uploads"),
		MaxBytes: func() int64 { return 100 },
		Reserve:  10,
		Free:     func(string) (uint64, error) { return free, nil },
		Clock:    func() time.Time { return t0 },
	}
}

func TestSave_ダンプを書き込みIDで取り出せる(t *testing.T) {
	s := newStore(t, 1000)
	u, err := s.Save(strings.NewReader("CREATE TABLE t (a int);"), "prod.sql.gz", 23)
	if err != nil {
		t.Fatal(err)
	}
	if u.Size != 23 || u.Name != "prod.sql.gz" || !strings.HasSuffix(u.Path, ".sql.gz") || len(u.ID) != 32 {
		t.Errorf("upload = %+v", u)
	}
	if b, _ := os.ReadFile(u.Path); string(b) != "CREATE TABLE t (a int);" {
		t.Errorf("content %q", b)
	}
	got, err := s.Get(u.ID)
	if err != nil || got.Path != u.Path || got.Size != 23 {
		t.Errorf("get %+v %v", got, err)
	}
	if err := s.Remove(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("after remove: %v", err)
	}
	if err := s.Remove(u.ID); err != nil {
		t.Errorf("removing twice: %v", err)
	}
}

func TestSave_大きさと空き容量を確かめる(t *testing.T) {
	s := newStore(t, 1000)
	if _, err := s.Save(strings.NewReader(""), "a.sql", 101); !errors.Is(err, domain.ErrTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := newStore(t, 50).Save(strings.NewReader(strings.Repeat("x", 45)), "a.sql", 45); !errors.Is(err, domain.ErrNoSpace) {
		t.Errorf("no room with the reserve: %v", err)
	}
	if _, err := s.Save(strings.NewReader("x"), "a.sql", 0); err == nil {
		t.Error("an empty upload should fail")
	}
	// The body must be exactly the announced size; nothing is left behind.
	if _, err := s.Save(strings.NewReader("short"), "a.sql", 10); err == nil {
		t.Error("a short body should fail")
	}
	if _, err := s.Save(strings.NewReader("longer than said"), "a.sql", 3); err == nil {
		t.Error("a long body should fail")
	}
	if left, _ := os.ReadDir(s.Dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	s.Free = func(string) (uint64, error) { return 0, errors.New("statfs failed") }
	if _, err := s.Save(strings.NewReader("x"), "a.sql", 1); err == nil {
		t.Error("free space unknown")
	}
}

func TestSave_ダンプ以外の名前は受け付けない(t *testing.T) {
	s := newStore(t, 1000)
	for _, n := range []string{"a.txt", "a.tar.gz", "", ".sql"} {
		if _, err := s.Save(strings.NewReader("x"), n, 1); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
	for _, n := range []string{"a.sql", "../dir/b.SQL", "c.sql.gz", "d.dump.sql.GZ"} {
		u, err := s.Save(strings.NewReader("x"), n, 1)
		if err != nil || filepath.Dir(u.Path) != s.Dir || strings.Contains(u.Name, "/") {
			t.Errorf("%q: %+v %v", n, u, err)
		}
	}
	// The directory cannot be written.
	ro := newStore(t, 1000)
	_ = os.MkdirAll(ro.Dir, 0o500)
	_ = os.Chmod(ro.Dir, 0o500)
	if os.Getuid() != 0 {
		if _, err := ro.Save(strings.NewReader("x"), "a.sql", 1); err == nil {
			t.Error("read-only dir")
		}
	}
	// The directory cannot be made.
	blocked := newStore(t, 1000)
	_ = os.WriteFile(filepath.Dir(blocked.Dir)+"/uploads", []byte("a file"), 0o600)
	if _, err := blocked.Save(strings.NewReader("x"), "a.sql", 1); err == nil {
		t.Error("unusable dir")
	}
	for _, id := range []string{"", "../etc", "ZZ"} {
		if _, err := s.Get(id); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("get %q: %v", id, err)
		}
	}
}

func TestSweep_古いアップロードと書きかけを消す(t *testing.T) {
	s := newStore(t, 1000)
	old, _ := s.Save(strings.NewReader("x"), "old.sql", 1)
	_ = os.Chtimes(old.Path, t0.Add(-25*time.Hour), t0.Add(-25*time.Hour))
	part := filepath.Join(s.Dir, "abc.part")
	_ = os.WriteFile(part, []byte("x"), 0o600)
	_ = os.Chtimes(part, t0.Add(-25*time.Hour), t0.Add(-25*time.Hour))
	fresh, _ := s.Save(strings.NewReader("y"), "new.sql", 1)
	if n := s.Sweep(24 * time.Hour); n != 2 {
		t.Errorf("swept %d", n)
	}
	if _, err := s.Get(old.ID); err == nil {
		t.Error("old upload kept")
	}
	if _, err := s.Get(fresh.ID); err != nil {
		t.Errorf("fresh upload: %v", err)
	}
	if n := newStore(t, 1).Sweep(time.Hour); n != 0 {
		t.Errorf("no dir: %d", n)
	}
}

func TestDiskFree_実際のディスクの空きを返す(t *testing.T) {
	free, err := uploads.DiskFree(t.TempDir())
	if err != nil || free == 0 {
		t.Errorf("%d %v", free, err)
	}
	if _, err := uploads.DiskFree("/nonexistent/dir"); err != nil {
		t.Errorf("a missing dir uses its parent: %v", err)
	}
}

var _ io.Reader = strings.NewReader("")

func TestRoom_送れる大きさは上限と空きの小さい方(t *testing.T) {
	s := newStore(t, 1000) // limit 100, reserve 10
	if n, err := s.Room(); err != nil || n != 100 {
		t.Errorf("limited by the setting: %d %v", n, err)
	}
	s.Free = func(string) (uint64, error) { return 50, nil }
	if n, _ := s.Room(); n != 40 {
		t.Errorf("limited by the disk: %d", n)
	}
	s.Free = func(string) (uint64, error) { return 5, nil }
	if n, _ := s.Room(); n != 0 {
		t.Errorf("below the reserve: %d", n)
	}
	s.Free = func(string) (uint64, error) { return 0, errors.New("statfs failed") }
	if _, err := s.Room(); err == nil {
		t.Error("unknown free space")
	}
}

func TestSave_並行アップロードの予約バイトを排他的に合算して空き容量を超える受け付けを断る(t *testing.T) {
	// free 100, reserve 10: one 60-byte upload fits, two do not.
	s := newStore(t, 100)
	pr, pw := io.Pipe()
	first := make(chan error, 1)
	go func() { _, err := s.Save(pr, "a.sql", 60); first <- err }()
	// Wait until the first upload holds its reservation.
	deadline := time.Now().Add(5 * time.Second)
	for room, _ := s.Room(); room > 30 && time.Now().Before(deadline); room, _ = s.Room() {
		time.Sleep(time.Millisecond)
	}
	if room, _ := s.Room(); room != 30 {
		t.Fatalf("room while one 60-byte upload runs = %d, want 30", room)
	}
	if _, err := s.Save(strings.NewReader(strings.Repeat("x", 60)), "b.sql", 60); !errors.Is(err, domain.ErrNoSpace) {
		t.Fatalf("second upload: %v, want ErrNoSpace", err)
	}
	if _, err := pw.Write([]byte(strings.Repeat("y", 60))); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	if err := <-first; err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// The reservation is released: once it is on disk the (fake) free space is
	// what the file system reports, so a small upload fits again.
	if _, err := s.Save(strings.NewReader("zz"), "c.sql", 2); err != nil {
		t.Errorf("after the first finished: %v", err)
	}
}

func TestSave_失敗したアップロードは予約バイトを返す(t *testing.T) {
	s := newStore(t, 100)
	if _, err := s.Save(strings.NewReader("short"), "a.sql", 60); err == nil {
		t.Fatal("want an error for a truncated body")
	}
	if room, _ := s.Room(); room != 90 {
		t.Errorf("room = %d, want 90 (reservation released)", room)
	}
}
