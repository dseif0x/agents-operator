package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

func TestReposAndWorkDir(t *testing.T) {
	root := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Setenv(runner.EnvRepos, `[{"url":"https://x/app.git","branch":"main","path":"app"},{"url":"https://x/lib.git","path":"lib"}]`)
	ws := &Workspace{Root: root, Log: log}
	repos, err := ws.Repos()
	if err != nil || len(repos) != 2 || repos[0].Path != "app" || repos[1].Path != "lib" {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	// The agent always starts in the workspace root, next to AGENTS.md,
	// never inside a repository.
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ws.WorkDir(); got != root {
		t.Fatalf("WorkDir = %q, want %q", got, root)
	}
	if got := (&Workspace{Root: filepath.Join(root, "missing"), Log: log}).WorkDir(); got != "" {
		t.Fatalf("WorkDir for a missing root = %q", got)
	}

	// Unsafe paths are refused even if the hub sent them.
	for _, bad := range []string{`[{"url":"https://x/a","path":"../etc"}]`, `[{"url":"https://x/a","path":"home"}]`, `[{"url":"https://x/a","path":"a/b"}]`, `not json`} {
		t.Setenv(runner.EnvRepos, bad)
		if _, err := (&Workspace{Root: root, Log: log}).Repos(); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}

	// No REPOS at all is fine: empty workspace.
	t.Setenv(runner.EnvRepos, "")
	empty := &Workspace{Root: root, Log: log}
	if repos, err := empty.Repos(); err != nil || len(repos) != 0 {
		t.Fatalf("empty repos = %+v, %v", repos, err)
	}
}

func TestEnvHidesSecretsAndAddsGitHubToken(t *testing.T) {
	t.Setenv(runner.EnvRunnerToken, "secret")
	t.Setenv(runner.EnvGitSSHKey, "KEY")
	t.Setenv(runner.EnvSeedPrefix+"CLAUDE_LOGIN", "abc")
	t.Setenv(runner.EnvGitHubToken, "ghp_x")
	t.Setenv("GITHUB_TOKEN", "")
	os.Unsetenv("GITHUB_TOKEN")
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := map[string]string{}
	for _, kv := range ws.Env() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if _, ok := env[runner.EnvRunnerToken]; ok {
		t.Fatal("runner token leaked into the agent env")
	}
	if _, ok := env[runner.EnvGitSSHKey]; ok {
		t.Fatal("ssh key leaked into the agent env")
	}
	if _, ok := env[runner.EnvSeedPrefix+"CLAUDE_LOGIN"]; ok {
		t.Fatal("login seed leaked into the agent env")
	}
	if env["GITHUB_TOKEN"] != "ghp_x" || env["GH_TOKEN"] != "ghp_x" {
		t.Fatalf("github token env = %q / %q", env["GH_TOKEN"], env["GITHUB_TOKEN"])
	}
	if env["HOME"] != ws.HomeDir() || env["GIT_SSH_COMMAND"] == "" {
		t.Fatalf("env = %v", env)
	}
	// With an SSH key present, remotes are not rewritten but the helper is on.
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "credential.helper" {
		t.Fatalf("git config env = %v", env)
	}

	// Token only: git@github.com: remotes go over HTTPS.
	t.Setenv(runner.EnvGitSSHKey, "")
	env = map[string]string{}
	for _, kv := range ws.Env() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["GIT_CONFIG_COUNT"] != "3" || env["GIT_CONFIG_KEY_1"] != "url.https://github.com/.insteadOf" || env["GIT_CONFIG_VALUE_1"] != "git@github.com:" {
		t.Fatalf("rewrite config env = %v", env)
	}
	// The test host may carry its own GIT_SSH_COMMAND; ours must not be there.
	if v := env["GIT_SSH_COMMAND"]; strings.Contains(v, ws.sshKeyPath()) {
		t.Fatal("GIT_SSH_COMMAND points at a key that was not installed")
	}
}

func TestAgentsFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv(runner.EnvGitHubToken, "ghp_x")
	t.Setenv(runner.EnvGitSSHKey, "")
	t.Setenv(runner.EnvGitUserName, "Alice")
	t.Setenv(runner.EnvGitUserEmail, "a@b.c")
	ws := &Workspace{Root: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := os.MkdirAll(filepath.Join(ws.HomeDir(), ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hand-written global file must survive.
	own := filepath.Join(ws.HomeDir(), ".claude", "CLAUDE.md")
	if err := os.WriteFile(own, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repos := []store.Repo{{URL: "git@github.com:x/app.git", Branch: "main", Path: "app"}, {URL: "https://github.com/x/lib.git", Path: "lib"}}
	if err := ws.writeAgentsFile(repos); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ws.AgentsFile())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{agentsMarker, filepath.Join(root, "app"), "git@github.com:x/app.git", "branch `main`", "You start in `" + root + "`", filepath.Join(root, "lib"), "`gh` is installed and authenticated", "rewritten to HTTPS", "Alice <a@b.c>"} {
		if !strings.Contains(s, want) {
			t.Errorf("AGENTS.md missing %q", want)
		}
	}
	if got, _ := os.ReadFile(own); string(got) != "# mine\n" {
		t.Fatal("user's CLAUDE.md was overwritten")
	}
	for _, p := range []string{filepath.Join(root, "CLAUDE.md"), filepath.Join(ws.HomeDir(), ".codex", "AGENTS.md"), filepath.Join(ws.HomeDir(), ".config", "opencode", "AGENTS.md")} {
		if got, err := os.ReadFile(p); err != nil || string(got) != s {
			t.Errorf("%s not generated: %v", p, err)
		}
	}
	// A second boot regenerates the generated files.
	if err := ws.writeAgentsFile(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(ws.HomeDir(), ".codex", "AGENTS.md")); !strings.Contains(string(got), "No repository was cloned") {
		t.Fatal("generated file not refreshed")
	}
}

func TestAgentCommand(t *testing.T) {
	c := AgentCommand(runner.AgentClaude, true, "/w", nil)
	if c.Path != "claude" || len(c.Args) != 1 || c.Args[0] != "--dangerously-skip-permissions" {
		t.Fatalf("claude autonomous = %+v", c)
	}
	if c := AgentCommand(runner.AgentClaude, false, "/w", nil); len(c.Args) != 0 {
		t.Fatalf("claude non-autonomous = %+v", c)
	}
	if c := AgentCommand(runner.AgentShell, true, "/w", nil); c.Path != "bash" {
		t.Fatalf("shell = %+v", c)
	}
}
