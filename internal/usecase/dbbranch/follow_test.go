package dbbranch_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/dbbranch"
)

type fakeFollower struct {
	runs []domain.MaskRun
	err  error
	got  []string
}

func (f *fakeFollower) AfterRefresh(_ context.Context, base domain.Name, snap string) ([]domain.MaskRun, error) {
	f.got = append(f.got, base.String()+"@"+snap)
	return f.runs, f.err
}

type fakeLauncher struct {
	busy    map[domain.Name]bool
	started []dbbranch.MaskRequest
	nodes   []string
	err     error
}

func (l *fakeLauncher) Busy(t domain.Name) bool { return l.busy[t] }
func (l *fakeLauncher) Start(node string, r dbbranch.MaskRequest) error {
	if l.err != nil {
		return l.err
	}
	l.started, l.nodes = append(l.started, r), append(l.nodes, node)
	l.busy[r.Target] = true
	return nil
}

// followEnv is a refreshable "prod" with bases "dev" and "qa" to mask into.
func followEnv(t *testing.T) (*env, *fakeFollower, *fakeLauncher, *[]string) {
	t.Helper()
	e := newEnv(t)
	e.withRefresh(t)
	for _, n := range []string{"dev", "qa"} {
		if _, err := e.svc.CreateBase(ctx, dbbranch.CreateBaseRequest{Name: name(n), Runtime: mysql84}, nil); err != nil {
			t.Fatal(err)
		}
	}
	f, l, logs := &fakeFollower{}, &fakeLauncher{busy: map[domain.Name]bool{}}, &[]string{}
	e.svc.Follower, e.svc.Launcher = f, l
	e.svc.Logf = func(format string, args ...any) { *logs = append(*logs, fmt.Sprintf(format, args...)) }
	return e, f, l, logs
}

func TestRefreshBase_成功したら返された実行を新しいスナップショットで一つずつ起動する(t *testing.T) {
	e, f, l, logs := followEnv(t)
	f.runs = []domain.MaskRun{{Target: "dev", Masker: "greenmask"}, {Target: "qa", Masker: "anon"}}
	e.now = e.now.Add(25 * time.Hour)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	snap := snapNames(t, e)[0]
	want := []dbbranch.MaskRequest{
		{Target: name("dev"), Source: name("prod"), Snapshot: snap, Masker: "greenmask", By: "refresh"},
		{Target: name("qa"), Source: name("prod"), Snapshot: snap, Masker: "anon", By: "refresh"},
	}
	if len(l.started) != 2 || l.started[0] != want[0] || l.started[1] != want[1] || len(*logs) != 0 {
		t.Errorf("started %+v logs %v", l.started, *logs)
	}
	if len(f.got) != 1 || f.got[0] != "prod@"+snap {
		t.Errorf("asked %v", f.got)
	}
}

func TestRefreshBase_失敗した更新の後は何も起動せず聞きもしない(t *testing.T) {
	e, f, l, _ := followEnv(t)
	f.runs = []domain.MaskRun{{Target: "dev", Masker: "greenmask"}}
	e.svc.Dumper.(*fakeDumper).err = errors.New("refused")
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err == nil {
		t.Fatal("expected failure")
	}
	if len(l.started) != 0 || len(f.got) != 0 {
		t.Errorf("started %v asked %v", l.started, f.got)
	}
}

func TestRefreshBase_起動できない対象は理由を残して飛ばし更新は成功のまま(t *testing.T) {
	e, f, l, logs := followEnv(t)
	l.busy[name("dev")] = true
	f.runs = []domain.MaskRun{
		{Target: "dev", Masker: "greenmask"},   // busy
		{Target: "ghost", Masker: "greenmask"}, // unknown
		{Target: "Bad Name!", Masker: "x"},     // invalid
		{Target: "prod", Masker: "x"},          // the source itself
		{Target: "qa", Masker: "anon"},         // fine
	}
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	if len(l.started) != 1 || l.started[0].Target != name("qa") {
		t.Errorf("started %+v", l.started)
	}
	if len(*logs) != 4 || !strings.Contains((*logs)[0], "busy") {
		t.Errorf("logs %q", *logs)
	}
}

func TestRefreshBase_別のノードにあるマスク先はそのノードで起動する(t *testing.T) {
	e, f, l, logs := followEnv(t)
	far := domain.Base{Name: name("far"), Runtime: mysql84, Node: "node2", Status: domain.BaseReady}
	if err := e.store.PutBase(ctx, far); err != nil {
		t.Fatal(err)
	}
	f.runs = []domain.MaskRun{{Target: "far", Masker: "greenmask"}, {Target: "dev", Masker: "greenmask"}}
	e.now = e.now.Add(25 * time.Hour)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	if len(l.started) != 2 || l.nodes[0] != "node2" || l.nodes[1] != "node1" || len(*logs) != 0 {
		t.Errorf("started %+v on %v, logs %v", l.started, l.nodes, *logs)
	}
}

func TestRefreshBase_同じ対象への二つの実行は後のを飛ばす(t *testing.T) {
	e, f, l, logs := followEnv(t)
	f.runs = []domain.MaskRun{{Target: "dev", Masker: "a"}, {Target: "dev", Masker: "b"}}
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	if len(l.started) != 1 || l.started[0].Masker != "a" || len(*logs) != 1 {
		t.Errorf("started %+v logs %v", l.started, *logs)
	}
}

func TestRefreshBase_起動と問い合わせの失敗は記録するだけで更新を失敗させない(t *testing.T) {
	e, f, l, logs := followEnv(t)
	f.err = errors.New("licence")
	f.runs = []domain.MaskRun{{Target: "dev", Masker: "a"}}
	e.now = e.now.Add(25 * time.Hour)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	f.err, l.err = nil, errors.New("record store down")
	e.now = e.now.Add(25 * time.Hour)
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	if len(l.started) != 0 || len(*logs) != 2 || !strings.Contains((*logs)[0], "licence") || !strings.Contains((*logs)[1], "record store down") {
		t.Errorf("logs %q", *logs)
	}
}

func TestRefreshBase_拡張がなければ後続は何もなく静かに終わる(t *testing.T) {
	e, _, l, _ := followEnv(t)
	e.svc.Follower, e.svc.Logf = nil, nil
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	e.svc.Follower, e.svc.Launcher = &fakeFollower{err: errors.New("x"), runs: []domain.MaskRun{{Target: "dev"}}}, nil
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil {
		t.Fatal(err)
	}
	e.svc.Launcher = l
	if err := e.svc.RefreshBase(ctx, name("prod"), nil); err != nil { // Logf nil: still fine
		t.Fatal(err)
	}
	if len(l.started) != 0 {
		t.Errorf("started %v", l.started)
	}
}
