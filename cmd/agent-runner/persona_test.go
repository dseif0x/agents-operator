package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func TestPersonaPerAgent(t *testing.T) {
	env := []string{"HOME=/h", runner.EnvPersona + "=You review.\nBe blunt."}

	// Claude Code: on its command line, not in its environment.
	c := AgentCommand(runner.AgentClaude, false, "/w", env)
	if i := slices.Index(c.Args, "--append-system-prompt"); i < 0 || c.Args[i+1] != "You review.\nBe blunt." {
		t.Fatalf("claude args = %q", c.Args)
	}
	if envValue(c.Env, runner.EnvPersona) != "" {
		t.Fatal("persona left in claude's environment")
	}
	if c := AgentCommand(runner.AgentClaude, false, "/w", []string{"HOME=/h"}); slices.Contains(c.Args, "--append-system-prompt") {
		t.Fatalf("empty persona passed: %q", c.Args)
	}
	for _, agent := range []string{runner.AgentCodex, runner.AgentShell} {
		if c := AgentCommand(agent, false, "/w", env); envValue(c.Env, runner.EnvPersona) != "" {
			t.Errorf("persona left in %s's environment", agent)
		}
	}

	// Codex: a section of its global instructions file only.
	t.Setenv(runner.EnvPersona, "You review.")
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ws.writeAgentsFile(nil); err != nil {
		t.Fatal(err)
	}
	codex, _ := os.ReadFile(filepath.Join(ws.HomeDir(), ".codex", "AGENTS.md"))
	if !strings.Contains(string(codex), "## Your persona\n\nYou review.") {
		t.Fatalf("codex AGENTS.md:\n%s", codex)
	}
	shared, _ := os.ReadFile(ws.AgentsFile())
	if strings.Contains(string(shared), "Your persona") {
		t.Fatal("persona in the shared AGENTS.md")
	}
}
