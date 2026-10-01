package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Bootstrap prepares the PVC on first boot and is idempotent on later boots:
//
//   - create /workspace/home
//   - seed ~/.gitconfig from GIT_USER_NAME / GIT_USER_EMAIL
//   - install the SSH key or HTTPS credential helper
//   - seed saved CLI logins (AGENTS_OPERATOR_LOGIN_<KIND>) when the file is absent
//   - answer Claude Code's first-run prompts in ~/.claude.json when credentials exist,
//     and its bypass-permissions consent in ~/.claude/settings.json when autonomous
//   - clone every entry of REPOS into /workspace/<path> when that directory is empty
func (w *Workspace) Bootstrap(ctx context.Context) error {
	home := w.HomeDir()
	for _, d := range []string{home, filepath.Join(home, ".ssh"), filepath.Join(home, ".config")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	_ = os.Chmod(filepath.Join(home, ".ssh"), 0o700)
	// /tmp in the pod is an emptyDir shared with nothing, but a private 0700
	// temp dir is still the tidier default for the agent (TMPDIR in Env).
	if err := os.MkdirAll(w.TempDir(), 0o700); err != nil {
		w.Log.Warn("cannot create agent temp dir", "dir", w.TempDir(), "err", err)
	}

	if err := w.seedGitConfig(); err != nil {
		return err
	}
	if err := w.installGitCredentials(); err != nil {
		return err
	}
	w.seedLogins()

	repos, err := w.Repos()
	if err != nil {
		return err
	}
	w.prepareClaudeConfig(repos)
	w.prepareClaudeSettings()
	if err := w.writeAgentsFile(repos); err != nil {
		w.Log.Warn("cannot write AGENTS.md", "err", err)
	}
	var firstErr error
	for _, r := range repos {
		dst := filepath.Join(w.Root, r.Path)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dst, err)
		}
		empty, err := isEmptyDir(dst)
		if err != nil {
			return err
		}
		if !empty {
			w.Log.Info("repo already present, skipping clone", "dir", dst)
			continue
		}
		if err := w.clone(ctx, r.URL, r.Branch, dst); err != nil {
			// Keep going: a second repo failing must not hide the first.
			w.Log.Error("clone failed", "url", r.URL, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

var safePathRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// saTokenPath is where the kubelet mounts a pod's ServiceAccount token.
var saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// hasServiceAccount reports whether this pod carries Kubernetes credentials.
func hasServiceAccount() bool {
	_, err := os.Stat(saTokenPath)
	return err == nil
}

// Repos parses the REPOS env var (JSON list of {url, branch, path}). Paths
// are single directory names under the workspace; anything else is refused
// here too, so a compromised hub cannot escape /workspace.
func (w *Workspace) Repos() ([]store.Repo, error) {
	if w.repos != nil {
		return w.repos, nil
	}
	raw := os.Getenv(runner.EnvRepos)
	repos := []store.Repo{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &repos); err != nil {
			return nil, fmt.Errorf("parse %s: %w", runner.EnvRepos, err)
		}
	}
	for _, r := range repos {
		if !safePathRE.MatchString(r.Path) || r.Path == "home" {
			return nil, fmt.Errorf("unsafe repo path %q", r.Path)
		}
	}
	w.repos = repos
	return repos, nil
}

// Env is the environment the agent runs with.
func (w *Workspace) Env() []string {
	env := []string{}
	skip := map[string]bool{
		runner.EnvRunnerToken: true, runner.EnvGitSSHKey: true,
	}
	hasGitHubToken := false
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if skip[k] || strings.HasPrefix(k, runner.EnvSeedPrefix) {
			continue
		}
		if k == "GITHUB_TOKEN" {
			hasGitHubToken = true
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+w.HomeDir(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=C.UTF-8",
		"XDG_CONFIG_HOME="+filepath.Join(w.HomeDir(), ".config"),
		"XDG_CACHE_HOME="+filepath.Join(w.HomeDir(), ".cache"),
		"npm_config_cache="+filepath.Join(w.HomeDir(), ".npm"),
		"TMPDIR="+w.TempDir(),
	)
	hasSSHKey := os.Getenv(runner.EnvGitSSHKey) != ""
	if hasSSHKey {
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+w.sshKeyPath()+" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new")
	}
	// gh reads GH_TOKEN; some tools (and GitHub Actions conventions) want GITHUB_TOKEN.
	ghToken := os.Getenv(runner.EnvGitHubToken)
	if ghToken != "" && !hasGitHubToken {
		env = append(env, "GITHUB_TOKEN="+ghToken)
	}
	// Git configuration through the environment (git >= 2.31), so it never
	// depends on ~/.gitconfig and follows the credentials of *this* boot.
	var gitcfg [][2]string
	if os.Getenv(runner.EnvGitHTTPSToken) != "" || ghToken != "" {
		gitcfg = append(gitcfg, [2]string{"credential.helper", "!agent-runner git-credential"})
	}
	if ghToken != "" && !hasSSHKey {
		// Only a token, no SSH key: make git@github.com: and ssh://git@github.com/
		// remotes go over HTTPS so private repos still clone and push.
		gitcfg = append(gitcfg,
			[2]string{"url.https://github.com/.insteadOf", "git@github.com:"},
			[2]string{"url.https://github.com/.insteadOf", "ssh://git@github.com/"},
		)
	}
	if len(gitcfg) > 0 {
		env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(gitcfg)))
		for i, kv := range gitcfg {
			env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
		}
	}
	return env
}

