package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
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
	fetchModels = func(kind, base, key string) []string { calls = append(calls, kind+" "+base+" "+key); return ids }
	t.Cleanup(func() { fetchModels = old })
	return &calls
}

// accountsEnv is the AI account description for accounts.
func accountsEnv(t *testing.T, accounts ...runner.AIAccount) string {
	t.Helper()
	return runner.EnvAIAccounts + "=" + jsonEnv(t, accounts)
}

var unjailedAccount = runner.AIAccount{ID: "u", Name: "Unjailed", Kind: store.AcctUnjailed, KeyEnv: runner.EnvAIKeyPrefix + "UNJAILED"}

func TestOpenCodeUnjailedProvider(t *testing.T) {
	home := t.TempDir()
	calls := stubModels(t, []string{"glm-5.3", "glm-5.3-flash", "glm-5.3-uncensored", "deepseek-v4-pro-uncensored", "kimi-k3", "grok-4.7", "brand-new-model"})
	key := runner.EnvAIKeyPrefix + "UNJAILED=sk-uj"
	args, cfg, vars := openCodeRun(t, true, "HOME="+home, accountsEnv(t, unjailedAccount), key, "SENTRY_DSN=x")

	if !slices.Equal(args, []string{"--auto"}) {
		t.Fatalf("args = %q", args)
	}
	if len(*calls) != 1 || (*calls)[0] != "unjailed https://unjailedai.com/v1 sk-uj" {
		t.Fatalf("model list fetched as %q", *calls)
	}
	prov := cfg["provider"].(map[string]any)["unjailed"].(map[string]any)
	opts := prov["options"].(map[string]any)
	if prov["npm"] != "@ai-sdk/openai-compatible" || prov["name"] != "Unjailed" || opts["baseURL"] != "https://unjailedai.com/v1" || opts["apiKey"] != "{env:"+runner.EnvAIKeyPrefix+"UNJAILED}" {
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
	plugins, _ := cfg["plugin"].([]any)
	if len(plugins) != 1 {
		t.Fatalf("plugin = %v", cfg["plugin"])
	}
	if b, err := os.ReadFile(strings.TrimPrefix(plugins[0].(string), "file://")); err != nil || !strings.Contains(string(b), `"experimental.chat.messages.transform"`) || !strings.Contains(string(b), `["unjailed"]`) {
		t.Fatalf("plugin %v: %q %v", plugins[0], b, err)
	}
	for k, want := range map[string]string{"OPENCODE_DISABLE_AUTOUPDATE": "1", "OPENCODE_DISABLE_SHARE": "1", "OPENCODE_DISABLE_CLAUDE_CODE": "1", runner.EnvAIKeyPrefix + "UNJAILED": "sk-uj"} {
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
	if _, cfg, _ := openCodeRun(t, false, "HOME="+home, accountsEnv(t, unjailedAccount), key); cfg["model"] != nil {
		t.Fatalf("default model set over a picked one: %v", cfg["model"])
	}

	// An endpoint without glm-5.3 starts on its first model.
	stubModels(t, []string{"some-model", "other"})
	if _, cfg, _ := openCodeRun(t, false, "HOME="+t.TempDir(), accountsEnv(t, unjailedAccount), key); cfg["model"] != "unjailed/some-model" || cfg["small_model"] != nil {
		t.Fatalf("first-model default = %v / %v", cfg["model"], cfg["small_model"])
	}

	// The account's base URL; and a list that cannot be fetched falls back.
	stubModels(t, nil)
	own := unjailedAccount
	own.BaseURL = "http://10.1.2.3:8000/v1"
	_, cfg, _ = openCodeRun(t, false, "HOME="+t.TempDir(), accountsEnv(t, own), key)
	prov = cfg["provider"].(map[string]any)["unjailed"].(map[string]any)
	if prov["options"].(map[string]any)["baseURL"] != "http://10.1.2.3:8000/v1" || len(prov["models"].(map[string]any)) != len(unjailedFallbackModels) {
		t.Fatalf("fallback provider = %v", prov)
	}

	// No accounts: no providers, OpenCode as before with its own.
	if _, cfg, _ := openCodeRun(t, false, "HOME="+home); cfg["provider"] != nil || cfg["model"] != nil || cfg["plugin"] != nil {
		t.Fatalf("config without accounts = %v", cfg)
	}
	// An account whose key variable is missing is left out.
	if _, cfg, _ := openCodeRun(t, false, "HOME="+home, accountsEnv(t, unjailedAccount)); cfg["provider"] != nil {
		t.Fatalf("provider without its key = %v", cfg["provider"])
	}
}

func TestOpenCodeProviders(t *testing.T) {
	calls := stubModels(t, []string{"claude-x", "text-embedding-3", "gpt-x"})
	k := runner.EnvAIKeyPrefix
	_, cfg, vars := openCodeRun(t, false, "HOME="+t.TempDir(),
		accountsEnv(t,
			runner.AIAccount{Name: "Anthropic", Kind: store.AcctAnthropicAPIKey, BaseURL: "http://litellm:4000", KeyEnv: k + "ANTHROPIC"},
			runner.AIAccount{Name: "Work Anthropic", Kind: store.AcctAnthropicAPIKey, KeyEnv: k + "WORK_ANTHROPIC"},
			runner.AIAccount{Name: "OpenAI", Kind: store.AcctOpenAIAPIKey, KeyEnv: k + "OPENAI"},
			runner.AIAccount{Name: "Second OpenAI", Kind: store.AcctOpenAIAPIKey, KeyEnv: k + "SECOND_OPENAI"},
			runner.AIAccount{Name: "Local LLM", Kind: store.AcctOpenAICompatible, BaseURL: "http://llm:8080/v1", Models: []string{"qwen"}, KeyEnv: k + "LOCAL_LLM"},
			runner.AIAccount{Name: "Empty", Kind: store.AcctOpenAICompatible, BaseURL: "http://none/v1", KeyEnv: k + "EMPTY"},
			runner.AIAccount{Name: "Token", Kind: store.AcctClaudeOAuthToken, KeyEnv: k + "TOKEN"},
			runner.AIAccount{Name: "Sneaky", Kind: store.AcctOpenAIAPIKey, KeyEnv: "PATH"},
		),
		k+"ANTHROPIC=a1", k+"WORK_ANTHROPIC=a2", k+"OPENAI=o1", k+"SECOND_OPENAI=o2", k+"LOCAL_LLM=l", k+"EMPTY=e", k+"TOKEN=t")
	providers := cfg["provider"].(map[string]any)
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	// The first Anthropic and OpenAI accounts are OpenCode's own providers;
	// the others are named after their accounts. Empty has no models of its
	// own, so its list is fetched. Token is not a provider kind, and
	// Sneaky's key variable is not an account key.
	if want := []string{"anthropic", "empty", "local-llm", "openai", "second-openai", "work-anthropic"}; !slices.Equal(ids, want) {
		t.Fatalf("providers = %v, want %v", ids, want)
	}
	ant := providers["anthropic"].(map[string]any)
	if ant["name"] != "Anthropic" || ant["npm"] != nil || ant["models"] != nil || ant["options"].(map[string]any)["baseURL"] != "http://litellm:4000/v1" {
		t.Fatalf("anthropic = %v", ant)
	}
	work := providers["work-anthropic"].(map[string]any)
	if work["npm"] != "@ai-sdk/anthropic" || work["options"].(map[string]any)["apiKey"] != "{env:"+k+"WORK_ANTHROPIC}" || work["models"].(map[string]any)["claude-x"] == nil {
		t.Fatalf("work-anthropic = %v", work)
	}
	second := providers["second-openai"].(map[string]any)["models"].(map[string]any)
	if second["gpt-x"] == nil || second["text-embedding-3"] != nil {
		t.Fatalf("second-openai models = %v", second)
	}
	local := providers["local-llm"].(map[string]any)
	if local["npm"] != "@ai-sdk/openai-compatible" || len(local["models"].(map[string]any)) != 1 || local["options"].(map[string]any)["baseURL"] != "http://llm:8080/v1" {
		t.Fatalf("local-llm = %v", local)
	}
	// Model lists: second Anthropic (Anthropic headers), second OpenAI, and
	// Empty, which has no list of its own; Local LLM has one.
	if want := []string{"anthropic_api_key https://api.anthropic.com/v1 a2", "openai_api_key https://api.openai.com/v1 o2", "openai_compatible http://none/v1 e"}; !slices.Equal(*calls, want) {
		t.Fatalf("fetches = %q", *calls)
	}
	if cfg["model"] != nil || cfg["plugin"] != nil {
		t.Fatalf("Unjailed defaults without an Unjailed account: %v", cfg)
	}
	if strings.Contains(vars["OPENCODE_CONFIG_CONTENT"], "a2") {
		t.Fatal("a key in the config")
	}

	// An OpenAI-compatible endpoint with no models at all is left out.
	stubModels(t, nil)
	_, cfg, _ = openCodeRun(t, false, "HOME="+t.TempDir(), accountsEnv(t, runner.AIAccount{Name: "Empty", Kind: store.AcctOpenAICompatible, BaseURL: "http://none/v1", KeyEnv: k + "EMPTY"}), k+"EMPTY=e")
	if cfg["provider"] != nil {
		t.Fatalf("provider without models = %v", cfg["provider"])
	}
}

func TestProviderID(t *testing.T) {
	used := map[string]bool{}
	for _, c := range [][3]string{
		{"unjailed", "Unjailed", "unjailed"},
		{"unjailed", "Unjailed", "unjailed-account"},
		{"unjailed", "Unjailed", "unjailed-account-2"},
		{"", "Work LiteLLM", "work-litellm"},
		{"", "work  litellm!", "work-litellm-2"},
		{"", "äöü", "provider-account"},
		{"openai", "Second", "openai"},
		{"openai", "Second", "second"},
	} {
		if got := providerID(c[0], c[1], used); got != c[2] {
			t.Errorf("%q %q = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestOpenCodePersonaAndAgent(t *testing.T) {
	home := t.TempDir()
	stubModels(t, nil)

	// The persona: an instructions file the runner writes, added to
	// OpenCode's own prompt.
	_, cfg, vars := openCodeRun(t, false, "HOME="+home, runner.EnvPersona+"=Be terse.")
	if _, ok := vars[runner.EnvPersona]; ok {
		t.Fatal("persona left in the CLI's environment")
	}
	if cfg["agent"] != nil {
		t.Fatalf("persona replaced an agent prompt: %v", cfg["agent"])
	}
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
