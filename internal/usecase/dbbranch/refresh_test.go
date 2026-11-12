package dbbranch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

type fakeDumper struct {
	j       *journal
	err     error
	started chan struct{}
	release chan struct{}
	jobs    []domain.SourceDumpJob
}

func (f *fakeDumper) Dump(_ context.Context, job domain.SourceDumpJob) (string, error) {
	f.jobs = append(f.jobs, job)
	f.j.add("dump %s", strings.Join(job.Source.Databases, ","))
	if f.started != nil {
		f.started <- struct{}{}
		<-f.release
	}
	if f.err != nil {
		return "", f.err
	}
	return "/work/refresh-1.sql", nil
}

func (f *fakeDumper) Discard(path string) { f.j.add("discard %s", path) }

func mysqlRefresh() dbbranch.RefreshRequest {
	return dbbranch.RefreshRequest{
		Source: domain.RefreshSource{Host: "db.example.com", Port: 3306, User: "dumper", Password: "hunter2", Databases: []string{"app"}},
		Every:  24 * time.Hour,
	}
}

// withRefresh makes a base with a schedule and a dumper.
func (e *env) withRefresh(t *testing.T) *fakeDumper {
	t.Helper()
	e.withBase(t)
	d := &fakeDumper{j: e.j}
	e.svc.Dumper = d
	if _, err := e.svc.SetRefresh(ctx, name("prod"), mysqlRefresh()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSetRefresh_検証して保存しパスワードは空なら前のを残す(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	r, err := e.svc.SetRefresh(ctx, name("prod"), mysqlRefresh())
	if err != nil || !r.UpdatedAt.Equal(e.now) || r.Every != 24*time.Hour {
		t.Fatalf("set: %+v %v", r, err)
	}
	again := mysqlRefresh()
	again.Source.Password = ""
	again.Every = 48 * time.Hour
	r, err = e.svc.SetRefresh(ctx, name("prod"), again)
	if err != nil || r.Source.Password != "hunter2" || r.Every != 48*time.Hour {
		t.Fatalf("keep password: %+v %v", r, err)
	}
	got, _ := e.svc.GetRefresh(ctx, name("prod"))
	if got.Source.Password != "hunter2" {
		t.Errorf("stored = %+v", got)
	}
}

func TestSetRefresh_不正な設定とベースがない場合は断る(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	bad := mysqlRefresh()
	bad.Every = time.Minute
	if _, err := e.svc.SetRefresh(ctx, name("prod"), bad); !errors.Is(err, domain.ErrInvalidRefresh) {
		t.Errorf("short interval: %v", err)
	}
	if _, err := e.svc.SetRefresh(ctx, name("nope"), mysqlRefresh()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing base: %v", err)
	}
	if _, err := e.svc.GetRefresh(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("nothing was stored: %v", err)
	}
}

func TestRefresh_スケジュールを持てないストアでは使えない(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	e.svc.Store = struct{ domain.Store }{e.store}
	if _, err := e.svc.SetRefresh(ctx, name("prod"), mysqlRefresh()); err == nil {
		t.Error("want error")
	}
	if _, err := e.svc.GetRefresh(ctx, name("prod")); err == nil {
		t.Error("want error")
	}
	if err := e.svc.ClearRefresh(ctx, name("prod")); err == nil {
		t.Error("want error")
	}
	if _, err := e.svc.DueRefreshes(ctx); err == nil {
		t.Error("want error")
	}
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err == nil {
		t.Error("want error")
	}
}

func TestClearRefresh_スケジュールを消す(t *testing.T) {
	e := newEnv(t)
	e.withRefresh(t)
	if err := e.svc.ClearRefresh(ctx, name("prod")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ClearRefresh(ctx, name("prod")); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
}

func TestRefreshBase_ダンプしてロードしダンプを消してからスナップショットを撮る(t *testing.T) {
	e := newEnv(t)
	d := e.withRefresh(t)
	first := snapNames(t, e)[0]
	e.now = e.now.Add(25 * time.Hour)
	var phases []string
	if err := e.svc.RefreshBase(ctx, name("prod"), func(p string) { phases = append(phases, p) }); err != nil {
		t.Fatal(err)
	}
	idx := func(s string) int { return e.j.indexOf(s) }
	dump, load, discard := idx("dump app"), idx("load base-prod /work/refresh-1.sql"), idx("discard /work/refresh-1.sql")
	snap := -1
	for i, x := range e.j.entries {
		if strings.HasPrefix(x, "zfs snapshot") {
			snap = i
		}
	}
	if !(dump >= 0 && dump < load && load < discard && discard < snap) {
		t.Fatalf("order dump=%d load=%d discard=%d snapshot=%d: %v", dump, load, discard, snap, e.j.entries)
	}
	if d.jobs[0].Tools.BinDir != "/rt/bin" || d.jobs[0].Source.Password != "hunter2" {
		t.Errorf("job = %+v", d.jobs[0])
	}
	after, _ := e.store.GetBase(ctx, name("prod"))
	if got := snapNames(t, e); len(got) != 1 || got[0] == first {
		t.Errorf("snapshots = %v, first was %s", got, first)
	}
	if after.Status != domain.BaseReady {
		t.Errorf("status = %s", after.Status)
	}
	r, _ := e.svc.GetRefresh(ctx, name("prod"))
	if r.LastRun == nil || !r.LastRun.OK || r.LastRun.Error != "" || !r.LastRun.FinishedAt.Equal(e.now) {
		t.Errorf("last run = %+v", r.LastRun)
	}
	if len(phases) == 0 || phases[0] != "dumping" {
		t.Errorf("phases = %v", phases)
	}
}

func TestRefreshBase_古い未使用のスナップショットは既存の整理で消える(t *testing.T) {
	e := newEnv(t)
	e.withRefresh(t)
	first := snapNames(t, e)[0]
	e.now = e.now.Add(time.Hour)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	got := snapNames(t, e)
	if len(got) != 1 || got[0] == first {
		t.Errorf("snapshots = %v (first %s)", got, first)
	}
}

func TestRefreshBase_整理を止める設定なら古いスナップショットを残す(t *testing.T) {
	e := newEnv(t)
	e.withRefresh(t)
	e.svc.AutoPrune = func() bool { return false }
	e.now = e.now.Add(time.Hour)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	if got := snapNames(t, e); len(got) != 2 {
		t.Errorf("snapshots = %v", got)
	}
}

func TestRefreshBase_ダンプに失敗したら失敗を記録し何も変えない(t *testing.T) {
	e := newEnv(t)
	d := e.withRefresh(t)
	d.err = errors.New("pg_dump: connection to db refused; password hunter2 rejected")
	err := e.svc.RefreshBase(ctx, name("prod"), nil)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error = %v", err)
	}
	r, _ := e.svc.GetRefresh(ctx, name("prod"))
	if r.LastRun == nil || r.LastRun.OK || strings.Contains(r.LastRun.Error, "hunter2") || !strings.Contains(r.LastRun.Error, "refused") {
		t.Errorf("last run = %+v", r.LastRun)
	}
	for _, x := range e.j.entries {
		if strings.HasPrefix(x, "load") {
			t.Errorf("loaded after a failed dump: %v", e.j.entries)
		}
	}
	if b, _ := e.store.GetBase(ctx, name("prod")); b.Status != domain.BaseReady || len(b.Snapshots) != 1 {
		t.Errorf("base = %+v", b)
	}
	// The failure is retried at the next interval, not at once.
	if r.Due(e.now.Add(time.Hour)) || !r.Due(e.now.Add(24*time.Hour)) {
		t.Errorf("next run = %v", r.NextRun())
	}
}

func TestRefreshBase_ロードに失敗してもダンプは消しスナップショットは撮らない(t *testing.T) {
	e := newEnv(t)
	e.withRefresh(t)
	e.server.failOn = "load"
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err == nil {
		t.Fatal("want error")
	}
	if !e.j.has("discard /work/refresh-1.sql") {
		t.Errorf("dump not discarded: %v", e.j.entries)
	}
	for _, x := range e.j.entries {
		if strings.HasPrefix(x, "zfs snapshot") {
			t.Errorf("snapshot after a failed load: %v", e.j.entries)
		}
	}
	if r, _ := e.svc.GetRefresh(ctx, name("prod")); r.LastRun == nil || r.LastRun.OK {
		t.Errorf("last run = %+v", r.LastRun)
	}
}

func TestRefreshBase_スナップショットに失敗したら失敗として記録する(t *testing.T) {
	e := newEnv(t)
	e.withRefresh(t)
	e.storage.failOn = "snapshot"
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err == nil {
		t.Fatal("want error")
	}
	if r, _ := e.svc.GetRefresh(ctx, name("prod")); r.LastRun == nil || r.LastRun.OK {
		t.Errorf("last run = %+v", r.LastRun)
	}
}

func TestRefreshBase_同じベースの更新は同時に一つだけ(t *testing.T) {
	e := newEnv(t)
	d := e.withRefresh(t)
	d.started, d.release = make(chan struct{}), make(chan struct{})
	e.now = e.now.Add(24 * time.Hour)
	done := make(chan error, 1)
	go func() { done <- e.svc.RefreshBase(ctx, name("prod"), nil) }()
	<-d.started
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("second run: %v", err)
	}
	if due, _ := e.svc.DueRefreshes(ctx); len(due) != 0 {
		t.Errorf("a running base is not due: %v", due)
	}
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	d.started = nil
	e.now = e.now.Add(time.Minute)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Errorf("after the first finished: %v", err)
	}
}

