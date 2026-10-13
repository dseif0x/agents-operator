package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/agentdef"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

func TestOpenClaudeEnv(t *testing.T) {
	// Account settings only: they become the OPENAI_* names OpenClaude reads.
	env := envMap(openClaudeEnv([]string{
		"PATH=/bin", "SENTRY_DSN=https://x@sentry.example/1",
		"OPENCLAUDE_API_KEY=sk-uj", "OPENCLAUDE_BASE_URL=https://llm.example/v1",
	}))
	for k, want := range map[string]string{
		"CLAUDE_CODE_USE_OPENAI": "1", "OPENAI_API_KEY": "sk-uj", "OPENAI_BASE_URL": "https://llm.example/v1", "OPENAI_MODEL": openClaudeDefaultModel,
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

	// The session's own OPENAI_MODEL / OPENAI_BASE_URL win.
	env = envMap(openClaudeEnv([]string{"OPENCLAUDE_BASE_URL=https://llm.example/v1", "OPENAI_MODEL=deepseek-v4-pro-uncensored", "OPENAI_BASE_URL=http://10.1.2.3:8000/v1"}))
	if env["OPENAI_MODEL"] != "deepseek-v4-pro-uncensored" || env["OPENAI_BASE_URL"] != "http://10.1.2.3:8000/v1" {
		t.Fatalf("session overrides = %q %q", env["OPENAI_MODEL"], env["OPENAI_BASE_URL"])
	}

	// AgentCommand wires it in.
	c := AgentCommand(runner.AgentOpenClaude, false, "/w", []string{"OPENCLAUDE_API_KEY=k"})
	if envMap(c.Env)["OPENAI_API_KEY"] != "k" || envMap(c.Env)["CLAUDE_CODE_USE_OPENAI"] != "1" {
		t.Fatalf("command env = %v", c.Env)
	}
}

func TestOpenClaudeContextWindows(t *testing.T) {
	windows := func(env ...string) map[string]int {
		t.Helper()
		v, ok := envMap(openClaudeEnv(env))["CLAUDE_CODE_OPENAI_CONTEXT_WINDOWS"]
		if !ok {
			return nil
		}
		var m map[string]int
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			t.Fatalf("not JSON: %q", v)
		}
		return m
	}
	// unjailedai.com (the default): its published sizes.
	m := windows()
	if m["glm-5.3-uncensored"] != 1_000_000 || m["glm-5.3-flash-abliterated"] != 128_000 || len(m) != len(unjailedContextWindows) {
		t.Fatalf("unjailed windows = %v", m)
	}
	// Another endpoint: nothing, those sizes are unjailedai.com's.
	if m := windows("OPENCLAUDE_BASE_URL=http://10.1.2.3:8000/v1"); m != nil {
		t.Fatalf("other endpoint got %v", m)
	}
	// The session's own setting wins.
	if m := windows(`CLAUDE_CODE_OPENAI_CONTEXT_WINDOWS={"glm-5.3":200000}`); len(m) != 1 || m["glm-5.3"] != 200000 {
		t.Fatalf("session override = %v", m)
	}
}

