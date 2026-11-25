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
	s.Deliver(context.Background(), Pending{Input: Input{ID: "chat-1", Text: "hi"}, Source: SourceChat, OnSent: func() { sent <- struct{}{} }})
	if len(d.steered) != 1 || d.steered[0].Text != "hi" {
		t.Fatalf("steered = %+v, want the chat message", d.steered)
	}
	<-sent
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) != 0 {
		t.Fatalf("queue = %d, want empty", len(s.queue))
	}
	item := false
	for _, ev := range s.outbox {
		if ev.Item != nil && ev.Item.Source == SourceChat && ev.Item.Key == "input:chat-1" {
			item = true
		}
	}
	if !item {
		t.Fatalf("no chat user item pushed: %+v", s.outbox)
	}
	// delivered once only
	s.mu.Unlock()
	s.Deliver(context.Background(), Pending{Input: Input{ID: "chat-1", Text: "hi"}, Source: SourceChat})
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
		s.Deliver(context.Background(), Pending{Input: Input{ID: "chat-2", Text: "x"}, Source: SourceChat})
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
	s.Deliver(context.Background(), Pending{Input: Input{ID: "chat-3", Text: "second"}, Source: SourceChat})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(d.steered) != 0 || len(s.queue) != 2 || s.queue[1].ID != "chat-3" {
		t.Fatalf("steered=%d queue=%+v, want both queued in order", len(d.steered), s.queue)
	}
}

func TestDeliverClaimsHubInputsAndReportsTheTurn(t *testing.T) {
	d := &steerDriver{}
	s := deliverSession(d, "t1")
	claimed := map[string]bool{}
	s.ClaimInput = func(_ context.Context, id string) (bool, error) {
		claimed[id] = true
		return id != "gone", nil
	}
	s.Deliver(context.Background(),
		Pending{Input: Input{ID: "owner-1", Text: "do this too"}, Source: SourceOwner, Claim: true},
		Pending{Input: Input{ID: "gone", Text: "cancelled meanwhile"}, Source: SourceOwner, Claim: true},
		Pending{Input: Input{ID: "rt-1", Text: "webhook"}, Source: SourceRoutine, Claim: true},
	)
	if !claimed["owner-1"] || !claimed["gone"] || !claimed["rt-1"] {
		t.Fatalf("claimed = %v", claimed)
	}
	if len(d.steered) != 2 || d.steered[0].ID != "owner-1" || d.steered[1].ID != "rt-1" {
		t.Fatalf("steered = %+v, want owner-1 and rt-1 (not the cancelled one)", d.steered)
	}
	s.mu.Lock()
	var joined []string
	for _, ev := range s.outbox {
		if ev.Type == EventInputsJoined && ev.Turn == "t1" {
			joined = append(joined, ev.Inputs...)
		}
	}
	s.mu.Unlock()
	if len(joined) != 2 {
		t.Fatalf("inputs_joined = %v, want owner-1 and rt-1", joined)
	}
	if rt, ok := s.TurnRoutine(); !ok || rt.ID != "rt-1" {
		t.Fatalf("TurnRoutine = %v %v, want the steered routine run", rt.ID, ok)
	}
}

func TestDeliverQueuesWhenTheClaimFails(t *testing.T) {
	d := &steerDriver{}
	s := deliverSession(d, "t1")
	s.ClaimInput = func(context.Context, string) (bool, error) { return false, context.DeadlineExceeded }
	s.Deliver(context.Background(), Pending{Input: Input{ID: "owner-2"}, Source: SourceOwner, Claim: true})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(d.steered) != 0 || len(s.queue) != 1 || !s.queue[0].Claim {
		t.Fatalf("steered=%d queue=%+v, want it queued, still to be claimed", len(d.steered), s.queue)
	}
}
