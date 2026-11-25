package convo

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// steerDriver records steered inputs; noSteer makes it refuse (like ACP).
type steerDriver struct {
	steered []Input
	noSteer bool
}

func (d *steerDriver) Start(context.Context, string) error { return nil }
func (d *steerDriver) Send(Input) error                    { return nil }
func (d *steerDriver) Interrupt() error                    { return nil }
func (d *steerDriver) Respond(string, Response) error      { return nil }
func (d *steerDriver) SetModel(string) error               { return nil }
func (d *steerDriver) Events() <-chan Event                { return nil }
func (d *steerDriver) Wait() error                         { return nil }
func (d *steerDriver) Close()                              {}
func (d *steerDriver) Steer(in Input) error {
	if d.noSteer {
		return ErrNoSteer
	}
	d.steered = append(d.steered, in)
	return nil
}

func deliverSession(d Driver, turn string) *Session {
	s := &Session{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.mu.Lock()
	s.init()
	s.driver, s.turn = d, turn
	s.mu.Unlock()
	return s
}

func TestDeliverSteersIntoRunningTurn(t *testing.T) {
	d := &steerDriver{}
	s := deliverSession(d, "t1")
	sent := make(chan struct{}, 1)
	s.Deliver(Pending{Input: Input{ID: "chat-1", Text: "hi"}, Source: SourceChat, OnSent: func() { sent <- struct{}{} }})
	if len(d.steered) != 1 || d.steered[0].Text != "hi" {
		t.Fatalf("steered = %+v, want the chat message", d.steered)
	}
	<-sent
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) != 0 {
		t.Fatalf("queue = %d, want empty", len(s.queue))
	}
	if len(s.outbox) == 0 || s.outbox[len(s.outbox)-1].Item == nil || s.outbox[len(s.outbox)-1].Item.Source != SourceChat {
		t.Fatalf("no chat user item pushed: %+v", s.outbox)
	}
	// delivered once only
	s.mu.Unlock()
	s.Deliver(Pending{Input: Input{ID: "chat-1", Text: "hi"}, Source: SourceChat})
	s.mu.Lock()
	if len(d.steered) != 1 {
		t.Fatalf("steered twice")
	}
}

func TestDeliverQueuesWhenIdleOrNoSteer(t *testing.T) {
	for name, tc := range map[string]struct {
		d    *steerDriver
		turn string
	}{
		"idle":     {&steerDriver{}, ""},
		"pending":  {&steerDriver{}, "pending"},
		"no steer": {&steerDriver{noSteer: true}, "t1"},
	} {
		s := deliverSession(tc.d, tc.turn)
		s.Deliver(Pending{Input: Input{ID: "chat-2", Text: "x"}, Source: SourceChat})
		s.mu.Lock()
		q := len(s.queue)
		s.mu.Unlock()
		if q != 1 || len(tc.d.steered) != 0 {
			t.Errorf("%s: queue=%d steered=%d, want queued", name, q, len(tc.d.steered))
		}
	}
}

func TestDeliverKeepsOrderBehindQueuedInputs(t *testing.T) {
	d := &steerDriver{}
	s := deliverSession(d, "t1")
	s.Enqueue(Pending{Input: Input{ID: "owner-1", Text: "first"}})
	s.Deliver(Pending{Input: Input{ID: "chat-3", Text: "second"}, Source: SourceChat})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(d.steered) != 0 || len(s.queue) != 2 || s.queue[1].ID != "chat-3" {
		t.Fatalf("steered=%d queue=%+v, want both queued in order", len(d.steered), s.queue)
	}
}
