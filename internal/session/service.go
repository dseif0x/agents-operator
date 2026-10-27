// Package session holds the business logic: create, start, stop, delete,
// list, the runner status poller and the idle policy. It calls the store
// and the orchestrator; it never talks to Kubernetes for session objects.
package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/dseif0x/agents-operator/internal/agentdef"
	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/egress"
	"github.com/dseif0x/agents-operator/internal/github"
	"github.com/dseif0x/agents-operator/internal/reconcile"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
)

// ErrInvalidTransition is returned when an action does not apply to the
// session's current state.
var ErrInvalidTransition = errors.New("action not allowed in the session's current state")

// ValidationError describes a bad create request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Defaults are applied to create requests that leave fields empty.
type Defaults struct {
	PVCSize      string `json:"pvc_size"`
	StorageClass string `json:"storage_class"`
	ImageTag     string `json:"image_tag"`
	Autonomous   bool   `json:"autonomous"`
	// Resources is what a session gets when it asks for nothing;
	// MaxResources the most it may ask for. Both are shown on the form.
	Resources    config.Resources `json:"resources"`
	MaxResources config.Resources `json:"max_resources"`
	RuntimeClass string           `json:"runtime_class"`
	// ServiceAccount is the name of the read-only runner ServiceAccount a
	// session may opt into; empty when the chart does not create one.
	ServiceAccount string `json:"service_account"`
	// K8sNamespaceWrite is whether the "namespace" access mode (write
	// access in chosen namespaces) is enabled on this hub.
	K8sNamespaceWrite bool `json:"k8s_namespace_write"`
	// K8sSandbox is whether the "sandbox" access mode (a vcluster per
	// session) is enabled; SandboxDomain is where its Ingress hosts live
	// (<session name>.<domain>), empty without Ingress sync.
	K8sSandbox    bool   `json:"k8s_sandbox"`
	SandboxDomain string `json:"sandbox_domain"`
	// Sandbox mirrors the reconciler's naming of sandbox namespaces.
	Sandbox reconcile.Config `json:"-"`
	// Namespace is the hub's own, which no session may write to.
	Namespace string `json:"-"`
	// Egress is what sessions may open on the local network; EgressAllowed
	// shows its ranges on the form (empty: the field is hidden).
	Egress        egress.Policy `json:"-"`
	EgressAllowed []string      `json:"egress_allowed"`
}

// Service is the session business logic.
type Service struct {
	Store         store.Store
	Orch          reconcile.Orchestrator
	Term          *term.Proxy
	Broker        *Broker
	Creds         *Credentials
	GitHub        *github.Client
	Defaults      Defaults
	IdleStopAfter time.Duration
	Log           *slog.Logger
	// Leading reports whether this process is the elected leader. Every
	// replica polls runners so its API answers carry live status, but only
	// the leader acts on what it sees (login export, idle stop, output
	// timestamps). nil means always.
	Leading func() bool

	live sync.Map // session id -> *Live
	// loginSynced remembers, per running session, the credential-file
	// change the poller last acted on, so each rotation is exported once.
	loginSynced sync.Map // session id -> time.Time
	now         func() time.Time
}

// Live is the last status polled from a session's runner.
type Live struct {
	Status    runner.Status
	FetchedAt time.Time
	Err       string
}

// CreateRequest is the JSON body of POST /sessions.
type CreateRequest struct {
	Name  string `json:"name"`
	Agent string `json:"agent"`
	// Repos are cloned side by side under /workspace, where the agent starts.
	Repos []store.Repo `json:"repos"`
	// RepoURL and Branch are a shorthand for a single first repo.
	RepoURL      string              `json:"repo_url"`
	Branch       string              `json:"branch"`
	ImageTag     string              `json:"image_tag"`
	PVCSize      string              `json:"pvc_size"`
	StorageClass string              `json:"storage_class"`
	RuntimeClass string              `json:"runtime_class"`
	Resources    config.Resources    `json:"resources"`
	NodeSelector map[string]string   `json:"node_selector"`
	Tolerations  []config.Toleration `json:"tolerations"`
	Env          map[string]string   `json:"env"`
	Autonomous   *bool               `json:"autonomous"`
	// GitCredentials false keeps the owner's SSH key and HTTPS/GitHub
	// tokens from the agent (the runner still clones with them). Default
	// true; on an edit, absent keeps the current value.
	GitCredentials *bool `json:"git_credentials"`
	// Egress lists local network destinations the agent may reach beyond
	// the chart's NetworkPolicy: `192.168.1.10`, `192.168.1.0/24`,
	// `192.168.1.10:8080`. Each must lie in Defaults.Egress's allowed ranges.
	Egress []string `json:"egress"`
	// SystemPrompt is a custom system prompt for OpenClaude sessions,
	// appended to the CLI's own, or used instead of it with
	// SystemPromptReplace. Other agents refuse it.
	SystemPrompt        string `json:"system_prompt"`
	SystemPromptReplace bool   `json:"system_prompt_replace"`
	// AgentDefinition is an OpenClaude agent definition (Markdown with
	// front matter: name, description, optionally tools and model; the
	// body is the agent's prompt) the session starts with --agent. Its
	// prompt replaces OpenClaude's own, so SystemPrompt can only be
	// appended alongside it.
	AgentDefinition string `json:"agent_definition"`
	// K8sAccess is what kubectl in the pod may do: "off" (default), "readonly"
	// (the chart's read-only runner ServiceAccount) or "namespace" (read-only
	// plus write access in K8sNamespaces). Refused when the hub is not set
	// up for the mode.
	K8sAccess     string   `json:"k8s_access"`
	K8sNamespaces []string `json:"k8s_namespaces"`
	// AIAccount is the account a Claude Code or Codex session signs in
	// with ("" = the owner's default for the agent). Other agents refuse
	// it: OpenCode gets every provider account, the shell every account.
	// On an edit, absent keeps the current value.
	AIAccount *string `json:"ai_account"`
}

