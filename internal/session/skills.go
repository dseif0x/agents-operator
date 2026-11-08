package session

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// SkillInput is the body of creating or changing a skill.
type SkillInput struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
}

var skillNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Limits of a skill (Agent Skills: a name of at most 64 characters, a
// description of at most 1024).
const (
	MaxSkillName         = 64
	MaxSkillDescription  = 1024
	MaxSkillInstructions = 1 << 20
)

func (in SkillInput) validate() (store.Skill, error) {
	sk := store.Skill{
		Name:         strings.TrimSpace(in.Name),
		Description:  strings.Join(strings.Fields(in.Description), " "),
		Instructions: strings.TrimSpace(strings.ReplaceAll(in.Instructions, "\r\n", "\n")),
	}
	switch {
	case sk.Name == "" || len(sk.Name) > MaxSkillName || !skillNameRE.MatchString(sk.Name):
		return sk, &ValidationError{"a skill's name is lower case letters, digits and hyphens (at most 64), such as energy-report"}
	case sk.Description == "":
		return sk, &ValidationError{"a skill needs a description: what it does and when the agent should use it"}
	case len(sk.Description) > MaxSkillDescription:
		return sk, &ValidationError{"a skill's description is limited to 1024 characters"}
	case sk.Instructions == "":
		return sk, &ValidationError{"a skill needs instructions"}
	case len(sk.Instructions) > MaxSkillInstructions:
		return sk, &ValidationError{"a skill's instructions are limited to 1 MiB"}
	case !utf8.ValidString(sk.Instructions) || !utf8.ValidString(sk.Description):
		return sk, &ValidationError{"skills are UTF-8 text"}
	}
	return sk, nil
}

// ListSkills returns the owner's skill library.
func (s *Service) ListSkills(ctx context.Context, owner *store.User) ([]*store.Skill, error) {
	list, err := s.Store.Skills().List(ctx, owner.ID)
	if list == nil {
		list = []*store.Skill{}
	}
	return list, err
}

// CreateSkill adds a skill to the owner's library.
func (s *Service) CreateSkill(ctx context.Context, owner *store.User, in SkillInput) (*store.Skill, error) {
	sk, err := in.validate()
	if err != nil {
		return nil, err
	}
	sk.UserID = owner.ID
	if err := s.Store.Skills().Create(ctx, &sk); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &ValidationError{"you have a skill named " + sk.Name + " already"}
		}
		return nil, err
	}
	return &sk, nil
}

// UpdateSkill changes a skill; sessions with it get the new text when they
// restart.
func (s *Service) UpdateSkill(ctx context.Context, owner *store.User, id string, in SkillInput) (*store.Skill, error) {
	sk, err := in.validate()
	if err != nil {
		return nil, err
	}
	sk.ID, sk.UserID = id, owner.ID
	out, err := s.Store.Skills().Update(ctx, &sk)
	if errors.Is(err, store.ErrConflict) {
		return nil, &ValidationError{"you have a skill named " + sk.Name + " already"}
	}
	if err == nil {
		s.pushSkillChange(ctx, owner.ID, id)
	}
	return out, err
}

// DeleteSkill removes a skill from the library; sessions that had it lose
// it when they restart.
func (s *Service) DeleteSkill(ctx context.Context, owner *store.User, id string) error {
	if _, err := s.Store.Skills().Get(ctx, owner.ID, id); err != nil {
		return err
	}
	if err := s.Store.Skills().Delete(ctx, owner.ID, id); err != nil {
		return err
	}
	s.pushSkillChange(ctx, owner.ID, id)
	return nil
}

// SessionSkillDefs are the skills the session gets, in its order.
func (s *Service) SessionSkillDefs(ctx context.Context, sess *store.Session) []runner.SkillDef {
	out := []runner.SkillDef{}
	if len(sess.Skills) == 0 {
		return out
	}
	library, err := s.Store.Skills().List(ctx, sess.OwnerID)
	if err != nil {
		return out
	}
	for _, id := range sess.Skills {
		for _, sk := range library {
			if sk.ID == id {
				out = append(out, runner.SkillDef{Name: sk.Name, Description: sk.Description, Instructions: sk.Instructions})
			}
		}
	}
	return out
}

// pushSkills sends a running session its skills; the runner rewrites them.
func (s *Service) pushSkills(ctx context.Context, sess *store.Session) {
	if us, ok := s.Uplinks.(uplinkSender); ok && sess.State == store.StateRunning && sess.Agent != runner.AgentShell {
		us.Send(sess.ID, runner.UplinkFrame{Event: runner.EventSkills, Skills: s.SessionSkillDefs(ctx, sess), HasSkills: true})
	}
}

// pushSkillChange sends the new skills to every running session of the
// owner that has the skill.
func (s *Service) pushSkillChange(ctx context.Context, ownerID, skillID string) {
	sessions, err := s.Store.Sessions().List(ctx, ownerID)
	if err != nil {
		return
	}
	for _, sess := range sessions {
		if slices.Contains(sess.Skills, skillID) {
			s.pushSkills(ctx, sess)
		}
	}
}
