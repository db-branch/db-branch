package membership_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/membership"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fakeMembers struct {
	notLeader bool
	servers   []membership.Server
	calls     []string
	err       error
}

func (f *fakeMembers) IsLeader() bool { return !f.notLeader }
func (f *fakeMembers) Servers(context.Context) ([]membership.Server, error) {
	return f.servers, f.err
}
func (f *fakeMembers) set(id string, voter bool) {
	for i := range f.servers {
		if f.servers[i].ID == id {
			f.servers[i].Voter = voter
		}
	}
}
func (f *fakeMembers) Promote(_ context.Context, id string) error {
	f.calls = append(f.calls, "promote "+id)
	if f.err == nil {
		f.set(id, true)
	}
	return f.err
}
func (f *fakeMembers) Demote(_ context.Context, id string) error {
	f.calls = append(f.calls, "demote "+id)
	if f.err == nil {
		f.set(id, false)
	}
	return f.err
}

type env struct {
	m     *fakeMembers
	nodes map[string]time.Time // last seen; absent = no record
	now   time.Time
	svc   *membership.Service
}

func name(i int) string { return fmt.Sprintf("n%02d", i) }

// newEnv has total nodes n00.., the first voters of them voting, all healthy;
// n00 leads.
func newEnv(total, voters int) *env {
	e := &env{m: &fakeMembers{}, nodes: map[string]time.Time{}, now: t0}
	for i := 0; i < total; i++ {
		e.m.servers = append(e.m.servers, membership.Server{ID: name(i), Voter: i < voters})
		e.nodes[name(i)] = t0
	}
	e.svc = &membership.Service{
		Members: e.m, Self: name(0), Clock: func() time.Time { return e.now },
		Nodes: func(context.Context) ([]domain.Node, error) {
			var out []domain.Node
			for n, seen := range e.nodes {
				out = append(out, domain.Node{Name: n, LastSeen: seen})
			}
			return out, nil
		},
		DownAfter: 30 * time.Second,
	}
	return e
}

// beat marks every node but those in skip as just heard from.
func (e *env) advance(d time.Duration, skip ...string) {
	e.now = e.now.Add(d)
	for n := range e.nodes {
		down := false
		for _, s := range skip {
			down = down || s == n
		}
		if !down {
			e.nodes[n] = e.now
		}
	}
}

// settle reconciles, letting time pass between passes, until nothing changes.
func (e *env) settle(t *testing.T, skip ...string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		e.advance(10*time.Second, skip...)
		changed, err := e.svc.Reconcile(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !changed && i > 12 {
			return
		}
	}
}

func (e *env) voters() []string {
	var out []string
	for _, s := range e.m.servers {
		if s.Voter {
			out = append(out, s.ID)
		}
	}
	return out
}

func TestTargetVoters_健全なノード数以下で最大の奇数かつ5以下(t *testing.T) {
	want := map[int]int{0: 1, 1: 1, 2: 1, 3: 3, 4: 3, 5: 5, 6: 5, 7: 5, 10: 5}
	for n, w := range want {
		if got := membership.TargetVoters(n); got != w {
			t.Errorf("TargetVoters(%d) = %d, want %d", n, got, w)
		}
	}
}

func TestReconcile_ノード数ごとに投票者が1_3_5へ収束する(t *testing.T) {
	for total, want := range map[int]int{1: 1, 2: 1, 3: 3, 4: 3, 5: 5, 6: 5, 9: 5, 10: 5} {
		e := newEnv(total, 1)
		e.settle(t)
		if got := len(e.voters()); got != want {
			t.Errorf("%d nodes: voters = %d (%v), want %d", total, got, e.voters(), want)
		}
	}
}

func TestReconcile_10ノードが全員投票者でも5に減らし_リーダーは残る(t *testing.T) {
	e := newEnv(10, 10)
	e.settle(t)
	if got := len(e.voters()); got != 5 {
		t.Fatalf("voters = %v", e.voters())
	}
	if !e.m.servers[0].Voter {
		t.Error("the leader was demoted")
	}
}

func TestReconcile_リーダーが名前順で最後の余剰投票者でも降格しない(t *testing.T) {
	e := newEnv(10, 10)
	e.svc.Self = name(9)
	e.settle(t)
	if !e.m.servers[9].Voter || len(e.voters()) != 5 {
		t.Errorf("voters = %v, calls = %v", e.voters(), e.m.calls)
	}
}

func TestReconcile_ノードが減ったら余剰の投票者を降格する(t *testing.T) {
	e := newEnv(3, 3)
	e.m.servers = e.m.servers[:2] // one node was removed
	e.settle(t)
	if got := e.voters(); len(got) != 1 || got[0] != name(0) {
		t.Errorf("voters = %v", got)
	}
}

func TestReconcile_安定時間が経つまで昇格しない(t *testing.T) {
	e := newEnv(3, 1)
	e.advance(10 * time.Second)
	if changed, _ := e.svc.Reconcile(context.Background()); changed {
		t.Fatalf("promoted at first sight: %v", e.m.calls)
	}
	e.advance(10 * time.Second)
	if changed, _ := e.svc.Reconcile(context.Background()); changed {
		t.Fatalf("promoted after 10 s: %v", e.m.calls)
	}
	e.advance(25 * time.Second)
	if changed, _ := e.svc.Reconcile(context.Background()); !changed {
		t.Fatal("not promoted after the stabilisation time")
	}
}

