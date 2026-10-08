// Package store is the Postgres access layer. One interface per aggregate,
// a Postgres implementation, and an in-memory implementation for tests.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
)

// K8sAccessModes lists every mode, for validation.
var K8sAccessModes = []string{K8sAccessOff, K8sAccessReadOnly, K8sAccessNamespace}

// User is a login account.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	Disabled     bool
}

// Credential kinds. Values live in the per-user Kubernetes Secret, never
// here; the row only records which kinds are set.
const (
	CredAnthropicAPIKey  = "anthropic_api_key"
	CredAnthropicBaseURL = "anthropic_base_url"
	CredOpenAIAPIKey     = "openai_api_key"
	CredGitSSHKey        = "git_ssh_key"
	CredGitHTTPSToken    = "git_https_token"
	CredGitHubToken      = "github_token"
	CredGitUserName      = "git_user_name"
	CredGitUserEmail     = "git_user_email"
	// CredClaudeOAuthToken is a long-lived Claude Code token from
	// `claude setup-token`, projected as CLAUDE_CODE_OAUTH_TOKEN. Unlike a
	// saved login it never rotates, so any number of sessions can share it.
	CredClaudeOAuthToken = "claude_oauth_token"
	CredClaudeLogin      = "claude_login"
	CredCodexLogin       = "codex_login"
)

// CredentialKinds lists every accepted kind.
var CredentialKinds = []string{
	CredAnthropicAPIKey, CredAnthropicBaseURL, CredClaudeOAuthToken, CredOpenAIAPIKey,
	CredGitSSHKey, CredGitHTTPSToken, CredGitHubToken, CredGitUserName, CredGitUserEmail,
	CredClaudeLogin, CredCodexLogin,
}

// SecretKinds are the credential kinds whose values must never be echoed
// back by the API. The others (base URL, git identity) are plain settings.
var SecretKinds = map[string]bool{
	CredAnthropicAPIKey: true, CredClaudeOAuthToken: true, CredOpenAIAPIKey: true, CredGitSSHKey: true,
	CredGitHTTPSToken: true, CredGitHubToken: true, CredClaudeLogin: true, CredCodexLogin: true,
}

// ValidCredentialKind reports whether kind is known.
func ValidCredentialKind(kind string) bool {
	for _, k := range CredentialKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Credential records that a user has a value of a given kind stored in
// their Kubernetes Secret under SecretRef.
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
	// like the shared one plus write access in K8sNamespaces).
	K8sAccess      string
	K8sNamespaces  []string
	Resources      config.Resources
	NodeSelector   map[string]string
	Tolerations    []config.Toleration
	Env            map[string]string
	Autonomous     bool
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
	// tolerations, env, autonomous) and returns the updated row.
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

// Credentials is the user_credentials aggregate.
type Credentials interface {
	Upsert(ctx context.Context, c *Credential) error
	Delete(ctx context.Context, userID, kind string) error
	List(ctx context.Context, userID string) ([]*Credential, error)
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
