package store

import (
	"context"
	"encoding/json"
	"time"
)

// Routine triggers.
const (
	TriggerSchedule = "schedule"
	TriggerWebhook  = "webhook"
)

// Run sources and states.
const (
	RunSchedule = "schedule"
	RunWebhook  = "webhook"
	RunTest     = "test"

	RunQueued      = "queued"
	RunRunning     = "running"
	RunSucceeded   = "succeeded"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
	RunSent        = "sent" // a chat routine's message was posted
)

// Routine is an instruction an agent (SessionID) or a chat (ChatID) gets on
// a schedule or when a webhook arrives.
type Routine struct {
	ID          string
	OwnerID     string
	SessionID   string
	ChatID      string
	Name        string
	Instruction string
	Trigger     string
	// Schedule is a routine.Schedule as JSON.
	Schedule      json.RawMessage
	Enabled       bool
	WebhookSecret string
	EventType     string
	// Filters is a list of {"path","value"} the webhook body must match.
	Filters   json.RawMessage
	NextRunAt *time.Time
	LastRunAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RoutineRun is one run of a routine.
type RoutineRun struct {
	ID           int64
	RoutineID    string
	Source       string
	Status       string
	InputID      string
	Turn         string
	ChatMessage  *int64
	Quiet        bool
	Error        string
	Payload      string
	ScheduledFor *time.Time
	StartedAt    time.Time
	EndedAt      *time.Time
}

// Routines is the routine aggregate.
type Routines interface {
	Create(ctx context.Context, r *Routine) error
	// Update replaces name, instruction, trigger, schedule, enabled, event
	// type, filters, the secret and the next run.
	Update(ctx context.Context, r *Routine) error
	Get(ctx context.Context, id string) (*Routine, error)
	// List returns the owner's routines, oldest first.
	List(ctx context.Context, ownerID string) ([]*Routine, error)
	Delete(ctx context.Context, id string) error
	// Due returns enabled scheduled routines whose next run is at or before
	// now.
	Due(ctx context.Context, now time.Time) ([]*Routine, error)
	// SetRun records a run time: next (nil: none) and last.
	SetRun(ctx context.Context, id string, next *time.Time, last time.Time) error

	AddRun(ctx context.Context, run *RoutineRun) error
	UpdateRun(ctx context.Context, run *RoutineRun) error
	// Runs returns a routine's newest runs first.
	Runs(ctx context.Context, routineID string, limit int) ([]*RoutineRun, error)
	// RunsSince returns the owner's runs started at or after since.
	RunsSince(ctx context.Context, ownerID string, since time.Time) ([]*RoutineRun, error)
	RunByInput(ctx context.Context, inputID string) (*RoutineRun, error)
	RunsByTurn(ctx context.Context, turn string) ([]*RoutineRun, error)
}
