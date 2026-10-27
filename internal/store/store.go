// Package store is the Postgres access layer. One interface per aggregate,
// a Postgres implementation, and an in-memory implementation for tests.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/config"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique violations.
var ErrConflict = errors.New("conflict")

// Session states. See docs/ARCHITECTURE.md for the transitions.
const (
	StateCreating = "creating"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateFailed   = "failed"
	StateDeleting = "deleting"
)

// States lists every state, for validation and metrics.
var States = []string{StateCreating, StateRunning, StateStopping, StateStopped, StateFailed, StateDeleting}

// Kubernetes access modes of a session (Session.K8sAccess).
const (
	K8sAccessOff       = "off"
	K8sAccessReadOnly  = "readonly"
	K8sAccessNamespace = "namespace"
	// K8sAccessSandbox gives the session a virtual cluster (vcluster) of its
	// own, in a host namespace created for it, and only that cluster's
	// kubeconfig: the agent is cluster-admin there and has no host access.
	K8sAccessSandbox = "sandbox"
)

// K8sAccessModes lists every mode, for validation.
var K8sAccessModes = []string{K8sAccessOff, K8sAccessReadOnly, K8sAccessNamespace, K8sAccessSandbox}

// User is a login account.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	Disabled     bool
}

// Account kinds. An account is one credential with a name: AI accounts
// sign in to a model provider, git accounts to a git host. Values live in
// the per-user Kubernetes Secret under AccountSecretKey, never here.
const (
	// AcctAnthropicAPIKey is an Anthropic API key, with an optional base URL
	// (a gateway such as LiteLLM).
	AcctAnthropicAPIKey = "anthropic_api_key"
	// AcctClaudeOAuthToken is a long-lived Claude Code token from
	// `claude setup-token`, projected as CLAUDE_CODE_OAUTH_TOKEN. Unlike a
	// saved login it never rotates, so any number of sessions can share it.
	AcctClaudeOAuthToken = "claude_oauth_token"
	// AcctClaudeLogin and AcctCodexLogin are a CLI's saved login files
	// (runner.LoginBundle), saved from a session.
	AcctClaudeLogin = "claude_login"
	AcctCodexLogin  = "codex_login"
	// AcctOpenAIAPIKey is an OpenAI API key.
	AcctOpenAIAPIKey = "openai_api_key"
	// AcctOpenAICompatible is any OpenAI-compatible endpoint: base URL, key
	// and optionally the models to offer (else the endpoint's model list).
	AcctOpenAICompatible = "openai_compatible"
	// AcctUnjailed is an unjailedai.com key (base URL optional): an
	// OpenAI-compatible endpoint whose models OpenCode is told the limits of.
	AcctUnjailed = "unjailed"
	// AcctGitHTTPS is a token for one git host over HTTPS (username
	// optional); the one for github.com is also the GitHub CLI's token.
	AcctGitHTTPS = "git_https"
	// AcctGitSSH is an SSH private key for one git host.
	AcctGitSSH = "git_ssh"
)

// AccountKinds lists every accepted kind, in the order the UI offers them.
var AccountKinds = []string{
	AcctAnthropicAPIKey, AcctClaudeOAuthToken, AcctClaudeLogin, AcctOpenAIAPIKey, AcctCodexLogin,
	AcctOpenAICompatible, AcctUnjailed, AcctGitHTTPS, AcctGitSSH,
}

// ValidAccountKind reports whether kind is known.
func ValidAccountKind(kind string) bool { return slices.Contains(AccountKinds, kind) }

// GitAccountKind reports whether kind is a git account (one per host).
func GitAccountKind(kind string) bool { return kind == AcctGitHTTPS || kind == AcctGitSSH }

// AnyHost is the host of a git account that serves every host no other
// account of its kind names.
const AnyHost = "*"

