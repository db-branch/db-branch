package operation_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/operation"
)

var ctx = context.Background()

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func newManager() (*operation.Manager, *operation.Bus) {
	bus := operation.NewBus(100)
	return &operation.Manager{Bus: bus, Clock: func() time.Time { return t0 }}, bus
}

func start(t *testing.T, m *operation.Manager, kind string, target operation.Target, f operation.Func) operation.Operation {
	t.Helper()
	op, err := m.Start(kind, target, f)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func wait(t *testing.T, m *operation.Manager, id string) operation.Operation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, err := m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if op.State == operation.Succeeded || op.State == operation.Failed {
			return op
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return operation.Operation{}
}

func TestStart_裏で実行し段階を記録して成功で終わる(t *testing.T) {
	m, _ := newManager()
	release := make(chan struct{})

	op := start(t, m, "branch.create", operation.Target{Resource: "branch", Name: "feature-x"},
		func(ctx context.Context, report func(string)) error {
			report("cloning")
			<-release
			return nil
		})
	if !strings.HasPrefix(op.ID, "op_") || op.State != operation.Running || op.Kind != "branch.create" {
		t.Fatalf("op = %+v", op)
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, _ := m.Get(op.ID)
		if got.Phase == "cloning" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase not reported: %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	done := wait(t, m, op.ID)
	if done.Phase != "" || done.FinishedAt == nil || done.Error != "" {
		t.Errorf("done = %+v", done)
	}
}

func TestStart_失敗したら理由を残す(t *testing.T) {
	m, _ := newManager()
	op := start(t, m, "branch.reset", operation.Target{Resource: "branch", Name: "x"},
		func(context.Context, func(string)) error { return errors.New("boom") })

	done := wait(t, m, op.ID)
	if done.State != operation.Failed || done.Error != "boom" {
		t.Errorf("done = %+v", done)
	}
}

func TestGet_知らないIDはNotFound(t *testing.T) {
	m, _ := newManager()
	if _, err := m.Get("op_nope"); !errors.Is(err, operation.ErrNotFound) {
		t.Errorf("error = %v", err)
	}
}

func TestActive_対象ごとの実行中の操作を返す(t *testing.T) {
	m, _ := newManager()
	release := make(chan struct{})
	op := start(t, m, "branch.reset", operation.Target{Resource: "branch", Name: "x"},
		func(context.Context, func(string)) error { <-release; return nil })

	got, ok := m.Active("branch", "x")
	if !ok || got.ID != op.ID {
		t.Errorf("Active = %+v, %v", got, ok)
	}
	if _, ok := m.Active("branch", "y"); ok {
		t.Error("no active op for y")
	}
	close(release)
	wait(t, m, op.ID)
	if _, ok := m.Active("branch", "x"); ok {
		t.Error("finished op should not be active")
	}
}

func TestList_新しい順に返す(t *testing.T) {
	m, _ := newManager()
	a := start(t, m, "a", operation.Target{}, func(context.Context, func(string)) error { return nil })
	b := start(t, m, "b", operation.Target{}, func(context.Context, func(string)) error { return nil })
	wait(t, m, a.ID)
	wait(t, m, b.ID)

	list := m.List()
	if len(list) != 2 || list[0].ID != b.ID {
		t.Errorf("List = %+v", list)
	}
}

func TestStart_変化をイベントとして流す(t *testing.T) {
	m, bus := newManager()
	sub := bus.Subscribe(0)
	defer sub.Close()

	op := start(t, m, "x", operation.Target{}, func(_ context.Context, report func(string)) error {
		report("one")
		return nil
	})
	wait(t, m, op.ID)

	var states []string
	timeout := time.After(time.Second)
	for len(states) < 3 {
		select {
		case ev := <-sub.Events():
			if ev.Type != "operation.updated" {
				t.Fatalf("event %+v", ev)
			}
			o := ev.Data.(operation.Operation)
			states = append(states, string(o.State)+":"+o.Phase)
		case <-timeout:
			t.Fatalf("events = %v", states)
		}
	}
	if strings.Join(states, ",") != "running:,running:one,succeeded:" {
		t.Errorf("states = %v", states)
	}
}

type failingStore struct{ operation.MemoryStore }

func (*failingStore) PutOperation(context.Context, operation.Operation) error {
	return errors.New("no leader")
}

func TestStart_記録できなければ実行しない(t *testing.T) {
	m := &operation.Manager{Store: &failingStore{}, Clock: func() time.Time { return t0 }}
	ran := make(chan struct{}, 1)
	if _, err := m.Start("x", operation.Target{}, func(context.Context, func(string)) error { ran <- struct{}{}; return nil }); err == nil {
		t.Fatal("want error")
	}
	select {
	case <-ran:
		t.Error("task ran without a record")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestStart_実行したノードを記録する(t *testing.T) {
	m, _ := newManager()
	m.Node = "node1"
	op := start(t, m, "x", operation.Target{}, func(context.Context, func(string)) error { return nil })
	if op.Node != "node1" {
		t.Errorf("node = %q", op.Node)
	}
}

func TestRecover_自分が実行中だった操作を中断扱いにする(t *testing.T) {
	store := &operation.MemoryStore{}
	_ = store.PutOperation(ctx, operation.Operation{ID: "mine", Node: "a", State: operation.Running})
	_ = store.PutOperation(ctx, operation.Operation{ID: "other", Node: "b", State: operation.Running})
	_ = store.PutOperation(ctx, operation.Operation{ID: "done", Node: "a", State: operation.Succeeded})
	m := &operation.Manager{Store: store, Node: "a", Clock: func() time.Time { return t0 }}
	if err := m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if op, _ := m.Get("mine"); op.State != operation.Failed || op.Error == "" || op.FinishedAt == nil {
		t.Errorf("mine = %+v", op)
	}
	if op, _ := m.Get("other"); op.State != operation.Running {
		t.Errorf("another node's op changed: %+v", op)
	}
	if op, _ := m.Get("done"); op.State != operation.Succeeded {
		t.Errorf("finished op changed: %+v", op)
	}
}

func TestRecover_一覧を読めなければエラー(t *testing.T) {
	m := &operation.Manager{Store: &brokenList{}, Clock: func() time.Time { return t0 }}
	if err := m.Recover(ctx); err == nil {
		t.Error("want error")
	}
}

type brokenList struct{ operation.MemoryStore }

func (*brokenList) ListOperations(context.Context) ([]operation.Operation, error) {
	return nil, errors.New("broken")
}

func TestMemoryStore_終わった操作は新しいものだけ残す(t *testing.T) {
	s := &operation.MemoryStore{}
	_ = s.PutOperation(ctx, operation.Operation{ID: "running", State: operation.Running, Seq: 0})
	for i := 1; i <= operation.Keep+5; i++ {
		_ = s.PutOperation(ctx, operation.Operation{ID: fmt.Sprint("op", i), State: operation.Succeeded, Seq: uint64(i)})
	}
	list, _ := s.ListOperations(ctx)
	if len(list) != operation.Keep+1 {
		t.Errorf("kept %d", len(list))
	}
	if _, err := s.GetOperation(ctx, "op1"); !errors.Is(err, operation.ErrNotFound) {
		t.Error("oldest finished op should be pruned")
	}
	if _, err := s.GetOperation(ctx, "running"); err != nil {
		t.Error("running op must be kept")
	}
}

func TestList_同じ時刻に始めても開始順が保たれる(t *testing.T) {
	m, _ := newManager()
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, start(t, m, "x", operation.Target{}, func(context.Context, func(string)) error { return nil }).ID)
	}
	list := m.List()
	for i, op := range list {
		if op.ID != ids[len(ids)-1-i] {
			t.Fatalf("order = %v", list)
		}
	}
}