// MaxK8sNamespaces caps the namespaces a session may write to.
const MaxK8sNamespaces = 20

// protectedNamespaces never get write access from a session.
var protectedNamespaces = map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true}

// validateK8sAccess checks the access mode against what the hub offers and
// normalises the namespace list (trimmed, deduplicated, empty outside
// namespace mode).
func (s *Service) validateK8sAccess(req *CreateRequest) error {
	req.K8sAccess = cmp.Or(strings.TrimSpace(req.K8sAccess), store.K8sAccessOff)
	switch req.K8sAccess {
	case store.K8sAccessOff:
		req.K8sNamespaces = nil
		return nil
	case store.K8sAccessSandbox:
		if !s.Defaults.K8sSandbox {
			return &ValidationError{"sandbox clusters are not enabled (chart value runner.sandbox.enabled)"}
		}
		req.K8sNamespaces = nil
		return nil
	case store.K8sAccessReadOnly, store.K8sAccessNamespace:
		if s.Defaults.ServiceAccount == "" {
			return &ValidationError{"no runner service account is configured (chart value runner.serviceAccount.enabled)"}
		}
	default:
		return &ValidationError{"k8s_access must be one of " + strings.Join(store.K8sAccessModes, ", ")}
	}
	if req.K8sAccess == store.K8sAccessReadOnly {
		req.K8sNamespaces = nil
		return nil
	}
	if !s.Defaults.K8sNamespaceWrite {
		return &ValidationError{"write access per namespace is not enabled (chart value runner.serviceAccount.namespaceWrite)"}
	}
	var namespaces []string
	seen := map[string]bool{}
	for _, ns := range req.K8sNamespaces {
		ns = strings.TrimSpace(ns)
		if ns == "" || seen[ns] {
			continue
		}
		if !dnsLabelRE.MatchString(ns) {
			return &ValidationError{"invalid namespace " + ns}
		}
		if ns == s.Defaults.Namespace || protectedNamespaces[ns] {
			return &ValidationError{"namespace " + ns + " cannot be written to from a session"}
		}
		seen[ns] = true
		namespaces = append(namespaces, ns)
	}
	if len(namespaces) == 0 {
		return &ValidationError{"namespace access needs at least one namespace"}
	}
	if len(namespaces) > MaxK8sNamespaces {
		return &ValidationError{fmt.Sprintf("at most %d namespaces per session", MaxK8sNamespaces)}
	}
	req.K8sNamespaces = namespaces
	return nil
}

// checkSandboxHost keeps sandbox Ingress hosts apart: a sandbox's hosts
// live under <session name>.<domain>, so two sandbox sessions (of any
// owner) cannot share a name that maps to the same DNS label.
func (s *Service) checkSandboxHost(ctx context.Context, selfID string, req CreateRequest) error {
	if req.K8sAccess != store.K8sAccessSandbox || s.Defaults.SandboxDomain == "" {
		return nil
	}
	label := reconcile.Hostname(req.Name)
	all, err := s.Store.Sessions().ListAll(ctx)
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ID != selfID && other.K8sAccess == store.K8sAccessSandbox && reconcile.Hostname(other.Name) == label {
			return &ValidationError{fmt.Sprintf("another sandbox session already uses %s.%s; pick a different name", label, s.Defaults.SandboxDomain)}
		}
	}
	return nil
}

// Kubernetes name and label syntax, checked here so a bad value is a 400
// with a message instead of a session stuck in "creating" with an API
// server error.
var (
	dnsLabelRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	labelNameRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelValRE  = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
)

// extendedResourceRE matches Kubernetes extended resource names: a
// domain-qualified name (nvidia.com/gpu, example.com/fpga-1) or hugepages.
var extendedResourceRE = regexp.MustCompile(`^(hugepages-[A-Za-z0-9]+|([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z0-9]([-a-z0-9]*[a-z0-9])?/[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)$`)

var tolerationEffects = map[string]bool{"": true, "NoSchedule": true, "PreferNoSchedule": true, "NoExecute": true}