// agentsMarker identifies files this runner generated, so a file the user
// wrote by hand is never overwritten.
const agentsMarker = "<!-- agents-operator:generated -->"

// AgentsFile is the path of the generated workspace guide.
func (w *Workspace) AgentsFile() string { return filepath.Join(w.Root, "AGENTS.md") }

// writeAgentsFile generates /workspace/AGENTS.md (and a CLAUDE.md twin,
// since Claude Code only reads that name) describing the environment and
// the cloned repositories. The agent starts in /workspace, so Codex and
// OpenCode find AGENTS.md and Claude Code finds CLAUDE.md in the working
// directory. The same content also goes into each CLI's global
// instructions file (created only when absent or previously generated).
func (w *Workspace) writeAgentsFile(repos []store.Repo) error {
	var b strings.Builder
	b.WriteString(agentsMarker + "\n")
	b.WriteString("# Your workspace\n\n")
	b.WriteString("You are running inside an agents-operator session: a Kubernetes pod created for this task, with its own persistent volume mounted at `" + w.Root + "`.\n\n")
	b.WriteString("You start in `" + w.Root + "`, which is not a git repository itself; the repositories below are subdirectories. `cd` into one before running git or project commands.\n\n")
	b.WriteString("## Repositories\n\n")
	if len(repos) == 0 {
		b.WriteString("No repository was cloned. `" + w.Root + "` is an empty workspace.\n")
	} else {
		for _, r := range repos {
			line := fmt.Sprintf("- `%s` ← %s", filepath.Join(w.Root, r.Path), r.URL)
			if r.Branch != "" {
				line += " (branch `" + r.Branch + "`)"
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\n## Environment\n\n")
	b.WriteString("- Everything under `" + w.Root + "` (including `HOME=" + w.HomeDir() + "`) survives stops, restarts and reconnects. `/tmp` is scratch. The rest of the filesystem is read-only.\n")
	b.WriteString("- You run as an unprivileged user (UID 1000). Network access is limited to DNS, HTTPS (443) and SSH (22) outside the cluster.\n")
	b.WriteString("- Available tools: git, ripgrep, jq, curl, tmux, kubectl, Node.js, Python 3, build-essential.\n")
	if hasServiceAccount() {
		b.WriteString("- This pod carries a read-only Kubernetes ServiceAccount, so `kubectl` works in-cluster for reading: `kubectl get`, `describe`, `logs`, `top`, `-A` for all namespaces. It cannot create, change or delete anything, read Secrets, or exec into pods; say so rather than retrying when such a command is denied.\n")
	} else {
		b.WriteString("- This pod has no Kubernetes credentials; `kubectl` is installed but cannot reach a cluster. The session's owner can turn on the read-only ServiceAccount in the session settings.\n")
	}
	if os.Getenv(runner.EnvGitHubToken) != "" {
		b.WriteString("- The GitHub CLI `gh` is installed and authenticated (`GH_TOKEN`). Use it for pull requests, reviews, issues, checks and Actions runs, e.g. `gh pr view`, `gh pr create`, `gh run list`, `gh run view <id> --log-failed`. HTTPS pushes to github.com use the same token.\n")
	} else {
		b.WriteString("- The GitHub CLI `gh` is installed but no GitHub token is configured; it will not be able to call the API.\n")
	}
	switch {
	case os.Getenv(runner.EnvGitSSHKey) != "":
		b.WriteString("- Git over SSH is configured with a deploy key at `~/.ssh/id_ed25519`.\n")
	case os.Getenv(runner.EnvGitHubToken) != "":
		b.WriteString("- No SSH key is configured; `git@github.com:` remotes are rewritten to HTTPS automatically.\n")
	}
	if name := os.Getenv(runner.EnvGitUserName); name != "" {
		b.WriteString("- Commits are authored as " + name + " <" + os.Getenv(runner.EnvGitUserEmail) + ">.\n")
	}
	b.WriteString("\n## Conventions\n\n")
	b.WriteString("- Work on branches and open pull requests rather than pushing to the default branch, unless told otherwise.\n")
	b.WriteString("- Never write credentials to files inside the repositories; tokens come from the environment.\n")
	b.WriteString("- Each repository may contain its own AGENTS.md or CLAUDE.md with project rules; those take precedence over this file.\n")
	content := b.String()

	if err := os.WriteFile(w.AgentsFile(), []byte(content), 0o644); err != nil {
		return err
	}
	// The CLAUDE.md twin in the working directory and the global
	// instruction files: only touch ours.
	home := w.HomeDir()
	for _, p := range []string{
		filepath.Join(w.Root, "CLAUDE.md"),
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(home, ".codex", "AGENTS.md"),
		filepath.Join(home, ".config", "opencode", "AGENTS.md"),
	} {
		if old, err := os.ReadFile(p); err == nil && !strings.HasPrefix(string(old), agentsMarker) {
			continue // the user wrote their own
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (w *Workspace) sshKeyPath() string { return filepath.Join(w.HomeDir(), ".ssh", "id_ed25519") }

func (w *Workspace) seedGitConfig() error {
	name, email := os.Getenv(runner.EnvGitUserName), os.Getenv(runner.EnvGitUserEmail)
	if name == "" {
		name = "agents-operator"
	}
	if email == "" {
		email = "agents-operator@localhost"
	}
	cfg := filepath.Join(w.HomeDir(), ".gitconfig")
	if _, err := os.Stat(cfg); err == nil {
		return nil // never overwrite what the user changed
	}
	content := fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n[init]\n\tdefaultBranch = main\n[safe]\n\tdirectory = *\n", name, email)
	return os.WriteFile(cfg, []byte(content), 0o644)
}

// installGitCredentials writes the SSH key with mode 0600. Kubernetes
// Secret volumes cannot produce a file ssh accepts (they are group readable
// once fsGroup applies), so the key is passed through the environment and
// materialised here.
func (w *Workspace) installGitCredentials() error {
	key := os.Getenv(runner.EnvGitSSHKey)
	path := w.sshKeyPath()
	if key == "" {
		_ = os.Remove(path)
		return nil
	}
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return fmt.Errorf("write ssh key: %w", err)
	}
	return os.Chmod(path, 0o600)
}

// seedLogins writes saved CLI credential files when they do not exist yet.
func (w *Workspace) seedLogins() {
	for kind, allowed := range runner.LoginFiles {
		envKey := runner.EnvSeedPrefix + strings.ToUpper(kind)
		v := os.Getenv(envKey)
		if v == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			w.Log.Warn("bad saved login, ignoring", "kind", kind)
			continue
		}
		bundle := runner.DecodeLoginBundle(kind, data)
		// Only the known files of this kind may be written; the bundle
		// comes from the hub, but a compromised hub must not get a
		// write-anywhere primitive.
		for _, rel := range allowed {
			content, ok := bundle.Files[rel]
			if !ok {
				continue
			}
			dst := filepath.Join(w.HomeDir(), rel)
			if _, err := os.Stat(dst); err == nil {
				continue // the volume already has this session's own copy
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				w.Log.Warn("cannot create login dir", "kind", kind, "err", err)
				continue
			}
			if err := os.WriteFile(dst, content, 0o600); err != nil {
				w.Log.Warn("cannot write login", "kind", kind, "file", rel, "err", err)
				continue
			}
			w.Log.Info("seeded saved login", "kind", kind, "file", rel)
		}
	}
}

// claudeAPIKeySuffix is how Claude Code remembers which ANTHROPIC_API_KEY
// the user approved: the key's last 20 characters.
const claudeAPIKeySuffix = 20

// prepareClaudeConfig lets Claude Code start straight at its prompt. The CLI
// decides from ~/.claude.json whether to run onboarding (theme, login),
// whether to ask before using ANTHROPIC_API_KEY from the environment, and
// whether the working directory is trusted. In a session pod the user has
// already chosen the credentials and the repositories, so those prompts are
// answered here: keys that exist are never changed, so a seeded .claude.json
// keeps its account state, and onboarding is only marked complete when
// there is something to sign in with (a seeded login, CLAUDE_CODE_OAUTH_TOKEN
// or ANTHROPIC_API_KEY). Without credentials the CLI's own login flow is the
// right first screen.
func (w *Workspace) prepareClaudeConfig(repos []store.Repo) {
	if w.Agent != runner.AgentClaude {
		return
	}
	path := filepath.Join(w.HomeDir(), ".claude.json")
	cfg := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // keep timestamps and counters exactly as they were
		if err := dec.Decode(&cfg); err != nil || cfg == nil {
			w.Log.Warn("~/.claude.json is not a JSON object, leaving it alone", "err", err)
			return
		}
	case os.IsNotExist(err):
		cfg["theme"] = "dark"
	default:
		w.Log.Warn("cannot read ~/.claude.json", "err", err)
		return
	}
	changed := false
	setDefault := func(m map[string]any, key string, v any) {
		if _, ok := m[key]; !ok {
			m[key] = v
			changed = true
		}
	}

	_, statErr := os.Stat(filepath.Join(w.HomeDir(), runner.LoginFile[runner.LoginClaude]))
	haveAuth := statErr == nil || os.Getenv(runner.EnvClaudeOAuthToken) != "" || os.Getenv(runner.EnvAnthropicAPIKey) != ""
	if haveAuth {
		setDefault(cfg, "hasCompletedOnboarding", true)
	}
	if key := os.Getenv(runner.EnvAnthropicAPIKey); len(key) >= claudeAPIKeySuffix {
		resp, _ := cfg["customApiKeyResponses"].(map[string]any)
		if resp == nil {
			resp = map[string]any{"approved": []any{}, "rejected": []any{}}
		}
		approved, _ := resp["approved"].([]any)
		suffix := key[len(key)-claudeAPIKeySuffix:]
		if !slices.Contains(approved, any(suffix)) {
			resp["approved"] = append(approved, suffix)
			cfg["customApiKeyResponses"] = resp
			changed = true
		}
	}
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	dirs := []string{w.Root}
	for _, r := range repos {
		dirs = append(dirs, filepath.Join(w.Root, r.Path))
	}
	for _, d := range dirs {
		p, _ := projects[d].(map[string]any)
		if p == nil {
			p = map[string]any{}
		}
		setDefault(p, "hasTrustDialogAccepted", true)
		projects[d] = p
	}
	cfg["projects"] = projects
	if !changed {
		return
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		w.Log.Warn("cannot encode ~/.claude.json", "err", err)
		return
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		w.Log.Warn("cannot write ~/.claude.json", "err", err)
		return
	}
	w.Log.Info("prepared Claude Code config", "onboarding_done", haveAuth, "trusted_dirs", len(dirs))
}

// prepareClaudeSettings answers the consent dialog behind
// --dangerously-skip-permissions for autonomous sessions. The CLI records
// that consent in ~/.claude/settings.json (older versions kept it in
// .claude.json and migrate it there); the user gave it by ticking
// "autonomous" when creating the session. Other settings are left as they are.
func (w *Workspace) prepareClaudeSettings() {
	if w.Agent != runner.AgentClaude || !autonomousEnv() {
		return
	}
	path := filepath.Join(w.HomeDir(), ".claude", "settings.json")
	changed, err := setJSONDefaults(path, map[string]any{"skipDangerousModePermissionPrompt": true})
	if err != nil {
		w.Log.Warn("cannot prepare Claude Code settings", "err", err)
		return
	}
	if changed {
		w.Log.Info("accepted bypass-permissions consent for the autonomous session")
	}
}

// setJSONDefaults adds the given keys to the JSON object in path when they
// are absent, creating the file if needed. Existing keys are never changed
// and numbers are kept exactly as written.
func setJSONDefaults(path string, defaults map[string]any) (bool, error) {
	obj := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil || obj == nil {
			return false, fmt.Errorf("%s is not a JSON object: %w", path, err)
		}
	case os.IsNotExist(err):
	default:
		return false, err
	}
	changed := false
	for k, v := range defaults {
		if _, ok := obj[k]; !ok {
			obj[k] = v
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, out, 0o600)
}

// autonomousEnv reports whether the hub asked for an autonomous session.
func autonomousEnv() bool {
	v := os.Getenv(runner.EnvAutonomous)
	return strings.EqualFold(v, "true") || v == "1"
}

func (w *Workspace) clone(ctx context.Context, url, branch, dst string) error {
	args := []string{"clone", "--recurse-submodules"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", url, dst)
	w.Log.Info("cloning repository", "url", url, "branch", branch, "dir", dst)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Env = w.Env()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %w", url, err)
	}
	return nil
}

func isEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// gitCredentialHelper implements the `git credential` helper protocol from
// the environment so tokens never land in a file on the PVC. GIT_HTTPS_TOKEN
// answers for every host; GH_TOKEN answers for github.com only.
// Invoked by git as `agent-runner git-credential get` with key=value lines
// on stdin.
func gitCredentialHelper(args []string) {
	if len(args) == 0 || args[0] != "get" {
		return // store/erase are no-ops
	}
	host := ""
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && k == "host" {
			host = v
		}
	}
	token := os.Getenv(runner.EnvGitHTTPSToken)
	user := os.Getenv("GIT_HTTPS_USERNAME")
	if token == "" && (host == "github.com" || strings.HasSuffix(host, ".github.com")) {
		token = os.Getenv(runner.EnvGitHubToken)
		user = "x-access-token"
	}
	if token == "" {
		return
	}
	if user == "" {
		user = "x-access-token"
	}
	fmt.Printf("username=%s\npassword=%s\n", user, token)
}
