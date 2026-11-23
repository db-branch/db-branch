package app

import (
	"context"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/usecase/operation"
)

func waitKick(t *testing.T, kick <-chan struct{}, want bool) {
	t.Helper()
	select {
	case <-kick:
		if !want {
			t.Fatal("unexpected kick")
		}
	case <-time.After(200 * time.Millisecond):
		if want {
			t.Fatal("no kick")
		}
	}
}

func TestBranchKicksOnBranchEventsOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := operation.NewBus(10)
	kick := branchKicks(ctx, bus)
	bus.Publish("operation.updated", nil)
	waitKick(t, kick, false)
	bus.Publish("branch.updated", nil)
	waitKick(t, kick, true)
	// a burst while nobody reads leaves at most one pending kick
	sub := bus.Subscribe(0)
	for i := 0; i < 50; i++ {
		bus.Publish("branch.deleted", nil)
	}
	burstKick := make(chan struct{}, 1)
	go pumpBranchKicks(ctx, bus, sub, burstKick)
	waitKick(t, burstKick, true)
	waitKick(t, burstKick, false)
}

func TestBranchKicksAfterBeingDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := operation.NewBus(10)
	// Overflow the subscription buffer (256) before the reader starts so the bus
	// deterministically drops it even under -race scheduling.
	sub := bus.Subscribe(0)
	for i := 0; i < 400; i++ {
		bus.Publish("node.updated", nil)
	}
	if !sub.Dropped() {
		t.Fatal("expected subscription to be dropped")
	}
	kick := make(chan struct{}, 1)
	go pumpBranchKicks(ctx, bus, sub, kick)
	waitKick(t, kick, true) // renewed with a kick: events may have been missed
	bus.Publish("branch.updated", nil)
	waitKick(t, kick, true)
}