func TestRefreshBase_スケジュールもダンパーもベースの持ち主も必要(t *testing.T) {
	e := newEnv(t)
	e.withBase(t)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no schedule: %v", err)
	}
	if _, err := e.svc.SetRefresh(ctx, name("prod"), mysqlRefresh()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no dumper: %v", err)
	}
	e.svc.Dumper = &fakeDumper{j: e.j}
	b, _ := e.store.GetBase(ctx, name("prod"))
	b.Node = "node2"
	_ = e.store.PutBase(ctx, b)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); !errors.Is(err, dbbranch.ErrOtherNode) {
		t.Errorf("other node: %v", err)
	}
}

func TestRefreshBase_実行中にスケジュールが消えても更新自体は成功する(t *testing.T) {
	e := newEnv(t)
	d := e.withRefresh(t)
	d.started, d.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- e.svc.RefreshBase(ctx, name("prod"), nil) }()
	<-d.started
	_ = e.svc.ClearRefresh(ctx, name("prod"))
	close(d.release)
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
}

func TestDueRefreshes_期限が来た自ノードの準備できたベースだけ返す(t *testing.T) {
	e := newEnv(t)
	e.withRefresh(t)
	due := func() []domain.Name {
		t.Helper()
		got, err := e.svc.DueRefreshes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if len(due()) != 0 {
		t.Error("due before the interval")
	}
	e.now = e.now.Add(24 * time.Hour)
	if got := due(); len(got) != 1 || got[0] != name("prod") {
		t.Fatalf("due = %v", got)
	}
	b, _ := e.store.GetBase(ctx, name("prod"))
	b.Status = domain.BaseMaintenance
	_ = e.store.PutBase(ctx, b)
	if len(due()) != 0 {
		t.Error("a base in maintenance is not due")
	}
	b.Status, b.Node = domain.BaseReady, "node2"
	_ = e.store.PutBase(ctx, b)
	if len(due()) != 0 {
		t.Error("another node's base is not ours to refresh")
	}
	_ = e.store.DeleteBase(ctx, name("prod"))
	if len(due()) != 0 {
		t.Error("a deleted base has no schedule")
	}
}