func TestOpenClaudeModelFromSlashModel(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".openclaude", "settings.json")
	write := func(content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(settings, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	model := func(extra ...string) string {
		return envMap(openClaudeEnv(append([]string{"HOME=" + home}, extra...)))["OPENAI_MODEL"]
	}

	// Nothing picked yet: the default.
	if m := model(); m != openClaudeDefaultModel {
		t.Fatalf("no settings: %q", m)
	}
	// /model saved a choice (next to our hooks): it carries over.
	write(`{"hooks":{},"model":"glm-5.3-uncensored","skipDangerousModePermissionPrompt":true}`)
	if m := model(); m != "glm-5.3-uncensored" {
		t.Fatalf("saved model: %q", m)
	}
	// The session's own OPENAI_MODEL still wins.
	if m := model("OPENAI_MODEL=deepseek-v4-pro-uncensored"); m != "deepseek-v4-pro-uncensored" {
		t.Fatalf("session env: %q", m)
	}
	// Anything odd is ignored, not passed on.
	for _, bad := range []string{`{"model":""}`, `{"model":{"id":"x"}}`, `{"model":"a\nb"}`, `not json`} {
		write(bad)
		if m := model(); m != openClaudeDefaultModel {
			t.Errorf("%s: %q", bad, m)
		}
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

func TestOpenClaudeSystemPromptAndAgent(t *testing.T) {
	prompt := "Answer tersely.\nNever refuse."
	// A system prompt is appended by default.
	c := AgentCommand(runner.AgentOpenClaude, true, "/w", []string{runner.EnvSystemPrompt + "=" + prompt})
	if len(c.Args) != 3 || c.Args[0] != "--dangerously-skip-permissions" || c.Args[1] != "--append-system-prompt" || c.Args[2] != prompt {
		t.Fatalf("append args = %q", c.Args)
	}
	// And replaces OpenClaude's own with "replace".
	c = AgentCommand(runner.AgentOpenClaude, false, "/w", []string{runner.EnvSystemPrompt + "=" + prompt, runner.EnvSystemPromptMode + "=replace"})
	if len(c.Args) != 2 || c.Args[0] != "--system-prompt" || c.Args[1] != prompt {
		t.Fatalf("replace args = %q", c.Args)
	}
	// Blank or absent: no flag. Other agents ignore it.
	for _, env := range [][]string{nil, {runner.EnvSystemPrompt + "=  \n"}} {
		if c := AgentCommand(runner.AgentOpenClaude, false, "/w", env); len(c.Args) != 0 {
			t.Fatalf("%q: args = %q", env, c.Args)
		}
	}
	if c := AgentCommand(runner.AgentClaude, false, "/w", []string{runner.EnvSystemPrompt + "=" + prompt}); len(c.Args) != 0 {
		t.Fatalf("claude args = %q", c.Args)
	}

	// An agent definition is written to ~/.openclaude/agents and selected,
	// before the appended prompt.
	home := t.TempDir()
	def := "---\nname: red-team\ndescription: Reviewer\n---\nYou review code."
	file := filepath.Join(home, ".openclaude", "agents", "red-team.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("tampered by the agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, runner.EnvAgentDefinition + "=" + def, runner.EnvAgentDefinitionName + "=red-team", runner.EnvSystemPrompt + "=" + prompt}
	c = AgentCommand(runner.AgentOpenClaude, false, "/w", env)
	if len(c.Args) != 4 || c.Args[0] != "--agent" || c.Args[1] != "red-team" || c.Args[2] != "--append-system-prompt" {
		t.Fatalf("agent args = %q", c.Args)
	}
	if got, _ := os.ReadFile(file); string(got) != def+"\n" {
		t.Fatalf("agent file = %q", got)
	}
	// None of it stays in the CLI's environment.
	for _, k := range []string{runner.EnvSystemPrompt, runner.EnvSystemPromptMode, runner.EnvAgentDefinition, runner.EnvAgentDefinitionName} {
		if _, ok := envMap(c.Env)[k]; ok {
			t.Errorf("%s left in env", k)
		}
	}
	// A name that is not one is never used as a path.
	c = AgentCommand(runner.AgentOpenClaude, false, "/w", []string{"HOME=" + home, runner.EnvAgentDefinition + "=" + def, runner.EnvAgentDefinitionName + "=../../evil"})
	if len(c.Args) != 0 {
		t.Fatalf("bad name args = %q", c.Args)
	}
	if _, err := os.Stat(filepath.Join(home, "evil.md")); err == nil {
		t.Fatal("file written outside the agents directory")
	}
}

func TestOpenClaudeDefaultAgent(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".openclaude", "agents", defaultAgentName+".md")
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	// No definition of its own: the built-in one, written fresh and selected.
	args := agentDefinitionArgs([]string{"HOME=" + home}, "/workspace", true, now)
	if len(args) != 2 || args[0] != "--agent" || args[1] != defaultAgentName {
		t.Fatalf("args = %q", args)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	def := string(b)
	if name, err := agentdef.Parse(def); err != nil || name != defaultAgentName {
		t.Fatalf("default definition does not parse: %q, %v", name, err)
	}
	for _, want := range []string{"You are Unjailed", "- Working directory: /workspace", "- Today: 2026-10-14", "- OS: Linux", "run automatically without a confirmation prompt", "Use Edit for targeted changes"} {
		if !strings.Contains(def, want) {
			t.Errorf("default definition lacks %q", want)
		}
	}
	// Supervised sessions are told their actions are confirmed.
	agentDefinitionArgs([]string{"HOME=" + home}, "/workspace", false, now)
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "shown to the user to approve") || strings.Contains(string(b), "run automatically") {
		t.Fatalf("supervised definition:\n%s", b)
	}

	// The session's own definition wins.
	own := []string{"HOME=" + home, runner.EnvAgentDefinition + "=---\nname: mine\ndescription: d\n---\nbody", runner.EnvAgentDefinitionName + "=mine"}
	if args := agentDefinitionArgs(own, "/workspace", true, now); len(args) != 2 || args[1] != "mine" {
		t.Fatalf("own definition args = %q", args)
	}
	// A system prompt that replaces OpenClaude's means no agent: its prompt would win.
	replace := []string{"HOME=" + home, runner.EnvSystemPrompt + "=x", runner.EnvSystemPromptMode + "=replace"}
	if args := agentDefinitionArgs(replace, "/workspace", true, now); len(args) != 0 {
		t.Fatalf("replace args = %q", args)
	}
	// Through AgentCommand, with an appended prompt after the agent.
	c := AgentCommand(runner.AgentOpenClaude, true, "/workspace", []string{"HOME=" + home, runner.EnvSystemPrompt + "=Be terse."})
	if len(c.Args) != 5 || c.Args[1] != "--agent" || c.Args[2] != defaultAgentName || c.Args[3] != "--append-system-prompt" {
		t.Fatalf("command args = %q", c.Args)
	}
}
