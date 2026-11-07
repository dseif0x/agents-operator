package convo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// Input sources.
const (
	SourceOwner   = "owner"   // a message from the owner, queued in the hub
	SourceChat    = "chat"    // chat messages for the agent
	SourceRoutine = "routine" // a routine run, queued in the hub
)

// Pending is an input waiting for the agent.
type Pending struct {
	Input
	Source string
	// Claim is set for inputs queued in the hub: the session asks the hub
	// before sending them, so an edit or cancel there wins.
	Claim bool
	// OnSent runs once the agent took the input.
	OnSent func()
}

// Session runs one conversation: it keeps the CLI running (restarting it
// when it exits), hands it queued inputs one turn at a time, and passes its
// events on to the hub.
type Session struct {
	// New makes a driver for each (re)start, on model ("" = the default).
	New func(model string) Driver
	// StateFile keeps the CLI's conversation id across restarts.
	StateFile string
	// Send passes a batch of events to the hub; an error keeps them for the
	// next try.
	Send func(ctx context.Context, events []Event) error
	// Adopt finds an earlier CLI conversation to continue (a session that
	// was switched from the terminal) and its history; called once, before
	// the first start without a conversation id.
	Adopt func() (ref string, history []Event)
	// ClaimInput asks the hub whether a queued input may be sent now.
	ClaimInput func(ctx context.Context, id string) (bool, error)
	Log        *slog.Logger

	mu       sync.Mutex
	driver   Driver
	running  bool
	queue    []Pending
	known    map[string]bool // ids queued or sent while this runner lives
	turn     string
	turnSent []func()
	activity runner.Activity
	text     map[string]*strings.Builder // streamed text per item of the turn
	lastText string
	tools    map[string]string // running tools of the turn -> title
	outbox   []Event
	wake     chan struct{}
	flush    chan struct{}
	startErr string
	prefix   string // makes turn ids unique across CLI restarts
	lastAt   time.Time
	current  Pending           // the input of the running (or last) turn
	requests map[string]string // waiting for the owner: id -> title
	memories []Note
	memVer   string // of memories
	memSent  string // the version the agent has
}

// Note is one of the agent's memories.
type Note struct {
	ID   string
	Text string
}

// SetMemories replaces the agent's memories; the next input carries them.
func (s *Session) SetMemories(notes []Note) {
	var b strings.Builder
	for _, n := range notes {
		b.WriteString(n.ID + "\x00" + n.Text + "\x00")
	}
	s.mu.Lock()
	s.memories = notes
	s.memVer = b.String()
	s.mu.Unlock()
}

// memoryBlock is the memories to put before an input, "" when the agent
// has them already.
func (s *Session) memoryBlock() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memVer == s.memSent {
		return ""
	}
	s.memSent = s.memVer
	var b strings.Builder
	if len(s.memories) == 0 {
		return "[agents-operator memories] You have no memories saved now (all were forgotten).\n\n"
	}
	b.WriteString("[agents-operator memories] What you keep across conversations (memory_save adds one, memory_forget removes one by its id):\n")
	for _, n := range s.memories {
		id := n.ID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Fprintf(&b, "- (%s) %s\n", id, strings.ReplaceAll(n.Text, "\n", " "))
	}
	b.WriteString("\n")
	return b.String()
}

// Respond answers a request of the agent.
func (s *Session) Respond(id string, r Response) error {
	s.mu.Lock()
	d := s.driver
	s.mu.Unlock()
	if d == nil {
		return ErrNotRunning
	}
	return d.Respond(id, r)
}

// SetModel switches the agent's model and keeps it for restarts.
func (s *Session) SetModel(model string) error {
	s.mu.Lock()
	d := s.driver
	s.mu.Unlock()
	if d == nil {
		return ErrNotRunning
	}
	if err := d.SetModel(model); err != nil {
		return err
	}
	st := s.load()
	st.Model = model
	s.save(st)
	return nil
}