// Account is one row of the accounts table.
type Account struct {
	ID     string
	UserID string
	Kind   string
	Name   string
	// Host is the git host (git accounts only): "github.com", a pattern
	// such as "*.example.com", or AnyHost.
	Host string
	// BaseURL is the endpoint (Anthropic key, OpenAI-compatible, Unjailed).
	BaseURL string
	// Username is the HTTPS username (git_https; empty = x-access-token).
	Username string
	// Models is the model list of an OpenAI-compatible endpoint; empty =
	// ask the endpoint.
	Models    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AccountSecretKey is the key of an account's value in the user's Secret.
func AccountSecretKey(id string) string { return "acct-" + id }

// Profile is a user's settings that are not credentials.
type Profile struct {
	UserID   string
	GitName  string
	GitEmail string
	// Defaults maps an agent (claude, codex) to the account its sessions
	// use when they do not pick one.
	Defaults  map[string]string
	UpdatedAt time.Time
}

// Legacy credential kinds: the fixed slots of user_credentials before
// accounts. Only the migration (session.MigrateCredentials) reads them.
const (
	CredAnthropicAPIKey  = "anthropic_api_key"
	CredAnthropicBaseURL = "anthropic_base_url"
	CredOpenAIAPIKey     = "openai_api_key"
	CredUnjailedAPIKey   = "unjailed_api_key"
	CredUnjailedBaseURL  = "openclaude_base_url"
	CredGitSSHKey        = "git_ssh_key"
	CredGitHTTPSToken    = "git_https_token"
	CredGitHubToken      = "github_token"
	CredGitUserName      = "git_user_name"
	CredGitUserEmail     = "git_user_email"
	CredClaudeOAuthToken = "claude_oauth_token"
	CredClaudeLogin      = "claude_login"
	CredCodexLogin       = "codex_login"
)

// Credential is a legacy user_credentials row: the user has a value of a
// given kind in their Kubernetes Secret under SecretRef.
type Credential struct {
	UserID    string
	Kind      string
	SecretRef string
	UpdatedAt time.Time
}

// Repo is one repository checked out into /workspace/<Path> on first boot.
// The agent starts in /workspace itself, one level above every repo.
type Repo struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path"`
}

// Session is one row of the sessions table.
type Session struct {
	ID           string
	OwnerID      string
	Name         string
	Agent        string
	Repos        []Repo
	ImageTag     string
	PVCSize      string
	StorageClass string
	// RuntimeClass overrides the chart-wide runtimeClassName; empty = default.
	RuntimeClass string
	// K8sAccess is what the pod may do in the cluster: K8sAccessOff (no
	// identity), K8sAccessReadOnly (the chart's read-only runner
	// ServiceAccount) or K8sAccessNamespace (an account of its own, read-only
	// like the shared one plus write access in K8sNamespaces) or
	// K8sAccessSandbox (a vcluster of its own, see reconcile/sandbox.go).
	K8sAccess     string
	K8sNamespaces []string
	Resources     config.Resources
	NodeSelector  map[string]string
	Tolerations   []config.Toleration
	Env           map[string]string
	Autonomous    bool
	// NoGitCredentials keeps the owner's git credentials (SSH key, HTTPS
	// and GitHub tokens) from the agent. The runner still clones with them
	// on first boot; the agent can commit but not push or use the GitHub API.
	NoGitCredentials bool
	// Egress lists the local network destinations the agent may reach on
	// top of the chart's NetworkPolicy, in internal/egress canonical form
	// (`192.168.1.10`, `192.168.1.0/24`, `192.168.1.10:8080`).
	Egress []string
	// SystemPrompt is a custom system prompt for OpenClaude, appended to
	// its own or, with SystemPromptReplace, used instead of it.
	SystemPrompt        string
	SystemPromptReplace bool
	// AgentDefinition is an OpenClaude agent definition (Markdown with
	// front matter, see internal/agentdef) the session starts with --agent.
	AgentDefinition string
	// AIAccount is the account a Claude Code or Codex session signs in
	// with; empty = the owner's default for the agent.
	AIAccount      string
	State          string
	StateReason    string
	Generation     int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastAttachedAt *time.Time
	LastOutputAt   *time.Time
	DeletedAt      *time.Time
}

