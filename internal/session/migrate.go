package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// legacyAccount is how one old credential slot becomes an account.
type legacyAccount struct {
	kind, name, host string
	value            string // the legacy kind holding the value
	baseURL          string // the legacy kind holding the base URL, if any
}

// legacyAccounts lists the slots in the order the accounts are created,
// which is also the order the old sessions preferred them in: Claude Code
// took ANTHROPIC_API_KEY over its OAuth token and that over a saved login;
// Codex used its saved login before OPENAI_API_KEY. The oldest account a
// session can use is its default (reconcile.SelectAccount), and the
// migration also records these as the profile's defaults.
var legacyAccounts = []legacyAccount{
	{kind: store.AcctAnthropicAPIKey, name: "Anthropic", value: store.CredAnthropicAPIKey, baseURL: store.CredAnthropicBaseURL},
	{kind: store.AcctClaudeOAuthToken, name: "Claude Code token", value: store.CredClaudeOAuthToken},
	{kind: store.AcctClaudeLogin, name: "Claude Code login", value: store.CredClaudeLogin},
	{kind: store.AcctCodexLogin, name: "Codex login", value: store.CredCodexLogin},
	{kind: store.AcctOpenAIAPIKey, name: "OpenAI", value: store.CredOpenAIAPIKey},
	{kind: store.AcctUnjailed, name: "Unjailed", value: store.CredUnjailedAPIKey, baseURL: store.CredUnjailedBaseURL},
	// The GitHub token answered for github.com only, the HTTPS token for
	// every host. An account for github.com now comes before the one for
	// any host, so with both set github.com gets the GitHub token.
	{kind: store.AcctGitHTTPS, host: "github.com", value: store.CredGitHubToken},
	{kind: store.AcctGitHTTPS, host: store.AnyHost, value: store.CredGitHTTPSToken},
	{kind: store.AcctGitSSH, host: store.AnyHost, value: store.CredGitSSHKey},
}

// MigrateCredentials turns every user's old credential slots
// (user_credentials and the matching keys of their Secret) into accounts
// and profile settings. Per user, the accounts and their values are written
// first; only once that has succeeded are the old keys and rows removed. A
// run that stops halfway is finished by the next one: accounts that already
// exist are reused, not duplicated. Several hubs may run it at once; the
// unique indexes stop duplicates there too.
func (c *Credentials) MigrateCredentials(ctx context.Context, log *slog.Logger) error {
	users, err := c.Store.Credentials().UserIDs(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range users {
		n, err := c.migrateUser(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("user %s: %w", id, err))
			continue
		}
		log.Info("moved credentials to accounts", "user", id, "accounts", n)
	}
	return errors.Join(errs...)
}

func (c *Credentials) migrateUser(ctx context.Context, userID string) (int, error) {
	rows, err := c.Store.Credentials().List(ctx, userID)
	if err != nil {
		return 0, err
	}
	data, err := c.secretData(ctx, userID)
	if err != nil {
		return 0, err
	}
	has := map[string]bool{}
	for _, r := range rows {
		has[r.Kind] = true
	}
	get := func(kind string) []byte {
		if !has[kind] {
			return nil
		}
		return data[kind]
	}
	existing, err := c.Store.Accounts().List(ctx, userID)
	if err != nil {
		return 0, err
	}

	values := map[string][]byte{}
	created := map[string]*store.Account{} // account kind -> the first one, for defaults
	n := 0
	for _, l := range legacyAccounts {
		v := get(l.value)
		if len(v) == 0 {
			continue
		}
		a := findMigrated(existing, l)
		if a == nil {
			base := ""
			if l.baseURL != "" {
				base = strings.TrimRight(strings.TrimSpace(string(get(l.baseURL))), "/")
			}
			a = &store.Account{UserID: userID, Kind: l.kind, Name: l.name, Host: l.host, BaseURL: base}
			if store.GitAccountKind(l.kind) {
				a.Name = l.host
			}
			if err := c.Store.Accounts().Create(ctx, a); err != nil {
				if !errors.Is(err, store.ErrConflict) {
					return 0, err
				}
				// Another hub got there first, or the user already has an
				// account by that name: reuse a matching one, else keep both.
				if existing, err = c.Store.Accounts().List(ctx, userID); err != nil {
					return 0, err
				}
				if a = findMigrated(existing, l); a == nil {
					a = &store.Account{UserID: userID, Kind: l.kind, Name: uniqueName(l.name, existing), Host: l.host, BaseURL: base}
					if err := c.Store.Accounts().Create(ctx, a); err != nil {
						return 0, err
					}
				}
			}
			existing = append(existing, a)
		}
		values[store.AccountSecretKey(a.ID)] = v
		if created[l.kind] == nil {
			created[l.kind] = a
		}
		n++
	}
	if len(values) > 0 {
		if err := c.setValues(ctx, userID, values); err != nil {
			return 0, err
		}
	}

	p, err := c.Store.Profiles().Get(ctx, userID)
	if err != nil {
		return 0, err
	}
	if p.GitName == "" {
		p.GitName = strings.TrimSpace(string(get(store.CredGitUserName)))
	}
	if p.GitEmail == "" {
		p.GitEmail = strings.TrimSpace(string(get(store.CredGitUserEmail)))
	}
	for agent, order := range map[string][]string{
		runner.AgentClaude: {store.AcctAnthropicAPIKey, store.AcctClaudeOAuthToken, store.AcctClaudeLogin},
		runner.AgentCodex:  {store.AcctCodexLogin, store.AcctOpenAIAPIKey},
	} {
		if p.Defaults[agent] != "" {
			continue
		}
		for _, kind := range order {
			if a := created[kind]; a != nil {
				p.Defaults[agent] = a.ID
				break
			}
		}
	}
	if err := c.Store.Profiles().Put(ctx, p); err != nil {
		return 0, err
	}

	// Everything is in its new place: now the old slots go.
	var old []string
	for _, r := range rows {
		old = append(old, r.Kind)
	}
	if err := c.deleteValues(ctx, userID, old...); err != nil {
		return 0, err
	}
	for _, r := range rows {
		if err := c.Store.Credentials().Delete(ctx, userID, r.Kind); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// findMigrated finds the account an earlier run made from slot l.
func findMigrated(accounts []*store.Account, l legacyAccount) *store.Account {
	for _, a := range accounts {
		if a.Kind != l.kind {
			continue
		}
		if store.GitAccountKind(l.kind) && strings.EqualFold(a.Host, l.host) {
			return a
		}
		if !store.GitAccountKind(l.kind) && strings.EqualFold(a.Name, l.name) {
			return a
		}
	}
	return nil
}
