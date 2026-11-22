package operation_test

import (
	"context"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/operation"
)

func next(t *testing.T, s *operation.Subscription) operation.Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("channel closed")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	return operation.Event{}
}

func TestPublish_番号を振って購読者全員に届ける(t *testing.T) {
	bus := operation.NewBus(10)
	a, b := bus.Subscribe(0), bus.Subscribe(0)
	defer a.Close()
	defer b.Close()

	bus.Publish("branch.updated", "x")
	bus.Publish("branch.deleted", "y")

	for _, s := range []*operation.Subscription{a, b} {
		e1, e2 := next(t, s), next(t, s)
		if e1.ID != 1 || e1.Type != "branch.updated" || e2.ID != 2 || e2.Data != "y" {
			t.Errorf("events %+v %+v", e1, e2)
		}
	}
}

func TestSubscribe_続きの番号から取りこぼしを再送する(t *testing.T) {
	bus := operation.NewBus(10)
	for i := 0; i < 3; i++ {
		bus.Publish("x", i)
	}
	s := bus.Subscribe(1)
	defer s.Close()

	if e := next(t, s); e.ID != 2 {
		t.Errorf("first replayed = %+v", e)
	}
	if e := next(t, s); e.ID != 3 {
		t.Errorf("second replayed = %+v", e)
	}
	if s.Resync {
		t.Error("no resync needed")
	}
}

func TestSubscribe_古すぎて再送できなければResyncを知らせる(t *testing.T) {
	bus := operation.NewBus(2)
	for i := 0; i < 5; i++ {
		bus.Publish("x", i)
	}
	s := bus.Subscribe(1)
	defer s.Close()
	if !s.Resync {
		t.Error("want resync")
	}
}

func TestPublish_読まない購読者がいても止まらない(t *testing.T) {
	bus := operation.NewBus(10)
	slow := bus.Subscribe(0)
	defer slow.Close()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			bus.Publish("x", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow subscriber")
	}
	if !slow.Dropped() {
		t.Error("slow subscriber should be marked dropped")
	}
}

func TestClose_購読をやめたら届かない(t *testing.T) {
	bus := operation.NewBus(10)
	s := bus.Subscribe(0)
	s.Close()
	s.Close() // idempotent
	bus.Publish("x", 1)
	if _, ok := <-s.Events(); ok {
		t.Error("channel should be closed")
	}
}

func TestPublishTo_宛先を付けて届け再送でも保つ(t *testing.T) {
	bus := operation.NewBus(10)
	bus.Publish("setting.updated", "x") // no audience: admins only
	bus.PublishTo(operation.Audience{Owner: "token:a", Open: true, Action: "base.read"}, "branch.updated", "y")

	s := bus.Subscribe(1)
	defer s.Close()
	ev := next(t, s)
	if ev.Audience != (operation.Audience{Owner: "token:a", Open: true, Action: "base.read"}) {
		t.Errorf("audience = %+v", ev.Audience)
	}
}

func TestStart_操作の更新は開始した人宛てに届く(t *testing.T) {
	m, bus := newManager()
	s := bus.Subscribe(0)
	defer s.Close()
	op, err := m.StartAs("token:a", "branch.create", operation.Target{Resource: "branch", Name: "x"},
		func(context.Context, func(string)) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ev := next(t, s)
	if ev.Audience.Owner != "token:a" || op.Owner != "token:a" {
		t.Errorf("event %+v op %+v", ev, op)
	}
}

func TestSubscribe_再送が購読の余裕を超えてもバスを止めずResyncを知らせる(t *testing.T) {
	bus := operation.NewBus(1000)
	for i := 0; i < 500; i++ {
		bus.Publish("x", i)
	}
	done := make(chan *operation.Subscription)
	go func() { done <- bus.Subscribe(1) }()
	select {
	case s := <-done:
		defer s.Close()
		if !s.Resync {
			t.Error("a replay too long to deliver must ask the client to reload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe blocked on a replay larger than the subscriber's buffer")
	}
	published := make(chan struct{})
	go func() { bus.Publish("y", 0); close(published) }()
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("the bus is stuck")
	}
}