// Event is one row of session_events.
type Event struct {
	ID        int64
	SessionID string
	At        time.Time
	Kind      string
	Message   string
}

// Users is the user aggregate.
type Users interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id string) (*User, error)
	GetByUsername(ctx context.Context, username string) (*User, error)
	// UpsertPassword creates the user or replaces its password hash.
	UpsertPassword(ctx context.Context, username, passwordHash string) (*User, error)
}

// Sessions is the session aggregate.
type Sessions interface {
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context, ownerID string) ([]*Session, error)
	ListAll(ctx context.Context) ([]*Session, error)
	// Update replaces the editable settings (name, repos, image tag,
	// runtime class, service account, resources, node selector,
	// tolerations, env, autonomous, git credentials) and returns the
	// updated row.
	Update(ctx context.Context, s *Session) (*Session, error)
	// SetState updates state and reason. It returns the updated row.
	SetState(ctx context.Context, id, state, reason string) (*Session, error)
	// Bump increments generation and sets the state, in one statement.
	Bump(ctx context.Context, id, state string) (*Session, error)
	TouchAttached(ctx context.Context, id string, at time.Time) error
	TouchOutput(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
	CountByState(ctx context.Context) (map[string]int, error)
}

// Credentials is the legacy user_credentials aggregate, kept for the
// migration to accounts.
type Credentials interface {
	Upsert(ctx context.Context, c *Credential) error
	Delete(ctx context.Context, userID, kind string) error
	List(ctx context.Context, userID string) ([]*Credential, error)
	// UserIDs lists the users that still have rows.
	UserIDs(ctx context.Context) ([]string, error)
}

// Accounts is the accounts aggregate.
type Accounts interface {
	// Create inserts a, filling ID and timestamps. ErrConflict when the user
	// has an AI account of that name or a git account of that kind and host.
	Create(ctx context.Context, a *Account) error
	// Update replaces name, host, base URL, username and models and bumps
	// updated_at (also the way to record that the value changed).
	Update(ctx context.Context, a *Account) error
	Get(ctx context.Context, userID, id string) (*Account, error)
	// List returns the user's accounts, oldest first.
	List(ctx context.Context, userID string) ([]*Account, error)
	Delete(ctx context.Context, userID, id string) error
}

// Profiles is the user_profiles aggregate.
type Profiles interface {
	// Get returns the profile, or an empty one when none is stored.
	Get(ctx context.Context, userID string) (*Profile, error)
	Put(ctx context.Context, p *Profile) error
}

// Events is the session_events aggregate.
type Events interface {
	Add(ctx context.Context, sessionID, kind, message string) error
	List(ctx context.Context, sessionID string, limit int) ([]*Event, error)
	// Prune keeps only the newest keep events for the session.
	Prune(ctx context.Context, sessionID string, keep int) error
}

// RepoUsage counts how often a user started a session with a repository.
// It survives session deletion so the picker can rank by real use.
type RepoUsage interface {
	Increment(ctx context.Context, userID, repoKey string) error
	// List returns repoKey -> count for the user.
	List(ctx context.Context, userID string) (map[string]int, error)
}

// RepoKey normalises a git URL so https, ssh and git@ spellings of the same
// repository count together: "github.com/owner/repo" in lower case.
func RepoKey(u string) string {
	s := strings.TrimSpace(strings.ToLower(u))
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	for _, prefix := range []string{"https://", "http://", "ssh://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	if i := strings.Index(s, "@"); i >= 0 && !strings.Contains(s[:i], "/") {
		s = s[i+1:] // drop user@
	}
	s = strings.Replace(s, ":", "/", 1) // git@host:owner/repo -> host/owner/repo
	return strings.TrimSuffix(s, "/")
}

// Store bundles the aggregates.
type Store interface {
	Users() Users
	Sessions() Sessions
	Credentials() Credentials
	Accounts() Accounts
	Profiles() Profiles
	Events() Events
	RepoUsage() RepoUsage
	Ping(ctx context.Context) error
	Close()
}

// EventsKeep is how many events are retained per session.
const EventsKeep = 200

// NewID returns a random UUIDv4 string.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
