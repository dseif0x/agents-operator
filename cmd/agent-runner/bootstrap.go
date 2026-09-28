package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
//   - clone every entry of REPOS into /workspace/<path> when that directory is empty
func (w *Workspace) Bootstrap(ctx context.Context) error {
	home := w.HomeDir()
	for _, d := range []string{home, filepath.Join(home, ".ssh"), filepath.Join(home, ".config")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	_ = os.Chmod(filepath.Join(home, ".ssh"), 0o700)

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
	)
	if os.Getenv(runner.EnvGitSSHKey) != "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+w.sshKeyPath()+" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new")
	}
	// gh reads GH_TOKEN; some tools (and GitHub Actions conventions) want GITHUB_TOKEN.
	if tok := os.Getenv(runner.EnvGitHubToken); tok != "" && !hasGitHubToken {
		env = append(env, "GITHUB_TOKEN="+tok)
	}
	return env
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
	if os.Getenv(runner.EnvGitHTTPSToken) != "" || os.Getenv(runner.EnvGitHubToken) != "" {
		content += "[credential]\n\thelper = !agent-runner git-credential\n"
	}
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
	for kind, rel := range runner.LoginFile {
		envKey := runner.EnvSeedPrefix + strings.ToUpper(kind)
		v := os.Getenv(envKey)
		if v == "" {
			continue
		}
		dst := filepath.Join(w.HomeDir(), rel)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			w.Log.Warn("bad saved login, ignoring", "kind", kind)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			w.Log.Warn("cannot create login dir", "kind", kind, "err", err)
			continue
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			w.Log.Warn("cannot write login", "kind", kind, "err", err)
			continue
		}
		w.Log.Info("seeded saved login", "kind", kind)
	}
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
