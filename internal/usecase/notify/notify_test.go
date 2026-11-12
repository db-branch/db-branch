package notify_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/usecase/notify"
	"github.com/db-branch/db-branch/internal/usecase/operation"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
)

type sink struct {
	got  []notify.Notice
	fail error
}

func (s *sink) Send(_ context.Context, n notify.Notice) error {
	s.got = append(s.got, n)
	return s.fail
}

type state struct {
	fail     string
	branches []domain.Branch
	ops      []operation.Operation
	nodes    []domain.Node
}

func (s *state) err(what string) error {
	if s.fail == what {
		return errors.New(what + " unavailable")
	}
	return nil
}

func (s *state) ListBranches(context.Context) ([]domain.Branch, error) {
	return s.branches, s.err("branches")
}
func (s *state) ListOperations(context.Context) ([]operation.Operation, error) {
	return s.ops, s.err("ops")
}
func (s *state) ListNodes(context.Context) ([]domain.Node, error) { return s.nodes, s.err("nodes") }

func name(s string) domain.Name { n, _ := domain.ParseName(s); return n }

func at(d time.Duration) *time.Time { t := t0.Add(d); return &t }

func TestCheck_期限切れ間近と失敗とノード停止を一度ずつ知らせる(t *testing.T) {
	now := t0
	st := &state{
		branches: []domain.Branch{
			{Name: name("soon"), Owner: "a@x.io", ExpiresAt: at(30 * time.Minute)},
			{Name: name("later"), ExpiresAt: at(5 * time.Hour)},
			{Name: name("forever")},
		},
		ops: []operation.Operation{
			{ID: "op1", Kind: "branch.create", Target: operation.Target{Resource: "branch", Name: "x"}, Node: "n1", State: operation.Failed, Error: "disk full", UpdatedAt: t0.Add(-time.Minute)},
			{ID: "op2", Kind: "branch.create", State: operation.Succeeded, UpdatedAt: t0},
			{ID: "op0", Kind: "base.snapshot", State: operation.Failed, UpdatedAt: t0.Add(-time.Hour)}, // before we started watching
		},
		nodes: []domain.Node{{Name: "n1", LastSeen: t0}, {Name: "n2", LastSeen: t0.Add(-10 * time.Minute)}},
	}
	out := &sink{}
	s := &notify.Service{State: st, Senders: []notify.Sender{out}, Clock: func() time.Time { return now }, Warn: func() time.Duration { return time.Hour },
		DownAfter: time.Minute, Since: t0.Add(-5 * time.Minute)}
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, n := range out.got {
		kinds = append(kinds, n.Event+":"+n.Subject)
	}
	got := strings.Join(kinds, " ")
	for _, want := range []string{"branch.expiring:soon", "operation.failed:x", "node.down:n2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if len(out.got) != 3 {
		t.Errorf("notices %v", got)
	}
	if n := out.got[0]; !strings.Contains(n.Text, "expires in 30m (") || n.Owner != "a@x.io" {
		t.Errorf("expiry notice %+v", n)
	}
	if n := out.got[1]; !strings.Contains(n.Text, "disk full") || n.Level != "error" {
		t.Errorf("failure notice %+v", n)
	}
	// Nothing new: nothing sent again.
	_ = s.Check(ctx)
	if len(out.got) != 3 {
		t.Errorf("repeated: %d", len(out.got))
	}
	// n2 comes back; a branch's expiry is extended then nears again.
	st.nodes[1].LastSeen = now
	st.branches[0].ExpiresAt = at(time.Hour)
	_ = s.Check(ctx)
	if n := out.got[len(out.got)-2]; !strings.Contains(n.Text, "expires in 1h (") {
		t.Errorf("hours: %s", n.Text)
	}
	got = out.got[len(out.got)-2].Event + " " + out.got[len(out.got)-1].Event
	if !strings.Contains(got, "node.up") || !strings.Contains(got, "branch.expiring") {
		t.Errorf("after changes: %s", got)
	}
}

func TestCheck_送り先の失敗は伝えるが止まらない(t *testing.T) {
	st := &state{branches: []domain.Branch{{Name: name("soon"), ExpiresAt: at(time.Minute)}}}
	bad, good := &sink{fail: errors.New("slack down")}, &sink{}
	s := &notify.Service{State: st, Senders: []notify.Sender{bad, good}, Clock: func() time.Time { return t0 }, Warn: func() time.Duration { return time.Hour }}
	if err := s.Check(ctx); err == nil || len(good.got) != 1 {
		t.Errorf("%v %d", err, len(good.got))
	}
	none := &notify.Service{State: st, Clock: func() time.Time { return t0 }, Warn: func() time.Duration { return 0 }}
	if err := none.Check(ctx); err != nil {
		t.Errorf("nothing to send to: %v", err)
	}
}

func TestCheck_状態を読めなければ伝える(t *testing.T) {
	for _, what := range []string{"branches", "ops", "nodes"} {
		s := &notify.Service{State: &state{fail: what}, Clock: func() time.Time { return t0 }, Warn: func() time.Duration { return time.Hour }, DownAfter: time.Minute}
		if err := s.Check(ctx); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
}

func TestPublish_状態によらない通知をすべての送り先へ送る(t *testing.T) {
	bad, good := &sink{fail: errors.New("slack down")}, &sink{}
	s := &notify.Service{Senders: []notify.Sender{bad, good}, Clock: func() time.Time { return t0 }}
	err := s.Publish(ctx, "license.grace", "error", "L1", "in grace")
	if err == nil || len(good.got) != 1 {
		t.Fatalf("%v %d", err, len(good.got))
	}
	if n := good.got[0]; n.Event != "license.grace" || n.Level != "error" || n.Subject != "L1" || n.Text != "in grace" || !n.Time.Equal(t0) {
		t.Errorf("%+v", n)
	}
}