// validateScheduling checks the per-session runtime class, node selector
// and tolerations.
func validateScheduling(req CreateRequest) error {
	if req.RuntimeClass != "" && !dnsLabelRE.MatchString(req.RuntimeClass) {
		return &ValidationError{"runtime_class must be a lowercase DNS label such as gvisor"}
	}
	for k, v := range req.NodeSelector {
		if !labelNameRE.MatchString(k) || len(k) > 317 {
			return &ValidationError{"invalid node selector key " + k}
		}
		if !labelValRE.MatchString(v) {
			return &ValidationError{"invalid node selector value for " + k}
		}
	}
	for _, t := range req.Tolerations {
		switch t.Operator {
		case "", "Equal":
			if t.Key == "" {
				return &ValidationError{"a toleration without a key must use operator Exists"}
			}
		case "Exists":
			if t.Value != "" {
				return &ValidationError{"a toleration with operator Exists cannot have a value"}
			}
		default:
			return &ValidationError{"toleration operator must be Equal or Exists"}
		}
		if t.Key != "" && (!labelNameRE.MatchString(t.Key) || len(t.Key) > 317) {
			return &ValidationError{"invalid toleration key " + t.Key}
		}
		if !labelValRE.MatchString(t.Value) {
			return &ValidationError{"invalid toleration value " + t.Value}
		}
		if !tolerationEffects[t.Effect] {
			return &ValidationError{"toleration effect must be NoSchedule, PreferNoSchedule or NoExecute"}
		}
		if t.TolerationSeconds != nil && t.Effect != "NoExecute" {
			return &ValidationError{"tolerationSeconds only applies to the NoExecute effect"}
		}
	}
	return nil
}

var nameRE = regexp.MustCompile(`^[\pL\pN][\pL\pN ._-]{0,62}$`)
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv cannot be overridden per session.
var reservedEnv = map[string]bool{
	runner.EnvRunnerToken: true, runner.EnvAgent: true, runner.EnvAutonomous: true, runner.EnvAgentGitCredentials: true, runner.EnvRepos: true,
	runner.EnvWorkspace: true, runner.EnvListen: true, "HOME": true, "PATH": true,
	runner.EnvGitSSH: true, runner.EnvGitHTTPS: true, runner.EnvGitHubToken: true, runner.EnvAIAccounts: true,
	runner.EnvGitUserName: true, runner.EnvGitUserEmail: true, runner.EnvPrivateFile: true,
	runner.EnvK8sAccess: true, runner.EnvK8sNamespaces: true, runner.EnvEgress: true,
	runner.EnvSandboxNamespace: true, runner.EnvSandboxHost: true, runner.EnvSandboxSecurity: true,
	runner.EnvSystemPrompt: true, runner.EnvSystemPromptMode: true, runner.EnvAgentDefinition: true, runner.EnvAgentDefinitionName: true,
}

// reservedEnvPrefixes start variables the hub sets that cannot be
// overridden per session either: login seeds and account keys.
var reservedEnvPrefixes = []string{runner.EnvSeedPrefix, runner.EnvSeedAccountPrefix, runner.EnvAIKeyPrefix}

func reservedEnvKey(k string) bool {
	return reservedEnv[k] || slices.ContainsFunc(reservedEnvPrefixes, func(p string) bool { return strings.HasPrefix(k, p) })
}

// MaxRepos caps the repositories per session.
const MaxRepos = 10

var repoPathRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// reservedPaths cannot be used as a repo directory under /workspace.
var reservedPaths = map[string]bool{"home": true, ".": true, "..": true}

// normaliseRepos merges the shorthand fields into the list, fills default
// paths from the URL and validates everything.
func normaliseRepos(req CreateRequest) ([]store.Repo, error) {
	repos := []store.Repo{}
	if u := strings.TrimSpace(req.RepoURL); u != "" {
		repos = append(repos, store.Repo{URL: u, Branch: req.Branch})
	}
	repos = append(repos, req.Repos...)
	if len(repos) > MaxRepos {
		return nil, &ValidationError{fmt.Sprintf("at most %d repositories per session", MaxRepos)}
	}
	seen := map[string]bool{}
	for i := range repos {
		r := &repos[i]
		r.URL = strings.TrimSpace(r.URL)
		r.Branch = strings.TrimSpace(r.Branch)
		r.Path = strings.TrimSpace(r.Path)
		if !validRepoURL(r.URL) {
			return nil, &ValidationError{"repository URL must be an https://, ssh:// or git@host:path URL: " + r.URL}
		}
		if strings.ContainsAny(r.Branch, " \t\n") || strings.HasPrefix(r.Branch, "-") {
			return nil, &ValidationError{"invalid branch " + r.Branch}
		}
		if r.Path == "" {
			r.Path = RepoPathFromURL(r.URL)
		}
		if !repoPathRE.MatchString(r.Path) || reservedPaths[r.Path] {
			return nil, &ValidationError{"invalid repository path " + r.Path + " (a single directory name, not \"home\")"}
		}
		if seen[r.Path] {
			return nil, &ValidationError{"duplicate repository path " + r.Path}
		}
		seen[r.Path] = true
	}
	return repos, nil
}

// RepoPathFromURL derives a checkout directory name from a git URL:
// git@github.com:x/y.git -> y.
func RepoPathFromURL(u string) string {
	s := strings.TrimRight(u, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = unsafePathChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		s = "repo"
	}
	return s
}

