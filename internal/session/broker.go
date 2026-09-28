package session

import (
	"sync"
)

// Event is what the SSE endpoint streams to the list page.
type Event struct {
	Type    string `json:"type"` // "session" (changed) or "deleted"
	ID      string `json:"id"`
	Session *View  `json:"session,omitempty"`
}

// Broker fans session events out to SSE subscribers, per owner.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]string
}

// NewBroker returns an empty broker.
func NewBroker() *Broker { return &Broker{subs: map[chan Event]string{}} }

// Subscribe returns a channel of events for ownerID and a cancel func.
func (b *Broker) Subscribe(ownerID string) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = ownerID
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish delivers ev to every subscriber of ownerID. Slow subscribers
// miss events; the list page reloads on reconnect anyway.
func (b *Broker) Publish(ownerID string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, owner := range b.subs {
		if owner != ownerID {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
}
