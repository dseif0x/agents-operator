package convo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// hubFake stores events like the hub, refusing (with a hub answer) every
// batch that contains a poison event or, while down, every batch.
type hubFake struct {
	mu     sync.Mutex
	got    []string
	calls  int
	down   bool
	noLink bool
}

func (h *hubFake) send(_ context.Context, evs []Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.noLink {
		return errors.New("no uplink")
	}
	if h.down {
		return &runner.HubError{Msg: "the hub could not do that: internal error"}
	}
	for _, e := range evs {
		if e.Error == "poison" {
			return &runner.HubError{Msg: "the hub could not do that: internal error"}
		}
	}
	for _, e := range evs {
		h.got = append(h.got, e.Turn)
	}
	return nil
}

func (h *hubFake) snapshot() ([]string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.got...), h.calls
}

func newSenderSession(h *hubFake, events []Event) (*Session, context.CancelFunc) {
	s := &Session{Send: h.send}
	s.mu.Lock()
	s.init()
	s.outbox = append(s.outbox, events...)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	go s.sender(ctx)
	s.kick(s.flush)
	return s, cancel
}

func events(n, poison int) []Event {
	var evs []Event
	for i := 0; i < n; i++ {
		e := Event{Type: EventItem, Turn: string(rune('a' + i%26)) + string(rune('0'+i/26))}
		if i == poison {
			e.Error = "poison"
		}
		evs = append(evs, e)
	}
	return evs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSenderDropsOnlyThePoisonEvent(t *testing.T) {
	h := &hubFake{}
	in := events(40, 17)
	s, cancel := newSenderSession(h, in)
	defer cancel()
	waitFor(t, "all good events", func() bool { got, _ := h.snapshot(); return len(got) == 39 })
	got, calls := h.snapshot()
	var want []string
	for i, e := range in {
		if i != 17 {
			want = append(want, e.Turn)
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v", got, want)
		}
	}
	if calls > 20 {
		t.Errorf("%d sends for one poison event", calls)
	}
	s.mu.Lock()
	left := len(s.outbox)
	s.mu.Unlock()
	if left != 0 {
		t.Errorf("%d events left in the outbox", left)
	}
}

func TestSenderKeepsEventsWhileTheHubFails(t *testing.T) {
	for name, h := range map[string]*hubFake{"hub error": {down: true}, "no uplink": {noLink: true}} {
		t.Run(name, func(t *testing.T) {
			s, cancel := newSenderSession(h, events(10, -1))
			defer cancel()
			waitFor(t, "a few attempts", func() bool { _, c := h.snapshot(); return c >= 3 })
			s.mu.Lock()
			left := len(s.outbox)
			s.mu.Unlock()
			if left != 10 {
				t.Fatalf("outbox has %d events, want all 10 kept", left)
			}
			h.mu.Lock()
			h.down, h.noLink = false, false
			h.mu.Unlock()
			s.kick(s.flush)
			waitFor(t, "delivery after recovery", func() bool { got, _ := h.snapshot(); return len(got) == 10 })
		})
	}
}

// A refused event at the end of the outbox stays until a later event shows
// the hub works; then it is dropped and the later event delivered.
func TestSenderPoisonLastWaitsForProof(t *testing.T) {
	h := &hubFake{}
	s, cancel := newSenderSession(h, events(5, 4))
	defer cancel()
	waitFor(t, "the good events", func() bool { got, _ := h.snapshot(); return len(got) == 4 })
	time.Sleep(300 * time.Millisecond)
	s.mu.Lock()
	left := len(s.outbox)
	s.mu.Unlock()
	if left != 1 {
		t.Fatalf("outbox has %d events, want the refused one kept", left)
	}
	s.push(Event{Type: EventItem, Turn: "late"})
	waitFor(t, "the later event", func() bool { got, _ := h.snapshot(); return len(got) == 5 && got[4] == "late" })
}
