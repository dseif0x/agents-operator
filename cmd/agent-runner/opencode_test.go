package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// openCodeRun builds the command for env and returns its arguments and the
// configuration it carries.
func openCodeRun(t *testing.T, autonomous bool, env ...string) ([]string, map[string]any, map[string]string) {
	t.Helper()
	c := AgentCommand(runner.AgentOpenCode, autonomous, "/workspace", env)
	if c.Path != "opencode" {
		t.Fatalf("path = %q", c.Path)
	}
	vars := envMap(c.Env)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(vars["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatalf("OPENCODE_CONFIG_CONTENT: %v", err)
	}
	return c.Args, cfg, vars
}

func stubModels(t *testing.T, ids []string) *[]string {
	t.Helper()
	var calls []string
	old := fetchModels
	fetchModels = func(base, key string) []string { calls = append(calls, base+" "+key); return ids }
	t.Cleanup(func() { fetchModels = old })
	return &calls
}

func TestOpenCodeUnjailedProvider(t *testing.T) {
	home := t.TempDir()
	calls := stubModels(t, []string{"glm-5.3", "glm-5.3-flash", "glm-5.3-uncensored", "deepseek-v4-pro-uncensored", "kimi-k3", "grok-4.7", "brand-new-model"})
	args, cfg, vars := openCodeRun(t, true, "HOME="+home, "UNJAILED_API_KEY=sk-uj", "SENTRY_DSN=x")

	if !slices.Equal(args, []string{"--auto"}) {
		t.Fatalf("args = %q", args)
	}
	if len(*calls) != 1 || (*calls)[0] != "https://unjailedai.com/v1 sk-uj" {
		t.Fatalf("model list fetched as %q", *calls)
	}
	prov := cfg["provider"].(map[string]any)["unjailed"].(map[string]any)
	opts := prov["options"].(map[string]any)
	if prov["npm"] != "@ai-sdk/openai-compatible" || opts["baseURL"] != "https://unjailedai.com/v1" || opts["apiKey"] != "{env:UNJAILED_API_KEY}" {
		t.Fatalf("provider = %v", prov)
	}
	if strings.Contains(vars["OPENCODE_CONFIG_CONTENT"], "sk-uj") {
		t.Fatal("the key is written into the config")
	}
	models := prov["models"].(map[string]any)
	limit := func(id string) map[string]any { return models[id].(map[string]any)["limit"].(map[string]any) }
	for id, want := range map[string]int{
		"glm-5.3-uncensored":         1_000_000,
		"glm-5.3-flash":              128_000,
		"deepseek-v4-pro-uncensored": 256_000,
		"kimi-k3":                    1_000_000,
		"grok-4.7":                   400_000,
		"brand-new-model":            unjailedDefaultContext, // not on the site's list yet
	} {
		if got := limit(id)["context"]; got != float64(want) {
			t.Errorf("%s context = %v, want %d", id, got, want)
		}
	}
	if len(models) != 7 || limit("glm-5.3")["output"] != float64(unjailedMaxOutput) {
		t.Fatalf("models = %v", models)
	}
	if cfg["model"] != "unjailed/glm-5.3" || cfg["small_model"] != "unjailed/glm-5.3-flash" || cfg["autoupdate"] != false || cfg["share"] != "disabled" {
		t.Fatalf("config = %v", cfg)
	}
	for k, want := range map[string]string{"OPENCODE_DISABLE_AUTOUPDATE": "1", "OPENCODE_DISABLE_SHARE": "1", "OPENCODE_DISABLE_CLAUDE_CODE": "1", "UNJAILED_API_KEY": "sk-uj"} {
		if vars[k] != want {
			t.Errorf("%s = %q", k, vars[k])
		}
	}

	// A model picked in OpenCode wins from then on: no default any more.
	state := filepath.Join(home, ".local", "state", "opencode", "model.json")
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte(`{"recent":[{"providerID":"unjailed","modelID":"deepseek-v4-pro-uncensored"}],"favorite":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, cfg, _ := openCodeRun(t, false, "HOME="+home, "UNJAILED_API_KEY=sk-uj"); cfg["model"] != nil {
		t.Fatalf("default model set over a picked one: %v", cfg["model"])
	}

	// An endpoint without glm-5.3 starts on its first model.
	stubModels(t, []string{"some-model", "other"})
	if _, cfg, _ := openCodeRun(t, false, "HOME="+t.TempDir(), "UNJAILED_API_KEY=k"); cfg["model"] != "unjailed/some-model" || cfg["small_model"] != nil {
		t.Fatalf("first-model default = %v / %v", cfg["model"], cfg["small_model"])
	}

	// The account's base URL; and a list that cannot be fetched falls back.
	stubModels(t, nil)
	_, cfg, _ = openCodeRun(t, false, "HOME="+t.TempDir(), "UNJAILED_API_KEY=k", "UNJAILED_BASE_URL=http://10.1.2.3:8000/v1/")
	prov = cfg["provider"].(map[string]any)["unjailed"].(map[string]any)
	if prov["options"].(map[string]any)["baseURL"] != "http://10.1.2.3:8000/v1" || len(prov["models"].(map[string]any)) != len(unjailedFallbackModels) {
		t.Fatalf("fallback provider = %v", prov)
	}

	// No key: no provider, OpenCode as before with its own providers.
	if _, cfg, _ := openCodeRun(t, false, "HOME="+home, "ANTHROPIC_API_KEY=sk-ant"); cfg["provider"] != nil || cfg["model"] != nil {
		t.Fatalf("config without a key = %v", cfg)
	}
}

func TestOpenCodeSystemPromptAndAgent(t *testing.T) {
	home := t.TempDir()
	stubModels(t, nil)

	// Replace: the default agent's prompt.
	_, cfg, vars := openCodeRun(t, false, "HOME="+home, runner.EnvSystemPrompt+"=You are Unjailed.", runner.EnvSystemPromptMode+"=replace")
	if build := cfg["agent"].(map[string]any)["build"].(map[string]any); build["prompt"] != "You are Unjailed." {
		t.Fatalf("replace agent = %v", cfg["agent"])
	}
	if _, ok := vars[runner.EnvSystemPrompt]; ok {
		t.Fatal("prompt left in the CLI's environment")
	}

	// Append: an instructions file the runner writes.
	_, cfg, _ = openCodeRun(t, false, "HOME="+home, runner.EnvSystemPrompt+"=Be terse.")
	files, _ := cfg["instructions"].([]any)
	if len(files) != 1 {
		t.Fatalf("instructions = %v", cfg["instructions"])
	}
	if b, err := os.ReadFile(files[0].(string)); err != nil || string(b) != "Be terse.\n" || !strings.HasPrefix(files[0].(string), home) {
		t.Fatalf("instructions file %v: %q %v", files[0], b, err)
	}

	// An agent definition becomes an OpenCode agent and is selected.
	def := "---\nname: red-team\ndescription: Reviewer\ntemperature: 0.2\n---\nYou review code."
	args, cfg, vars := openCodeRun(t, true, "HOME="+home, runner.EnvAgentDefinition+"="+def, runner.EnvAgentDefinitionName+"=red-team")
	if !slices.Equal(args, []string{"--auto", "--agent", "red-team"}) {
		t.Fatalf("args = %q", args)
	}
	agent := cfg["agent"].(map[string]any)["red-team"].(map[string]any)
	if agent["prompt"] != "You review code." || agent["description"] != "Reviewer" || agent["temperature"] != 0.2 || agent["mode"] != "primary" || agent["name"] != nil {
		t.Fatalf("agent = %v", agent)
	}
	if _, ok := vars[runner.EnvAgentDefinition]; ok {
		t.Fatal("definition left in the CLI's environment")
	}

	// One that does not parse is left out rather than breaking the start.
	if args, cfg, _ := openCodeRun(t, false, "HOME="+home, runner.EnvAgentDefinition+"=nonsense"); len(args) != 0 || cfg["agent"] != nil {
		t.Fatalf("bad definition: %q %v", args, cfg["agent"])
	}
}

func TestUnjailedModelSizes(t *testing.T) {
	// The 32 models unjailedai.com lists, all with a size, and the fallback
	// list covering them.
	if len(unjailedContextWindows) != 32 || len(unjailedFallbackModels) != 32 {
		t.Fatalf("%d sizes, %d fallback models", len(unjailedContextWindows), len(unjailedFallbackModels))
	}
	counts := map[int]int{}
	for _, c := range unjailedContextWindows {
		counts[c]++
	}
	// 1M: 4 GLM, 2 DeepSeek, Kimi; 128K: 3 GLM flash; 256K: 2 DeepSeek Pro; 400K: the other 20.
	if counts[1_000_000] != 7 || counts[128_000] != 3 || counts[256_000] != 2 || counts[400_000] != 20 {
		t.Fatalf("sizes = %v", counts)
	}
	if !slices.Contains(unjailedFallbackModels, unjailedDefaultModel) || !slices.Contains(unjailedFallbackModels, unjailedSmallModel) {
		t.Fatal("the fallback list lacks the default or the title model")
	}
}