func TestReconcile_落ちて戻ったノードは安定時間を数え直す(t *testing.T) {
	e := newEnv(2, 1)
	e.svc.Reconcile(context.Background())
	e.advance(20*time.Second, name(1)) // n01 silent: unhealthy
	e.svc.Reconcile(context.Background())
	e.advance(20 * time.Second) // back
	e.svc.Reconcile(context.Background())
	e.advance(20 * time.Second)
	if changed, _ := e.svc.Reconcile(context.Background()); changed {
		t.Errorf("promoted too early: %v", e.m.calls)
	}
}

func TestReconcile_一度に一つしか変更しない(t *testing.T) {
	e := newEnv(6, 1)
	e.svc.Reconcile(context.Background())
	e.advance(40 * time.Second)
	if _, err := e.svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.m.calls) != 1 {
		t.Errorf("calls = %v", e.m.calls)
	}
}

func TestReconcile_最も長く健全な非投票者から昇格する(t *testing.T) {
	e := newEnv(4, 1)
	e.nodes[name(1)] = t0
	e.svc.Reconcile(context.Background()) // sees n01 n02 n03 healthy at t0
	e.advance(5*time.Second, name(2))     // n02 silent
	e.svc.Reconcile(context.Background())
	e.advance(60 * time.Second)
	e.svc.Reconcile(context.Background())
	// n03 and n01 have been healthy since t0 (n02 restarted counting), alphabetical among ties
	if len(e.m.calls) == 0 || e.m.calls[0] != "promote n01" {
		t.Errorf("calls = %v", e.m.calls)
	}
}

func TestReconcile_投票者が長く停止したら降格し健全な非投票者を昇格する(t *testing.T) {
	e := newEnv(4, 3)
	e.settle(t) // settled: 3 voters, n03 non-voter
	e.m.calls = nil
	e.settle(t, name(2)) // n02 goes silent
	if e.m.servers[2].Voter || !e.m.servers[3].Voter {
		t.Fatalf("voters = %v, calls = %v", e.voters(), e.m.calls)
	}
	if e.m.calls[0] != "demote n02" || e.m.calls[1] != "promote n03" {
		t.Errorf("calls = %v", e.m.calls)
	}
}

func TestReconcile_停止が短ければ降格しない(t *testing.T) {
	e := newEnv(4, 3)
	e.settle(t)
	e.m.calls = nil
	for i := 0; i < 6; i++ { // 60 s down
		e.advance(10*time.Second, name(2))
		e.svc.Reconcile(context.Background())
	}
	if len(e.m.calls) != 0 {
		t.Errorf("calls = %v", e.m.calls)
	}
}

func TestReconcile_代わりがいなければ停止中の投票者を降格しない(t *testing.T) {
	e := newEnv(3, 3)
	e.settle(t)
	e.settle(t, name(2))
	for _, c := range e.m.calls {
		t.Errorf("unexpected change: %s", c)
	}
}

func TestReconcile_非投票者が停止しても投票者は変わらず昇格の対象外(t *testing.T) {
	e := newEnv(4, 3)
	e.settle(t, name(3))
	if len(e.m.calls) != 0 || len(e.voters()) != 3 {
		t.Errorf("calls = %v", e.m.calls)
	}
}

func TestReconcile_停止した非投票者は昇格しない(t *testing.T) {
	e := newEnv(3, 1)
	e.settle(t, name(2))
	if e.m.servers[2].Voter {
		t.Errorf("silent node promoted: %v", e.m.calls)
	}
}

func TestReconcile_停止した投票者が残る間は健全な投票者を降格して多数派を割らない(t *testing.T) {
	// 3 voters, one down (below the demotion threshold), cluster shrank to 2 healthy
	// members (target 1): demoting a healthy voter would leave 1 healthy of 2.
	e := newEnv(3, 3)
	e.settle(t)
	e.m.calls = nil
	e.advance(20*time.Second, name(2))
	e.svc.Reconcile(context.Background())
	for _, c := range e.m.calls {
		if c != "demote n02" {
			t.Errorf("unsafe change: %v", e.m.calls)
		}
	}
}

func TestReconcile_リーダーでなければ何もしない(t *testing.T) {
	e := newEnv(5, 1)
	e.m.notLeader = true
	e.settle(t)
	if len(e.m.calls) != 0 {
		t.Errorf("calls = %v", e.m.calls)
	}
}

func TestReconcile_ノード記録のない投票者は降格しない(t *testing.T) {
	e := newEnv(4, 3)
	e.settle(t)
	delete(e.nodes, name(2))
	e.m.calls = nil
	e.settle(t)
	if len(e.m.calls) != 0 {
		t.Errorf("calls = %v", e.m.calls)
	}
}

func TestReconcile_取得や変更の失敗は返す(t *testing.T) {
	e := newEnv(3, 1)
	e.m.err = errors.New("raft busy")
	if _, err := e.svc.Reconcile(context.Background()); err == nil {
		t.Error("servers error: want error")
	}
	e = newEnv(3, 1)
	e.svc.Reconcile(context.Background())
	e.advance(40 * time.Second)
	f := &failing{fakeMembers: e.m}
	e.svc.Members = f
	if _, err := e.svc.Reconcile(context.Background()); err == nil {
		t.Error("promote error: want error")
	}
	e = newEnv(6, 6)
	e.svc.Members = &failing{fakeMembers: e.m}
	if _, err := e.svc.Reconcile(context.Background()); err == nil {
		t.Error("demote error: want error")
	}
	e.svc.Nodes = func(context.Context) ([]domain.Node, error) { return nil, errors.New("x") }
	if _, err := e.svc.Reconcile(context.Background()); err == nil {
		t.Error("nodes error: want error")
	}
}

type failing struct{ *fakeMembers }

func (f *failing) Promote(context.Context, string) error { return errors.New("no") }
func (f *failing) Demote(context.Context, string) error  { return errors.New("no") }
