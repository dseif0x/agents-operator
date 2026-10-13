package main

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/agentdef"
	"github.com/dseif0x/agents-operator/internal/runner"
)

// OpenCode sessions get their settings through OPENCODE_CONFIG_CONTENT,
// which OpenCode merges over its own config files, so nothing the user keeps
// in ~/.config/opencode is rewritten:
//
//   - with an Unjailed key on the account, an "unjailed" provider for
//     unjailedai.com (or the account's base URL) through OpenCode's
//     OpenAI-compatible provider, with the endpoint's models and their
//     context sizes;
//   - the session's custom system prompt: replacing OpenCode's own as the
//     prompt of its default "build" agent, or appended as an instructions
//     file;
//   - the session's agent definition as an OpenCode agent, selected with
//     --agent.
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

// fetchModels lists the endpoint's models (GET <base>/models); nil when it
// cannot. Swapped out in tests.
var fetchModels = func(baseURL, key string) []string {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		slog.Warn("cannot list the Unjailed models; using the built-in list", "err", err)
		return nil
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&list) != nil {
		slog.Warn("cannot list the Unjailed models; using the built-in list", "status", resp.StatusCode)
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

	if key := get(runner.EnvUnjailedAPIKey); key != "" {
		base := strings.TrimRight(cmp.Or(get(runner.EnvUnjailedBaseURL), unjailedDefaultBaseURL), "/")
		ids := fetchModels(base, key)
		if len(ids) == 0 {
			ids = unjailedFallbackModels
		}
		models := map[string]any{}
		for _, id := range ids {
			ctx := unjailedDefaultContext
			if c, ok := unjailedContextWindows[id]; ok {
				ctx = c
			}
			models[id] = map[string]any{"name": id, "limit": map[string]any{"context": ctx, "output": unjailedMaxOutput}}
		}
		cfg["provider"] = map[string]any{unjailedProviderID: map[string]any{
			"npm":  "@ai-sdk/openai-compatible",
			"name": "Unjailed",
			// The key stays in the environment, out of the config text.
			"options": map[string]any{"baseURL": base, "apiKey": "{env:" + runner.EnvUnjailedAPIKey + "}"},
			"models":  models,
		}}
		// OpenCode starts on the configured model before the one picked last
		// in its model list, so the default is only set until there is one:
		// glm-5.3, or the endpoint's first model when it has no such one.
		if !openCodeHasRecentModel(home) {
			model := ids[0]
			if slices.Contains(ids, unjailedDefaultModel) {
				model = unjailedDefaultModel
			}
			cfg["model"] = unjailedProviderID + "/" + model
		}
		if slices.Contains(ids, unjailedSmallModel) {
			cfg["small_model"] = unjailedProviderID + "/" + unjailedSmallModel
		}
	}

	if prompt := strings.TrimSpace(get(runner.EnvSystemPrompt)); prompt != "" {
		if get(runner.EnvSystemPromptMode) == "replace" {
			cfg["agent"] = map[string]any{"build": map[string]any{"prompt": prompt}}
		} else if file := writeSessionFile(home, "system-prompt.md", prompt); file != "" {
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
		case runner.EnvSystemPrompt, runner.EnvSystemPromptMode, runner.EnvAgentDefinition, runner.EnvAgentDefinitionName, "OPENCODE_CONFIG_CONTENT":
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
// ~/.config/opencode/agents-operator and returns its path ("" on failure).
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