func (s *Service) leading() bool { return s.Leading == nil || s.Leading() }

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// validate checks and normalises a request in place and returns the
// normalised repositories. It covers everything Create and Update share.
func (s *Service) validate(req *CreateRequest) ([]store.Repo, error) {
	req.Name = strings.TrimSpace(req.Name)
	if !nameRE.MatchString(req.Name) {
		return nil, &ValidationError{"name must be 1-63 characters: letters, digits, space, dot, underscore or dash"}
	}
	if !runner.ValidAgent(req.Agent) {
		return nil, &ValidationError{"agent must be one of " + strings.Join(runner.Agents, ", ")}
	}
	repos, err := normaliseRepos(*req)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{req.Resources.Requests.CPU, req.Resources.Requests.Memory, req.Resources.Limits.CPU, req.Resources.Limits.Memory} {
		if q != "" {
			if _, err := resource.ParseQuantity(q); err != nil {
				return nil, &ValidationError{"invalid resource quantity " + q}
			}
		}
	}
	if len(req.Resources.Requests.Extended) > 0 {
		return nil, &ValidationError{"extended resources go under limits; Kubernetes sets the request to the same amount"}
	}
	for name, v := range req.Resources.Limits.Extended {
		if !extendedResourceRE.MatchString(name) {
			return nil, &ValidationError{"invalid extended resource name " + name + " (expected vendor.com/name or hugepages-<size>)"}
		}
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			return nil, &ValidationError{"invalid quantity for " + name}
		}
		if !strings.HasPrefix(name, "hugepages-") && q.MilliValue()%1000 != 0 {
			return nil, &ValidationError{name + " must be a whole number"}
		}
	}
	for k := range req.Env {
		if !envKeyRE.MatchString(k) || reservedEnvKey(k) {
			return nil, &ValidationError{"invalid or reserved env var " + k}
		}
	}
	if strings.ContainsAny(req.ImageTag, "/: \t") {
		return nil, &ValidationError{"invalid image tag"}
	}
	if err := validateScheduling(*req); err != nil {
		return nil, err
	}
	if err := s.validateK8sAccess(req); err != nil {
		return nil, err
	}
	egress, err := s.Defaults.Egress.Check(req.Egress)
	if err != nil {
		return nil, &ValidationError{"egress: " + err.Error()}
	}
	req.Egress = egress
	if err := validateSystemPrompt(req); err != nil {
		return nil, err
	}
	return repos, nil
}

// MaxSystemPrompt caps a session's custom system prompt.
const MaxSystemPrompt = 32 << 10

// validateSystemPrompt checks the session's custom system prompt and agent
// definition: only OpenClaude takes them, they go on its command line and
// into a file, so they are plain text of bounded size.
func validateSystemPrompt(req *CreateRequest) error {
	clean := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	req.SystemPrompt, req.AgentDefinition = clean(req.SystemPrompt), clean(req.AgentDefinition)
	if req.SystemPrompt == "" {
		req.SystemPromptReplace = false
	}
	if req.SystemPrompt == "" && req.AgentDefinition == "" {
		return nil
	}
	if req.Agent != runner.AgentOpenCode {
		return &ValidationError{"a custom system prompt or agent definition is only supported for " + runner.AgentOpenCode + " sessions"}
	}
	for _, t := range []string{req.SystemPrompt, req.AgentDefinition} {
		if !utf8.ValidString(t) || strings.ContainsRune(t, 0) {
			return &ValidationError{"the system prompt and agent definition must be plain UTF-8 text"}
		}
	}
	if len(req.SystemPrompt) > MaxSystemPrompt {
		return &ValidationError{fmt.Sprintf("the system prompt is limited to %d KiB", MaxSystemPrompt>>10)}
	}
	if req.AgentDefinition != "" {
		if _, err := agentdef.Parse(req.AgentDefinition + "\n"); err != nil {
			return &ValidationError{err.Error()}
		}
		if req.SystemPromptReplace {
			return &ValidationError{"with an agent definition the agent's prompt replaces OpenClaude's own; the system prompt can only be appended"}
		}
	}
	return nil
}

// Create validates the request, inserts the row and asks for convergence.
func (s *Service) Create(ctx context.Context, owner *store.User, req CreateRequest) (*store.Session, error) {
	if req.PVCSize == "" {
		req.PVCSize = s.Defaults.PVCSize
	}
	if q, err := resource.ParseQuantity(req.PVCSize); err != nil || q.Sign() <= 0 {
		return nil, &ValidationError{"pvc_size must be a positive Kubernetes quantity such as 20Gi"}
	}
	if req.StorageClass == "" {
		req.StorageClass = s.Defaults.StorageClass
	}
	repos, err := s.validate(&req)
	if err != nil {
		return nil, err
	}
	if err := s.checkSandboxHost(ctx, "", req); err != nil {
		return nil, err
	}
	aiAccount := ""
	if req.AIAccount != nil {
		aiAccount = *req.AIAccount
	}
	if err := s.checkAIAccount(ctx, owner.ID, req.Agent, aiAccount); err != nil {
		return nil, err
	}
	autonomous := s.Defaults.Autonomous
	if req.Autonomous != nil {
		autonomous = *req.Autonomous
	}
	noGit := req.GitCredentials != nil && !*req.GitCredentials
	sess := &store.Session{
		OwnerID: owner.ID, Name: req.Name, Agent: req.Agent, Repos: repos,
		ImageTag: req.ImageTag, PVCSize: req.PVCSize, StorageClass: req.StorageClass, RuntimeClass: req.RuntimeClass, Resources: req.Resources,
		NodeSelector: req.NodeSelector, Tolerations: req.Tolerations, Env: req.Env, Autonomous: autonomous,
		NoGitCredentials: noGit, Egress: req.Egress, SystemPrompt: req.SystemPrompt, SystemPromptReplace: req.SystemPromptReplace, AgentDefinition: req.AgentDefinition, K8sAccess: req.K8sAccess, K8sNamespaces: req.K8sNamespaces,
		AIAccount: aiAccount, State: store.StateCreating,
	}
	if err := s.Store.Sessions().Create(ctx, sess); err != nil {
		return nil, err
	}
	s.event(ctx, sess.ID, "user", "created by "+owner.Username)
	s.recordRepoUsage(ctx, owner.ID, nil, repos)
	s.Orch.Notify(sess.ID)
	s.SessionChanged(ctx, sess)
	return sess, nil
}

