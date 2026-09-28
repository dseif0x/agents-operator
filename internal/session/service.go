// Package session holds the business logic: create, start, stop, delete,
// list, the runner status poller and the idle policy. It calls the store
// and the orchestrator; it never talks to Kubernetes for session objects.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/dseif0x/agents-operator/internal/config"
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
	PVCSize      string
	StorageClass string
	ImageTag     string
	Autonomous   bool
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

	live sync.Map // session id -> *Live
	now  func() time.Time
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
	Resources    config.Resources    `json:"resources"`
	NodeSelector map[string]string   `json:"node_selector"`
	Tolerations  []config.Toleration `json:"tolerations"`
	Env          map[string]string   `json:"env"`
	Autonomous   *bool               `json:"autonomous"`
}

var nameRE = regexp.MustCompile(`^[\pL\pN][\pL\pN ._-]{0,62}$`)
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv cannot be overridden per session.
var reservedEnv = map[string]bool{
	runner.EnvRunnerToken: true, runner.EnvAgent: true, runner.EnvAutonomous: true, runner.EnvRepos: true,
	runner.EnvWorkspace: true, runner.EnvListen: true, "HOME": true, "PATH": true,
	runner.EnvGitSSHKey: true, runner.EnvGitHTTPSToken: true, runner.EnvGitHubToken: true,
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

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Create validates the request, inserts the row and asks for convergence.
func (s *Service) Create(ctx context.Context, owner *store.User, req CreateRequest) (*store.Session, error) {
	req.Name = strings.TrimSpace(req.Name)
	if !nameRE.MatchString(req.Name) {
		return nil, &ValidationError{"name must be 1-63 characters: letters, digits, space, dot, underscore or dash"}
	}
	if !runner.ValidAgent(req.Agent) {
		return nil, &ValidationError{"agent must be one of " + strings.Join(runner.Agents, ", ")}
	}
	repos, err := normaliseRepos(req)
	if err != nil {
		return nil, err
	}
	if req.PVCSize == "" {
		req.PVCSize = s.Defaults.PVCSize
	}
	if q, err := resource.ParseQuantity(req.PVCSize); err != nil || q.Sign() <= 0 {
		return nil, &ValidationError{"pvc_size must be a positive Kubernetes quantity such as 20Gi"}
	}
	if req.StorageClass == "" {
		req.StorageClass = s.Defaults.StorageClass
	}
	for _, q := range []string{req.Resources.Requests.CPU, req.Resources.Requests.Memory, req.Resources.Limits.CPU, req.Resources.Limits.Memory} {
		if q != "" {
			if _, err := resource.ParseQuantity(q); err != nil {
				return nil, &ValidationError{"invalid resource quantity " + q}
			}
		}
	}
	for k := range req.Env {
		if !envKeyRE.MatchString(k) || reservedEnv[k] {
			return nil, &ValidationError{"invalid or reserved env var " + k}
		}
	}
	if strings.ContainsAny(req.ImageTag, "/: \t") {
		return nil, &ValidationError{"invalid image tag"}
	}
	autonomous := s.Defaults.Autonomous
	if req.Autonomous != nil {
		autonomous = *req.Autonomous
	}
	sess := &store.Session{
		OwnerID: owner.ID, Name: req.Name, Agent: req.Agent, Repos: repos,
		ImageTag: req.ImageTag, PVCSize: req.PVCSize, StorageClass: req.StorageClass, Resources: req.Resources,
		NodeSelector: req.NodeSelector, Tolerations: req.Tolerations, Env: req.Env, Autonomous: autonomous,
		State: store.StateCreating,
	}
	if err := s.Store.Sessions().Create(ctx, sess); err != nil {
		return nil, err
	}
	s.event(ctx, sess.ID, "user", "created by "+owner.Username)
	for _, r := range repos {
		if err := s.Store.RepoUsage().Increment(ctx, owner.ID, store.RepoKey(r.URL)); err != nil {
			s.Log.Debug("record repo usage failed", "err", err)
		}
	}
	s.Orch.Notify(sess.ID)
	s.SessionChanged(ctx, sess)
	return sess, nil
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

// SaveLogin exports the CLI's credential file from the running session and
// stores it in the user's Secret so later sessions start logged in.
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
	reply, err := s.Term.Control(ctx, id, runner.Control{T: runner.MsgExportLogin, Kind: kind}, runner.MsgLogin)
	if err != nil {
		return err
	}
	data, err := decodeBase64(reply.Data)
	if err != nil || len(data) == 0 {
		return errors.New("runner returned an empty login")
	}
	if err := s.Creds.Set(ctx, owner.ID, kind, data); err != nil {
		return err
	}
	s.event(ctx, id, "user", kind+" saved to account by "+owner.Username)
	return nil
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
		if st.LastOutputAt != nil {
			_ = s.Store.Sessions().TouchOutput(ctx, sess.ID, *st.LastOutputAt)
		}
		if changed(prev, live) {
			s.SessionChanged(ctx, refresh(sess, st))
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
		(a.ExitCode == nil) != (b.ExitCode == nil) || a.Tail != b.Tail
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
	Resources      config.Resources    `json:"resources"`
	NodeSelector   map[string]string   `json:"node_selector"`
	Tolerations    []config.Toleration `json:"tolerations"`
	Env            map[string]string   `json:"env"`
	Autonomous     bool                `json:"autonomous"`
	State          string              `json:"state"`
	StateReason    string              `json:"state_reason"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	LastAttachedAt *time.Time          `json:"last_attached_at"`
	LastOutputAt   *time.Time          `json:"last_output_at"`
	// Live fields come from the runner and are absent when it is unreachable.
	AgentRunning   *bool  `json:"agent_running,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	NeedsAttention bool   `json:"needs_attention"`
	Tail           string `json:"tail,omitempty"`
	Clients        int    `json:"clients"`
	RunnerError    string `json:"runner_error,omitempty"`
	PodName        string `json:"pod_name"`
}

// View converts a row plus live status into the API shape.
func (s *Service) View(sess *store.Session) View {
	v := View{
		ID: sess.ID, Name: sess.Name, Agent: sess.Agent, Repos: sess.Repos,
		ImageTag: sess.ImageTag, PVCSize: sess.PVCSize, StorageClass: sess.StorageClass, Resources: sess.Resources,
		NodeSelector: sess.NodeSelector, Tolerations: sess.Tolerations, Env: sess.Env, Autonomous: sess.Autonomous,
		State: sess.State, StateReason: sess.StateReason, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt,
		LastAttachedAt: sess.LastAttachedAt, LastOutputAt: sess.LastOutputAt, PodName: reconcile.ObjectName(sess.ID),
	}
	if v.Repos == nil {
		v.Repos = []store.Repo{}
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
