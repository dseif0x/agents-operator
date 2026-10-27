package main

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/agentdef"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// OpenCode sessions get their settings through OPENCODE_CONFIG_CONTENT,
// which OpenCode merges over its own config files, so nothing the user keeps
// in ~/.config/opencode is rewritten:
//
//   - a provider for each of the owner's AI accounts, under its name
//     (openCodeProviders): OpenCode's own anthropic and openai providers for
//     the first Anthropic and OpenAI key, the AI SDK packages for further
//     ones, and OpenCode's OpenAI-compatible provider for OpenAI-compatible
//     endpoints and Unjailed (with the endpoint's models and, for Unjailed,
//     their context sizes);
//   - the session's persona, as an instructions file added to OpenCode's
//     own prompt;
//   - the session's agent definition as an OpenCode agent, selected with
//     --agent;
//   - with the Unjailed provider, a plugin that keeps earlier replies'
//     reasoning out of requests to the GLM models (unjailedReasoningPlugin).
//
// Its Claude Code compatibility (reading ~/.claude/CLAUDE.md and Claude
// skills), sharing and self-update are turned off in the environment.

const (
	unjailedProviderID     = "unjailed"
	unjailedDefaultBaseURL = "https://unjailedai.com/v1"
	unjailedDefaultModel   = "glm-5.3"
	unjailedSmallModel     = "glm-5.3-flash"
	// Limits OpenCode needs per model; unjailedai.com does not publish them
	// in its model list, so models without a known size get these.
	unjailedDefaultContext = 128_000
	unjailedMaxOutput      = 32_768
)

// unjailedContextWindows are the context sizes unjailedai.com lists for its
// models ("the arsenal"; "1M", "400K", "256K" and "128K" taken as 1,000,000,
// 400,000, 256,000 and 128,000 to stay below the real limit). A model the
// endpoint lists that is missing here gets unjailedDefaultContext.
var unjailedContextWindows = map[string]int{
	// Z.ai
	"glm-5.3":                   1_000_000,
	"glm-5.3-uncensored":        1_000_000,
	"glm-5.3-abliterated":       1_000_000,
	"glm-5.2":                   1_000_000,
	"glm-5.3-flash":             128_000,
	"glm-5.3-flash-uncensored":  128_000,
	"glm-5.3-flash-abliterated": 128_000,
	// DeepSeek
	"deepseek-v4.1-flash":        1_000_000,
	"deepseek-v4-flash-0731":     1_000_000,
	"deepseek-v4-pro-0813":       256_000,
	"deepseek-v4-pro-uncensored": 256_000,
	// Moonshot AI
	"kimi-k3": 1_000_000,
	// The rest of the site's models
	"gpt-6-astra":       400_000,
	"claude-fable-5":    400_000,
	"claude-opus-5-5":   400_000,
	"claude-opus-5":     400_000,
	"claude-opus-4-8":   400_000,
	"claude-opus-4-7":   400_000,
	"claude-opus-4-6":   400_000,
	"claude-sonnet-5-5": 400_000,
	"claude-sonnet-5":   400_000,
	"claude-haiku-5-5":  400_000,
	"gpt-5.5":           400_000,
	"gpt-5.6-sol":       400_000,
	"gpt-6-sol":         400_000,
	"gpt-6.1-sol":       400_000,
	"gpt-5.6-terra":     400_000,
	"gpt-5.6-luna":      400_000,
	"gpt-6-luna":        400_000,
	"grok-4.7":          400_000,
	"grok-4.6":          400_000,
	"grok-4.5":          400_000,
}

// unjailedFallbackModels is used when the endpoint's model list cannot be
// fetched at start: every model above.
var unjailedFallbackModels = func() []string {
	ids := make([]string, 0, len(unjailedContextWindows))
	for id := range unjailedContextWindows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}()

// unjailedReasoningPlugin is an OpenCode plugin the runner writes next to
// the session's other files. OpenCode sends every earlier reply's reasoning
// back as reasoning_content, and Unjailed's GLM models refuse a request with
// one over 30,000 characters ("retry with clear_thinking and without
// historical reasoning"); GLM does not use past reasoning anyway. So when
// the turn goes to a GLM model of an Unjailed provider (ids), the reasoning
// parts are left out of the request; the session keeps them. DeepSeek's
// thinking mode is the other way round (it needs the reasoning back) and is
// left alone.
func unjailedReasoningPlugin(ids []string) string {
	list, _ := json.Marshal(ids)
	return `// Written by agents-operator on every start: edits are overwritten.
const providers = ` + string(list) + `
export const AgentsOperatorUnjailedReasoning = async () => ({
  "experimental.chat.messages.transform": async (_input, output) => {
    const user = output.messages.findLast((m) => m.info.role === "user")
    const model = user?.info.model
    if (!providers.includes(model?.providerID) || !model.modelID.startsWith("glm")) return
    // Copies in the request's list: OpenCode's own message objects stay as
    // they are.
    output.messages.forEach((m, i) => {
      if (m.info.role === "assistant") output.messages[i] = { ...m, parts: m.parts.filter((p) => p.type !== "reasoning") }
    })
  },
})`
}

