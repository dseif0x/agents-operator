package store

import (
	"context"
	"time"
)

// AgentMemory is a note an agent keeps across conversations.
type AgentMemory struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Text      string    `json:"text"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Memories is the memory aggregate.
type Memories interface {
	// List returns a session's memories, oldest first.
	List(ctx context.Context, sessionID string) ([]*AgentMemory, error)
	Add(ctx context.Context, m *AgentMemory) error
	// Update replaces the text.
	Update(ctx context.Context, sessionID, id, text string) (*AgentMemory, error)
	Delete(ctx context.Context, sessionID, id string) error
}
