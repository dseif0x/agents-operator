package store

import (
	"context"
	"encoding/json"
	"time"
)

// MainThread is a session's main conversation.
const MainThread = "main"

// Input states.
const (
	InputQueued    = "queued"
	InputSent      = "sent"
	InputCancelled = "cancelled"
)

// ConvItem is one item of a conversation (docs/design/conversations.md):
// a message given to the agent, a piece of its answer, a tool call, an
// error. Data holds the kind's fields as JSON.
type ConvItem struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"session_id"`
	Thread    string          `json:"thread"`
	Turn      string          `json:"turn,omitempty"`
	Key       string          `json:"key"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ConvInput is a message from the owner until the agent takes it.
type ConvInput struct {
	ID        string
	SessionID string
	Thread    string
	Text      string
	Source    string
	State     string
	CreatedAt time.Time
	SentAt    *time.Time
	Files     []ConvFile
}

// ConvFile is a file that comes with an input; Data is only loaded by
// GetFile.
type ConvFile struct {
	ID          string `json:"id"`
	InputID     string `json:"-"`
	SessionID   string `json:"-"`
	OwnerID     string `json:"-"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Data        []byte `json:"-"`
}

// Conversations is the conversation aggregate.
type Conversations interface {
	// UpsertItem inserts the item of (session, thread, key) or replaces its
	// turn, kind, status and data; it returns the stored row.
	UpsertItem(ctx context.Context, it *ConvItem) (*ConvItem, error)
	GetItem(ctx context.Context, sessionID, thread, key string) (*ConvItem, error)
	DeleteItem(ctx context.Context, sessionID, thread, key string) error
	// ListItems returns up to limit items before the id before (0: the
	// newest), oldest first.
	ListItems(ctx context.Context, sessionID, thread string, before int64, limit int) ([]*ConvItem, error)
	// TurnItems returns the items of a turn, oldest first.
	TurnItems(ctx context.Context, sessionID, thread, turn string) ([]*ConvItem, error)
	// RunningItems returns the items of a turn still running.
	RunningItems(ctx context.Context, sessionID, thread, turn string) ([]*ConvItem, error)

	// AddInput stores a queued input with its files.
	AddInput(ctx context.Context, in *ConvInput) error
	GetInput(ctx context.Context, sessionID, id string) (*ConvInput, error)
	// QueuedInputs lists the queued inputs, oldest first, with their files
	// (without data).
	QueuedInputs(ctx context.Context, sessionID string) ([]*ConvInput, error)
	// UpdateInput replaces the text of a queued input (ErrConflict when it
	// is no longer queued).
	UpdateInput(ctx context.Context, sessionID, id, text string) (*ConvInput, error)
	// SetInputState moves a queued input to state; it reports false when the
	// input was not queued any more.
	SetInputState(ctx context.Context, sessionID, id, state string) (bool, error)
	// GetFile returns a file with its data.
	GetFile(ctx context.Context, sessionID, id string) (*ConvFile, error)
}
