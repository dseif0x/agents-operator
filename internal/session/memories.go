package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Memories: short notes an agent keeps across conversations. The agent
// writes them with its memory tools, the owner in the details panel; the
// hub pushes them to the runner, which gives them to the agent with its
// next input whenever they changed.

// Limits.
const (
	MaxMemories    = 100
	MaxMemoryBytes = 1000
)

func cleanMemory(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", &ValidationError{"an empty memory"}
	}
	if len(text) > MaxMemoryBytes {
		return "", &ValidationError{fmt.Sprintf("a memory is limited to %d bytes; keep it short", MaxMemoryBytes)}
	}
	return text, nil
}

// ListMemories returns an agent's memories.
func (s *Service) ListMemories(ctx context.Context, owner *store.User, id string) ([]*store.AgentMemory, error) {
	if _, err := s.Get(ctx, owner.ID, id); err != nil {
		return nil, err
	}
	ms, err := s.Store.Memories().List(ctx, id)
	if ms == nil {
		ms = []*store.AgentMemory{}
	}
	return ms, err
}

func (s *Service) addMemory(ctx context.Context, sess *store.Session, text, source string) (*store.AgentMemory, error) {
	text, err := cleanMemory(text)
	if err != nil {
		return nil, err
	}
	if ms, _ := s.Store.Memories().List(ctx, sess.ID); len(ms) >= MaxMemories {
		return nil, &ValidationError{fmt.Sprintf("at most %d memories; forget some first", MaxMemories)}
	}
	m := &store.AgentMemory{SessionID: sess.ID, Text: text, Source: source}
	if err := s.Store.Memories().Add(ctx, m); err != nil {
		return nil, err
	}
	s.pushMemories(ctx, sess)
	return m, nil
}

// AddMemory adds a memory for the owner.
func (s *Service) AddMemory(ctx context.Context, owner *store.User, id, text string) (*store.AgentMemory, error) {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	return s.addMemory(ctx, sess, text, "owner")
}

// UpdateMemory changes a memory's text.
func (s *Service) UpdateMemory(ctx context.Context, owner *store.User, id, memID, text string) (*store.AgentMemory, error) {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	text, err = cleanMemory(text)
	if err != nil {
		return nil, err
	}
	m, err := s.Store.Memories().Update(ctx, sess.ID, memID, text)
	if err != nil {
		return nil, err
	}
	s.pushMemories(ctx, sess)
	return m, nil
}

// DeleteMemory forgets a memory.
func (s *Service) DeleteMemory(ctx context.Context, owner *store.User, id, memID string) error {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return err
	}
	if err := s.Store.Memories().Delete(ctx, sess.ID, memID); err != nil {
		return err
	}
	s.pushMemories(ctx, sess)
	return nil
}

// MemoryNotes is what the runner gets.
func (s *Service) MemoryNotes(ctx context.Context, sess *store.Session) []runner.MemoryNote {
	ms, err := s.Store.Memories().List(ctx, sess.ID)
	if err != nil {
		return nil
	}
	out := make([]runner.MemoryNote, 0, len(ms))
	for _, m := range ms {
		out = append(out, runner.MemoryNote{ID: m.ID, Text: m.Text})
	}
	return out
}

func (s *Service) pushMemories(ctx context.Context, sess *store.Session) {
	if us, ok := s.Uplinks.(uplinkSender); ok && sess.Interface == store.InterfaceConversation {
		notes := s.MemoryNotes(ctx, sess)
		if notes == nil {
			notes = []runner.MemoryNote{}
		}
		us.Send(sess.ID, runner.UplinkFrame{Event: runner.EventMemories, Memories: notes, HasMemories: true})
	}
}

// AgentMemory runs one of the agent's memory tools.
func (s *Service) AgentMemory(ctx context.Context, sess *store.Session, op, text, id string) (any, error) {
	switch op {
	case runner.OpMemorySave:
		m, err := s.addMemory(ctx, sess, text, "agent")
		if err != nil {
			return nil, err
		}
		return map[string]string{"saved": m.ID[:8], "text": m.Text}, nil
	case runner.OpMemoryForget:
		ms, _ := s.Store.Memories().List(ctx, sess.ID)
		id = strings.TrimSpace(id)
		for _, m := range ms {
			if id != "" && strings.HasPrefix(m.ID, id) {
				if err := s.Store.Memories().Delete(ctx, sess.ID, m.ID); err != nil {
					return nil, err
				}
				s.pushMemories(ctx, sess)
				return map[string]string{"forgot": m.Text}, nil
			}
		}
		return nil, &ValidationError{"no memory with that id (memory_list shows them)"}
	default: // list
		return s.MemoryNotes(ctx, sess), nil
	}
}
