package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

func TestOpenClaudeEnv(t *testing.T) {
	// Account settings only: they become the OPENAI_* names OpenClaude reads.
	env := envMap(openClaudeEnv([]string{
		"PATH=/bin", "SENTRY_DSN=https://x@sentry.example/1",
		"OPENCLAUDE_API_KEY=sk-uj", "OPENCLAUDE_BASE_URL=https://llm.example/v1", "OPENCLAUDE_MODEL=glm-5.3-uncensored",
	}))
	for k, want := range map[string]string{
		"CLAUDE_CODE_USE_OPENAI": "1", "OPENAI_API_KEY": "sk-uj", "OPENAI_BASE_URL": "https://llm.example/v1", "OPENAI_MODEL": "glm-5.3-uncensored",
		"DISABLE_TELEMETRY": "1", "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1", "PATH": "/bin",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["SENTRY_DSN"]; ok {
		t.Error("SENTRY_DSN passed on")
	}

	// Nothing configured: unjailedai.com and a default model, never the
	// CLI's own fallback gateway; no key invented.
	env = envMap(openClaudeEnv([]string{"PATH=/bin"}))
	if env["OPENAI_BASE_URL"] != openClaudeDefaultBaseURL || env["OPENAI_MODEL"] != openClaudeDefaultModel {
		t.Fatalf("defaults = %q %q", env["OPENAI_BASE_URL"], env["OPENAI_MODEL"])
	}
	if _, ok := env["OPENAI_API_KEY"]; ok {
		t.Fatal("OPENAI_API_KEY set without a key")
	}

	// The session's own OPENAI_MODEL / OPENAI_BASE_URL win over the account.
	env = envMap(openClaudeEnv([]string{"OPENCLAUDE_MODEL=glm-5.3", "OPENAI_MODEL=deepseek-v4-pro-uncensored", "OPENAI_BASE_URL=http://10.1.2.3:8000/v1"}))
	if env["OPENAI_MODEL"] != "deepseek-v4-pro-uncensored" || env["OPENAI_BASE_URL"] != "http://10.1.2.3:8000/v1" {
		t.Fatalf("session overrides = %q %q", env["OPENAI_MODEL"], env["OPENAI_BASE_URL"])
	}

	// AgentCommand wires it in.
	c := AgentCommand(runner.AgentOpenClaude, false, "/w", []string{"OPENCLAUDE_API_KEY=k"})
	if envMap(c.Env)["OPENAI_API_KEY"] != "k" || envMap(c.Env)["CLAUDE_CODE_USE_OPENAI"] != "1" {
		t.Fatalf("command env = %v", c.Env)
	}
}

func TestPrepareOpenClaude(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, Agent: runner.AgentOpenClaude, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := os.MkdirAll(ws.HomeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	readJSON := func(p string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return m
	}

	// First-run answers in ~/.openclaude.json, with no credential check: the
	// provider comes from the environment.
	ws.prepareClaudeConfig([]store.Repo{{Path: "app"}})
	cfg := readJSON(filepath.Join(ws.HomeDir(), ".openclaude.json"))
	projects, _ := cfg["projects"].(map[string]any)
	app, _ := projects[filepath.Join(root, "app")].(map[string]any)
	if cfg["hasCompletedOnboarding"] != true || cfg["theme"] != "dark" || app["hasTrustDialogAccepted"] != true {
		t.Fatalf(".openclaude.json = %v", cfg)
	}
	if _, err := os.Stat(filepath.Join(ws.HomeDir(), ".claude.json")); err == nil {
		t.Fatal("~/.claude.json written for an OpenClaude session")
	}

	// Hooks and, when autonomous, the bypass consent in ~/.openclaude/settings.json.
	t.Setenv(runner.EnvAutonomous, "true")
	ws.prepareClaudeSettings()
	settings := readJSON(filepath.Join(ws.HomeDir(), ".openclaude", "settings.json"))
	hooks, _ := settings["hooks"].(map[string]any)
	if settings["skipDangerousModePermissionPrompt"] != true || hooks == nil {
		t.Fatalf("settings.json = %v", settings)
	}
	for _, ev := range claudeHookEvents {
		groups, _ := hooks[ev].([]any)
		if !hasRunnerHook(groups) {
			t.Errorf("no runner hook for %s", ev)
		}
	}
	if _, err := os.Stat(filepath.Join(ws.HomeDir(), ".claude", "settings.json")); err == nil {
		t.Fatal("Claude Code settings written for an OpenClaude session")
	}

	// Its global instructions get the generated AGENTS.md content.
	if err := ws.writeAgentsFile(nil); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(ws.AgentsFile())
	if got, err := os.ReadFile(filepath.Join(ws.HomeDir(), ".openclaude", "CLAUDE.md")); err != nil || string(got) != string(want) {
		t.Fatalf("~/.openclaude/CLAUDE.md: %v", err)
	}
}
