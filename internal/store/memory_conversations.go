package store

import (
	"context"
	"slices"
	"sync"
	"time"
)

type memConvData struct {
	mu     sync.Mutex
	nextID int64
	items  []*ConvItem
	inputs []*ConvInput
	files  map[string]*ConvFile
}

// Conversations returns the conversation aggregate.
func (m *Memory) Conversations() Conversations { return memConversations{m.conv} }

type memConversations struct{ d *memConvData }

func copyItem(it *ConvItem) *ConvItem {
	c := *it
	c.Data = append([]byte(nil), it.Data...)
	return &c
}

func (r memConversations) UpsertItem(_ context.Context, it *ConvItem) (*ConvItem, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	now := time.Now()
	data := append([]byte(nil), it.Data...)
	if len(data) == 0 {
		data = []byte(`{}`)
	}
	for _, cur := range r.d.items {
		if cur.SessionID == it.SessionID && cur.Thread == it.Thread && cur.Key == it.Key {
			cur.Turn, cur.Kind, cur.Status, cur.Data, cur.UpdatedAt = it.Turn, it.Kind, it.Status, data, now
			return copyItem(cur), nil
		}
	}
	r.d.nextID++
	n := &ConvItem{ID: r.d.nextID, SessionID: it.SessionID, Thread: it.Thread, Turn: it.Turn, Key: it.Key, Kind: it.Kind, Status: it.Status, Data: data, CreatedAt: now, UpdatedAt: now}
	r.d.items = append(r.d.items, n)
	return copyItem(n), nil
}

func (r memConversations) GetItem(_ context.Context, sessionID, thread, key string) (*ConvItem, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	for _, cur := range r.d.items {
		if cur.SessionID == sessionID && cur.Thread == thread && cur.Key == key {
			return copyItem(cur), nil
		}
	}
	return nil, ErrNotFound
}

func (r memConversations) DeleteItem(_ context.Context, sessionID, thread, key string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	r.d.items = slices.DeleteFunc(r.d.items, func(cur *ConvItem) bool {
		return cur.SessionID == sessionID && cur.Thread == thread && cur.Key == key
	})
	return nil
}

func (r memConversations) ListItems(_ context.Context, sessionID, thread string, before int64, limit int) ([]*ConvItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*ConvItem
	for i := len(r.d.items) - 1; i >= 0 && len(out) < limit; i-- {
		cur := r.d.items[i]
		if cur.SessionID == sessionID && cur.Thread == thread && (before == 0 || cur.ID < before) {
			out = append(out, copyItem(cur))
		}
	}
	slices.Reverse(out)
	return out, nil
}

func (r memConversations) TurnItems(_ context.Context, sessionID, thread, turn string) ([]*ConvItem, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*ConvItem
	for _, cur := range r.d.items {
		if cur.SessionID == sessionID && cur.Thread == thread && cur.Turn == turn && turn != "" {
			out = append(out, copyItem(cur))
		}
	}
	return out, nil
}

func (r memConversations) RunningItems(_ context.Context, sessionID, thread, turn string) ([]*ConvItem, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*ConvItem
	for _, cur := range r.d.items {
		if cur.SessionID == sessionID && cur.Thread == thread && cur.Turn == turn && cur.Status == "running" {
			out = append(out, copyItem(cur))
		}
	}
	return out, nil
}

func copyInput(in *ConvInput) *ConvInput {
	c := *in
	c.Files = nil
	for _, f := range in.Files {
		f.Data = nil
		c.Files = append(c.Files, f)
	}
	return &c
}

func (r memConversations) AddInput(_ context.Context, in *ConvInput) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if in.ID == "" {
		in.ID = NewID()
	}
	if in.Thread == "" {
		in.Thread = MainThread
	}
	if in.State == "" {
		in.State = InputQueued
	}
	in.CreatedAt = time.Now()
	for i := range in.Files {
		f := &in.Files[i]
		if f.ID == "" {
			f.ID = NewID()
		}
		f.InputID, f.SessionID = in.ID, in.SessionID
		cp := *f
		r.d.files[f.ID] = &cp
	}
	stored := *in
	stored.Files = append([]ConvFile(nil), in.Files...)
	r.d.inputs = append(r.d.inputs, &stored)
	return nil
}

func (r memConversations) find(sessionID, id string) *ConvInput {
	for _, in := range r.d.inputs {
		if in.SessionID == sessionID && in.ID == id {
			return in
		}
	}
	return nil
}

func (r memConversations) GetInput(_ context.Context, sessionID, id string) (*ConvInput, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if in := r.find(sessionID, id); in != nil {
		return copyInput(in), nil
	}
	return nil, ErrNotFound
}

func (r memConversations) QueuedInputs(_ context.Context, sessionID string) ([]*ConvInput, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*ConvInput
	for _, in := range r.d.inputs {
		if in.SessionID == sessionID && in.State == InputQueued {
			out = append(out, copyInput(in))
		}
	}
	return out, nil
}

func (r memConversations) UpdateInput(_ context.Context, sessionID, id, text string) (*ConvInput, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	in := r.find(sessionID, id)
	if in == nil {
		return nil, ErrNotFound
	}
	if in.State != InputQueued {
		return nil, ErrConflict
	}
	in.Text = text
	return copyInput(in), nil
}

func (r memConversations) SetInputState(_ context.Context, sessionID, id, state string) (bool, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	in := r.find(sessionID, id)
	if in == nil || in.State != InputQueued {
		return false, nil
	}
	in.State = state
	if state == InputSent {
		now := time.Now()
		in.SentAt = &now
	}
	return true, nil
}

func (r memConversations) GetFile(_ context.Context, sessionID, id string) (*ConvFile, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	f := r.d.files[id]
	if f == nil || f.SessionID != sessionID {
		return nil, ErrNotFound
	}
	cp := *f
	return &cp, nil
}