// Current is the input the agent works on, and whether a turn runs.
func (s *Session) Current() (Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.turn != ""
}

// LastActive is when the conversation last did something: now while a
// turn runs (so idle stops never cut one off), else the last event.
func (s *Session) LastActive() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != "" {
		return time.Now()
	}
	return s.lastAt
}

func (s *Session) init() {
	if s.wake == nil {
		s.known = map[string]bool{}
		s.text = map[string]*strings.Builder{}
		s.tools = map[string]string{}
		s.wake = make(chan struct{}, 1)
		s.flush = make(chan struct{}, 1)
		s.activity = runner.Activity{State: runner.ActivityExited, Since: time.Now()}
	}
}

func (s *Session) kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Enqueue adds inputs; ids already known are ignored.
func (s *Session) Enqueue(ps ...Pending) {
	s.mu.Lock()
	s.init()
	added := false
	for _, p := range ps {
		if s.known[p.ID] {
			continue
		}
		s.known[p.ID] = true
		s.queue = append(s.queue, p)
		added = true
	}
	s.mu.Unlock()
	if added {
		s.Log.Debug("inputs queued", "count", len(ps))
		s.kick(s.wake)
	}
}

// Cancel drops queued inputs that were not sent yet.
func (s *Session) Cancel(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	kept := s.queue[:0]
	for _, p := range s.queue {
		if !drop[p.ID] {
			kept = append(kept, p)
		}
	}
	s.queue = kept
}

// Interrupt stops the running turn.
func (s *Session) Interrupt() error {
	s.mu.Lock()
	d, turn := s.driver, s.turn
	s.mu.Unlock()
	if d == nil || turn == "" {
		return nil
	}
	return d.Interrupt()
}

// Busy reports whether a turn runs.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn != ""
}

// Activity is what the agent is doing, for the runner's /status.
func (s *Session) Activity() *runner.Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	a := s.activity
	return &a
}

// Queued is how many inputs wait.
func (s *Session) Queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *Session) setActivity(state, detail, message string) {
	if s.activity.State != state || s.activity.Detail != detail {
		s.activity = runner.Activity{State: state, Detail: detail, Message: message, Since: time.Now()}
	}
}

// Run keeps the CLI running and feeds it until ctx ends.
func (s *Session) Run(ctx context.Context) {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	go s.sender(ctx)
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.Log.Warn("agent CLI stopped", "err", err)
		}
		if time.Since(started) > time.Minute {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (s *Session) runOnce(ctx context.Context) error {
	st := s.load()
	if st.Ref == "" && !st.Adopted {
		if s.Adopt != nil {
			if ref, history := s.Adopt(); ref != "" {
				s.Log.Info("continuing the earlier CLI conversation", "ref", ref, "items", len(history))
				st.Ref = ref
				for _, ev := range history {
					s.push(ev)
				}
			}
		}
		st.Adopted = true
		s.save(st)
	}
	d := s.New(st.Model)
	if err := d.Start(ctx, st.Ref); err != nil {
		s.mu.Lock()
		s.startErr = err.Error()
		s.setActivity(runner.ActivityError, "the agent CLI does not start: "+err.Error(), "")
		s.mu.Unlock()
		s.push(Event{Type: EventInfo, Info: &Info{}, Error: "the agent CLI does not start: " + err.Error()})
		return err
	}
	s.mu.Lock()
	s.memSent = "" // a new CLI process gets the memories again
	s.prefix = randKey() + "-"
	s.driver, s.running = d, true
	hadErr := s.startErr != ""
	s.setActivity(runner.ActivityWaitingInput, "", s.lastText)
	s.mu.Unlock()
	// An earlier start error is cleared once the CLI stays up a while, so a
	// CLI that dies right away keeps showing why.
	upTimer := time.AfterFunc(10*time.Second, func() {
		s.mu.Lock()
		s.startErr = ""
		s.mu.Unlock()
		if hadErr {
			s.push(Event{Type: EventInfo, Info: &Info{}})
		}
	})
	defer upTimer.Stop()
	started := time.Now()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range d.Events() {
			s.handle(ev)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			d.Close()
			<-done
			return ctx.Err()
		case <-done:
			err := d.Wait()
			s.mu.Lock()
			s.driver, s.running, s.turn = nil, false, ""
			s.setActivity(runner.ActivityExited, "", "")
			if time.Since(started) < 10*time.Second {
				msg := "the agent CLI stopped right after starting"
				if err != nil {
					msg += ": " + err.Error()
				}
				s.startErr = msg
				s.setActivity(runner.ActivityError, msg, "")
			}
			startErr := s.startErr
			s.mu.Unlock()
			if startErr != "" {
				s.push(Event{Type: EventInfo, Info: &Info{}, Error: startErr})
			}
			return err
		case <-s.wake:
			s.dispatch(ctx, d)
		case <-time.After(2 * time.Second):
			s.dispatch(ctx, d)
		}
	}
}

