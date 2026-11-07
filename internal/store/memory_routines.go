package store

import (
	"context"
	"slices"
	"sync"
	"time"
)

type memRoutineData struct {
	mu       sync.Mutex
	routines []*Routine
	runs     []*RoutineRun
	nextRun  int64
}

// Routines returns the routine aggregate.
func (m *Memory) Routines() Routines { return memRoutines{m.routines} }

type memRoutines struct{ d *memRoutineData }

func copyRoutine(r *Routine) *Routine {
	c := *r
	c.Schedule = append([]byte(nil), r.Schedule...)
	c.Filters = append([]byte(nil), r.Filters...)
	return &c
}

func (s memRoutines) Create(_ context.Context, r *Routine) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if r.ID == "" {
		r.ID = NewID()
	}
	normaliseRoutine(r)
	r.CreatedAt, r.UpdatedAt = time.Now(), time.Now()
	s.d.routines = append(s.d.routines, copyRoutine(r))
	return nil
}

func (s memRoutines) find(id string) *Routine {
	for _, r := range s.d.routines {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s memRoutines) Update(_ context.Context, r *Routine) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	cur := s.find(r.ID)
	if cur == nil {
		return ErrNotFound
	}
	normaliseRoutine(r)
	cur.Name, cur.Instruction, cur.Trigger, cur.Schedule, cur.Enabled = r.Name, r.Instruction, r.Trigger, append([]byte(nil), r.Schedule...), r.Enabled
	cur.WebhookSecret, cur.EventType, cur.Filters, cur.NextRunAt = r.WebhookSecret, r.EventType, append([]byte(nil), r.Filters...), r.NextRunAt
	cur.UpdatedAt = time.Now()
	return nil
}

func (s memRoutines) Get(_ context.Context, id string) (*Routine, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if r := s.find(id); r != nil {
		return copyRoutine(r), nil
	}
	return nil, ErrNotFound
}

func (s memRoutines) List(_ context.Context, ownerID string) ([]*Routine, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	var out []*Routine
	for _, r := range s.d.routines {
		if r.OwnerID == ownerID {
			out = append(out, copyRoutine(r))
		}
	}
	return out, nil
}

func (s memRoutines) Delete(_ context.Context, id string) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.d.routines = slices.DeleteFunc(s.d.routines, func(r *Routine) bool { return r.ID == id })
	s.d.runs = slices.DeleteFunc(s.d.runs, func(r *RoutineRun) bool { return r.RoutineID == id })
	return nil
}

func (s memRoutines) Due(_ context.Context, now time.Time) ([]*Routine, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	var out []*Routine
	for _, r := range s.d.routines {
		if r.Enabled && r.Trigger == TriggerSchedule && r.NextRunAt != nil && !r.NextRunAt.After(now) {
			out = append(out, copyRoutine(r))
		}
	}
	return out, nil
}

func (s memRoutines) SetRun(_ context.Context, id string, next *time.Time, last time.Time) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if r := s.find(id); r != nil {
		r.NextRunAt, r.LastRunAt = next, &last
	}
	return nil
}

func (s memRoutines) AddRun(_ context.Context, r *RoutineRun) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.d.nextRun++
	r.ID = s.d.nextRun
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	c := *r
	s.d.runs = append(s.d.runs, &c)
	return nil
}

func (s memRoutines) UpdateRun(_ context.Context, r *RoutineRun) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	for _, cur := range s.d.runs {
		if cur.ID == r.ID {
			cur.Status, cur.InputID, cur.Turn, cur.ChatMessage, cur.Quiet, cur.Error, cur.EndedAt = r.Status, r.InputID, r.Turn, r.ChatMessage, r.Quiet, r.Error, r.EndedAt
		}
	}
	return nil
}

func (s memRoutines) Runs(_ context.Context, routineID string, limit int) ([]*RoutineRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	var out []*RoutineRun
	for i := len(s.d.runs) - 1; i >= 0 && len(out) < limit; i-- {
		if s.d.runs[i].RoutineID == routineID {
			c := *s.d.runs[i]
			out = append(out, &c)
		}
	}
	return out, nil
}

func (s memRoutines) RunsSince(_ context.Context, ownerID string, since time.Time) ([]*RoutineRun, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	owned := map[string]bool{}
	for _, r := range s.d.routines {
		if r.OwnerID == ownerID {
			owned[r.ID] = true
		}
	}
	var out []*RoutineRun
	for _, r := range s.d.runs {
		if owned[r.RoutineID] && !r.StartedAt.Before(since) {
			c := *r
			out = append(out, &c)
		}
	}
	return out, nil
}

func (s memRoutines) RunByInput(_ context.Context, inputID string) (*RoutineRun, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	for i := len(s.d.runs) - 1; i >= 0; i-- {
		if s.d.runs[i].InputID == inputID && inputID != "" {
			c := *s.d.runs[i]
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (s memRoutines) RunsByTurn(_ context.Context, turn string) ([]*RoutineRun, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	var out []*RoutineRun
	for _, r := range s.d.runs {
		if r.Turn == turn && turn != "" {
			c := *r
			out = append(out, &c)
		}
	}
	return out, nil
}
