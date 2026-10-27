package reconcile

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// UserCredentials is what a session Secret is built from: the owner's
// accounts, the data of their Secret (values under store.AccountSecretKey)
// and their profile.
type UserCredentials struct {
	Accounts []*store.Account
	Values   map[string][]byte
	Profile  *store.Profile
}

func (u UserCredentials) value(a *store.Account) []byte {
	return u.Values[store.AccountSecretKey(a.ID)]
}

// usable is the accounts that have a value.
func (u UserCredentials) usable() []*store.Account {
	var out []*store.Account
	for _, a := range u.Accounts {
		if len(u.value(a)) > 0 {
			out = append(out, a)
		}
	}
	return out
}

// SignInKinds lists, per agent that signs in with one account, the account
// kinds its CLI can use. A session of such an agent gets exactly one of
// them (SelectAccount), so a prompt-injected agent cannot read the keys of
// accounts it does not use.
var SignInKinds = map[string][]string{
	runner.AgentClaude: {store.AcctAnthropicAPIKey, store.AcctClaudeOAuthToken, store.AcctClaudeLogin},
	runner.AgentCodex:  {store.AcctOpenAIAPIKey, store.AcctCodexLogin},
}

// ProviderKinds are the account kinds OpenCode gets as providers: every
// account of them, each under its own name.
var ProviderKinds = []string{store.AcctAnthropicAPIKey, store.AcctOpenAIAPIKey, store.AcctOpenAICompatible, store.AcctUnjailed}

// SelectAccount is the account a session of agent signs in with: pick (the
// session's choice), else the owner's default for the agent, else the
// oldest account the agent can use. nil when there is none.
func SelectAccount(agent, pick string, accounts []*store.Account, profile *store.Profile) *store.Account {
	kinds := SignInKinds[agent]
	if len(kinds) == 0 {
		return nil
	}
	find := func(id string) *store.Account {
		for _, a := range accounts {
			if id != "" && a.ID == id && slices.Contains(kinds, a.Kind) {
				return a
			}
		}
		return nil
	}
	if a := find(pick); a != nil {
		return a
	}
	if profile != nil {
		if a := find(profile.Defaults[agent]); a != nil {
			return a
		}
	}
	for _, a := range accounts {
		if slices.Contains(kinds, a.Kind) {
			return a
		}
	}
	return nil
}

// SignInAccounts are the accounts a session signs its CLIs in with: the
// selected one for Claude Code and Codex, and for the shell, which has no
// agent of its own, the owner's default for each so both CLIs work when
// run by hand. OpenCode signs in through its providers instead.
func SignInAccounts(s *store.Session, accounts []*store.Account, profile *store.Profile) []*store.Account {
	var out []*store.Account
	switch s.Agent {
	case runner.AgentClaude, runner.AgentCodex:
		if a := SelectAccount(s.Agent, s.AIAccount, accounts, profile); a != nil {
			out = append(out, a)
		}
	case runner.AgentShell:
		for _, agent := range []string{runner.AgentClaude, runner.AgentCodex} {
			if a := SelectAccount(agent, "", accounts, profile); a != nil {
				out = append(out, a)
			}
		}
	}
	return out
}

// credentialEnv adds the session's accounts to the Secret data:
//
//   - git: every HTTPS and SSH account (JSON, see runner.GitHTTPSCredential
//     and runner.GitSSHKey), GH_TOKEN from the github.com HTTPS account,
//     and the git identity;
//   - the sign-in accounts (SignInAccounts) in the CLIs' own variables, or
//     as a login seed;
//   - for OpenCode every provider account (ProviderKinds), for the shell
//     every account with a key: each key in a variable of its own plus a
//     description of them all (runner.EnvAIAccounts).
func credentialEnv(data map[string][]byte, s *store.Session, uc UserCredentials) {
	usable := uc.usable()
	if p := uc.Profile; p != nil {
		if p.GitName != "" {
			data[runner.EnvGitUserName] = []byte(p.GitName)
		}
		if p.GitEmail != "" {
			data[runner.EnvGitUserEmail] = []byte(p.GitEmail)
		}
	}

	var https []runner.GitHTTPSCredential
	var ssh []runner.GitSSHKey
	for _, a := range usable {
		switch a.Kind {
		case store.AcctGitHTTPS:
			https = append(https, runner.GitHTTPSCredential{Host: a.Host, Username: a.Username, Token: string(uc.value(a))})
			if strings.EqualFold(a.Host, "github.com") {
				data[runner.EnvGitHubToken] = uc.value(a)
			}
		case store.AcctGitSSH:
			ssh = append(ssh, runner.GitSSHKey{Host: a.Host, Key: string(uc.value(a))})
		}
	}
	if len(https) > 0 {
		data[runner.EnvGitHTTPS], _ = json.Marshal(https)
	}
	if len(ssh) > 0 {
		data[runner.EnvGitSSH], _ = json.Marshal(ssh)
	}

	for _, a := range SignInAccounts(s, usable, uc.Profile) {
		v := uc.value(a)
		switch a.Kind {
		case store.AcctAnthropicAPIKey:
			data[runner.EnvAnthropicAPIKey] = v
			if a.BaseURL != "" {
				data[runner.EnvAnthropicBaseURL] = []byte(a.BaseURL)
			}
		case store.AcctClaudeOAuthToken:
			data[runner.EnvClaudeOAuthToken] = v
		case store.AcctOpenAIAPIKey:
			data[runner.EnvOpenAIAPIKey] = v
		case store.AcctClaudeLogin, store.AcctCodexLogin:
			// Saved CLI logins are seeded on first boot; the runner expects base64.
			data[runner.EnvSeedPrefix+strings.ToUpper(a.Kind)] = []byte(base64.StdEncoding.EncodeToString(v))
			data[runner.EnvSeedAccountPrefix+strings.ToUpper(a.Kind)] = []byte(a.ID)
		}
	}

	var described []runner.AIAccount
	used := map[string]bool{}
	for _, a := range usable {
		switch {
		case store.GitAccountKind(a.Kind), a.Kind == store.AcctClaudeLogin, a.Kind == store.AcctCodexLogin:
			continue
		case s.Agent == runner.AgentOpenCode && slices.Contains(ProviderKinds, a.Kind), s.Agent == runner.AgentShell:
		default:
			continue
		}
		env := keyEnv(a.Name, used)
		data[env] = uc.value(a)
		described = append(described, runner.AIAccount{ID: a.ID, Name: a.Name, Kind: a.Kind, BaseURL: a.BaseURL, Models: a.Models, KeyEnv: env})
	}
	if len(described) > 0 {
		data[runner.EnvAIAccounts], _ = json.Marshal(described)
	}
}

var nonEnvChars = regexp.MustCompile(`[^A-Z0-9]+`)

// keyEnv names the variable of an account's key after the account:
// "Work LiteLLM" -> AGENTS_OPERATOR_KEY_WORK_LITELLM, numbered on clashes.
func keyEnv(name string, used map[string]bool) string {
	base := runner.EnvAIKeyPrefix + strings.Trim(nonEnvChars.ReplaceAllString(strings.ToUpper(name), "_"), "_")
	if base == runner.EnvAIKeyPrefix {
		base += "ACCOUNT"
	}
	env := base
	for i := 2; used[env]; i++ {
		env = base + "_" + strconv.Itoa(i)
	}
	used[env] = true
	return env
}