// dispatch sends the next input when the agent is idle.
func (s *Session) dispatch(ctx context.Context, d Driver) {
	for {
		s.mu.Lock()
		if s.turn != "" || len(s.queue) == 0 {
			s.mu.Unlock()
			return
		}
		p := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		if p.Claim && s.ClaimInput != nil {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			ok, err := s.ClaimInput(cctx, p.ID)
			cancel()
			if err != nil {
				s.Log.Debug("cannot claim an input", "id", p.ID, "err", err)
				// No answer from the hub: try again later.
				s.mu.Lock()
				s.queue = append([]Pending{p}, s.queue...)
				s.mu.Unlock()
				return
			}
			if !ok {
				s.Log.Debug("input no longer queued in the hub", "id", p.ID)
				continue // edited or cancelled meanwhile
			}
		}
		if p.Source == SourceChat {
			// The hub has no message for it yet: show what the agent got.
			s.push(Event{Type: EventItem, Item: &Item{Key: "input:" + p.ID, Kind: KindUser, Status: StatusDone, Text: p.Text, Source: p.Source}})
		}
		if block := s.memoryBlock(); block != "" {
			p.Text = block + p.Text
		}
		s.mu.Lock()
		s.turn = "pending"
		s.current = p
		if p.OnSent != nil {
			s.turnSent = append(s.turnSent, p.OnSent)
		}
		s.mu.Unlock()
		if err := d.Send(p.Input); err != nil {
			s.Log.Warn("cannot send an input to the agent", "err", err)
			s.mu.Lock()
			if s.turn == "pending" {
				s.turn = ""
			}
			s.mu.Unlock()
		}
		return
	}
}

