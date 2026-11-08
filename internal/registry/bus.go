package registry

import "sync"

// Bus is a tiny in-process fanout for live events. Slow subscribers drop
// messages rather than stalling the daemon.
type Bus struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	closed bool
}

func NewBus() *Bus {
	return &Bus{subs: make(map[chan Event]struct{})}
}

func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 128)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	unsub := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsub
}

func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Dropping is visible in the DB-backed event log.
		}
	}
}

func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}