// Update replaces a stopped or failed session's editable settings. The
// agent, PVC size and storage class stay: they shape the volume and the
// CLI state on it. Everything else only matters when the next pod is
// built, which is exactly what a stopped session is waiting for; repos
// added here are cloned on that start (existing directories are left
// alone), and the AGENTS.md is regenerated.
func (s *Service) Update(ctx context.Context, owner *store.User, id string, req CreateRequest) (*store.Session, error) {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	if sess.State != store.StateStopped && sess.State != store.StateFailed {
		return nil, ErrInvalidTransition
	}
	req.Agent, req.PVCSize, req.StorageClass = sess.Agent, sess.PVCSize, sess.StorageClass
	repos, err := s.validate(&req)
	if err != nil {
		return nil, err
	}
	if err := s.checkSandboxHost(ctx, sess.ID, req); err != nil {
		return nil, err
	}
	next := *sess
	next.Name, next.Repos, next.ImageTag, next.RuntimeClass = req.Name, repos, req.ImageTag, req.RuntimeClass
	next.K8sAccess, next.K8sNamespaces = req.K8sAccess, req.K8sNamespaces
	next.Egress = req.Egress
	next.SystemPrompt, next.SystemPromptReplace, next.AgentDefinition = req.SystemPrompt, req.SystemPromptReplace, req.AgentDefinition
	next.Resources, next.NodeSelector, next.Tolerations, next.Env = req.Resources, req.NodeSelector, req.Tolerations, req.Env
	if req.Autonomous != nil {
		next.Autonomous = *req.Autonomous
	}
	if req.GitCredentials != nil {
		next.NoGitCredentials = !*req.GitCredentials
	}
	if req.AIAccount != nil {
		if err := s.checkAIAccount(ctx, owner.ID, sess.Agent, *req.AIAccount); err != nil {
			return nil, err
		}
		next.AIAccount = *req.AIAccount
	}
	updated, err := s.Store.Sessions().Update(ctx, &next)
	if err != nil {
		return nil, err
	}
	s.event(ctx, id, "user", "settings changed by "+owner.Username)
	s.recordRepoUsage(ctx, owner.ID, sess.Repos, repos)
	s.SessionChanged(ctx, updated)
	return updated, nil
}

// checkAIAccount accepts "" or an account of the owner's that agent signs
// in with.
func (s *Service) checkAIAccount(ctx context.Context, ownerID, agent, id string) error {
	if id == "" {
		return nil
	}
	kinds, ok := reconcile.SignInKinds[agent]
	if !ok {
		return &ValidationError{"only Claude Code and Codex sessions pick an AI account; " + agent + " sessions get all of them"}
	}
	a, err := s.Store.Accounts().Get(ctx, ownerID, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !slices.Contains(kinds, a.Kind)) {
		return &ValidationError{"ai_account is not an account " + agent + " can sign in with"}
	}
	return err
}

// recordRepoUsage counts repositories that are new to the session.
func (s *Service) recordRepoUsage(ctx context.Context, ownerID string, before, after []store.Repo) {
	had := map[string]bool{}
	for _, r := range before {
		had[store.RepoKey(r.URL)] = true
	}
	for _, r := range after {
		if had[store.RepoKey(r.URL)] {
			continue
		}
		if err := s.Store.RepoUsage().Increment(ctx, ownerID, store.RepoKey(r.URL)); err != nil {
			s.Log.Debug("record repo usage failed", "err", err)
		}
	}
}

func validRepoURL(raw string) bool {
	if strings.HasPrefix(raw, "git@") && strings.Contains(raw, ":") && !strings.ContainsAny(raw, " \t\n") {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https", "ssh", "http":
		return true
	}
	return false
}

// Get returns the session if it belongs to ownerID, else ErrNotFound. Not
// leaking the existence of other users' sessions is deliberate.
func (s *Service) Get(ctx context.Context, ownerID, id string) (*store.Session, error) {
	sess, err := s.Store.Sessions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.OwnerID != ownerID {
		return nil, store.ErrNotFound
	}
	return sess, nil
}

// List returns the owner's sessions, newest first.
func (s *Service) List(ctx context.Context, ownerID string) ([]*store.Session, error) {
	return s.Store.Sessions().List(ctx, ownerID)
}

// Start recreates the pod for a stopped or failed session.
func (s *Service) Start(ctx context.Context, owner *store.User, id string) (*store.Session, error) {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	if sess.State != store.StateStopped && sess.State != store.StateFailed {
		return nil, ErrInvalidTransition
	}
	updated, err := s.Store.Sessions().Bump(ctx, id, store.StateCreating)
	if err != nil {
		return nil, err
	}
	s.event(ctx, id, "user", "start requested by "+owner.Username)
	s.Orch.Notify(id)
	s.SessionChanged(ctx, updated)
	return updated, nil
}

