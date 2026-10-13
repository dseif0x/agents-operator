package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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
	if env["HOME"] != ws.HomeDir() || env["GIT_SSH_COMMAND"] == "" || env["TMPDIR"] != ws.TempDir() {
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
	// Hermetic even inside a pod that has a token and access env of its own.
	saTokenPath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" })
	for _, k := range []string{runner.EnvK8sAccess, runner.EnvK8sNamespaces, runner.EnvSandboxNamespace, runner.EnvSandboxHost, runner.EnvSandboxSecurity} {
		t.Setenv(k, "")
	}
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
	if !strings.Contains(s, "no Kubernetes credentials") || strings.Contains(s, "read-only Kubernetes ServiceAccount") {
		t.Fatal("AGENTS.md claims cluster access without a token")
	}
	// With a ServiceAccount token mounted, the agent is told what it may do.
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	saTokenPath = token
	t.Cleanup(func() { saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" })
	if err := ws.writeAgentsFile(repos); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ws.AgentsFile()); !strings.Contains(string(b), "read-only Kubernetes ServiceAccount") {
		t.Fatal("AGENTS.md does not mention the mounted ServiceAccount")
	}
	// In namespace mode it is told where it may write.
	t.Setenv(runner.EnvK8sAccess, "namespace")
	t.Setenv(runner.EnvK8sNamespaces, "dev, staging")
	if err := ws.writeAgentsFile(repos); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ws.AgentsFile()); !strings.Contains(string(b), "write access") || !strings.Contains(string(b), "`dev`, `staging`") || strings.Contains(string(b), "read-only Kubernetes ServiceAccount") {
		t.Fatalf("AGENTS.md in namespace mode:\n%s", b)
	}
	// In sandbox mode (no host token) it is told about its own cluster and the host's limits.
	saTokenPath = filepath.Join(t.TempDir(), "absent")
	t.Setenv(runner.EnvK8sAccess, "sandbox")
	t.Setenv(runner.EnvSandboxNamespace, "sandbox-123")
	t.Setenv(runner.EnvSandboxHost, "demo.sandbox.example.com")
	t.Setenv(runner.EnvSandboxSecurity, "restricted")
	if err := ws.writeAgentsFile(repos); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(ws.AgentsFile())
	for _, want := range []string{"virtual cluster (vcluster)", "cluster-admin", "`sandbox-123`", "`restricted` Pod Security Standard", "`app.demo.sandbox.example.com`", "deleted together with the session"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("AGENTS.md in sandbox mode lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "no Kubernetes credentials") {
		t.Fatal("sandbox mode reported as no access")
	}
	t.Setenv(runner.EnvSandboxHost, "")
	if err := ws.writeAgentsFile(repos); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ws.AgentsFile()); !strings.Contains(string(b), "port-forward") {
		t.Fatal("sandbox without a domain does not point to port-forward")
	}
	// A second boot regenerates the generated files.
	if err := ws.writeAgentsFile(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(ws.HomeDir(), ".codex", "AGENTS.md")); !strings.Contains(string(got), "No repository was cloned") {
		t.Fatal("generated file not refreshed")
	}
}

