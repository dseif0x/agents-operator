package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// The owner's MCP servers and skills the session picked
// (runner.EnvMCPServers, runner.EnvSkills). The tokens stay in their
// environment variables; every CLI's configuration names the variable and
// the CLI fills it in (Claude ${VAR}, OpenCode {env:VAR}, Codex
// bearer_token_env_var), so no token is written to disk.

// sessionMCPServers are the remote MCP servers in env.
func sessionMCPServers(env []string) []runner.MCPServer {
	var servers []runner.MCPServer
	if v := envValue(env, runner.EnvMCPServers); v != "" {
		_ = json.Unmarshal([]byte(v), &servers)
	}
	return slices.DeleteFunc(servers, func(s runner.MCPServer) bool {
		return s.Name == "" || s.Name == mcpServerName || s.URL == "" || strings.ContainsAny(s.Name, ".\"' ")
	})
}

// claudeMCPEntry is a server in Claude Code's MCP configuration.
func claudeMCPEntry(s runner.MCPServer) map[string]any {
	e := map[string]any{"type": "http", "url": s.URL}
	if s.TokenEnv != "" {
		e["headers"] = map[string]string{"Authorization": "Bearer ${" + s.TokenEnv + "}"}
	}
	return e
}

// openCodeMCPEntry is a server in OpenCode's configuration.
func openCodeMCPEntry(s runner.MCPServer) map[string]any {
	e := map[string]any{"type": "remote", "url": s.URL, "enabled": true}
	if s.TokenEnv != "" {
		e["headers"] = map[string]string{"Authorization": "Bearer {env:" + s.TokenEnv + "}"}
	}
	return e
}

// codexMCPArgs are Codex's -c settings for the servers.
func codexMCPArgs(servers []runner.MCPServer) []string {
	var args []string
	for _, s := range servers {
		args = append(args, "-c", "mcp_servers."+s.Name+".url="+tomlString(s.URL))
		if s.TokenEnv != "" {
			args = append(args, "-c", "mcp_servers."+s.Name+".bearer_token_env_var="+tomlString(s.TokenEnv))
		}
	}
	return args
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	b, _ := json.Marshal(s) // JSON string escapes are valid TOML
	return string(b)
}

// skillDirs are where the CLIs look for skills: Claude Code (OpenCode reads
// it too) and Codex.
func skillDirs(home string) []string {
	return []string{filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".codex", "skills")}
}

// skillsManifest lists the skill directories the runner wrote, so a skill
// the owner removed goes and skills the agent made itself stay.
func skillsManifest(home string) string {
	return filepath.Join(home, ".config", "agents-operator", "skills.json")
}

// writeSkills writes the session's skills as <dir>/<name>/SKILL.md for
// every CLI and removes the ones it wrote before that are gone.
func (w *Workspace) writeSkills() {
	home := w.HomeDir()
	var skills []runner.SkillDef
	if v := os.Getenv(runner.EnvSkills); v != "" {
		if err := json.Unmarshal([]byte(v), &skills); err != nil {
			w.Log.Warn("cannot read the session's skills", "err", err)
		}
	}
	// The manifest holds the directories written, relative to home.
	var before []string
	if b, err := os.ReadFile(skillsManifest(home)); err == nil {
		_ = json.Unmarshal(b, &before)
	}
	var written []string
	for _, s := range skills {
		if s.Name == "" || strings.ContainsAny(s.Name, "/\\.") {
			continue
		}
		for _, dir := range skillDirs(home) {
			d := filepath.Join(dir, s.Name)
			rel, _ := filepath.Rel(home, d)
			if _, err := os.Stat(d); err == nil && !slices.Contains(before, rel) {
				w.Log.Warn("a skill of that name exists already; leaving it", "dir", d)
				continue
			}
			if err := os.MkdirAll(d, 0o755); err != nil {
				w.Log.Warn("cannot write a skill", "dir", d, "err", err)
				continue
			}
			if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(skillFile(s)), 0o644); err != nil {
				w.Log.Warn("cannot write a skill", "dir", d, "err", err)
				continue
			}
			written = append(written, rel)
		}
	}
	for _, rel := range before {
		if slices.Contains(written, rel) || !managedSkillPath(rel) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(home, rel))
	}
	if len(written) == 0 && len(before) == 0 {
		return
	}
	b, _ := json.Marshal(written)
	if err := os.MkdirAll(filepath.Dir(skillsManifest(home)), 0o700); err == nil {
		_ = os.WriteFile(skillsManifest(home), b, 0o600)
	}
}

// managedSkillPath reports whether rel (from the manifest) is a skill
// directory the runner may remove: <.claude|.codex>/skills/<name>.
func managedSkillPath(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) == 3 && (parts[0] == ".claude" || parts[0] == ".codex") && parts[1] == "skills" && parts[2] != "" && parts[2] != "." && parts[2] != ".."
}

// skillFile is a skill's SKILL.md: front matter, then the instructions.
func skillFile(s runner.SkillDef) string {
	desc, _ := json.Marshal(s.Description) // a YAML double-quoted string
	return "---\nname: " + s.Name + "\ndescription: " + string(desc) + "\n---\n\n" + strings.TrimSpace(s.Instructions) + "\n"
}
