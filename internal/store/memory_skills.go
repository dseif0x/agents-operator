package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

type memSkillData struct {
	mu   sync.Mutex
	list []*Skill
}

// Skills returns the skill library aggregate.
func (m *Memory) Skills() Skills { return memSkills{m.skills} }

type memSkills struct{ d *memSkillData }

func (r memSkills) List(_ context.Context, userID string) ([]*Skill, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*Skill
	for _, s := range r.d.list {
		if s.UserID == userID {
			c := *s
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r memSkills) Get(_ context.Context, userID, id string) (*Skill, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	for _, s := range r.d.list {
		if s.UserID == userID && s.ID == id {
			c := *s
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (r memSkills) taken(s *Skill) bool {
	for _, o := range r.d.list {
		if o.UserID == s.UserID && o.Name == s.Name && o.ID != s.ID {
			return true
		}
	}
	return false
}

func (r memSkills) Create(_ context.Context, s *Skill) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if s.ID == "" {
		s.ID = NewID()
	}
	if r.taken(s) {
		return ErrConflict
	}
	s.CreatedAt, s.UpdatedAt = time.Now(), time.Now()
	c := *s
	r.d.list = append(r.d.list, &c)
	return nil
}

func (r memSkills) Update(_ context.Context, s *Skill) (*Skill, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if r.taken(s) {
		return nil, ErrConflict
	}
	for _, o := range r.d.list {
		if o.UserID == s.UserID && o.ID == s.ID {
			o.Name, o.Description, o.Instructions, o.UpdatedAt = s.Name, s.Description, s.Instructions, time.Now()
			c := *o
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (r memSkills) Delete(_ context.Context, userID, id string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	for i, s := range r.d.list {
		if s.UserID == userID && s.ID == id {
			r.d.list = append(r.d.list[:i], r.d.list[i+1:]...)
			return nil
		}
	}
	return nil
}
