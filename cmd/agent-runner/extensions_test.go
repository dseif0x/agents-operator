package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func TestSessionMCPServersInEveryCLI(t *testing.T) {
	home := t.TempDir()
	servers := `[{"name":"home-assistant","url":"http://ha:8080/api/mcp","token_env":"AGENTS_OPERATOR_MCP_TOKEN_HOME_ASSISTANT"},{"name":"chat","url":"http://x"},{"name":"bad name","url":"http://y"}]`
	env := []string{"HOME=" + home, runner.EnvMCPServers + "=" + servers, "AGENTS_OPERATOR_MCP_TOKEN_HOME_ASSISTANT=secret"}
	if got := sessionMCPServers(env); len(got) != 1 || got[0].Name != "home-assistant" {
		t.Fatalf("servers = %+v", got) // the runner's own name and odd names are dropped
	}

	// Claude Code: the server in --mcp-config, the token by reference.
	c := AgentCommand(runner.AgentClaude, true, "/w", env)
	i := slices.Index(c.Args, "--mcp-config")
	if i < 0 {
		t.Fatalf("args = %v", c.Args)
	}
	b, err := os.ReadFile(c.Args[i+1])
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	ha := cfg.MCPServers["home-assistant"]
	if ha["type"] != "http" || ha["url"] != "http://ha:8080/api/mcp" || ha["headers"].(map[string]any)["Authorization"] != "Bearer ${AGENTS_OPERATOR_MCP_TOKEN_HOME_ASSISTANT}" || cfg.MCPServers["chat"] == nil {
		t.Fatalf("claude config = %s", b)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatal("the token was written to disk")
	}

	// Codex: -c settings naming the variable.
	c = AgentCommand(runner.AgentCodex, true, "/w", env)
	joined := strings.Join(c.Args, " ")
	if !strings.Contains(joined, `mcp_servers.home-assistant.url="http://ha:8080/api/mcp"`) ||
		!strings.Contains(joined, `mcp_servers.home-assistant.bearer_token_env_var="AGENTS_OPERATOR_MCP_TOKEN_HOME_ASSISTANT"`) {
		t.Fatalf("codex args = %v", c.Args)
	}

	// OpenCode: a remote server in its configuration.
	oc, _ := openCodeConfig(env)
	e := oc["mcp"].(map[string]any)["home-assistant"].(map[string]any)
	if e["type"] != "remote" || e["headers"].(map[string]string)["Authorization"] != "Bearer {env:AGENTS_OPERATOR_MCP_TOKEN_HOME_ASSISTANT}" {
		t.Fatalf("opencode entry = %v", e)
	}
}

func TestWriteSkills(t *testing.T) {
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	home := ws.HomeDir()
	// A skill the agent made itself, named like one of the owner's.
	own := filepath.Join(home, ".claude", "skills", "mine")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	set := func(skills ...runner.SkillDef) { ws.writeSkills(skills) }
	set(runner.SkillDef{Name: "energy-report", Description: `Summarise "yesterday's" energy use`, Instructions: "Use the home-assistant tools.\n"},
		runner.SkillDef{Name: "mine", Description: "x", Instructions: "y"})
	for _, dir := range []string{".claude", ".codex"} {
		b, err := os.ReadFile(filepath.Join(home, dir, "skills", "energy-report", "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		want := "---\nname: energy-report\ndescription: \"Summarise \\\"yesterday's\\\" energy use\"\n---\n\nUse the home-assistant tools.\n"
		if string(b) != want {
			t.Fatalf("%s SKILL.md = %q", dir, b)
		}
	}
	if _, err := os.Stat(filepath.Join(own, "SKILL.md")); err == nil {
		t.Fatal("the agent's own skill was overwritten")
	}
	// The owner removes the skill: it goes; the agent's own stays.
	set()
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "energy-report")); !os.IsNotExist(err) {
		t.Fatal("a removed skill stayed")
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatal("the agent's own skill was removed")
	}
}
