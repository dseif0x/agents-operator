package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/runner/convo"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Conversations (docs/design/conversations.md): the runner drives the
// agent CLI through its protocol and sends what happens as events; the hub
// keeps the conversation as items and queues the owner's messages.

// ConvSummary is a session's conversation summary (sessions.conversation).
type ConvSummary struct {
	Model         string `json:"model,omitempty"`
	Ref           string `json:"ref,omitempty"`
	Context       int64  `json:"context,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	// Turn is the running turn ("" when idle).
	Turn       string     `json:"turn,omitempty"`
	TurnSince  *time.Time `json:"turn_since,omitempty"`
	LastTurnAt *time.Time `json:"last_turn_at,omitempty"`
	// Error is why the agent CLI does not start, if it does not.
	Error string `json:"error,omitempty"`
	// Models are what the owner can switch to.
	Models []convo.Model `json:"models,omitempty"`
	// Totals over the conversation.
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// ItemData is the data of an item, as the UI reads it.
type ItemData struct {
	Text   string         `json:"text,omitempty"`
	Tool   *convo.Tool    `json:"tool,omitempty"`
	Parent string         `json:"parent,omitempty"`
	Source string         `json:"source,omitempty"`
	Input  string         `json:"input,omitempty"` // the input id of a user item
	Files  []InputFile    `json:"files,omitempty"`
	Usage  *convo.Usage   `json:"usage,omitempty"`
	Error  string         `json:"error,omitempty"`
	Extra  map[string]any `json:"extra,omitempty"`
	// Request is what the agent waits for (request items); Response the
	// owner's answer.
	Request  *convo.Request        `json:"request,omitempty"`
	Response *runner.ConvoResponse `json:"response,omitempty"`
}

// InputFile describes a file of a message to the agent.
type InputFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

// Item statuses beyond the driver's.
const (
	ItemQueued  = "queued"
	ItemDeleted = "deleted" // only in events: the item is gone
)

// Limits.
const (
	MaxInputText = 64 << 10
)

// uplinkSender pushes a frame to a session's runner (Uplinks).
type uplinkSender interface {
	Send(sessionID string, f runner.UplinkFrame) bool
}

func (s *Service) convSession(ctx context.Context, ownerID, id string) (*store.Session, error) {
	sess, err := s.Get(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}
	if sess.Interface != store.InterfaceConversation {
		return nil, &ValidationError{"this session runs in a terminal; switch it to a conversation in its settings"}
	}
	return sess, nil
}

func (s *Service) publishItem(ownerID, sessionID string, it *store.ConvItem) {
	if s.Broker != nil {
		s.Broker.Publish(ownerID, Event{Type: "item", ID: sessionID, Item: it})
	}
}

// Items returns a page of a session's conversation.
func (s *Service) Items(ctx context.Context, ownerID, id string, before int64, limit int) ([]*store.ConvItem, error) {
	if _, err := s.Get(ctx, ownerID, id); err != nil {
		return nil, err
	}
	items, err := s.Store.Conversations().ListItems(ctx, id, store.MainThread, before, limit)
	if items == nil {
		items = []*store.ConvItem{}
	}
	return items, err
}

// SendInput queues a message for the agent.
func (s *Service) SendInput(ctx context.Context, owner *store.User, id, text string, files []ChatFile) (*store.ConvItem, error) {
	sess, err := s.convSession(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" && len(files) == 0 {
		return nil, &ValidationError{"an empty message"}
	}
	in, err := s.queueInput(ctx, owner.ID, sess, text, convo.SourceOwner, files, nil)
	if err != nil {
		return nil, err
	}
	return s.Store.Conversations().GetItem(ctx, sess.ID, store.MainThread, "input:"+in.ID)
}

// queueInput stores an input with its item and tells the runner.
func (s *Service) queueInput(ctx context.Context, ownerID string, sess *store.Session, text, source string, files []ChatFile, extra map[string]any) (*store.ConvInput, error) {
	if len(text) > MaxInputText {
		return nil, &ValidationError{fmt.Sprintf("a message is limited to %d KiB", MaxInputText>>10)}
	}
	if len(files) > MaxChatFiles {
		return nil, &ValidationError{fmt.Sprintf("at most %d files per message", MaxChatFiles)}
	}
	in := &store.ConvInput{SessionID: sess.ID, Thread: store.MainThread, Text: text, Source: source}
	for _, f := range files {
		if len(f.Data) == 0 || len(f.Data) > MaxChatFileBytes {
			return nil, &ValidationError{fmt.Sprintf("files are limited to %d MiB (and cannot be empty)", MaxChatFileBytes>>20)}
		}
		ct := http.DetectContentType(f.Data)
		name := CleanFileName(f.Name)
		if name == "" {
			name = "file"
			if ext, ok := ChatImageTypes[ct]; ok {
				name = "image." + ext
			}
		}
		in.Files = append(in.Files, store.ConvFile{OwnerID: ownerID, Name: name, ContentType: ct, Size: len(f.Data), Data: f.Data})
	}
	if err := s.Store.Conversations().AddInput(ctx, in); err != nil {
		return nil, err
	}
	data := ItemData{Text: text, Source: source, Input: in.ID, Extra: extra}
	for _, f := range in.Files {
		data.Files = append(data.Files, InputFile{ID: f.ID, Name: f.Name, ContentType: f.ContentType, Size: f.Size})
	}
	it, err := s.Store.Conversations().UpsertItem(ctx, &store.ConvItem{SessionID: sess.ID, Thread: store.MainThread, Key: "input:" + in.ID, Kind: convo.KindUser, Status: ItemQueued, Data: mustJSON(data)})
	if err != nil {
		return nil, err
	}
	s.publishItem(ownerID, sess.ID, it)
	if s.Uplinks != nil {
		s.Uplinks.Notify(sess.ID)
	}
	return in, nil
}

// EditInput changes a queued message.
func (s *Service) EditInput(ctx context.Context, owner *store.User, id, inputID, text string) (*store.ConvItem, error) {
	sess, err := s.convSession(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if len(text) > MaxInputText {
		return nil, &ValidationError{fmt.Sprintf("a message is limited to %d KiB", MaxInputText>>10)}
	}
	in, err := s.Store.Conversations().UpdateInput(ctx, sess.ID, inputID, text)
	if errors.Is(err, store.ErrConflict) {
		return nil, &ValidationError{"the agent already has this message"}
	}
	if err != nil {
		return nil, err
	}
	if text == "" && len(in.Files) == 0 {
		return nil, &ValidationError{"an empty message"}
	}
	it, err := s.Store.Conversations().GetItem(ctx, sess.ID, store.MainThread, "input:"+inputID)
	if err != nil {
		return nil, err
	}
	var data ItemData
	_ = json.Unmarshal(it.Data, &data)
	data.Text = text
	it.Data = mustJSON(data)
	if it, err = s.Store.Conversations().UpsertItem(ctx, it); err != nil {
		return nil, err
	}
	s.publishItem(owner.ID, sess.ID, it)
	return it, nil
}

// CancelInput drops a queued message.
func (s *Service) CancelInput(ctx context.Context, owner *store.User, id, inputID string) error {
	sess, err := s.convSession(ctx, owner.ID, id)
	if err != nil {
		return err
	}
	ok, err := s.Store.Conversations().SetInputState(ctx, sess.ID, inputID, store.InputCancelled)
	if err != nil {
		return err
	}
	if !ok {
		return &ValidationError{"the agent already has this message"}
	}
	key := "input:" + inputID
	if err := s.Store.Conversations().DeleteItem(ctx, sess.ID, store.MainThread, key); err != nil {
		return err
	}
	s.publishItem(owner.ID, sess.ID, &store.ConvItem{SessionID: sess.ID, Thread: store.MainThread, Key: key, Kind: convo.KindUser, Status: ItemDeleted})
	if us, ok := s.Uplinks.(uplinkSender); ok {
		us.Send(sess.ID, runner.UplinkFrame{Event: runner.EventCancel, IDs: []string{inputID}})
	}
	return nil
}

// Interrupt stops the agent's running turn.
func (s *Service) Interrupt(ctx context.Context, owner *store.User, id string) error {
	sess, err := s.convSession(ctx, owner.ID, id)
	if err != nil {
		return err
	}
	if sess.State != store.StateRunning {
		return ErrInvalidTransition
	}
	us, ok := s.Uplinks.(uplinkSender)
	if !ok || !us.Send(sess.ID, runner.UplinkFrame{Event: runner.EventInterrupt}) {
		return &ValidationError{"the session's agent is not connected yet; try again in a few seconds"}
	}
	return nil
}

// QueuedInputs is what the uplink pushes to a conversation session.
func (s *Service) QueuedInputs(ctx context.Context, sess *store.Session) ([]runner.ConvoInput, error) {
	ins, err := s.Store.Conversations().QueuedInputs(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	out := make([]runner.ConvoInput, 0, len(ins))
	for _, in := range ins {
		ci := runner.ConvoInput{ID: in.ID, Text: in.Text, Source: in.Source}
		for _, f := range in.Files {
			ci.Files = append(ci.Files, runner.ChatFile{ID: f.ID, Name: f.Name, ContentType: f.ContentType, Size: f.Size})
		}
		out = append(out, ci)
	}
	return out, nil
}

// ClaimInput marks a queued input sent, if it still is queued.
func (s *Service) ClaimInput(ctx context.Context, sess *store.Session, inputID string) (bool, error) {
	ok, err := s.Store.Conversations().SetInputState(ctx, sess.ID, inputID, store.InputSent)
	if err != nil || !ok {
		return ok, err
	}
	if it, err := s.Store.Conversations().GetItem(ctx, sess.ID, store.MainThread, "input:"+inputID); err == nil {
		it.Status = convo.StatusDone
		if it, err = s.Store.Conversations().UpsertItem(ctx, it); err == nil {
			s.publishItem(sess.OwnerID, sess.ID, it)
		}
	}
	return true, nil
}

// InputFile returns a file of one of the session's inputs.
func (s *Service) InputFile(ctx context.Context, sess *store.Session, fileID string) (*store.ConvFile, error) {
	return s.Store.Conversations().GetFile(ctx, sess.ID, fileID)
}

// OwnerInputFile returns a file for the owner's browser.
func (s *Service) OwnerInputFile(ctx context.Context, ownerID, id, fileID string) (*store.ConvFile, error) {
	if _, err := s.Get(ctx, ownerID, id); err != nil {
		return nil, err
	}
	return s.Store.Conversations().GetFile(ctx, id, fileID)
}

// ApplyEvents stores what the runner reports.
func (s *Service) ApplyEvents(ctx context.Context, sess *store.Session, events []convo.Event) error {
	var sum ConvSummary
	_ = json.Unmarshal(sess.Conversation, &sum)
	before := sum
	conv := s.Store.Conversations()
	// Items touched by this batch, written once at the end.
	touched := map[string]*store.ConvItem{}
	var order []string
	load := func(key string) (*store.ConvItem, ItemData) {
		it := touched[key]
		if it == nil {
			var err error
			it, err = conv.GetItem(ctx, sess.ID, store.MainThread, key)
			if err != nil {
				it = &store.ConvItem{SessionID: sess.ID, Thread: store.MainThread, Key: key}
			}
			touched[key] = it
			order = append(order, key)
		}
		var d ItemData
		_ = json.Unmarshal(it.Data, &d)
		return it, d
	}
	flush := func() error {
		for _, key := range order {
			it, err := conv.UpsertItem(ctx, touched[key])
			if err != nil {
				return err
			}
			s.publishItem(sess.OwnerID, sess.ID, it)
		}
		touched, order = map[string]*store.ConvItem{}, nil
		return nil
	}
	now := time.Now()
	for _, ev := range events {
		switch ev.Type {
		case convo.EventInfo:
			sum.Error = ev.Error
			if ev.Info != nil {
				if ev.Info.Model != "" {
					sum.Model = ev.Info.Model
				}
				if ev.Info.Ref != "" {
					sum.Ref = ev.Info.Ref
				}
				if ev.Info.ContextWindow > 0 {
					sum.ContextWindow = ev.Info.ContextWindow
				}
				if len(ev.Info.Models) > 0 {
					sum.Models = ev.Info.Models
				}
			}
		case convo.EventTurnStarted:
			// The owner's messages were marked sent when claimed; they get
			// the turn they started (a quiet routine run folds by it).
			sum.Turn, sum.TurnSince = ev.Turn, &now
			s.routineTurnStarted(ctx, ev.Turn, ev.Inputs)
			for _, id := range ev.Inputs {
				key := "input:" + id
				if touched[key] == nil {
					if _, err := conv.GetItem(ctx, sess.ID, store.MainThread, key); err != nil {
						continue // a chat message: its item comes from the runner
					}
				}
				it, _ := load(key)
				it.Turn = ev.Turn
			}
		case convo.EventItem:
			if ev.Item == nil || ev.Item.Key == "" {
				continue
			}
			e := ev.Item
			it, d := load(e.Key)
			if e.Kind != "" {
				it.Kind = e.Kind
			}
			if ev.Turn != "" {
				it.Turn = ev.Turn
			}
			if it.Status != convo.StatusDone && it.Status != convo.StatusFailed || e.Status != convo.StatusRunning {
				it.Status = e.Status
			}
			if e.Delta {
				d.Text += e.Text
			} else if e.Text != "" {
				d.Text = e.Text
			}
			if e.Parent != "" {
				d.Parent = e.Parent
			}
			if e.Source != "" {
				d.Source = e.Source
			}
			if e.Tool != nil {
				d.Tool = mergeTool(d.Tool, e.Tool)
			}
			it.Data = mustJSON(d)
		case convo.EventRequest:
			if ev.Request == nil {
				continue
			}
			it, d := load("request:" + ev.Request.ID)
			it.Turn, it.Kind, it.Status = ev.Turn, "request", "pending"
			d.Request = ev.Request
			it.Data = mustJSON(d)
		case convo.EventRequestDone:
			if ev.Request == nil {
				continue
			}
			key := "request:" + ev.Request.ID
			if touched[key] == nil {
				if _, err := conv.GetItem(ctx, sess.ID, store.MainThread, key); err != nil {
					continue
				}
			}
			it, _ := load(key)
			if it.Status == "pending" {
				it.Status = "cancelled" // moot: the turn ended or another client answered
			}
		case convo.EventTurnEnded:
			if err := flush(); err != nil {
				return err
			}
			// Items the CLI left running end with the turn.
			running, err := conv.RunningItems(ctx, sess.ID, store.MainThread, ev.Turn)
			if err != nil {
				return err
			}
			for _, it := range running {
				st := convo.StatusDone
				if ev.Status != convo.StatusDone {
					st = convo.StatusInterrupted
				}
				it.Status = st
				touched[it.Key], order = it, append(order, it.Key)
			}
			if ev.Status == convo.StatusFailed && ev.Error != "" {
				key := "turn-error:" + ev.Turn
				it, _ := load(key)
				it.Turn, it.Kind, it.Status = ev.Turn, convo.KindError, convo.StatusDone
				it.Data = mustJSON(ItemData{Text: ev.Error})
			}
			if ev.Status == convo.StatusInterrupted {
				key := "turn-stopped:" + ev.Turn
				it, _ := load(key)
				it.Turn, it.Kind, it.Status = ev.Turn, convo.KindNotice, convo.StatusDone
				it.Data = mustJSON(ItemData{Text: "Stopped"})
			}
			if sum.Turn == ev.Turn {
				sum.Turn, sum.TurnSince = "", nil
			}
			// A routine run with no news folds its answer away.
			for _, input := range s.routineTurnEnded(ctx, ev.Turn, ev.Status, ev.Error) {
				it, d := load("input:" + input)
				if it.Kind == "" {
					continue
				}
				if d.Extra == nil {
					d.Extra = map[string]any{}
				}
				d.Extra["quiet"] = true
				it.Data = mustJSON(d)
			}
			sum.LastTurnAt = &now
			if u := ev.Usage; u != nil {
				if u.Context > 0 {
					sum.Context = u.Context
				}
				if u.ContextWindow > 0 {
					sum.ContextWindow = u.ContextWindow
				}
				sum.InputTokens += u.Input + u.CacheRead
				sum.OutputTokens += u.Output
				sum.CostUSD += u.CostUSD
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if b := mustJSON(sum); string(b) != string(mustJSON(before)) {
		if err := s.Store.Sessions().SetConversation(ctx, sess.ID, b); err != nil {
			return err
		}
		sess.Conversation = b
		s.SessionChanged(ctx, sess)
	}
	return nil
}

// AnswerRequest answers a permission or a question of the agent.
func (s *Service) AnswerRequest(ctx context.Context, owner *store.User, id, requestID string, r runner.ConvoResponse) (*store.ConvItem, error) {
	sess, err := s.convSession(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	it, err := s.Store.Conversations().GetItem(ctx, sess.ID, store.MainThread, "request:"+requestID)
	if err != nil {
		return nil, err
	}
	if it.Status != "pending" {
		return nil, &ValidationError{"this was answered already, or the agent stopped waiting"}
	}
	us, ok := s.Uplinks.(uplinkSender)
	if !ok || !us.Send(sess.ID, runner.UplinkFrame{Event: runner.EventRespond, RequestID: requestID, Response: &r}) {
		return nil, &ValidationError{"the agent is not connected; try again in a few seconds"}
	}
	var d ItemData
	_ = json.Unmarshal(it.Data, &d)
	d.Response = &r
	it.Status, it.Data = "answered", mustJSON(d)
	if it, err = s.Store.Conversations().UpsertItem(ctx, it); err != nil {
		return nil, err
	}
	s.publishItem(owner.ID, sess.ID, it)
	return it, nil
}

// SetModel switches a conversation session's model.
func (s *Service) SetModel(ctx context.Context, owner *store.User, id, model string) error {
	sess, err := s.convSession(ctx, owner.ID, id)
	if err != nil {
		return err
	}
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 200 {
		return &ValidationError{"a model name is needed"}
	}
	us, ok := s.Uplinks.(uplinkSender)
	if !ok || !us.Send(sess.ID, runner.UplinkFrame{Event: runner.EventSetModel, Model: model}) {
		return &ValidationError{"the agent is not connected; start it first"}
	}
	return nil
}

// mergeTool updates a tool's description with what an event carries.
func mergeTool(cur, next *convo.Tool) *convo.Tool {
	if cur == nil {
		cp := *next
		return &cp
	}
	out := *cur
	if next.Name != "" {
		out.Name = next.Name
	}
	if next.Kind != "" {
		out.Kind = next.Kind
	}
	if next.Title != "" {
		out.Title = next.Title
	}
	if next.Input != "" {
		out.Input = next.Input
	}
	if next.Output != "" {
		out.Output = next.Output
	}
	if next.Error {
		out.Error = true
	}
	return &out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
