package main

import (
	"encoding/json"
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

func jsonEnv(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const testKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nKEY\n-----END OPENSSH PRIVATE KEY-----\n"

// clearGitConfigEnv drops git configuration the test host may carry.
func clearGitConfigEnv(t *testing.T) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_CONFIG_") || k == "GIT_SSH_COMMAND" {
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}
}

func TestGitSSHAccounts(t *testing.T) {
	clearGitConfigEnv(t)
	t.Setenv(runner.EnvAgentGitCredentials, "true")
	t.Setenv(runner.EnvGitSSH, jsonEnv(t, []runner.GitSSHKey{{Host: "github.com", Key: testKey}, {Host: "*", Key: "-----BEGIN KEY-----\nOTHER\n-----END KEY-----"}}))
	t.Setenv(runner.EnvGitHubToken, "ghp_x")
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// The key an older runner installed, now one of the accounts' keys.
	if err := os.MkdirAll(filepath.Dir(ws.legacySSHKeyPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.legacySSHKeyPath(), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ws.installGitCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.legacySSHKeyPath()); !os.IsNotExist(err) {
		t.Fatal("the old key file was left next to the new ones")
	}
	cfg, err := os.ReadFile(filepath.Join(ws.sshAccountsDir(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	k1, k2 := filepath.Join(ws.sshAccountsDir(), "key-1"), filepath.Join(ws.sshAccountsDir(), "key-2")
	for _, want := range []string{"Host github.com\n  IdentityFile \"" + k1 + "\"", "Host * !github.com\n  IdentityFile \"" + k2 + "\"", "Include ~/.ssh/config", "StrictHostKeyChecking accept-new"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	for _, k := range []string{k1, k2} {
		st, err := os.Stat(k)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", k, st, err)
		}
	}
	if b, _ := os.ReadFile(k2); !strings.HasSuffix(string(b), "-----END KEY-----\n") {
		t.Errorf("key without a final newline: %q", b)
	}
	env := envMap(ws.Env())
	if env["GIT_SSH_COMMAND"] != "ssh -F "+filepath.Join(ws.sshAccountsDir(), "config") {
		t.Errorf("GIT_SSH_COMMAND = %q", env["GIT_SSH_COMMAND"])
	}
	// github.com has a key: no rewrite to HTTPS.
	for k, v := range env {
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") && strings.Contains(v, "insteadOf") {
			t.Errorf("github.com rewritten to HTTPS despite its SSH key")
		}
	}

	// Without keys on the next boot the directory goes.
	t.Setenv(runner.EnvGitSSH, "")
	if err := ws.installGitCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.sshAccountsDir()); !os.IsNotExist(err) {
		t.Fatal("keys of an earlier boot left behind")
	}
	if _, ok := envMap(ws.Env())["GIT_SSH_COMMAND"]; ok {
		t.Error("ssh command without keys")
	}
}

func TestWithheldGitCredentials(t *testing.T) {
	clearGitConfigEnv(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv(runner.EnvAgentGitCredentials, "false")
	https := jsonEnv(t, []runner.GitHTTPSCredential{{Host: "github.com", Token: "ghp_x"}, {Host: "*", Token: "tok"}})
	t.Setenv(runner.EnvGitSSH, jsonEnv(t, []runner.GitSSHKey{{Host: "*", Key: testKey}}))
	t.Setenv(runner.EnvGitHubToken, "ghp_x")
	t.Setenv(runner.EnvGitHTTPS, https)
	t.Setenv("GITHUB_TOKEN", "ghp_y")
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// Keys installed by an earlier boot, when the agent still had them.
	if err := os.MkdirAll(ws.sshAccountsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.sshAccountsDir(), "key-1"), []byte("OLD\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// init moves the tokens out of PID 1's environment along with the rest.
	_, private := splitPrivateEnv([]string{"GH_TOKEN=a", runner.EnvGitHTTPS + "=b", "GITHUB_TOKEN=c", runner.EnvGitSSH + "=d", "PATH=/bin"})
	if len(private) != 4 {
		t.Fatalf("private = %v", private)
	}

	if err := ws.installGitCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.sshAccountsDir()); !os.IsNotExist(err) {
		t.Fatal("keys from an earlier boot left in HOME")
	}
	if ws.cloneSSHDir == "" || filepath.Dir(ws.cloneSSHDir) != tmp {
		t.Fatalf("clone keys in %q", ws.cloneSSHDir)
	}

	// The agent gets none of it.
	agent := envMap(ws.Env())
	for _, k := range []string{runner.EnvGitHubToken, "GITHUB_TOKEN", runner.EnvGitHTTPS, runner.EnvGitSSH, "GIT_SSH_COMMAND", "GIT_CONFIG_COUNT"} {
		if _, ok := agent[k]; ok {
			t.Errorf("agent env carries %s", k)
		}
	}

	// The clones get all of it.
	clone := envMap(ws.cloneEnv())
	if clone[runner.EnvGitHubToken] != "ghp_x" || clone[runner.EnvGitHTTPS] != https || clone["GIT_CONFIG_KEY_0"] != "credential.helper" {
		t.Fatalf("clone env = %v", clone)
	}
	if !strings.Contains(clone["GIT_SSH_COMMAND"], ws.cloneSSHDir) {
		t.Fatalf("clone ssh command = %q", clone["GIT_SSH_COMMAND"])
	}

	// And the keys are gone before the agent starts.
	dir := ws.cloneSSHDir
	ws.dropCloneKey()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("clone keys left behind")
	}
	if _, ok := envMap(ws.cloneEnv())["GIT_SSH_COMMAND"]; ok {
		t.Fatal("ssh command still points at the dropped keys")
	}
}

func TestGitHTTPSFor(t *testing.T) {
	list := []runner.GitHTTPSCredential{
		{Host: "*", Token: "any"},
		{Host: "*.corp.example", Token: "corp"},
		{Host: "github.com", Token: "gh"},
		{Host: "git.example.com:8443", Token: "port"},
	}
	for host, want := range map[string]string{
		"github.com":           "gh",
		"GitHub.com":           "gh",
		"git.corp.example":     "corp",
		"gitlab.com":           "any",
		"git.example.com:8443": "port",
		"github.com:443":       "gh",
	} {
		if got := gitHTTPSFor(list, host); got == nil || got.Token != want {
			t.Errorf("%s: %+v, want %s", host, got, want)
		}
	}
	if got := gitHTTPSFor(list[1:3], "gitlab.com"); got != nil {
		t.Errorf("no account for any host, got %+v", got)
	}
}

func TestAgentGitCredentialsDefault(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "false": false, "FALSE": false, "0": false} {
		t.Setenv(runner.EnvAgentGitCredentials, v)
		if got := agentGitCredentials(); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
	// With the agent allowed, the tokens are not runner-only; the SSH keys
	// always are (the runner writes them to files).
	t.Setenv(runner.EnvAgentGitCredentials, "true")
	if runnerPrivate(runner.EnvGitHubToken) || runnerPrivate(runner.EnvGitHTTPS) || !runnerPrivate(runner.EnvGitSSH) {
		t.Fatal("wrong split of git credentials")
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
	t.Setenv(runner.EnvGitSSH, jsonEnv(t, []runner.GitSSHKey{{Host: "gitlab.com", Key: testKey}}))
	t.Setenv(runner.EnvGitHubToken, "ghp_x")
	t.Setenv(runner.EnvGitHTTPS, "")

	// Withheld: the agent is told it cannot push, whatever the account holds.
	t.Setenv(runner.EnvAgentGitCredentials, "false")
	s := read()
	if !strings.Contains(s, "owner turned them off") || strings.Contains(s, "authenticated") || strings.Contains(s, "SSH has keys") || strings.Contains(s, "open pull requests") {
		t.Fatalf("withheld:\n%s", s)
	}

	// Allowed: hosts are listed, github.com has no key so it goes over HTTPS.
	t.Setenv(runner.EnvAgentGitCredentials, "true")
	s = read()
	if !strings.Contains(s, "SSH has keys for `gitlab.com`") || !strings.Contains(s, "rewritten to HTTPS") || !strings.Contains(s, "authenticated") {
		t.Fatalf("allowed:\n%s", s)
	}

	// Nothing on the account: same as withheld, for a different reason.
	t.Setenv(runner.EnvGitSSH, "")
	t.Setenv(runner.EnvGitHubToken, "")
	s = read()
	if !strings.Contains(s, "No git credentials are configured") || strings.Contains(s, "open pull requests") || !strings.Contains(s, "the owner pushes it") {
		t.Fatalf("none configured:\n%s", s)
	}

	// An HTTPS token alone is enough to push.
	t.Setenv(runner.EnvGitHTTPS, jsonEnv(t, []runner.GitHTTPSCredential{{Host: "git.example.com", Token: "t"}, {Host: "*", Token: "tok"}}))
	s = read()
	if !strings.Contains(s, "HTTPS has tokens for `git.example.com`, any other host") || !strings.Contains(s, "open pull requests") || strings.Contains(s, "No git credentials") {
		t.Fatalf("https token:\n%s", s)
	}
}
