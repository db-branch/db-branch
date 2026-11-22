package operation

import "sync"

// Audience says who may receive an event. An event without one goes to
// admins only, so a publisher that forgets to name the audience leaks nothing.
// Admins receive every event.
type Audience struct {
	// Action: anyone allowed this action (e.g. "setting.read").
	Action string
	// Owner: the principal with this ID and whoever shares its branches.
	Owner string
	// Open: every signed-in principal.
	Open bool
}

// Event is one change notification. IDs increase by one per event.
type Event struct {
	ID       uint64
	Type     string // e.g. "branch.updated", "operation.updated"
	Data     any
	Audience Audience
}

// Bus fans events out to subscribers and keeps the most recent ones so a
// reconnecting client can catch up from its Last-Event-ID.
type Bus struct {
	mu     sync.Mutex
	nextID uint64
	ring   []Event
	size   int
	subs   map[*Subscription]struct{}
}

// NewBus keeps the last size events for replay.
func NewBus(size int) *Bus {
	return &Bus{nextID: 1, size: size, subs: map[*Subscription]struct{}{}}
}

// Publish sends an event to every subscriber. It never blocks: a subscriber
// whose buffer is full is dropped and must resubscribe.
func (b *Bus) Publish(typ string, data any) {
	b.PublishTo(Audience{}, typ, data)
}

// PublishTo is Publish for an event only the given audience (and admins) may receive.
func (b *Bus) PublishTo(aud Audience, typ string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ev := Event{ID: b.nextID, Type: typ, Data: data, Audience: aud}
	b.nextID++
	b.ring = append(b.ring, ev)
	if len(b.ring) > b.size {
		b.ring = b.ring[len(b.ring)-b.size:]
	}
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			s.dropped = true
			b.remove(s)
		}
	}
}

// Subscription receives events published after Subscribe, preceded by any
// retained events newer than lastID.
type Subscription struct {
	bus     *Bus
	ch      chan Event
	dropped bool
	// Resync is true when events after lastID were already discarded; the
	// client should reload full state.
	Resync bool
}

// Subscribe starts a subscription. lastID 0 means "only new events".
func (b *Bus) Subscribe(lastID uint64) *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &Subscription{bus: b, ch: make(chan Event, 256)}
	if lastID > 0 {
		if len(b.ring) > 0 && b.ring[0].ID > lastID+1 {
			s.Resync = true
		}
		var missed []Event
		for _, ev := range b.ring {
			if ev.ID > lastID {
				missed = append(missed, ev)
			}
		}
		if len(missed) > cap(s.ch) {
			// More than the buffer holds: filling it would block here, with the
			// lock held, and stop the whole bus. Have the client reload instead.
			s.Resync = true
			missed = nil
		}
		for _, ev := range missed {
			s.ch <- ev
		}
	}
	b.subs[s] = struct{}{}
	return s
}

// Events returns the delivery channel; it is closed when the subscription ends.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped reports whether the bus gave up on this subscriber for reading too slowly.
func (s *Subscription) Dropped() bool {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.dropped
}

// Close ends the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.bus.remove(s)
}

func (b *Bus) remove(s *Subscription) {
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.ch)
	}
}
