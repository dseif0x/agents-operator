package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dseif0x/agents-operator/internal/reconcile"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// accountValue is the value of the user's first account of kind, or nil.
func accountValue(t *testing.T, c *Credentials, userID, kind string) []byte {
	t.Helper()
	ctx := context.Background()
	list, err := c.Store.Accounts().List(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.secretData(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.Kind == kind {
			return data[store.AccountSecretKey(a.ID)]
		}
	}
	return nil
}

func newCreds(t *testing.T) (*Credentials, string) {
	t.Helper()
	st := store.NewMemory()
	u := &store.User{Username: "alice", PasswordHash: "x"}
	if err := st.Users().Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return &Credentials{Store: st, CS: fake.NewClientset(), Namespace: "agents-operator"}, u.ID
}

func TestAccounts(t *testing.T) {
	c, uid := newCreds(t)
	ctx := context.Background()
	invalid := func(in AccountInput) {
		t.Helper()
		var ve *ValidationError
		if _, err := c.CreateAccount(ctx, uid, in); !errors.As(err, &ve) {
			t.Errorf("%+v accepted: %v", in, err)
		}
	}
	invalid(AccountInput{Kind: "nope", Name: "x", Value: "v"})
	invalid(AccountInput{Kind: store.AcctAnthropicAPIKey, Value: "sk"})                                                      // no name
	invalid(AccountInput{Kind: store.AcctAnthropicAPIKey, Name: "A"})                                                        // no key
	invalid(AccountInput{Kind: store.AcctClaudeOAuthToken, Name: "T", Value: "Store this token securely"})                   // not a token
	invalid(AccountInput{Kind: store.AcctClaudeLogin, Name: "L", Value: "x"})                                                // logins come from sessions
	invalid(AccountInput{Kind: store.AcctOpenAICompatible, Name: "C", Value: "k"})                                           // no base URL
	invalid(AccountInput{Kind: store.AcctUnjailed, Name: "U", BaseURL: "unjailedai.com/v1", Value: "k"})                     // not a URL
	invalid(AccountInput{Kind: store.AcctGitHTTPS, Host: "https://github.com/org", Value: "t"})                              // not a host
	invalid(AccountInput{Kind: store.AcctGitSSH, Host: "github.com:22", Value: "-----BEGIN KEY-----\nx\n-----END KEY-----"}) // ssh hosts have no port
	invalid(AccountInput{Kind: store.AcctGitSSH, Host: "github.com", Value: "ssh-ed25519 AAAA… public"})                     // public key

	// A setup token copied off a phone screen arrives wrapped; whitespace
	// goes, the token stays whole.
	tok, err := c.CreateAccount(ctx, uid, AccountInput{Kind: store.AcctClaudeOAuthToken, Name: " Max  plan ", Value: "  sk-ant-oat01-abc\n  def \n ghi\n"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Name != "Max plan" || string(accountValue(t, c, uid, store.AcctClaudeOAuthToken)) != "sk-ant-oat01-abcdefghi" {
		t.Fatalf("token account %+v, value %q", tok, accountValue(t, c, uid, store.AcctClaudeOAuthToken))
	}
	invalid(AccountInput{Kind: store.AcctAnthropicAPIKey, Name: "max PLAN", Value: "sk"}) // names are unique

	compat, err := c.CreateAccount(ctx, uid, AccountInput{Kind: store.AcctOpenAICompatible, Name: "LiteLLM", BaseURL: "http://litellm:4000/v1/", Models: []string{" a ", "b", "a", ""}, Value: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if compat.BaseURL != "http://litellm:4000/v1" || len(compat.Models) != 2 || !compat.HasValue {
		t.Fatalf("compat = %+v", compat)
	}

	// Git accounts: one per kind and host, named after the host.
	gh, err := c.CreateAccount(ctx, uid, AccountInput{Kind: store.AcctGitHTTPS, Host: " GitHub.com ", Value: "ghp_x"})
	if err != nil {
		t.Fatal(err)
	}
	if gh.Host != "github.com" || gh.Name != "github.com" {
		t.Fatalf("git account = %+v", gh)
	}
	invalid(AccountInput{Kind: store.AcctGitHTTPS, Host: "github.com", Value: "ghp_y"})
	if _, err := c.CreateAccount(ctx, uid, AccountInput{Kind: store.AcctGitSSH, Host: "github.com", Value: "-----BEGIN KEY-----\nx\n-----END KEY-----"}); err != nil {
		t.Fatalf("an SSH key for a host with an HTTPS token: %v", err)
	}
	if string(accountValue(t, c, uid, store.AcctGitSSH)) != "-----BEGIN KEY-----\nx\n-----END KEY-----\n" {
		t.Fatalf("ssh key = %q", accountValue(t, c, uid, store.AcctGitSSH))
	}
	if v, _ := c.GitHubToken(ctx, uid); string(v) != "ghp_x" {
		t.Fatalf("github token = %q", v)
	}

	// Changing settings keeps the value unless a new one is given.
	up, err := c.UpdateAccount(ctx, uid, compat.ID, AccountInput{Name: "Gateway", BaseURL: "http://litellm:4000/v1"})
	if err != nil || up.Name != "Gateway" || len(up.Models) != 0 || !up.HasValue {
		t.Fatalf("update = %+v, %v", up, err)
	}
	if _, err := c.UpdateAccount(ctx, uid, compat.ID, AccountInput{Kind: store.AcctUnjailed, Name: "G"}); err == nil {
		t.Fatal("kind changed")
	}

	// Defaults must be accounts the agent signs in with.
	if _, err := c.PutProfile(ctx, uid, ProfileInfo{Defaults: map[string]string{runner.AgentClaude: compat.ID}}); err == nil {
		t.Fatal("an OpenAI-compatible account accepted as Claude Code's default")
	}
	p, err := c.PutProfile(ctx, uid, ProfileInfo{GitName: " Alice  A ", GitEmail: "a@x", Defaults: map[string]string{runner.AgentClaude: tok.ID, runner.AgentCodex: ""}})
	if err != nil || p.GitName != "Alice A" || p.Defaults[runner.AgentClaude] != tok.ID || len(p.Defaults) != 1 {
		t.Fatalf("profile = %+v, %v", p, err)
	}

	// Deleting an account drops its value and any default naming it.
	if err := c.DeleteAccount(ctx, uid, tok.ID); err != nil {
		t.Fatal(err)
	}
	if v := accountValue(t, c, uid, store.AcctClaudeOAuthToken); v != nil {
		t.Fatal("value left behind")
	}
	data, _ := c.secretData(ctx, uid)
	if _, ok := data[store.AccountSecretKey(tok.ID)]; ok {
		t.Fatal("secret key left behind")
	}
	if p, _ := c.Profile(ctx, uid); p.Defaults[runner.AgentClaude] != "" {
		t.Fatalf("default still names the deleted account: %+v", p)
	}
	list, _ := c.ListAccounts(ctx, uid)
	if len(list) != 3 {
		t.Fatalf("accounts = %+v", list)
	}
	if err := c.DeleteAccount(ctx, uid, tok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

func TestSaveLoginAccounts(t *testing.T) {
	c, uid := newCreds(t)
	ctx := context.Background()
	a, err := c.SaveLogin(ctx, uid, "", store.AcctClaudeLogin, []byte("one"))
	if err != nil || a.Name != "Claude Code login" {
		t.Fatalf("first = %+v, %v", a, err)
	}
	b, err := c.SaveLogin(ctx, uid, "", store.AcctClaudeLogin, []byte("two"))
	if err != nil || b.Name != "Claude Code login 2" {
		t.Fatalf("second = %+v, %v", b, err)
	}
	before := a.UpdatedAt
	again, err := c.SaveLogin(ctx, uid, a.ID, store.AcctClaudeLogin, []byte("three"))
	if err != nil || again.ID != a.ID || !again.UpdatedAt.After(before) {
		t.Fatalf("replace = %+v, %v", again, err)
	}
	if v := accountValue(t, c, uid, store.AcctClaudeLogin); string(v) != "three" {
		t.Fatalf("value = %q", v)
	}
	if _, err := c.SaveLogin(ctx, uid, a.ID, store.AcctCodexLogin, []byte("x")); err == nil {
		t.Fatal("a Codex login written into a Claude login account")
	}
}

// legacy writes old-style credential slots: rows plus Secret keys.
func legacy(t *testing.T, c *Credentials, uid string, values map[string]string) {
	t.Helper()
	ctx := context.Background()
	data := map[string][]byte{}
	for k, v := range values {
		data[k] = []byte(v)
		if err := c.Store.Credentials().Upsert(ctx, &store.Credential{UserID: uid, Kind: k, SecretRef: k}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.CS.CoreV1().Secrets(c.Namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: reconcile.UserSecretName(uid), Namespace: c.Namespace},
		Data:       data,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrateCredentials(t *testing.T) {
	c, uid := newCreds(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	legacy(t, c, uid, map[string]string{
		store.CredAnthropicAPIKey:  "sk-ant",
		store.CredAnthropicBaseURL: "http://litellm:4000/",
		store.CredClaudeOAuthToken: "sk-ant-oat01-x",
		store.CredCodexLogin:       "codex-bundle",
		store.CredUnjailedAPIKey:   "sk-uj",
		store.CredGitHubToken:      "ghp_x",
		store.CredGitHTTPSToken:    "glpat",
		store.CredGitSSHKey:        "-----BEGIN KEY-----\nk\n-----END KEY-----\n",
		store.CredGitUserName:      "Alice",
		store.CredGitUserEmail:     "alice@example.com",
	})
	// Kept by the user next to the old slots: left alone.
	if _, err := c.CreateAccount(ctx, uid, AccountInput{Kind: store.AcctOpenAIAPIKey, Name: "Mine", Value: "sk-mine"}); err != nil {
		t.Fatal(err)
	}

	if err := c.MigrateCredentials(ctx, log); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListAccounts(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]AccountInfo{}
	for _, a := range list {
		if !a.HasValue {
			t.Errorf("%s has no value", a.Name)
		}
		byName[a.Kind+"/"+a.Name] = a
	}
	for _, want := range []string{
		"openai_api_key/Mine", "anthropic_api_key/Anthropic", "claude_oauth_token/Claude Code token", "codex_login/Codex login",
		"unjailed/Unjailed", "git_https/github.com", "git_https/*", "git_ssh/*",
	} {
		if _, ok := byName[want]; !ok {
			t.Errorf("no account %s in %v", want, byName)
		}
	}
	if len(list) != 8 {
		t.Errorf("%d accounts", len(list))
	}
	if a := byName["anthropic_api_key/Anthropic"]; a.BaseURL != "http://litellm:4000" {
		t.Errorf("base URL = %q", a.BaseURL)
	}
	data, _ := c.secretData(ctx, uid)
	if string(data[store.AccountSecretKey(byName["git_https/*"].ID)]) != "glpat" || string(data[store.AccountSecretKey(byName["git_https/github.com"].ID)]) != "ghp_x" {
		t.Error("git tokens mixed up")
	}
	// The old slots are gone, from the Secret and the table.
	for _, k := range []string{store.CredAnthropicAPIKey, store.CredGitSSHKey, store.CredGitUserName, store.CredAnthropicBaseURL} {
		if _, ok := data[k]; ok {
			t.Errorf("old key %s left in the Secret", k)
		}
	}
	if rows, _ := c.Store.Credentials().List(ctx, uid); len(rows) != 0 {
		t.Errorf("old rows left: %+v", rows)
	}
	// Identity and defaults: Claude Code kept preferring the API key, Codex its login.
	p, _ := c.Profile(ctx, uid)
	if p.GitName != "Alice" || p.GitEmail != "alice@example.com" ||
		p.Defaults[runner.AgentClaude] != byName["anthropic_api_key/Anthropic"].ID || p.Defaults[runner.AgentCodex] != byName["codex_login/Codex login"].ID {
		t.Fatalf("profile = %+v", p)
	}

	// Nothing left to do: a second run changes nothing.
	if err := c.MigrateCredentials(ctx, log); err != nil {
		t.Fatal(err)
	}
	if again, _ := c.ListAccounts(ctx, uid); len(again) != len(list) {
		t.Fatalf("second run: %d accounts", len(again))
	}
}

func TestMigrateCredentialsResumes(t *testing.T) {
	c, uid := newCreds(t)
	ctx := context.Background()
	legacy(t, c, uid, map[string]string{store.CredOpenAIAPIKey: "sk-new", store.CredGitHubToken: "ghp_x"})
	// A run that created the accounts but stopped before the values and the
	// clean-up: the next one reuses them.
	if err := c.Store.Accounts().Create(ctx, &store.Account{UserID: uid, Kind: store.AcctOpenAIAPIKey, Name: "OpenAI"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.Accounts().Create(ctx, &store.Account{UserID: uid, Kind: store.AcctGitHTTPS, Name: "github.com", Host: "github.com"}); err != nil {
		t.Fatal(err)
	}
	if err := c.MigrateCredentials(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	list, _ := c.ListAccounts(ctx, uid)
	if len(list) != 2 || !list[0].HasValue || !list[1].HasValue {
		t.Fatalf("accounts = %+v", list)
	}
	if v := accountValue(t, c, uid, store.AcctOpenAIAPIKey); string(v) != "sk-new" {
		t.Fatalf("value = %q", v)
	}
}
