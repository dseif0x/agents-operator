package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestWithheldGitCredentials(t *testing.T) {
	// The test host may carry git configuration in its environment.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_CONFIG_") {
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv(runner.EnvAgentGitCredentials, "false")
	t.Setenv(runner.EnvGitSSHKey, "KEY")
	t.Setenv(runner.EnvGitHubToken, "ghp_x")
	t.Setenv(runner.EnvGitHTTPSToken, "tok")
	t.Setenv("GITHUB_TOKEN", "ghp_y")
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := os.MkdirAll(filepath.Dir(ws.sshKeyPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	// A key installed by an earlier boot, when the agent still had it.
	if err := os.WriteFile(ws.sshKeyPath(), []byte("OLD\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// init moves the tokens out of PID 1's environment along with the rest.
	_, private := splitPrivateEnv([]string{"GH_TOKEN=a", "GIT_HTTPS_TOKEN=b", "GITHUB_TOKEN=c", "PATH=/bin"})
	if len(private) != 3 {
		t.Fatalf("private = %v", private)
	}

	if err := ws.installGitCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.sshKeyPath()); !os.IsNotExist(err) {
		t.Fatal("key from an earlier boot left in HOME")
	}
	if ws.cloneKey == "" || filepath.Dir(ws.cloneKey) != tmp {
		t.Fatalf("clone key = %q", ws.cloneKey)
	}
	if st, err := os.Stat(ws.cloneKey); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("clone key mode: %v %v", st, err)
	}

	// The agent gets none of it.
	agent := envMap(ws.Env())
	for _, k := range []string{runner.EnvGitHubToken, "GITHUB_TOKEN", runner.EnvGitHTTPSToken, runner.EnvGitSSHKey} {
		if _, ok := agent[k]; ok {
			t.Errorf("agent env carries %s", k)
		}
	}
	if _, ok := agent["GIT_CONFIG_COUNT"]; ok {
		t.Errorf("agent env configures git: %v", agent)
	}
	if strings.Contains(agent["GIT_SSH_COMMAND"], ws.cloneKey) || strings.Contains(agent["GIT_SSH_COMMAND"], ws.sshKeyPath()) {
		t.Errorf("agent env points ssh at a key: %q", agent["GIT_SSH_COMMAND"])
	}

	// The clones get all of it.
	clone := envMap(ws.cloneEnv())
	if clone[runner.EnvGitHubToken] != "ghp_x" || clone[runner.EnvGitHTTPSToken] != "tok" || clone["GIT_CONFIG_KEY_0"] != "credential.helper" {
		t.Fatalf("clone env = %v", clone)
	}
	if !strings.Contains(clone["GIT_SSH_COMMAND"], ws.cloneKey) {
		t.Fatalf("clone ssh command = %q", clone["GIT_SSH_COMMAND"])
	}

	// And the key is gone before the agent starts.
	key := ws.cloneKey
	ws.dropCloneKey()
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatal("clone key left behind")
	}
}

func TestAgentGitCredentialsDefault(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "false": false, "FALSE": false, "0": false} {
		t.Setenv(runner.EnvAgentGitCredentials, v)
		if got := agentGitCredentials(); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
	// With the agent allowed, the tokens are not runner-only.
	t.Setenv(runner.EnvAgentGitCredentials, "true")
	if runnerPrivate(runner.EnvGitHubToken) || runnerPrivate(runner.EnvGitHTTPSToken) {
		t.Fatal("tokens withheld from an agent that may have them")
	}
}

func TestAgentsFileGitCredentials(t *testing.T) {
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	read := func() string {
		t.Helper()
		if err := ws.writeAgentsFile(nil); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(ws.AgentsFile())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Setenv(runner.EnvGitSSHKey, "KEY")
	t.Setenv(runner.EnvGitHubToken, "ghp_x")
	t.Setenv(runner.EnvGitHTTPSToken, "")

	// Withheld: the agent is told it cannot push, whatever the account holds.
	t.Setenv(runner.EnvAgentGitCredentials, "false")
	s := read()
	if !strings.Contains(s, "owner turned them off") || strings.Contains(s, "authenticated") || strings.Contains(s, "deploy key") || strings.Contains(s, "open pull requests") {
		t.Fatalf("withheld:\n%s", s)
	}

	// Nothing on the account: same, for a different reason.
	t.Setenv(runner.EnvAgentGitCredentials, "true")
	t.Setenv(runner.EnvGitSSHKey, "")
	t.Setenv(runner.EnvGitHubToken, "")
	s = read()
	if !strings.Contains(s, "No git credentials are configured") || strings.Contains(s, "open pull requests") || !strings.Contains(s, "the owner pushes it") {
		t.Fatalf("none configured:\n%s", s)
	}

	// An HTTPS token alone is enough to push.
	t.Setenv(runner.EnvGitHTTPSToken, "tok")
	s = read()
	if !strings.Contains(s, "HTTPS uses a configured token") || !strings.Contains(s, "open pull requests") || strings.Contains(s, "No git credentials") {
		t.Fatalf("https token:\n%s", s)
	}
}