// handle keeps track of the turn and forwards the event.
func (s *Session) handle(ev Event) {
	s.mu.Lock()
	s.lastAt = time.Now()
	if ev.Turn != "" {
		ev.Turn = s.prefix + ev.Turn
	}
	switch ev.Type {
	case EventTurnStarted:
		s.turn = ev.Turn
		s.text = map[string]*strings.Builder{}
		s.tools = map[string]string{}
		s.setActivity(runner.ActivityThinking, "", "")
		sent := s.turnSent
		s.turnSent = nil
		defer func() {
			for _, fn := range sent {
				go fn()
			}
		}()
	case EventItem:
		it := ev.Item
		switch it.Kind {
		case KindText, KindThinking:
			b := s.text[it.Key]
			if b == nil {
				b = &strings.Builder{}
				s.text[it.Key] = b
			}
			if it.Delta {
				b.WriteString(it.Text)
			} else if it.Text != "" {
				b.Reset()
				b.WriteString(it.Text)
			}
			// A finished item always carries its whole text, so the hub
			// never depends on every delta arriving.
			if it.Status != StatusRunning {
				cp := *it
				cp.Text, cp.Delta = b.String(), false
				ev.Item = &cp
				if it.Kind == KindText && it.Parent == "" {
					s.lastText = cp.Text
				}
			}
		case KindTool:
			if it.Status == StatusRunning {
				title := ""
				if it.Tool != nil {
					title = firstNonEmpty(it.Tool.Title, it.Tool.Name)
				}
				if title != "" || s.tools[it.Key] == "" {
					s.tools[it.Key] = title
				}
			} else {
				delete(s.tools, it.Key)
			}
		}
		if s.turn != "" && len(s.requests) == 0 {
			if len(s.tools) > 0 {
				detail := ""
				for _, t := range s.tools {
					detail = t
				}
				s.setActivity(runner.ActivityTool, detail, "")
			} else {
				s.setActivity(runner.ActivityThinking, "", "")
			}
		}
	case EventTurnEnded:
		s.turn = ""
		s.requests = nil
		switch ev.Status {
		case StatusFailed:
			s.setActivity(runner.ActivityError, ev.Error, "")
		default:
			s.setActivity(runner.ActivityWaitingInput, "", s.lastText)
		}
	case EventInfo:
		if ev.Info != nil && ev.Info.Ref != "" {
			s.saveRef(ev.Info.Ref)
		}
	case EventRequest:
		if ev.Request != nil {
			if s.requests == nil {
				s.requests = map[string]string{}
			}
			s.requests[ev.Request.ID] = firstNonEmpty(ev.Request.Title, "Waiting for you")
			s.setActivity(runner.ActivityNeedsPermission, s.requests[ev.Request.ID], "")
		}
	case EventRequestDone:
		if ev.Request != nil {
			delete(s.requests, ev.Request.ID)
			if len(s.requests) == 0 && s.turn != "" {
				s.setActivity(runner.ActivityThinking, "", "")
			}
		}
	}
	s.mu.Unlock()
	if ev.Type == EventTurnEnded {
		s.kick(s.wake)
	}
	s.push(ev)
}

// push queues an event for the hub.
func (s *Session) push(ev Event) {
	s.mu.Lock()
	s.outbox = append(s.outbox, ev)
	// Without the hub for a long time: drop streamed deltas first, the
	// finished items carry the whole text.
	if len(s.outbox) > 20000 {
		kept := s.outbox[:0]
		for _, e := range s.outbox {
			if e.Type == EventItem && e.Item != nil && e.Item.Delta {
				continue
			}
			kept = append(kept, e)
		}
		s.outbox = kept
		if len(s.outbox) > 20000 {
			s.outbox = s.outbox[len(s.outbox)-20000:]
		}
	}
	s.mu.Unlock()
	s.kick(s.flush)
}

// sender batches events to the hub: at most every 100 ms, retrying while
// the hub is not connected.
func (s *Session) sender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.flush:
		case <-time.After(time.Second):
		}
		time.Sleep(100 * time.Millisecond)
		for {
			s.mu.Lock()
			n := min(len(s.outbox), 500)
			batch := append([]Event(nil), s.outbox[:n]...)
			s.mu.Unlock()
			if n == 0 || s.Send == nil {
				break
			}
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.Send(sctx, batch)
			cancel()
			if err != nil {
				break // the next tick tries again
			}
			s.mu.Lock()
			s.outbox = s.outbox[n:]
			s.mu.Unlock()
		}
	}
}

type state struct {
	Ref   string `json:"ref"`
	Model string `json:"model,omitempty"`
	// Adopted is set once an earlier CLI conversation was looked for.
	Adopted bool `json:"adopted,omitempty"`
}

func (s *Session) load() state {
	var st state
	if s.StateFile == "" {
		return st
	}
	if b, err := os.ReadFile(s.StateFile); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (s *Session) save(st state) {
	if s.StateFile == "" {
		return
	}
	b, _ := json.Marshal(st)
	_ = os.MkdirAll(filepath.Dir(s.StateFile), 0o700)
	tmp := s.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.StateFile)
	}
}

func (s *Session) loadRef() string { return s.load().Ref }

func (s *Session) saveRef(ref string) {
	st := s.load()
	if st.Ref == ref {
		return
	}
	st.Ref = ref
	s.save(st)
}