// Stop deletes the pod but keeps the PVC and the row.
func (s *Service) Stop(ctx context.Context, owner *store.User, id string) (*store.Session, error) {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	if sess.State != store.StateRunning && sess.State != store.StateCreating {
		return nil, ErrInvalidTransition
	}
	updated, err := s.Store.Sessions().SetState(ctx, id, store.StateStopping, "")
	if err != nil {
		return nil, err
	}
	s.event(ctx, id, "user", "stop requested by "+owner.Username)
	s.live.Delete(id)
	s.Orch.Notify(id)
	s.SessionChanged(ctx, updated)
	return updated, nil
}

// Delete removes everything. The row goes once the objects are gone.
func (s *Service) Delete(ctx context.Context, owner *store.User, id string) (*store.Session, error) {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return nil, err
	}
	if sess.State == store.StateDeleting {
		return sess, nil
	}
	updated, err := s.Store.Sessions().SetState(ctx, id, store.StateDeleting, "")
	if err != nil {
		return nil, err
	}
	s.event(ctx, id, "user", "delete requested by "+owner.Username)
	s.live.Delete(id)
	s.Orch.Notify(id)
	s.SessionChanged(ctx, updated)
	return updated, nil
}

// RestartAgent relaunches the agent CLI inside the running pod.
func (s *Service) RestartAgent(ctx context.Context, owner *store.User, id string) error {
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return err
	}
	if sess.State != store.StateRunning {
		return ErrInvalidTransition
	}
	if _, err := s.Term.Control(ctx, id, runner.Control{T: runner.MsgRestart}, ""); err != nil {
		return err
	}
	s.event(ctx, id, "user", "agent restart requested by "+owner.Username)
	return nil
}

// SaveLogin exports the CLI's credential files from the running session
// into the owner's accounts so later sessions start logged in: into the
// login account the session signed in with, else into a new one, which the
// session then uses.
func (s *Service) SaveLogin(ctx context.Context, owner *store.User, id, kind string) error {
	if _, ok := runner.LoginFiles[kind]; !ok {
		return &ValidationError{"unknown login kind"}
	}
	sess, err := s.Get(ctx, owner.ID, id)
	if err != nil {
		return err
	}
	if sess.State != store.StateRunning {
		return ErrInvalidTransition
	}
	target, err := s.Creds.loginAccount(ctx, sess, kind)
	if err != nil {
		return err
	}
	a, err := s.exportLogin(ctx, sess, kind, target)
	if err != nil {
		return err
	}
	if target == nil && runner.LoginKindForAgent(sess.Agent) == kind {
		next := *sess
		next.AIAccount = a.ID
		if _, err := s.Store.Sessions().Update(ctx, &next); err != nil {
			return err
		}
	}
	s.event(ctx, id, "user", kindLabel[kind]+" saved to account "+a.Name+" by "+owner.Username)
	return nil
}

// exportLogin asks the session's runner for the CLI's current login files
// and stores the bundle in account target, or a new login account when nil.
func (s *Service) exportLogin(ctx context.Context, sess *store.Session, kind string, target *store.Account) (*store.Account, error) {
	reply, err := s.Term.Control(ctx, sess.ID, runner.Control{T: runner.MsgExportLogin, Kind: kind}, runner.MsgLogin)
	if err != nil {
		return nil, err
	}
	data, err := decodeBase64(reply.Data)
	if err != nil || len(data) == 0 {
		return nil, errors.New("runner returned an empty login")
	}
	id := ""
	if target != nil {
		id = target.ID
	}
	return s.Creds.SaveLogin(ctx, sess.OwnerID, id, kind, data)
}

// syncLogin keeps a saved login current. Claude Code's OAuth refresh tokens
// are single use: once the CLI in one session has refreshed, the pair saved
// earlier is dead and a new session seeded from it lands on the login
// screen. So whenever a runner reports that its CLI rewrote the credential
// file, and the session signed in with a saved login account, that account
// is replaced with the fresh copy. Sessions on other accounts are left alone.
func (s *Service) syncLogin(ctx context.Context, sess *store.Session, st runner.Status) {
	if st.LoginUpdatedAt == nil {
		return
	}
	kind := runner.LoginKindForAgent(sess.Agent)
	if kind == "" {
		return
	}
	at := *st.LoginUpdatedAt
	if v, ok := s.loginSynced.Load(sess.ID); ok && !at.After(v.(time.Time)) {
		return
	}
	saved, err := s.Creds.loginAccount(ctx, sess, kind)
	if err != nil {
		s.Log.Warn("poller: list accounts failed", "session", sess.ID, "err", err)
		return
	}
	// One attempt per change, success or not: a runner that cannot export
	// (the CLI logged out, say) must not be asked again every ten seconds.
	s.loginSynced.Store(sess.ID, at)
	if saved == nil || !at.After(saved.UpdatedAt) {
		return
	}
	if _, err := s.exportLogin(ctx, sess, kind, saved); err != nil {
		s.Log.Warn("poller: refreshing saved login failed", "session", sess.ID, "kind", kind, "err", err)
		return
	}
	s.Log.Info("saved login refreshed from session", "session", sess.ID, "kind", kind, "account", saved.ID)
	s.event(ctx, sess.ID, "login", "account "+saved.Name+" refreshed from this session")
}

