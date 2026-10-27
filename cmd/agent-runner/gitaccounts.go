package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// The owner's git accounts arrive as JSON, one entry per host (see
// runner.GitHTTPSCredential and runner.GitSSHKey). A host is a name
// (github.com), an ssh-style pattern (*.example.com) or "*" for every host
// no other account names, as in an ssh config.

func gitHTTPSCredentials() []runner.GitHTTPSCredential {
	var list []runner.GitHTTPSCredential
	if v := os.Getenv(runner.EnvGitHTTPS); v != "" {
		_ = json.Unmarshal([]byte(v), &list)
	}
	return list
}

func gitSSHKeys() []runner.GitSSHKey {
	var list []runner.GitSSHKey
	if v := os.Getenv(runner.EnvGitSSH); v != "" {
		_ = json.Unmarshal([]byte(v), &list)
	}
	return list
}

// hostMatches reports whether an account's host (name, pattern or "*")
// covers host.
func hostMatches(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if pattern == store.AnyHost || pattern == host {
		return true
	}
	ok, _ := path.Match(pattern, host)
	return ok
}

// gitHTTPSFor picks the account for host: the one naming it exactly, then
// a matching pattern, then the one for any host. A port in host is tried
// with and then without it.
func gitHTTPSFor(list []runner.GitHTTPSCredential, host string) *runner.GitHTTPSCredential {
	candidates := []string{strings.ToLower(host)}
	if h, _, ok := strings.Cut(host, ":"); ok {
		candidates = append(candidates, strings.ToLower(h))
	}
	for _, rank := range []func(p, h string) bool{
		func(p, h string) bool { return strings.EqualFold(p, h) },
		func(p, h string) bool { return p != store.AnyHost && hostMatches(p, h) },
		func(p, _ string) bool { return p == store.AnyHost },
	} {
		for _, h := range candidates {
			for i := range list {
				if rank(list[i].Host, h) {
					return &list[i]
				}
			}
		}
	}
	return nil
}

// gitCredentialHelper implements the `git credential` helper protocol from
// the environment so tokens never land in a file on the PVC: it answers
// with the HTTPS account for the host git asks about.
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
	c := gitHTTPSFor(gitHTTPSCredentials(), host)
	if c == nil && strings.EqualFold(host, "github.com") && os.Getenv(runner.EnvGitHubToken) != "" {
		// A GH_TOKEN without an HTTPS account for github.com (exported by hand).
		c = &runner.GitHTTPSCredential{Host: host, Token: os.Getenv(runner.EnvGitHubToken)}
	}
	if c == nil || c.Token == "" {
		return
	}
	user := c.Username
	if user == "" {
		user = "x-access-token"
	}
	fmt.Printf("username=%s\npassword=%s\n", user, c.Token)
}

// sshAccountsDir is where the SSH keys and their ssh config go when the
// agent may have them; rewritten on every start.
func (w *Workspace) sshAccountsDir() string {
	return filepath.Join(w.HomeDir(), ".ssh", "agents-operator")
}

// legacySSHKeyPath is where runners before accounts kept the one SSH key.
func (w *Workspace) legacySSHKeyPath() string {
	return filepath.Join(w.HomeDir(), ".ssh", "id_ed25519")
}

// installGitCredentials writes the SSH keys (mode 0600) and an ssh config
// with one Host block per key, which git uses through GIT_SSH_COMMAND.
// Kubernetes Secret volumes cannot produce a file ssh accepts (they are
// group readable once fsGroup applies), so the keys are passed through the
// environment and materialised here. When the agent must not have them,
// they go to a temporary directory outside HOME for the clones instead
// (dropCloneKey removes it before the agent starts) and the keys an
// earlier boot installed are removed.
func (w *Workspace) installGitCredentials() error {
	keys := gitSSHKeys()
	// The key an older runner installed: now one of the accounts' keys.
	if old, err := os.ReadFile(w.legacySSHKeyPath()); err == nil && slices.ContainsFunc(keys, func(k runner.GitSSHKey) bool {
		return strings.TrimSpace(k.Key) == strings.TrimSpace(string(old))
	}) {
		_ = os.Remove(w.legacySSHKeyPath())
	}
	_ = os.RemoveAll(w.sshAccountsDir())
	w.sshConfig = ""
	if len(keys) == 0 {
		return nil
	}
	dir := w.sshAccountsDir()
	if !agentGitCredentials() {
		d, err := os.MkdirTemp("", "agents-operator-clone-ssh-*")
		if err != nil {
			return fmt.Errorf("write clone keys: %w", err)
		}
		dir, w.cloneSSHDir = d, d
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("write ssh keys: %w", err)
	}
	var cfg strings.Builder
	cfg.WriteString("# Written by agents-operator on every start from the owner's SSH git accounts; edits are overwritten.\n")
	var named []string
	for _, k := range keys {
		if k.Host != store.AnyHost {
			named = append(named, k.Host)
		}
	}
	for i, k := range keys {
		key := k.Key
		if !strings.HasSuffix(key, "\n") {
			key += "\n"
		}
		file := filepath.Join(dir, "key-"+strconv.Itoa(i+1))
		if err := os.WriteFile(file, []byte(key), 0o600); err != nil {
			return fmt.Errorf("write ssh key: %w", err)
		}
		hosts := k.Host
		if k.Host == store.AnyHost {
			hosts = "*"
			for _, h := range named {
				hosts += " !" + h
			}
		}
		fmt.Fprintf(&cfg, "\nHost %s\n  IdentityFile %q\n  IdentitiesOnly yes\n", hosts, file)
	}
	// The user's own config still applies, before the defaults below.
	cfg.WriteString("\nMatch all\nInclude ~/.ssh/config\n\nHost *\n  StrictHostKeyChecking accept-new\n")
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte(cfg.String()), 0o600); err != nil {
		return fmt.Errorf("write ssh config: %w", err)
	}
	w.sshConfig = config
	return nil
}

// dropCloneKey removes the temporary clone keys, if there are any.
func (w *Workspace) dropCloneKey() {
	if w.cloneSSHDir != "" {
		_ = os.RemoveAll(w.cloneSSHDir)
		w.cloneSSHDir, w.sshConfig = "", ""
	}
}

// sshCoversGitHub reports whether one of the SSH keys is for github.com.
func sshCoversGitHub(keys []runner.GitSSHKey) bool {
	return slices.ContainsFunc(keys, func(k runner.GitSSHKey) bool { return hostMatches(k.Host, "github.com") })
}

// aiAccountList describes the session's AI accounts for AGENTS.md: name,
// kind, endpoint and the variable holding the key; "" without any.
func aiAccountList() string {
	var accounts []runner.AIAccount
	if v := os.Getenv(runner.EnvAIAccounts); v == "" || json.Unmarshal([]byte(v), &accounts) != nil {
		return ""
	}
	var out []string
	for _, a := range accounts {
		s := a.Name + " (" + strings.ReplaceAll(a.Kind, "_", " ")
		if a.BaseURL != "" {
			s += " at " + a.BaseURL
		}
		out = append(out, s+", `$"+a.KeyEnv+"`)")
	}
	return strings.Join(out, "; ")
}

// hostList names account hosts for AGENTS.md: `github.com`, `*.corp`, and
// "any other host" for "*".
func hostList(hosts []string) string {
	var out []string
	anyHost := false
	for _, h := range hosts {
		if h == store.AnyHost {
			anyHost = true
			continue
		}
		out = append(out, "`"+h+"`")
	}
	if anyHost {
		if len(out) == 0 {
			return "every host"
		}
		out = append(out, "any other host")
	}
	return strings.Join(out, ", ")
}
