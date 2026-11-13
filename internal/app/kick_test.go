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
	time.Sleep(20 * time.Millisecond) // let it subscribe
	bus.Publish("operation.updated", nil)
	waitKick(t, kick, false)
	bus.Publish("branch.updated", nil)
	waitKick(t, kick, true)
	// a burst while nobody reads leaves at most one pending kick
	for i := 0; i < 50; i++ {
		bus.Publish("branch.deleted", nil)
	}
	time.Sleep(50 * time.Millisecond)
	if len(kick) != 1 {
		t.Fatalf("pending kicks = %d, want 1", len(kick))
	}
	waitKick(t, kick, true)
	waitKick(t, kick, false)
}

func TestBranchKicksAfterBeingDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := operation.NewBus(10)
	kick := branchKicks(ctx, bus)
	time.Sleep(20 * time.Millisecond)
	// overflow the subscription buffer (256) so the bus drops it
	for i := 0; i < 400; i++ {
		bus.Publish("node.updated", nil)
	}
	waitKick(t, kick, true) // renewed with a kick: events may have been missed
	time.Sleep(20 * time.Millisecond)
	bus.Publish("branch.updated", nil)
	waitKick(t, kick, true)
}
