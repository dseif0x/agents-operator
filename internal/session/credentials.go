package session

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/reconcile"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Credentials manages a user's accounts and profile. Account values live
// only in the per-user Secret `agents-operator-user-<id>` (under
// store.AccountSecretKey); the store holds everything else. The hub reads
// the Secret only to project values into session Secrets and for its own
// GitHub calls, and never returns a value through the API.
type Credentials struct {
	Store     store.Store
	CS        kubernetes.Interface
	Namespace string
}

// AccountInfo is the API view of an account: everything but the value.
type AccountInfo struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Host     string   `json:"host,omitempty"`
	BaseURL  string   `json:"base_url,omitempty"`
	Username string   `json:"username,omitempty"`
	Models   []string `json:"models"`
	// HasValue is false for an account whose value is missing from the
	// Secret (deleted by hand, say); sessions skip it.
	HasValue  bool      `json:"has_value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AccountInput is the body of creating or changing an account. Kind is
// fixed once created. Value is the secret (key, token, SSH key); on a
// change an empty one keeps the current value.
type AccountInput struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Host     string   `json:"host"`
	BaseURL  string   `json:"base_url"`
	Username string   `json:"username"`
	Models   []string `json:"models"`
	Value    string   `json:"value"`
}

// ProfileInfo is the API view (and input) of the profile.
type ProfileInfo struct {
	GitName  string            `json:"git_name"`
	GitEmail string            `json:"git_email"`
	Defaults map[string]string `json:"defaults"`
}

func accountInfo(a *store.Account, data map[string][]byte) AccountInfo {
	models := a.Models
	if models == nil {
		models = []string{}
	}
	return AccountInfo{
		ID: a.ID, Kind: a.Kind, Name: a.Name, Host: a.Host, BaseURL: a.BaseURL, Username: a.Username, Models: models,
		HasValue: len(data[store.AccountSecretKey(a.ID)]) > 0, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

// ListAccounts describes the user's accounts, oldest first.
func (c *Credentials) ListAccounts(ctx context.Context, userID string) ([]AccountInfo, error) {
	rows, err := c.Store.Accounts().List(ctx, userID)
	if err != nil {
		return nil, err
	}
	data, err := c.secretData(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]AccountInfo, 0, len(rows))
	for _, a := range rows {
		out = append(out, accountInfo(a, data))
	}
	return out, nil
}

// CreateAccount adds an account. Logins cannot be created here: they are
// saved from a session (Service.SaveLogin).
func (c *Credentials) CreateAccount(ctx context.Context, userID string, in AccountInput) (*AccountInfo, error) {
	if in.Kind == store.AcctClaudeLogin || in.Kind == store.AcctCodexLogin {
		return nil, &ValidationError{"a saved login is added from a running session: sign in there and use Save login"}
	}
	a := &store.Account{UserID: userID, Kind: in.Kind}
	value, err := applyInput(a, in, true)
	if err != nil {
		return nil, err
	}
	return c.create(ctx, a, value)
}

func (c *Credentials) create(ctx context.Context, a *store.Account, value []byte) (*AccountInfo, error) {
	if err := c.Store.Accounts().Create(ctx, a); err != nil {
		return nil, conflictMsg(a, err)
	}
	if err := c.setValues(ctx, a.UserID, map[string][]byte{store.AccountSecretKey(a.ID): value}); err != nil {
		_ = c.Store.Accounts().Delete(ctx, a.UserID, a.ID)
		return nil, err
	}
	info := accountInfo(a, map[string][]byte{store.AccountSecretKey(a.ID): value})
	return &info, nil
}

// UpdateAccount changes an account's settings and, when in.Value is set,
// its value.
func (c *Credentials) UpdateAccount(ctx context.Context, userID, id string, in AccountInput) (*AccountInfo, error) {
	a, err := c.Store.Accounts().Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if in.Kind != "" && in.Kind != a.Kind {
		return nil, &ValidationError{"an account's kind cannot change; add a new account instead"}
	}
	in.Kind = a.Kind
	value, err := applyInput(a, in, false)
	if err != nil {
		return nil, err
	}
	if err := c.Store.Accounts().Update(ctx, a); err != nil {
		return nil, conflictMsg(a, err)
	}
	if len(value) > 0 {
		if err := c.setValues(ctx, userID, map[string][]byte{store.AccountSecretKey(a.ID): value}); err != nil {
			return nil, err
		}
	}
	data, err := c.secretData(ctx, userID)
	if err != nil {
		return nil, err
	}
	info := accountInfo(a, data)
	return &info, nil
}

// DeleteAccount removes an account, its value and any default naming it.
// Sessions that picked it fall back to the default on their next start.
func (c *Credentials) DeleteAccount(ctx context.Context, userID, id string) error {
	if _, err := c.Store.Accounts().Get(ctx, userID, id); err != nil {
		return err
	}
	if err := c.deleteValues(ctx, userID, store.AccountSecretKey(id)); err != nil {
		return err
	}
	if err := c.Store.Accounts().Delete(ctx, userID, id); err != nil {
		return err
	}
	p, err := c.Store.Profiles().Get(ctx, userID)
	if err != nil {
		return err
	}
	changed := false
	for agent, acct := range p.Defaults {
		if acct == id {
			delete(p.Defaults, agent)
			changed = true
		}
	}
	if changed {
		return c.Store.Profiles().Put(ctx, p)
	}
	return nil
}

// SaveLogin stores a CLI's exported login bundle: into account id when
// given (a login account of that kind), else into a new login account. It
// returns the account.
func (c *Credentials) SaveLogin(ctx context.Context, userID, id, kind string, bundle []byte) (*store.Account, error) {
	if len(bundle) == 0 {
		return nil, errors.New("empty login")
	}
	if id != "" {
		a, err := c.Store.Accounts().Get(ctx, userID, id)
		if err != nil {
			return nil, err
		}
		if a.Kind != kind {
			return nil, &ValidationError{"account " + a.Name + " is not a " + kindLabel[kind]}
		}
		if err := c.setValues(ctx, userID, map[string][]byte{store.AccountSecretKey(a.ID): bundle}); err != nil {
			return nil, err
		}
		// Bumps updated_at: syncLogin compares the CLI's file against it.
		if err := c.Store.Accounts().Update(ctx, a); err != nil {
			return nil, err
		}
		return a, nil
	}
	existing, err := c.Store.Accounts().List(ctx, userID)
	if err != nil {
		return nil, err
	}
	a := &store.Account{UserID: userID, Kind: kind, Name: uniqueName(kindLabel[kind], existing)}
	if _, err := c.create(ctx, a, bundle); err != nil {
		return nil, err
	}
	return a, nil
}

// GitHubToken is the token of the user's github.com HTTPS account, or nil.
// It exists for the hub's own GitHub calls (the repository picker).
func (c *Credentials) GitHubToken(ctx context.Context, userID string) ([]byte, error) {
	rows, err := c.Store.Accounts().List(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, a := range rows {
		if a.Kind == store.AcctGitHTTPS && strings.EqualFold(a.Host, "github.com") {
			data, err := c.secretData(ctx, userID)
			if err != nil {
				return nil, err
			}
			return data[store.AccountSecretKey(a.ID)], nil
		}
	}
	return nil, nil
}

// Profile returns the user's profile.
func (c *Credentials) Profile(ctx context.Context, userID string) (*ProfileInfo, error) {
	p, err := c.Store.Profiles().Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &ProfileInfo{GitName: p.GitName, GitEmail: p.GitEmail, Defaults: p.Defaults}, nil
}

// PutProfile replaces the profile. A default must be an account the agent
// can sign in with; an empty one is dropped.
func (c *Credentials) PutProfile(ctx context.Context, userID string, in ProfileInfo) (*ProfileInfo, error) {
	accounts, err := c.Store.Accounts().List(ctx, userID)
	if err != nil {
		return nil, err
	}
	p := &store.Profile{UserID: userID, GitName: oneLine(in.GitName), GitEmail: oneLine(in.GitEmail), Defaults: map[string]string{}}
	if len(p.GitName) > 200 || len(p.GitEmail) > 200 {
		return nil, &ValidationError{"the git name and email are limited to 200 characters"}
	}
	for agent, id := range in.Defaults {
		if id == "" {
			continue
		}
		kinds, ok := reconcile.SignInKinds[agent]
		if !ok {
			return nil, &ValidationError{"agent " + agent + " has no default account"}
		}
		i := slices.IndexFunc(accounts, func(a *store.Account) bool { return a.ID == id })
		if i < 0 || !slices.Contains(kinds, accounts[i].Kind) {
			return nil, &ValidationError{"the default account for " + agent + " is not one it can sign in with"}
		}
		p.Defaults[agent] = id
	}
	if err := c.Store.Profiles().Put(ctx, p); err != nil {
		return nil, err
	}
	return &ProfileInfo{GitName: p.GitName, GitEmail: p.GitEmail, Defaults: p.Defaults}, nil
}

// ---- validation ----

// kindLabel names the kinds in messages and default names.
var kindLabel = map[string]string{
	store.AcctAnthropicAPIKey:  "Anthropic API key",
	store.AcctClaudeOAuthToken: "Claude Code token",
	store.AcctClaudeLogin:      "Claude Code login",
	store.AcctOpenAIAPIKey:     "OpenAI API key",
	store.AcctCodexLogin:       "Codex login",
	store.AcctOpenAICompatible: "OpenAI-compatible endpoint",
	store.AcctUnjailed:         "Unjailed",
	store.AcctGitHTTPS:         "HTTPS token",
	store.AcctGitSSH:           "SSH key",
}

// claudeOAuthTokenPrefix is what `claude setup-token` output starts with.
const claudeOAuthTokenPrefix = "sk-ant-oat"

// gitHostRE accepts a host name or ssh-style pattern (*, ?), with a port
// for HTTPS.
var gitHostRE = regexp.MustCompile(`^[a-z0-9*?]([a-z0-9*?.-]{0,252})(:[0-9]{1,5})?$`)

// MaxModels caps an OpenAI-compatible account's model list.
const MaxModels = 200

// applyInput validates in for a's kind and copies the settings onto a. It
// returns the normalised value (nil when a change keeps the current one).
func applyInput(a *store.Account, in AccountInput, create bool) ([]byte, error) {
	if !store.ValidAccountKind(a.Kind) {
		return nil, &ValidationError{"unknown account kind " + a.Kind}
	}
	value, err := normaliseValue(a.Kind, in.Value)
	if err != nil {
		return nil, err
	}
	if create && len(value) == 0 {
		return nil, &ValidationError{"the " + kindLabel[a.Kind] + " is missing"}
	}
	a.Host, a.BaseURL, a.Username, a.Models = "", "", "", nil
	if store.GitAccountKind(a.Kind) {
		host := strings.ToLower(strings.TrimSpace(in.Host))
		host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "ssh://"), "/")
		if !gitHostRE.MatchString(host) || (a.Kind == store.AcctGitSSH && strings.Contains(host, ":")) {
			return nil, &ValidationError{"the host must be a host name such as github.com, a pattern such as *.example.com, or * for any host"}
		}
		a.Host = host
		a.Name = cmp.Or(oneLine(in.Name), host)
		if a.Kind == store.AcctGitHTTPS {
			a.Username = oneLine(in.Username)
		}
	} else {
		a.Name = oneLine(in.Name)
		if a.Name == "" {
			return nil, &ValidationError{"an AI account needs a name"}
		}
	}
	if len(a.Name) > 64 {
		return nil, &ValidationError{"names are limited to 64 characters"}
	}
	switch a.Kind {
	case store.AcctAnthropicAPIKey, store.AcctUnjailed, store.AcctOpenAICompatible:
		base, err := normaliseURL(in.BaseURL)
		if err != nil {
			return nil, err
		}
		if base == "" && a.Kind == store.AcctOpenAICompatible {
			return nil, &ValidationError{"an OpenAI-compatible endpoint needs its base URL, such as http://litellm:4000/v1"}
		}
		a.BaseURL = base
	}
	if a.Kind == store.AcctOpenAICompatible {
		for _, m := range in.Models {
			if m = strings.TrimSpace(m); m != "" && len(m) <= 200 && !slices.Contains(a.Models, m) {
				a.Models = append(a.Models, m)
			}
		}
		if len(a.Models) > MaxModels {
			return nil, &ValidationError{fmt.Sprintf("at most %d models", MaxModels)}
		}
	}
	return value, nil
}

// normaliseValue removes whitespace from tokens (anything pasted from a
// terminal: Claude Code prints `claude setup-token`'s result wrapped to the
// terminal width, and on a phone that is several lines) and makes sure an
// SSH key ends with a newline.
func normaliseValue(kind, v string) ([]byte, error) {
	switch kind {
	case store.AcctGitSSH:
		s := strings.TrimRight(v, "\r\n\t ")
		s = strings.TrimLeft(s, "\r\n\t ")
		if s == "" {
			return nil, nil
		}
		if !strings.HasPrefix(s, "-----BEGIN ") {
			return nil, &ValidationError{"paste the private key itself, starting with -----BEGIN"}
		}
		return []byte(s + "\n"), nil
	case store.AcctClaudeLogin, store.AcctCodexLogin:
		if v != "" {
			return nil, &ValidationError{"a saved login can only be replaced from a session"}
		}
		return nil, nil
	default:
		s := strings.Join(strings.Fields(v), "")
		if kind == store.AcctClaudeOAuthToken && s != "" && !strings.HasPrefix(s, claudeOAuthTokenPrefix) {
			return nil, &ValidationError{"a Claude Code OAuth token starts with " + claudeOAuthTokenPrefix + "…; paste the whole token printed by `claude setup-token`"}
		}
		if s == "" {
			return nil, nil
		}
		return []byte(s), nil
	}
}

func normaliseURL(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", nil
	}
	if u, err := url.Parse(s); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", &ValidationError{"the base URL must be an http(s) URL such as https://unjailedai.com/v1"}
	}
	return s, nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func conflictMsg(a *store.Account, err error) error {
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	if store.GitAccountKind(a.Kind) {
		return &ValidationError{"there already is an " + kindLabel[a.Kind] + " for " + a.Host + "; change that one instead"}
	}
	return &ValidationError{"there already is an account named " + a.Name}
}

// uniqueName is base, or base with the first free number.
func uniqueName(base string, existing []*store.Account) string {
	taken := func(n string) bool {
		return slices.ContainsFunc(existing, func(a *store.Account) bool {
			return !store.GitAccountKind(a.Kind) && strings.EqualFold(a.Name, n)
		})
	}
	name := base
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s %d", base, i)
	}
	return name
}

// ---- the user Secret ----

func (c *Credentials) secretData(ctx context.Context, userID string) (map[string][]byte, error) {
	sec, err := c.CS.CoreV1().Secrets(c.Namespace).Get(ctx, reconcile.UserSecretName(userID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read user secret: %w", err)
	}
	return sec.Data, nil
}

// setValues writes values into the user's Secret, creating it if needed.
func (c *Credentials) setValues(ctx context.Context, userID string, values map[string][]byte) error {
	name := reconcile.UserSecretName(userID)
	secrets := c.CS.CoreV1().Secrets(c.Namespace)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: map[string]string{
				k8s.LabelUser: userID, k8s.LabelManagedBy: k8s.ManagedBy, k8s.LabelName: "agents-operator", k8s.LabelComponent: "user-credentials",
			}},
			Type: corev1.SecretTypeOpaque,
			Data: values,
		}
		if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create user secret: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read user secret: %w", err)
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range values {
		sec.Data[k] = v
	}
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update user secret: %w", err)
	}
	return nil
}

// deleteValues removes keys from the user's Secret.
func (c *Credentials) deleteValues(ctx context.Context, userID string, keys ...string) error {
	secrets := c.CS.CoreV1().Secrets(c.Namespace)
	sec, err := secrets.Get(ctx, reconcile.UserSecretName(userID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read user secret: %w", err)
	}
	n := len(sec.Data)
	for _, k := range keys {
		delete(sec.Data, k)
	}
	if len(sec.Data) == n {
		return nil
	}
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update user secret: %w", err)
	}
	return nil
}

// loginAccount is the login account a session's CLI of kind signed in
// with, or nil: what syncLogin refreshes and Save login replaces.
func (c *Credentials) loginAccount(ctx context.Context, sess *store.Session, kind string) (*store.Account, error) {
	accounts, err := c.Store.Accounts().List(ctx, sess.OwnerID)
	if err != nil {
		return nil, err
	}
	profile, err := c.Store.Profiles().Get(ctx, sess.OwnerID)
	if err != nil {
		return nil, err
	}
	for _, a := range reconcile.SignInAccounts(sess, accounts, profile) {
		if a.Kind == kind {
			return a, nil
		}
	}
	return nil, nil
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