func TestSeedLogins(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	bundle, _ := runner.EncodeLoginBundle(runner.LoginBundle{Files: map[string][]byte{
		".claude/.credentials.json": []byte(`{"tok":1}`),
		".claude.json":              []byte(`{"hasCompletedOnboarding":true}`),
		"../../etc/passwd":          []byte("nope"), // never written: not a known file
	}})
	t.Setenv(runner.EnvSeedPrefix+"CLAUDE_LOGIN", base64.StdEncoding.EncodeToString(bundle))
	// Legacy raw value for codex.
	t.Setenv(runner.EnvSeedPrefix+"CODEX_LOGIN", base64.StdEncoding.EncodeToString([]byte(`{"codex":1}`)))
	ws.seedLogins()
	for rel, want := range map[string]string{
		".claude/.credentials.json": `{"tok":1}`,
		".claude.json":              `{"hasCompletedOnboarding":true}`,
		".codex/auth.json":          `{"codex":1}`,
	} {
		got, err := os.ReadFile(filepath.Join(ws.HomeDir(), rel))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", rel, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "passwd")); err == nil {
		t.Fatal("unknown bundle path was written")
	}
	// A later boot keeps the session's own (possibly refreshed) files.
	if err := os.WriteFile(filepath.Join(ws.HomeDir(), ".claude", ".credentials.json"), []byte(`{"tok":"newer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ws.seedLogins()
	if got, _ := os.ReadFile(filepath.Join(ws.HomeDir(), ".claude", ".credentials.json")); string(got) != `{"tok":"newer"}` {
		t.Fatal("seed overwrote an existing file")
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
	if c := AgentCommand(runner.AgentOpenClaude, true, "/w", nil); c.Path != "openclaude" || len(c.Args) != 1 || c.Args[0] != "--dangerously-skip-permissions" {
		t.Fatalf("openclaude autonomous = %+v", c)
	}
	if c := AgentCommand(runner.AgentOpenClaude, false, "/w", nil); len(c.Args) != 0 {
		t.Fatalf("openclaude supervised = %+v", c)
	}
}

func TestPrepareClaudeConfig(t *testing.T) {
	root := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws := &Workspace{Root: root, Agent: runner.AgentClaude, Log: log}
	if err := os.MkdirAll(ws.HomeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(ws.HomeDir(), ".claude.json")
	read := func() map[string]any {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("invalid .claude.json: %v\n%s", err, b)
		}
		return m
	}
	trusted := func(m map[string]any, dir string) bool {
		projects, _ := m["projects"].(map[string]any)
		p, _ := projects[dir].(map[string]any)
		return p["hasTrustDialogAccepted"] == true
	}
	repos := []store.Repo{{URL: "https://x/app.git", Path: "app"}}
	t.Setenv(runner.EnvClaudeOAuthToken, "")
	t.Setenv(runner.EnvAnthropicAPIKey, "")

	// No credentials: the directories are trusted, onboarding stays with the CLI.
	ws.prepareClaudeConfig(repos)
	m := read()
	if m["hasCompletedOnboarding"] != nil {
		t.Fatalf("onboarding marked complete without credentials: %v", m)
	}
	if !trusted(m, root) || !trusted(m, filepath.Join(root, "app")) {
		t.Fatalf("directories not trusted: %v", m)
	}

	// An API key from the environment: onboarding done, key pre-approved by its suffix.
	key := "sk-ant-api03-0123456789abcdefghijklmnopqrstuvwxyz"
	t.Setenv(runner.EnvAnthropicAPIKey, key)
	ws.prepareClaudeConfig(repos)
	m = read()
	if m["hasCompletedOnboarding"] != true {
		t.Fatalf("onboarding not marked complete: %v", m)
	}
	resp, _ := m["customApiKeyResponses"].(map[string]any)
	approved, _ := resp["approved"].([]any)
	if len(approved) != 1 || approved[0] != key[len(key)-claudeAPIKeySuffix:] {
		t.Fatalf("approved = %v", approved)
	}
	// Idempotent: a second boot leaves the file byte for byte as it was.
	before, _ := os.ReadFile(cfgPath)
	ws.prepareClaudeConfig(repos)
	if after, _ := os.ReadFile(cfgPath); !bytes.Equal(before, after) {
		t.Fatalf("rewritten without changes:\n%s\n---\n%s", before, after)
	}

	// A seeded .claude.json keeps every value it had; numbers stay exact.
	seeded := `{"hasCompletedOnboarding":false,"oauthAccount":{"emailAddress":"a@b.c"},"firstStartTime":1759100000123,"projects":{"/elsewhere":{"hasTrustDialogAccepted":false}}}`
	if err := os.WriteFile(cfgPath, []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws.HomeDir(), ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.HomeDir(), ".claude", ".credentials.json"), []byte(`{"tok":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runner.EnvAnthropicAPIKey, "")
	ws.prepareClaudeConfig(nil)
	b, _ := os.ReadFile(cfgPath)
	for _, want := range []string{`"hasCompletedOnboarding": false`, `"emailAddress": "a@b.c"`, `1759100000123`, `"/elsewhere"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf(".claude.json lost %s:\n%s", want, b)
		}
	}
	if m = read(); !trusted(m, root) || trusted(m, "/elsewhere") {
		t.Fatalf("trust = %v", m["projects"])
	}
	if _, ok := m["customApiKeyResponses"]; ok {
		t.Fatal("API key approval added without a key")
	}

	// Something that is not a JSON object is left alone.
	if err := os.WriteFile(cfgPath, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws.prepareClaudeConfig(nil)
	if b, _ := os.ReadFile(cfgPath); string(b) != "nope" {
		t.Fatalf("invalid file rewritten: %s", b)
	}

	// Other agents get no Claude config at all.
	other := &Workspace{Root: t.TempDir(), Agent: runner.AgentCodex, Log: log}
	if err := os.MkdirAll(other.HomeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	other.prepareClaudeConfig(repos)
	if _, err := os.Stat(filepath.Join(other.HomeDir(), ".claude.json")); err == nil {
		t.Fatal(".claude.json written for a codex session")
	}
}

func TestPrepareClaudeSettings(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, Agent: runner.AgentClaude, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	settings := filepath.Join(ws.HomeDir(), ".claude", "settings.json")
	read := func() map[string]any {
		b, err := os.ReadFile(settings)
		if err != nil {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("invalid settings.json: %v\n%s", err, b)
		}
		return m
	}

	// Not autonomous: the consent dialog stays, but the activity hooks are
	// installed for every event, once each.
	t.Setenv(runner.EnvAutonomous, "false")
	ws.prepareClaudeSettings()
	m := read()
	if m == nil || m["skipDangerousModePermissionPrompt"] != nil {
		t.Fatalf("supervised session: %v", m)
	}
	hooks, _ := m["hooks"].(map[string]any)
	for _, ev := range claudeHookEvents {
		groups, _ := hooks[ev].([]any)
		if !hasRunnerHook(groups) {
			t.Fatalf("no runner hook for %s: %v", ev, hooks[ev])
		}
		group := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		if group["type"] != "command" || group["async"] != true {
			t.Fatalf("hook for %s = %v", ev, group)
		}
	}
	before, _ := os.ReadFile(settings)
	ws.prepareClaudeSettings()
	if after, _ := os.ReadFile(settings); string(after) != string(before) {
		t.Fatalf("hooks added twice:\n%s", after)
	}
	// A hook the user wrote stays, and ours is added next to it, not over it.
	if err := os.WriteFile(settings, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify-send done"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ws.prepareClaudeSettings()
	m = read()
	stop, _ := m["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 2 || stop[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "notify-send done" || !hasRunnerHook(stop) {
		t.Fatalf("user hook lost: %v", stop)
	}
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}

	// Autonomous: consent recorded; the user's other settings survive.
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"model":"opus","permissions":{"allow":["Bash(ls)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runner.EnvAutonomous, "true")
	ws.prepareClaudeSettings()
	m = read()
	if m["skipDangerousModePermissionPrompt"] != true || m["model"] != "opus" {
		t.Fatalf("settings = %v", m)
	}
	if perms, _ := m["permissions"].(map[string]any); perms == nil || perms["allow"] == nil {
		t.Fatalf("permissions lost: %v", m)
	}
	// A user who turned it off explicitly keeps their choice.
	if err := os.WriteFile(settings, []byte(`{"skipDangerousModePermissionPrompt":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ws.prepareClaudeSettings()
	if m := read(); m["skipDangerousModePermissionPrompt"] != false {
		t.Fatalf("explicit false overridden: %v", m)
	}
	// A missing file is created.
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	ws.prepareClaudeSettings()
	if m := read(); m == nil || m["skipDangerousModePermissionPrompt"] != true || m["hooks"] == nil {
		t.Fatalf("settings not created: %v", m)
	}
	// Other agents get nothing.
	other := &Workspace{Root: t.TempDir(), Agent: runner.AgentCodex, Log: ws.Log}
	other.prepareClaudeSettings()
	if _, err := os.Stat(filepath.Join(other.HomeDir(), ".claude", "settings.json")); err == nil {
		t.Fatal("settings written for a codex session")
	}
}

func TestAgentsFileEgress(t *testing.T) {
	ws := &Workspace{Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	read := func() string {
		t.Helper()
		if err := ws.writeAgentsFile(nil); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(ws.AgentsFile())
		return string(b)
	}
	t.Setenv(runner.EnvEgress, "")
	if s := read(); strings.Contains(s, "On the local network") {
		t.Fatalf("destinations claimed without any:\n%s", s)
	}
	t.Setenv(runner.EnvEgress, "192.168.1.10:8080, 192.168.2.0/24")
	if s := read(); !strings.Contains(s, "may also reach `192.168.1.10:8080`, `192.168.2.0/24`") {
		t.Fatalf("destinations missing:\n%s", s)
	}
}
