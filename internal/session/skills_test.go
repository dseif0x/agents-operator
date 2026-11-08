package session

import (
	"context"
	"errors"
	"github.com/dseif0x/agents-operator/internal/runner"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/store"
	"k8s.io/utils/ptr"
)

func TestMCPServerAccount(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	var ve *ValidationError
	if _, err := svc.Creds.CreateAccount(ctx, u.ID, AccountInput{Kind: store.AcctMCPServer, Name: "HA", Value: "tok"}); !errors.As(err, &ve) {
		t.Fatalf("an MCP server without a URL = %v", err)
	}
	if _, err := svc.Creds.CreateAccount(ctx, u.ID, AccountInput{Kind: store.AcctMCPServer, Name: "HA", BaseURL: "ftp://x", Value: "tok"}); !errors.As(err, &ve) {
		t.Fatalf("an ftp URL = %v", err)
	}
	a, err := svc.Creds.CreateAccount(ctx, u.ID, AccountInput{Kind: store.AcctMCPServer, Name: "Home Assistant", BaseURL: "http://ha:8080/api/mcp/", Value: "tok"})
	if err != nil || a.BaseURL != "http://ha:8080/api/mcp" {
		t.Fatalf("account = %+v, %v", a, err)
	}
}

func TestSkillsAndSessionPicks(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	var ve *ValidationError
	for _, in := range []SkillInput{
		{Name: "Energy Report", Description: "d", Instructions: "i"},
		{Name: "energy-report", Instructions: "i"},
		{Name: "energy-report", Description: "d"},
		{Name: "energy-report", Description: "d", Instructions: strings.Repeat("x", MaxSkillInstructions+1)},
	} {
		if _, err := svc.CreateSkill(ctx, u, in); !errors.As(err, &ve) {
			t.Errorf("%+v accepted: %v", in.Name, err)
		}
	}
	sk, err := svc.CreateSkill(ctx, u, SkillInput{Name: "energy-report", Description: "Summarise  energy\\nuse", Instructions: "Use HA.\\r\\n"})
	if err != nil || sk.Description != "Summarise energy\\nuse" {
		t.Fatalf("skill = %+v, %v", sk, err)
	}
	if _, err := svc.CreateSkill(ctx, u, SkillInput{Name: "energy-report", Description: "d", Instructions: "i"}); !errors.As(err, &ve) {
		t.Fatalf("a duplicate name = %v", err)
	}
	if sk, err = svc.UpdateSkill(ctx, u, sk.ID, SkillInput{Name: "energy", Description: "d2", Instructions: "i2"}); err != nil || sk.Name != "energy" {
		t.Fatalf("update = %+v, %v", sk, err)
	}

	ha, err := svc.Creds.CreateAccount(ctx, u.ID, AccountInput{Kind: store.AcctMCPServer, Name: "HA", BaseURL: "http://ha/mcp", Value: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.Creds.CreateAccount(ctx, u.ID, AccountInput{Kind: store.AcctOpenAIAPIKey, Name: "OpenAI", Value: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	// A session picks MCP servers and skills; other accounts or unknown
	// skills are refused.
	if _, err := svc.Create(ctx, u, CreateRequest{Name: "a", Agent: "claude", MCPServers: &[]string{key.ID}}); !errors.As(err, &ve) {
		t.Fatalf("a non-MCP account = %v", err)
	}
	if _, err := svc.Create(ctx, u, CreateRequest{Name: "a", Agent: "claude", Skills: &[]string{"nope"}}); !errors.As(err, &ve) {
		t.Fatalf("an unknown skill = %v", err)
	}
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "a", Agent: "claude", MCPServers: &[]string{ha.ID, ha.ID}, Skills: &[]string{sk.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if v := svc.View(sess); len(v.MCPServers) != 1 || v.MCPServers[0] != ha.ID || len(v.Skills) != 1 {
		t.Fatalf("view = %+v %+v", v.MCPServers, v.Skills)
	}
	// An edit without the lists keeps them; an empty list clears.
	if sess, err = svc.Update(ctx, u, sess.ID, CreateRequest{Name: "b", Agent: "claude", Autonomous: ptr.To(true)}); err != nil || len(sess.MCPServers) != 1 {
		t.Fatalf("kept = %+v, %v", sess, err)
	}
	if sess, err = svc.Update(ctx, u, sess.ID, CreateRequest{Name: "b", Agent: "claude", Skills: &[]string{}}); err != nil || len(sess.Skills) != 0 || len(sess.MCPServers) != 1 {
		t.Fatalf("cleared = %+v, %v", sess, err)
	}
	if err := svc.DeleteSkill(ctx, u, sk.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.ListSkills(ctx, u); len(list) != 0 {
		t.Fatalf("after delete = %+v", list)
	}
}

type recordingUplinks struct {
	frames map[string][]runner.UplinkFrame
}

func (r *recordingUplinks) Notify(string) {}

func (r *recordingUplinks) Send(id string, f runner.UplinkFrame) bool {
	r.frames[id] = append(r.frames[id], f)
	return true
}

func TestSkillsArePushedLive(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	rec := &recordingUplinks{frames: map[string][]runner.UplinkFrame{}}
	svc.Uplinks = rec
	sk, err := svc.CreateSkill(ctx, u, SkillInput{Name: "big", Description: "d", Instructions: strings.Repeat("x", 512<<10)}) // over the old 32 KiB cap
	if err != nil {
		t.Fatal(err)
	}
	other, _ := svc.CreateSkill(ctx, u, SkillInput{Name: "other", Description: "d", Instructions: "i"})
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "a", Agent: "claude", Skills: &[]string{sk.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	last := func() runner.UplinkFrame {
		t.Helper()
		f := rec.frames[sess.ID]
		if len(f) == 0 {
			t.Fatal("nothing pushed")
		}
		return f[len(f)-1]
	}
	// Editing a skill the session has pushes the new text.
	if _, err := svc.UpdateSkill(ctx, u, sk.ID, SkillInput{Name: "big", Description: "d2", Instructions: "short now"}); err != nil {
		t.Fatal(err)
	}
	if f := last(); f.Event != runner.EventSkills || !f.HasSkills || len(f.Skills) != 1 || f.Skills[0].Instructions != "short now" {
		t.Fatalf("frame = %+v", f)
	}
	// A skill it does not have: nothing new.
	n := len(rec.frames[sess.ID])
	if _, err := svc.UpdateSkill(ctx, u, other.ID, SkillInput{Name: "other", Description: "d", Instructions: "j"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.frames[sess.ID]) != n {
		t.Fatal("an unrelated skill was pushed")
	}
	// Changing the session's list pushes right away.
	if _, err := svc.Update(ctx, u, sess.ID, CreateRequest{Name: "a", Agent: "claude", Skills: &[]string{other.ID, sk.ID}}); err != nil {
		t.Fatal(err)
	}
	if f := last(); len(f.Skills) != 2 || f.Skills[0].Name != "other" {
		t.Fatalf("frame after edit = %+v", f)
	}
	// Deleting a skill pushes the rest.
	if err := svc.DeleteSkill(ctx, u, other.ID); err != nil {
		t.Fatal(err)
	}
	if f := last(); len(f.Skills) != 1 || f.Skills[0].Name != "big" {
		t.Fatalf("frame after delete = %+v", f)
	}
}
