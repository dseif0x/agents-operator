package store

import (
	"context"
	"time"
)

// Skill is an entry of the owner's skill library: instructions an agent
// loads when they fit the task (Agent Skills, a SKILL.md). Sessions pick
// the skills they get (Session.Skills).
type Skill struct {
	ID     string `json:"id"`
	UserID string `json:"-"`
	// Name is the skill's directory and front matter name: lower case
	// letters, digits and hyphens.
	Name string `json:"name"`
	// Description tells the agent when to use the skill.
	Description  string    `json:"description"`
	Instructions string    `json:"instructions"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Skills is the skill library aggregate.
type Skills interface {
	// List returns the user's skills by name.
	List(ctx context.Context, userID string) ([]*Skill, error)
	Get(ctx context.Context, userID, id string) (*Skill, error)
	// Create stores a new skill (ErrConflict when the name is taken).
	Create(ctx context.Context, s *Skill) error
	// Update replaces name, description and instructions.
	Update(ctx context.Context, s *Skill) (*Skill, error)
	Delete(ctx context.Context, userID, id string) error
}