// Events lists the session's event log, newest first.
func (s *Service) Events(ctx context.Context, ownerID, id string) ([]*store.Event, error) {
	if _, err := s.Get(ctx, ownerID, id); err != nil {
		return nil, err
	}
	return s.Store.Events().List(ctx, id, store.EventsKeep)
}

// Logs returns the runner container's logs (useful for failed sessions).
func (s *Service) Logs(ctx context.Context, ownerID, id string) ([]byte, error) {
	if _, err := s.Get(ctx, ownerID, id); err != nil {
		return nil, err
	}
	return s.Orch.PodLogs(ctx, id, 500)
}

// Attached records that a user opened the terminal.
func (s *Service) Attached(ctx context.Context, id string) {
	_ = s.Store.Sessions().TouchAttached(ctx, id, s.clock())
}

// Live returns the last polled runner status.
func (s *Service) Live(id string) (*Live, bool) {
	v, ok := s.live.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Live), true
}

func (s *Service) event(ctx context.Context, id, kind, msg string) {
	if err := s.Store.Events().Add(ctx, id, kind, msg); err != nil {
		s.Log.Debug("record event failed", "err", err)
		return
	}
	_ = s.Store.Events().Prune(ctx, id, store.EventsKeep)
}

// SessionChanged implements reconcile.Notifier.
func (s *Service) SessionChanged(_ context.Context, sess *store.Session) {
	if s.Broker != nil {
		s.Broker.Publish(sess.OwnerID, Event{Type: "session", ID: sess.ID, Session: s.viewPtr(sess)})
	}
}

// SessionDeleted implements reconcile.Notifier.
func (s *Service) SessionDeleted(_ context.Context, id, ownerID string) {
	s.live.Delete(id)
	if s.Broker != nil {
		s.Broker.Publish(ownerID, Event{Type: "deleted", ID: id})
	}
}

// ---- status polling, idle policy, metrics ----

var sessionsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "agents_operator_sessions_total",
	Help: "Number of sessions by state.",
}, []string{"state"})

var pollErrors = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "agents_operator_runner_status_poll_errors_total",
	Help: "Failed runner status polls.",
})

// RegisterMetrics registers the session metrics on reg.
func RegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(sessionsGauge, pollErrors)
}

