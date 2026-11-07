package store

import (
	"context"
	"slices"
	"sync"
	"time"
)

type memMemoryData struct {
	mu   sync.Mutex
	list []*AgentMemory
}

// Memories returns the memory aggregate.
func (m *Memory) Memories() Memories { return memMemories{m.mems} }

type memMemories struct{ d *memMemoryData }

func (r memMemories) List(_ context.Context, sessionID string) ([]*AgentMemory, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*AgentMemory
	for _, m := range r.d.list {
		if m.SessionID == sessionID {
			c := *m
			out = append(out, &c)
		}
	}
	return out, nil
}

func (r memMemories) Add(_ context.Context, m *AgentMemory) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if m.ID == "" {
		m.ID = NewID()
	}
	m.CreatedAt, m.UpdatedAt = time.Now(), time.Now()
	c := *m
	r.d.list = append(r.d.list, &c)
	return nil
}

func (r memMemories) Update(_ context.Context, sessionID, id, text string) (*AgentMemory, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	for _, m := range r.d.list {
		if m.SessionID == sessionID && m.ID == id {
			m.Text, m.UpdatedAt = text, time.Now()
			c := *m
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (r memMemories) Delete(_ context.Context, sessionID, id string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	r.d.list = slices.DeleteFunc(r.d.list, func(m *AgentMemory) bool { return m.SessionID == sessionID && m.ID == id })
	return nil
}
