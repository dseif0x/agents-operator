package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
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
	// Before the clone the first repo dir does not exist: fall back to the root.
	if got := ws.WorkDir(); got != root {
		t.Fatalf("WorkDir before clone = %q", got)
	}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ws.WorkDir(); got != filepath.Join(root, "app") {
		t.Fatalf("WorkDir = %q", got)
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
		k, v, _ := splitKV(kv)
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
}

func splitKV(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
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