// fetchModels lists an endpoint's models (GET <base>/models, with the
// Anthropic headers for an Anthropic key, else a bearer token); nil when it
// cannot. Swapped out in tests.
var fetchModels = func(kind, baseURL, key string) []string {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil
	}
	if kind == store.AcctAnthropicAPIKey {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		slog.Warn("cannot list an endpoint's models", "url", baseURL, "err", err)
		return nil
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&list) != nil {
		slog.Warn("cannot list an endpoint's models", "url", baseURL, "status", resp.StatusCode)
		return nil
	}
	var ids []string
	for _, m := range list.Data {
		if m.ID != "" && len(m.ID) <= 200 {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// Model lists used when an Anthropic or OpenAI account other than the
// first cannot list its models (the first is OpenCode's own provider, which
// knows them).
var (
	anthropicFallbackModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"}
	openAIFallbackModels    = []string{"gpt-6.1-sol", "gpt-6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
)

// orList is list, or fallback when list is empty.
func orList(list, fallback []string) []string {
	if len(list) > 0 {
		return list
	}
	return fallback
}

// chatModel drops what an OpenAI model list has besides chat models
// (embeddings, speech, images, moderation).
func chatModel(id string) bool {
	for _, skip := range []string{"embedding", "tts", "whisper", "dall-e", "image", "moderation", "transcribe", "audio", "realtime", "search"} {
		if strings.Contains(id, skip) {
			return false
		}
	}
	return true
}

// providerModels is a provider's model map for OpenCode with the limits it
// needs: known context sizes for Unjailed, a safe default otherwise.
func providerModels(ids []string, known map[string]int) map[string]any {
	models := map[string]any{}
	for _, id := range ids {
		ctx := unjailedDefaultContext
		if c, ok := known[id]; ok {
			ctx = c
		}
		models[id] = map[string]any{"name": id, "limit": map[string]any{"context": ctx, "output": unjailedMaxOutput}}
	}
	return models
}

var providerIDChars = regexp.MustCompile(`[^a-z0-9]+`)

// providerID is the OpenCode provider id of an account: canonical (the
// built-in provider: anthropic, openai, unjailed) for the first account of
// its kind, else the account's name made an id ("Work LiteLLM" ->
// work-litellm), numbered on clashes.
func providerID(canonical, name string, used map[string]bool) string {
	if canonical != "" && !used[canonical] {
		used[canonical] = true
		return canonical
	}
	base := strings.Trim(providerIDChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" || base == canonical {
		base = cmp.Or(canonical, "provider") + "-account"
	}
	id := base
	for i := 2; used[id]; i++ {
		id = base + "-" + strconv.Itoa(i)
	}
	used[id] = true
	return id
}

// openCodeProviders turns the session's AI accounts (runner.EnvAIAccounts)
// into OpenCode providers, each named after its account. It also returns
// the ids of the Unjailed providers and their model lists, for the default
// model and the reasoning plugin.
func openCodeProviders(get func(string) string) (map[string]any, []string, map[string][]string) {
	var accounts []runner.AIAccount
	if v := get(runner.EnvAIAccounts); v != "" {
		if err := json.Unmarshal([]byte(v), &accounts); err != nil {
			slog.Warn("ignoring AI accounts that do not parse", "err", err)
		}
	}
	providers := map[string]any{}
	used := map[string]bool{}
	var unjailed []string
	unjailedModels := map[string][]string{}
	for _, a := range accounts {
		key := get(a.KeyEnv)
		if key == "" || !strings.HasPrefix(a.KeyEnv, runner.EnvAIKeyPrefix) {
			continue
		}
		// The key stays in the environment, out of the config text.
		options := map[string]any{"apiKey": "{env:" + a.KeyEnv + "}"}
		p := map[string]any{"name": a.Name, "options": options}
		var id string
		switch a.Kind {
		case store.AcctAnthropicAPIKey:
			id = providerID("anthropic", a.Name, used)
			base := "https://api.anthropic.com/v1"
			if a.BaseURL != "" {
				// The base URL is Claude Code's (ANTHROPIC_BASE_URL, without
				// /v1); the AI SDK wants the API root.
				base = strings.TrimSuffix(a.BaseURL, "/v1") + "/v1"
				options["baseURL"] = base
			}
			if id != "anthropic" {
				p["npm"] = "@ai-sdk/anthropic"
				p["models"] = providerModels(orList(fetchModels(a.Kind, base, key), anthropicFallbackModels), nil)
			}
		case store.AcctOpenAIAPIKey:
			id = providerID("openai", a.Name, used)
			if id != "openai" {
				ids := slices.DeleteFunc(fetchModels(a.Kind, "https://api.openai.com/v1", key), func(m string) bool { return !chatModel(m) })
				p["npm"] = "@ai-sdk/openai"
				p["models"] = providerModels(orList(ids, openAIFallbackModels), nil)
			}
		case store.AcctOpenAICompatible:
			if a.BaseURL == "" {
				continue
			}
			ids := a.Models
			if len(ids) == 0 {
				ids = fetchModels(a.Kind, a.BaseURL, key)
			}
			if len(ids) == 0 {
				slog.Warn("leaving out an OpenAI-compatible account with no models", "account", a.Name)
				continue
			}
			id = providerID("", a.Name, used)
			p["npm"] = "@ai-sdk/openai-compatible"
			options["baseURL"] = a.BaseURL
			p["models"] = providerModels(ids, nil)
		case store.AcctUnjailed:
			base := cmp.Or(a.BaseURL, unjailedDefaultBaseURL)
			ids := fetchModels(a.Kind, base, key)
			if len(ids) == 0 {
				ids = unjailedFallbackModels
			}
			id = providerID(unjailedProviderID, a.Name, used)
			p["npm"] = "@ai-sdk/openai-compatible"
			options["baseURL"] = base
			p["models"] = providerModels(ids, unjailedContextWindows)
			unjailed = append(unjailed, id)
			unjailedModels[id] = ids
		default:
			continue
		}
		providers[id] = p
	}
	return providers, unjailed, unjailedModels
}

// openCodeConfig builds the session's OpenCode configuration and the
// arguments that go with it (--agent).
func openCodeConfig(env []string) (map[string]any, []string) {
	get := func(k string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if key, v, _ := strings.Cut(env[i], "="); key == k {
				return v
			}
		}
		return ""
	}
	home := get("HOME")
	cfg := map[string]any{"autoupdate": false, "share": "disabled"}
	var args []string

	providers, unjailed, unjailedModels := openCodeProviders(get)
	if len(providers) > 0 {
		cfg["provider"] = providers
	}
	if len(unjailed) > 0 {
		first, ids := unjailed[0], unjailedModels[unjailed[0]]
		// OpenCode starts on the configured model before the one picked last
		// in its model list, so the default is only set until there is one:
		// glm-5.3 of the first Unjailed account, or its first model.
		if !openCodeHasRecentModel(home) {
			model := ids[0]
			if slices.Contains(ids, unjailedDefaultModel) {
				model = unjailedDefaultModel
			}
			cfg["model"] = first + "/" + model
		}
		if slices.Contains(ids, unjailedSmallModel) {
			cfg["small_model"] = first + "/" + unjailedSmallModel
		}
		if file := writeSessionFile(home, "unjailed-reasoning.js", unjailedReasoningPlugin(unjailed)); file != "" {
			cfg["plugin"] = []string{"file://" + file}
		}
	}

	if persona := strings.TrimSpace(get(runner.EnvPersona)); persona != "" {
		if file := writeSessionFile(home, "persona.md", persona); file != "" {
			cfg["instructions"] = []string{file}
		}
	}

	if def := get(runner.EnvAgentDefinition); def != "" {
		if d, err := agentdef.ParseFull(def + "\n"); err != nil {
			slog.Warn("ignoring an agent definition that does not parse", "err", err)
		} else {
			agent := map[string]any{"mode": "primary"}
			for k, v := range d.Meta {
				if k != "name" {
					agent[k] = v
				}
			}
			agent["prompt"] = d.Prompt
			agents, _ := cfg["agent"].(map[string]any)
			if agents == nil {
				agents = map[string]any{}
			}
			agents[d.Name] = agent
			cfg["agent"] = agents
			args = append(args, "--agent", d.Name)
		}
	}
	return cfg, args
}

// openCodeEnv is the environment OpenCode starts with: the session's, minus
// what only the runner reads, plus the configuration and the switches.
func openCodeEnv(env []string, cfg map[string]any) []string {
	out := make([]string, 0, len(env)+6)
	for _, kv := range env {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case runner.EnvPersona, runner.EnvAgentDefinition, runner.EnvAgentDefinitionName, "OPENCODE_CONFIG_CONTENT":
		default:
			out = append(out, kv)
		}
	}
	if b, err := json.Marshal(cfg); err == nil {
		out = append(out, "OPENCODE_CONFIG_CONTENT="+string(b))
	}
	return append(out,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_SHARE=1",
		// No ~/.claude/CLAUDE.md or Claude skills: AGENTS.md says it all,
		// and other agents' files have no business in this one's context.
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
	)
}

// openCodeHasRecentModel reports whether a model was picked in OpenCode's
// model list before (its state file keeps the recent ones).
func openCodeHasRecentModel(home string) bool {
	if home == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(home, ".local", "state", "opencode", "model.json"))
	if err != nil {
		return false
	}
	var state struct {
		Recent []json.RawMessage `json:"recent"`
	}
	return json.Unmarshal(raw, &state) == nil && len(state.Recent) > 0
}

// writeSessionFile writes content to a file of the runner's under
// ~/.config/opencode/agents-operator and returns its path ("" on failure):
// instructions, and the runner's plugin.
// Rewritten on every start, so it always matches the session's settings.
func writeSessionFile(home, name, content string) string {
	if home == "" {
		return ""
	}
	dir := filepath.Join(home, ".config", "opencode", "agents-operator")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("cannot write an OpenCode instructions file", "err", err)
		return ""
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		slog.Warn("cannot write an OpenCode instructions file", "err", err)
		return ""
	}
	return path
}
