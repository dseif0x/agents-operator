package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// Bootstrap prepares the PVC on first boot and is idempotent on later boots:
//
//   - create /workspace/home and /workspace/repo
//   - seed ~/.gitconfig from GIT_USER_NAME / GIT_USER_EMAIL
//   - install the SSH key or HTTPS credential helper
//   - seed saved CLI logins (AGENTHUB_LOGIN_<KIND>) when the file is absent
//   - clone REPO_URL at REPO_BRANCH into /workspace/repo when it is empty
func (w Workspace) Bootstrap(ctx context.Context) error {
	home, repo := w.HomeDir(), w.RepoDir()
	for _, d := range []string{home, repo, filepath.Join(home, ".ssh"), filepath.Join(home, ".config")} {
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

	if url := os.Getenv(runner.EnvRepoURL); url != "" {
		empty, err := isEmptyDir(repo)
		if err != nil {
			return err
		}
		if empty {
			if err := w.clone(ctx, url, os.Getenv(runner.EnvRepoBranch), repo); err != nil {
				return err
			}
		} else {
			w.Log.Info("repo already present, skipping clone", "dir", repo)
		}
	}
	return nil
}

// Env is the environment the agent runs with.
func (w Workspace) Env() []string {
	env := []string{}
	skip := map[string]bool{
		runner.EnvRunnerToken: true, runner.EnvGitSSHKey: true,
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if skip[k] || strings.HasPrefix(k, runner.EnvSeedPrefix) {
			continue
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
	return env
}

func (w Workspace) sshKeyPath() string { return filepath.Join(w.HomeDir(), ".ssh", "id_ed25519") }

func (w Workspace) seedGitConfig() error {
	name, email := os.Getenv(runner.EnvGitUserName), os.Getenv(runner.EnvGitUserEmail)
	if name == "" {
		name = "agenthub"
	}
	if email == "" {
		email = "agenthub@localhost"
	}
	cfg := filepath.Join(w.HomeDir(), ".gitconfig")
	if _, err := os.Stat(cfg); err == nil {
		return nil // never overwrite what the user changed
	}
	content := fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n[init]\n\tdefaultBranch = main\n[safe]\n\tdirectory = *\n", name, email)
	if os.Getenv(runner.EnvGitHTTPSToken) != "" {
		content += "[credential]\n\thelper = !agent-runner git-credential\n"
	}
	return os.WriteFile(cfg, []byte(content), 0o644)
}

// installGitCredentials writes the SSH key with mode 0600. Kubernetes
// Secret volumes cannot produce a file ssh accepts (they are group readable
// once fsGroup applies), so the key is passed through the environment and
// materialised here.
func (w Workspace) installGitCredentials() error {
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
func (w Workspace) seedLogins() {
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

func (w Workspace) clone(ctx context.Context, url, branch, dst string) error {
	args := []string{"clone", "--recurse-submodules"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, url, dst)
	w.Log.Info("cloning repository", "url", url, "branch", branch)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Env = w.Env()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %w", err)
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
// the environment so the HTTPS token never lands in a file on the PVC.
// Invoked as `agent-runner git-credential get`.
func gitCredentialHelper(args []string) {
	if len(args) == 0 || args[0] != "get" {
		return // store/erase are no-ops
	}
	token := os.Getenv(runner.EnvGitHTTPSToken)
	if token == "" {
		return
	}
	user := os.Getenv("GIT_HTTPS_USERNAME")
	if user == "" {
		user = "x-access-token"
	}
	fmt.Printf("username=%s\npassword=%s\n", user, token)
}