// RunPoller polls every running session's runner status on the given
// cadence, records last output, applies the idle policy and refreshes the
// metrics. It returns when ctx is done.
func (s *Service) RunPoller(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.PollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollOnce does one polling pass. Exported for tests.
func (s *Service) PollOnce(ctx context.Context) {
	all, err := s.Store.Sessions().ListAll(ctx)
	if err != nil {
		s.Log.Warn("poller: list sessions failed", "err", err)
		return
	}
	counts := map[string]int{}
	for _, st := range store.States {
		counts[st] = 0
	}
	now := s.clock()
	for _, sess := range all {
		counts[sess.State]++
		if sess.State != store.StateRunning {
			s.live.Delete(sess.ID)
			s.loginSynced.Delete(sess.ID)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := s.Term.Status(pctx, sess.ID)
		cancel()
		if err != nil {
			pollErrors.Inc()
			s.live.Store(sess.ID, &Live{FetchedAt: now, Err: err.Error()})
			continue
		}
		live := &Live{Status: st, FetchedAt: now}
		prev, _ := s.live.Load(sess.ID)
		s.live.Store(sess.ID, live)
		if changed(prev, live) {
			s.SessionChanged(ctx, refresh(sess, st))
		}
		if !s.leading() {
			continue
		}
		s.syncLogin(ctx, sess, st)
		if st.LastOutputAt != nil {
			_ = s.Store.Sessions().TouchOutput(ctx, sess.ID, *st.LastOutputAt)
		}
		if s.IdleStopAfter > 0 {
			last := sess.UpdatedAt
			if st.LastOutputAt != nil && st.LastOutputAt.After(last) {
				last = *st.LastOutputAt
			}
			if now.Sub(last) > s.IdleStopAfter && st.Clients == 0 {
				s.Log.Info("stopping idle session", "session", sess.ID, "idle", now.Sub(last))
				if updated, err := s.Store.Sessions().SetState(ctx, sess.ID, store.StateStopping, "idle for "+now.Sub(last).Truncate(time.Minute).String()); err == nil {
					s.event(ctx, sess.ID, "idle", "stopped after "+s.IdleStopAfter.String()+" without output")
					s.Orch.Notify(sess.ID)
					s.SessionChanged(ctx, updated)
				}
			}
		}
	}
	for st, n := range counts {
		sessionsGauge.WithLabelValues(st).Set(float64(n))
	}
}

func refresh(sess *store.Session, st runner.Status) *store.Session {
	c := *sess
	if st.LastOutputAt != nil {
		t := *st.LastOutputAt
		c.LastOutputAt = &t
	}
	return &c
}

// changed reports whether the parts of the status the list page shows moved.
func changed(prev any, cur *Live) bool {
	p, ok := prev.(*Live)
	if !ok || p == nil {
		return true
	}
	a, b := p.Status, cur.Status
	return p.Err != cur.Err || a.Running != b.Running || a.NeedsAttention != b.NeedsAttention ||
		(a.ExitCode == nil) != (b.ExitCode == nil) || a.Tail != b.Tail || activityChanged(a.Activity, b.Activity)
}

func activityChanged(a, b *runner.Activity) bool {
	if (a == nil) != (b == nil) {
		return true
	}
	return a != nil && (a.State != b.State || a.Detail != b.Detail || a.Message != b.Message)
}

// ---- API view ----

// View is the JSON shape of a session in the API.
type View struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Agent          string              `json:"agent"`
	Repos          []store.Repo        `json:"repos"`
	ImageTag       string              `json:"image_tag"`
	PVCSize        string              `json:"pvc_size"`
	StorageClass   string              `json:"storage_class"`
	RuntimeClass   string              `json:"runtime_class"`
	Resources      config.Resources    `json:"resources"`
	NodeSelector   map[string]string   `json:"node_selector"`
	Tolerations    []config.Toleration `json:"tolerations"`
	Env            map[string]string   `json:"env"`
	Autonomous     bool                `json:"autonomous"`
	GitCredentials bool                `json:"git_credentials"`
	Egress         []string            `json:"egress"`
	SystemPrompt   string              `json:"system_prompt"`
	// SystemPromptReplace: the prompt replaces OpenClaude's own.
	SystemPromptReplace bool     `json:"system_prompt_replace"`
	AgentDefinition     string   `json:"agent_definition"`
	K8sAccess           string   `json:"k8s_access"`
	K8sNamespaces       []string `json:"k8s_namespaces"`
	AIAccount           string   `json:"ai_account"`
	// SandboxNamespace and SandboxHost describe the session's sandbox
	// cluster (sandbox mode only): its host namespace and the DNS name its
	// Ingress hosts go under.
	SandboxNamespace string     `json:"sandbox_namespace,omitempty"`
	SandboxHost      string     `json:"sandbox_host,omitempty"`
	State            string     `json:"state"`
	StateReason      string     `json:"state_reason"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	LastAttachedAt   *time.Time `json:"last_attached_at"`
	LastOutputAt     *time.Time `json:"last_output_at"`
	// Live fields come from the runner and are absent when it is unreachable.
	AgentRunning   *bool  `json:"agent_running,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	NeedsAttention bool   `json:"needs_attention"`
	Tail           string `json:"tail,omitempty"`
	// Activity is the agent's own account of what it is doing (from its
	// hooks), when the runner has one.
	Activity    *runner.Activity `json:"activity,omitempty"`
	Clients     int              `json:"clients"`
	RunnerError string           `json:"runner_error,omitempty"`
	PodName     string           `json:"pod_name"`
}

// View converts a row plus live status into the API shape.
func (s *Service) View(sess *store.Session) View {
	v := View{
		ID: sess.ID, Name: sess.Name, Agent: sess.Agent, Repos: sess.Repos,
		ImageTag: sess.ImageTag, PVCSize: sess.PVCSize, StorageClass: sess.StorageClass, RuntimeClass: sess.RuntimeClass, Resources: sess.Resources,
		NodeSelector: sess.NodeSelector, Tolerations: sess.Tolerations, Env: sess.Env, Autonomous: sess.Autonomous,
		GitCredentials: !sess.NoGitCredentials, Egress: sess.Egress, SystemPrompt: sess.SystemPrompt, SystemPromptReplace: sess.SystemPromptReplace, AgentDefinition: sess.AgentDefinition, K8sAccess: cmp.Or(sess.K8sAccess, store.K8sAccessOff), K8sNamespaces: sess.K8sNamespaces,
		AIAccount: sess.AIAccount, State: sess.State, StateReason: sess.StateReason, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt,
		LastAttachedAt: sess.LastAttachedAt, LastOutputAt: sess.LastOutputAt, PodName: reconcile.ObjectName(sess.ID),
	}
	if v.K8sAccess == store.K8sAccessSandbox && s.Defaults.K8sSandbox {
		v.SandboxNamespace = reconcile.SandboxNamespace(sess.ID, s.Defaults.Sandbox)
		v.SandboxHost = reconcile.SandboxHost(sess, s.Defaults.Sandbox)
	}
	if v.Repos == nil {
		v.Repos = []store.Repo{}
	}
	if v.K8sNamespaces == nil {
		v.K8sNamespaces = []string{}
	}
	if v.Egress == nil {
		v.Egress = []string{}
	}
	if v.NodeSelector == nil {
		v.NodeSelector = map[string]string{}
	}
	if v.Tolerations == nil {
		v.Tolerations = []config.Toleration{}
	}
	if v.Env == nil {
		v.Env = map[string]string{}
	}
	if live, ok := s.Live(sess.ID); ok {
		if live.Err != "" {
			v.RunnerError = live.Err
		} else {
			running := live.Status.Running
			v.AgentRunning = &running
			v.ExitCode = live.Status.ExitCode
			v.NeedsAttention = live.Status.NeedsAttention
			v.Tail = live.Status.Tail
			v.Activity = live.Status.Activity
			v.Clients = live.Status.Clients
			if live.Status.LastOutputAt != nil && (v.LastOutputAt == nil || live.Status.LastOutputAt.After(*v.LastOutputAt)) {
				v.LastOutputAt = live.Status.LastOutputAt
			}
		}
	}
	return v
}

func (s *Service) viewPtr(sess *store.Session) *View {
	v := s.View(sess)
	return &v
}

// String helps logging.
func (v View) String() string { return fmt.Sprintf("%s (%s, %s)", v.Name, v.ID, v.State) }
