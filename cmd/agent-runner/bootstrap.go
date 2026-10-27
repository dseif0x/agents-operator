package main

import (
	"bytes"
	"cmp"
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
	defer w.dropCloneKey()
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

// k8sWriteNamespaces lists the namespaces the hub gave this session write
// access in (K8S_NAMESPACES, set in namespace mode).
func k8sWriteNamespaces() []string {
	if os.Getenv(runner.EnvK8sAccess) != "namespace" {
		return nil
	}
	var out []string
	for _, ns := range strings.Split(os.Getenv(runner.EnvK8sNamespaces), ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// sandboxNotes describes the session's sandbox cluster (sandbox mode).
func sandboxNotes() string {
	ns := os.Getenv(runner.EnvSandboxNamespace)
	host := os.Getenv(runner.EnvSandboxHost)
	level := cmp.Or(os.Getenv(runner.EnvSandboxSecurity), "baseline")
	var b strings.Builder
	b.WriteString("- This session has a Kubernetes cluster of its own: a virtual cluster (vcluster) where you are cluster-admin. `kubectl` and `helm` use it through `KUBECONFIG`; create namespaces, CRDs, operators, ClusterRoles, Argo CD or anything else there. You have no access to the real (host) cluster, and nothing you do in the sandbox can reach it.\n")
	b.WriteString("- Workloads you create run as ordinary pods in the host namespace `" + ns + "`. The host enforces there: the `" + level + "` Pod Security Standard (no privileged pods, host paths, host network or added capabilities), a ResourceQuota with default requests and limits per container, no `LoadBalancer` or `NodePort` Services, and network policies that let workloads reach the internet but not the local network or other cluster services. When something is rejected for these reasons, adapt the manifests (smaller requests, `ClusterIP` plus an Ingress) rather than retrying.\n")
	if host != "" {
		b.WriteString("- Ingresses are published by the host's ingress controller: every Ingress host must be `" + host + "` or end in `." + host + "` (for example `app." + host + "`); others are rejected. DNS records appear automatically. For TLS add the annotation `cert-manager.io/cluster-issuer` with the host's issuer and a `tls` section, like any Ingress. Use `ingressClassName` the host provides (or leave it empty for the default).\n")
	} else {
		b.WriteString("- Ingresses are not published to the host on this hub; use `kubectl port-forward` to reach services.\n")
	}
	b.WriteString("- The sandbox keeps running while this session is stopped (so a demo stays up) and is deleted together with the session.\n")
	return b.String()
}

// egressDestinations lists the local network destinations the hub opened
// for this session (EGRESS).
func egressDestinations() []string {
	var out []string
	for _, d := range strings.Split(os.Getenv(runner.EnvEgress), ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// joinCode renders names as `a`, `b` for Markdown.
func joinCode(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return strings.Join(quoted, ", ")
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

// Env is the environment the agent runs with: the git credentials only
// when the session lets the agent have them.
func (w *Workspace) Env() []string { return w.env(agentGitCredentials()) }

// cloneEnv is the environment of the first-boot clones, which always use
// the git credentials there are.
func (w *Workspace) cloneEnv() []string { return w.env(true) }

func (w *Workspace) env(git bool) []string {
	env := []string{}
	hasGitHubToken := false
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if runnerPrivate(k) && !(git && gitToken(k)) {
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
	if !git {
		return env
	}
	// Without the agent's own keys sshConfig is set only while cloning.
	if w.sshConfig != "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -F "+w.sshConfig)
	}
	// gh reads GH_TOKEN; some tools (and GitHub Actions conventions) want GITHUB_TOKEN.
	ghToken := os.Getenv(runner.EnvGitHubToken)
	if ghToken != "" && !hasGitHubToken {
		env = append(env, "GITHUB_TOKEN="+ghToken)
	}
	// Git configuration through the environment (git >= 2.31), so it never
	// depends on ~/.gitconfig and follows the credentials of *this* boot.
	var gitcfg [][2]string
	if len(gitHTTPSCredentials()) > 0 || ghToken != "" {
		gitcfg = append(gitcfg, [2]string{"credential.helper", "!agent-runner git-credential"})
	}
	if ghToken != "" && (w.sshConfig == "" || !sshCoversGitHub(gitSSHKeys())) {
		// A token for github.com but no SSH key for it: make git@github.com:
		// and ssh://git@github.com/ remotes go over HTTPS so private repos
		// still clone and push.
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
	if dests := egressDestinations(); len(dests) > 0 {
		b.WriteString("- On the local network this session may also reach " + joinCode(dests) + " (every port unless one is given). Other private addresses, the cluster and its nodes stay blocked.\n")
	}
	b.WriteString("- Available tools: git, ripgrep, jq, curl, tmux, kubectl, Node.js, Python 3, build-essential.\n")
	switch namespaces := k8sWriteNamespaces(); {
	case os.Getenv(runner.EnvK8sAccess) == "sandbox":
		b.WriteString(sandboxNotes())
	case !hasServiceAccount():
		b.WriteString("- This pod has no Kubernetes credentials; `kubectl` is installed but cannot reach a cluster. The session's owner can turn on Kubernetes access in the session settings.\n")
	case len(namespaces) > 0:
		b.WriteString("- This pod carries a Kubernetes ServiceAccount with write access (the `edit` role: create, change and delete workloads, read Secrets, exec into pods) in the namespace(s) " + joinCode(namespaces) + ", and read-only access elsewhere (`kubectl get`, `describe`, `logs`, `top`; no Secrets, no changes). Always pass `-n <namespace>` for writes. It cannot change RBAC, namespaces or cluster-scoped objects; say so rather than retrying when a command is denied.\n")
	default:
		b.WriteString("- This pod carries a read-only Kubernetes ServiceAccount, so `kubectl` works in-cluster for reading: `kubectl get`, `describe`, `logs`, `top`, `-A` for all namespaces. It cannot create, change or delete anything, read Secrets, or exec into pods; say so rather than retrying when such a command is denied.\n")
	}
	// What git and gh can do: with the session's credentials withheld, or
	// none configured on the account, say plainly that pushing will fail.
	agentGit := agentGitCredentials()
	ghToken := agentGit && os.Getenv(runner.EnvGitHubToken) != ""
	var sshHosts, httpsHosts []string
	if agentGit {
		for _, k := range gitSSHKeys() {
			sshHosts = append(sshHosts, k.Host)
		}
		for _, c := range gitHTTPSCredentials() {
			httpsHosts = append(httpsHosts, c.Host)
		}
	}
	sshKey := len(sshHosts) > 0
	canPush := ghToken || sshKey || len(httpsHosts) > 0
	switch {
	case !agentGit:
		b.WriteString("- This session gives you no git credentials: its owner turned them off. You can commit, but pushing, fetching private repositories and the GitHub API (`gh`) will fail. Leave your work committed on a branch and say so when you finish; do not look for or ask for credentials.\n")
	case !canPush:
		b.WriteString("- No git credentials are configured (no SSH key or token on the owner's account). You can commit, but pushing, fetching private repositories and the GitHub API (`gh`) will fail. Leave your work committed on a branch and say so when you finish.\n")
	default:
		if ghToken {
			b.WriteString("- The GitHub CLI `gh` is installed and authenticated (`GH_TOKEN`). Use it for pull requests, reviews, issues, checks and Actions runs, e.g. `gh pr view`, `gh pr create`, `gh run list`, `gh run view <id> --log-failed`. HTTPS pushes to github.com use the same token.\n")
		} else {
			b.WriteString("- The GitHub CLI `gh` is installed but no GitHub token is configured; it will not be able to call the API.\n")
		}
		if sshKey {
			b.WriteString("- Git over SSH has keys for " + hostList(sshHosts) + ", set up through `GIT_SSH_COMMAND` (ssh config `~/.ssh/agents-operator/config`; for plain `ssh` or `scp` pass `-F ~/.ssh/agents-operator/config`).\n")
		}
		if ghToken && !sshCoversGitHub(gitSSHKeys()) {
			b.WriteString("- No SSH key is configured for github.com; `git@github.com:` remotes are rewritten to HTTPS automatically.\n")
		}
		if len(httpsHosts) > 0 {
			b.WriteString("- Git over HTTPS has tokens for " + hostList(httpsHosts) + ".\n")
		}
	}
	if name := os.Getenv(runner.EnvGitUserName); name != "" {
		b.WriteString("- Commits are authored as " + name + " <" + os.Getenv(runner.EnvGitUserEmail) + ">.\n")
	}
	if w.Agent == runner.AgentShell {
		if list := aiAccountList(); list != "" {
			b.WriteString("- The owner's AI accounts, each key in its own variable: " + list + ".\n")
		}
	}
	b.WriteString("\n## Conventions\n\n")
	if canPush {
		b.WriteString("- Work on branches and open pull requests rather than pushing to the default branch, unless told otherwise.\n")
	} else {
		b.WriteString("- Work on a branch and commit there; the owner pushes it.\n")
	}
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
		text := content
		// Codex has no flag to add to its system prompt; its global
		// instructions file carries the persona instead.
		if persona := strings.TrimSpace(os.Getenv(runner.EnvPersona)); persona != "" && strings.HasSuffix(p, filepath.Join(".codex", "AGENTS.md")) {
			text += "\n## Your persona\n\n" + persona + "\n"
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

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

// seedLogins writes saved CLI credential files when they do not exist yet,
// or when they came from another login account than the one the session
// signs in with now (it was switched in the settings). A marker file per
// kind remembers which account the files are from; files without one are
// the session's own and stay.
func (w *Workspace) seedLogins() {
	for kind, allowed := range runner.LoginFiles {
		envKey := runner.EnvSeedPrefix + strings.ToUpper(kind)
		v := os.Getenv(envKey)
		if v == "" {
			continue
		}
		account := os.Getenv(runner.EnvSeedAccountPrefix + strings.ToUpper(kind))
		marker := filepath.Join(w.HomeDir(), ".config", "agents-operator", "login-"+kind)
		prev, err := os.ReadFile(marker)
		switched := account != "" && err == nil && strings.TrimSpace(string(prev)) != account
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
			if _, err := os.Stat(dst); err == nil && !switched {
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
			w.Log.Info("seeded saved login", "kind", kind, "file", rel, "account", account)
		}
		if account != "" {
			if err := os.MkdirAll(filepath.Dir(marker), 0o700); err == nil {
				_ = os.WriteFile(marker, []byte(account+"\n"), 0o600)
			}
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
	switch w.Agent {
	case runner.AgentClaude:
		_, statErr := os.Stat(filepath.Join(w.HomeDir(), runner.LoginFile[runner.LoginClaude]))
		haveAuth := statErr == nil || os.Getenv(runner.EnvClaudeOAuthToken) != "" || os.Getenv(runner.EnvAnthropicAPIKey) != ""
		w.prepareCLIConfig(".claude.json", "Claude Code", haveAuth, os.Getenv(runner.EnvAnthropicAPIKey), repos)
	}
}

// prepareCLIConfig answers the first-run questions in a Claude Code style
// global config file (see prepareClaudeConfig).
func (w *Workspace) prepareCLIConfig(name, cli string, haveAuth bool, apiKey string, repos []store.Repo) {
	path := filepath.Join(w.HomeDir(), name)
	cfg := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // keep timestamps and counters exactly as they were
		if err := dec.Decode(&cfg); err != nil || cfg == nil {
			w.Log.Warn("~/"+name+" is not a JSON object, leaving it alone", "err", err)
			return
		}
	case os.IsNotExist(err):
		cfg["theme"] = "dark"
	default:
		w.Log.Warn("cannot read ~/"+name, "err", err)
		return
	}
	changed := false
	setDefault := func(m map[string]any, key string, v any) {
		if _, ok := m[key]; !ok {
			m[key] = v
			changed = true
		}
	}

	if haveAuth {
		setDefault(cfg, "hasCompletedOnboarding", true)
	}
	if key := apiKey; len(key) >= claudeAPIKeySuffix {
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
		w.Log.Warn("cannot encode ~/"+name, "err", err)
		return
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		w.Log.Warn("cannot write ~/"+name, "err", err)
		return
	}
	w.Log.Info("prepared "+cli+" config", "onboarding_done", haveAuth, "trusted_dirs", len(dirs))
}

// claudeHookEvents are the Claude Code lifecycle events the runner listens
// to; together they say what the agent is doing and when it waits.
var claudeHookEvents = []string{
	"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure",
	"PermissionRequest", "Notification", "Stop", "StopFailure", "Elicitation",
}

// claudeHookCommand is what Claude Code runs on each of them: a helper in
// the runner image that forwards the event to the runner (see hook.go).
const claudeHookCommand = "agent-runner hook"

// prepareClaudeSettings writes what the session needs into
// ~/.claude/settings.json, keeping everything else in the file:
//
//   - the activity hooks (every session): Claude Code runs `agent-runner
//     hook` on each lifecycle event, asynchronously so it never waits on or
//     is blocked by them, and the runner turns the events into the status
//     the hub shows. Hook groups the user added themselves are kept; ours is
//     recognised by its command and added once per event.
//   - the consent behind --dangerously-skip-permissions (autonomous
//     sessions): the CLI records it as skipDangerousModePermissionPrompt;
//     the user gave it by ticking "autonomous" when creating the session.
func (w *Workspace) prepareClaudeSettings() {
	var path string
	switch w.Agent {
	case runner.AgentClaude:
		path = filepath.Join(w.HomeDir(), ".claude", "settings.json")
	default:
		return
	}
	autonomous := autonomousEnv()
	changed, err := updateJSONFile(path, func(obj map[string]any) bool {
		changed := mergeClaudeHooks(obj)
		if autonomous {
			if _, ok := obj["skipDangerousModePermissionPrompt"]; !ok {
				obj["skipDangerousModePermissionPrompt"] = true
				changed = true
			}
		}
		return changed
	})
	if err != nil {
		w.Log.Warn("cannot prepare agent settings", "agent", w.Agent, "err", err)
		return
	}
	if changed {
		w.Log.Info("prepared agent settings", "agent", w.Agent, "hooks", true, "bypass_consent", autonomous)
	}
}

// mergeClaudeHooks adds the runner's hook to every event in
// claudeHookEvents that does not carry it yet. Returns whether it changed
// anything.
func mergeClaudeHooks(settings map[string]any) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for _, ev := range claudeHookEvents {
		groups, _ := hooks[ev].([]any)
		if hasRunnerHook(groups) {
			continue
		}
		groups = append(groups, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": claudeHookCommand, "async": true, "timeout": 5}},
		})
		hooks[ev] = groups
		changed = true
	}
	if changed {
		settings["hooks"] = hooks
	}
	return changed
}

func hasRunnerHook(groups []any) bool {
	for _, g := range groups {
		group, _ := g.(map[string]any)
		list, _ := group["hooks"].([]any)
		for _, h := range list {
			hook, _ := h.(map[string]any)
			if cmd, _ := hook["command"].(string); cmd == claudeHookCommand {
				return true
			}
		}
	}
	return false
}

// updateJSONFile reads the JSON object in path (an absent file is an empty
// object), lets mutate change it, and writes it back when mutate reports a
// change. Numbers are kept exactly as written.
func updateJSONFile(path string, mutate func(obj map[string]any) bool) (bool, error) {
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
	if !mutate(obj) {
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
	cmd.Env = w.cloneEnv()
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
