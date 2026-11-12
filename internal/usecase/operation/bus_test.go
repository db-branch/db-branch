package operation_test

import (
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
