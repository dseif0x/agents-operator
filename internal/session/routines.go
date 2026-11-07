package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/routine"
	"github.com/dseif0x/agents-operator/internal/runner/convo"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Routines (docs/design/routines.md): an instruction an agent or a chat
// gets on a schedule or when a webhook arrives. A run of an agent routine
// is a queued input of its conversation (source "routine"); a run of a chat
// routine is a message in the chat. The leader fires due routines from its
// poll loop and starts stopped agents that have a run waiting.

// Limits.
const (
	MaxRoutineName        = 80
	MaxRoutineInstruction = 16 << 10
	MaxRoutinesPerOwner   = 200
	MaxWebhookBody        = 1 << 20
	maxPayloadInPrompt    = 8 << 10
)

// RoutineInput creates or changes a routine; on a change, absent fields
// keep their value.
type RoutineInput struct {
	SessionID   string            `json:"session_id"`
	ChatID      string            `json:"chat_id"`
	Name        *string           `json:"name"`
	Instruction *string           `json:"instruction"`
	Trigger     *string           `json:"trigger"`
	Schedule    *routine.Schedule `json:"schedule"`
	Enabled     *bool             `json:"enabled"`
	EventType   *string           `json:"event_type"`
	Filters     *[]WebhookFilter  `json:"filters"`
}

// WebhookFilter is a field of the webhook's JSON body that must have a
// value: path "pull_request.base.ref", value "main".
type WebhookFilter struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

// RoutineView is a routine in the API.
type RoutineView struct {
	ID          string            `json:"id"`
	SessionID   string            `json:"session_id,omitempty"`
	ChatID      string            `json:"chat_id,omitempty"`
	Name        string            `json:"name"`
	Instruction string            `json:"instruction"`
	Trigger     string            `json:"trigger"`
	Schedule    *routine.Schedule `json:"schedule,omitempty"`
	Describe    string            `json:"describe"`
	Enabled     bool              `json:"enabled"`
	EventType   string            `json:"event_type,omitempty"`
	Filters     []WebhookFilter   `json:"filters,omitempty"`
	// WebhookPath is where the webhook is posted (relative to the hub).
	WebhookPath string `json:"webhook_path,omitempty"`
	// Secret is only set right after it was made.
	Secret    string     `json:"secret,omitempty"`
	HasSecret bool       `json:"has_secret,omitempty"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	LastRun   *RunView   `json:"last_run,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// RunView is a routine run in the API.
type RunView struct {
	ID           int64      `json:"id"`
	RoutineID    string     `json:"routine_id"`
	Source       string     `json:"source"`
	Status       string     `json:"status"`
	Quiet        bool       `json:"quiet,omitempty"`
	Error        string     `json:"error,omitempty"`
	InputID      string     `json:"input_id,omitempty"`
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
}

func runView(r *store.RoutineRun) *RunView {
	return &RunView{ID: r.ID, RoutineID: r.RoutineID, Source: r.Source, Status: r.Status, Quiet: r.Quiet, Error: r.Error, InputID: r.InputID, ScheduledFor: r.ScheduledFor, StartedAt: r.StartedAt, EndedAt: r.EndedAt}
}

func (s *Service) routineView(ctx context.Context, r *store.Routine) RoutineView {
	v := RoutineView{ID: r.ID, SessionID: r.SessionID, ChatID: r.ChatID, Name: r.Name, Instruction: r.Instruction, Trigger: r.Trigger, Enabled: r.Enabled,
		EventType: r.EventType, NextRunAt: r.NextRunAt, LastRunAt: r.LastRunAt, CreatedAt: r.CreatedAt, HasSecret: r.WebhookSecret != ""}
	if r.Trigger == store.TriggerSchedule {
		var sc routine.Schedule
		if json.Unmarshal(r.Schedule, &sc) == nil {
			v.Schedule = &sc
			v.Describe = sc.Describe()
		}
	} else {
		v.WebhookPath = "/api/v1/hooks/" + r.ID
		v.Describe = "When a webhook arrives"
		if r.EventType != "" {
			v.Describe = "When " + r.EventType + " arrives"
		}
		_ = json.Unmarshal(r.Filters, &v.Filters)
	}
	if runs, err := s.Store.Routines().Runs(ctx, r.ID, 1); err == nil && len(runs) == 1 {
		v.LastRun = runView(runs[0])
	}
	return v
}

func (s *Service) ownRoutine(ctx context.Context, owner *store.User, id string) (*store.Routine, error) {
	r, err := s.Store.Routines().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.OwnerID != owner.ID {
		return nil, store.ErrNotFound
	}
	return r, nil
}

// ListRoutines returns the owner's routines; sessionID or chatID narrow it.
func (s *Service) ListRoutines(ctx context.Context, owner *store.User, sessionID, chatID string) ([]RoutineView, error) {
	rs, err := s.Store.Routines().List(ctx, owner.ID)
	if err != nil {
		return nil, err
	}
	out := []RoutineView{}
	for _, r := range rs {
		if (sessionID != "" && r.SessionID != sessionID) || (chatID != "" && r.ChatID != chatID) {
			continue
		}
		out = append(out, s.routineView(ctx, r))
	}
	return out, nil
}

func newSecret() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "whsec_" + hex.EncodeToString(b[:])
}

// applyRoutine validates in onto r.
func (s *Service) applyRoutine(ctx context.Context, owner *store.User, r *store.Routine, in RoutineInput) error {
	if in.Name != nil {
		r.Name = strings.TrimSpace(*in.Name)
	}
	if in.Instruction != nil {
		r.Instruction = strings.TrimSpace(*in.Instruction)
	}
	if in.Trigger != nil {
		r.Trigger = *in.Trigger
	}
	if in.Enabled != nil {
		r.Enabled = *in.Enabled
	}
	if in.EventType != nil {
		r.EventType = strings.TrimSpace(*in.EventType)
	}
	if r.Name == "" || len(r.Name) > MaxRoutineName {
		return &ValidationError{fmt.Sprintf("a routine needs a name of up to %d characters", MaxRoutineName)}
	}
	if r.Instruction == "" || len(r.Instruction) > MaxRoutineInstruction {
		return &ValidationError{fmt.Sprintf("a routine needs an instruction of up to %d KiB", MaxRoutineInstruction>>10)}
	}
	switch r.Trigger {
	case store.TriggerSchedule:
		if in.Schedule != nil {
			if err := in.Schedule.Validate(); err != nil {
				return &ValidationError{err.Error()}
			}
			b, _ := json.Marshal(in.Schedule)
			r.Schedule = b
		}
		var sc routine.Schedule
		if json.Unmarshal(r.Schedule, &sc) != nil || sc.Validate() != nil {
			return &ValidationError{"a scheduled routine needs a schedule"}
		}
		r.NextRunAt = nil
		if r.Enabled {
			if next := sc.Next(s.clock()); !next.IsZero() {
				r.NextRunAt = &next
			}
		}
	case store.TriggerWebhook:
		r.NextRunAt = nil
		if r.WebhookSecret == "" {
			r.WebhookSecret = newSecret()
		}
		if in.Filters != nil {
			var clean []WebhookFilter
			for _, f := range *in.Filters {
				f.Path = strings.TrimSpace(f.Path)
				if f.Path == "" {
					continue
				}
				clean = append(clean, f)
			}
			if len(clean) > 20 {
				return &ValidationError{"at most 20 filters"}
			}
			b, _ := json.Marshal(clean)
			r.Filters = b
		}
	default:
		return &ValidationError{`trigger is "schedule" or "webhook"`}
	}
	return nil
}

// CreateRoutine adds a routine to an agent (a conversation session) or a
// chat.
func (s *Service) CreateRoutine(ctx context.Context, owner *store.User, in RoutineInput) (RoutineView, error) {
	r := &store.Routine{OwnerID: owner.ID, Enabled: true, Trigger: store.TriggerSchedule}
	switch {
	case in.SessionID != "" && in.ChatID == "":
		sess, err := s.Get(ctx, owner.ID, in.SessionID)
		if err != nil {
			return RoutineView{}, err
		}
		if sess.Interface != store.InterfaceConversation {
			return RoutineView{}, &ValidationError{"routines need an agent that runs as a conversation"}
		}
		r.SessionID = sess.ID
	case in.ChatID != "" && in.SessionID == "":
		c, err := s.ownChat(ctx, owner, in.ChatID)
		if err != nil {
			return RoutineView{}, err
		}
		r.ChatID = c.ID
	default:
		return RoutineView{}, &ValidationError{"a routine belongs to one agent (session_id) or one chat (chat_id)"}
	}
	if all, err := s.Store.Routines().List(ctx, owner.ID); err == nil && len(all) >= MaxRoutinesPerOwner {
		return RoutineView{}, &ValidationError{fmt.Sprintf("at most %d routines", MaxRoutinesPerOwner)}
	}
	if err := s.applyRoutine(ctx, owner, r, in); err != nil {
		return RoutineView{}, err
	}
	if err := s.Store.Routines().Create(ctx, r); err != nil {
		return RoutineView{}, err
	}
	v := s.routineView(ctx, r)
	if r.Trigger == store.TriggerWebhook {
		v.Secret = r.WebhookSecret
	}
	return v, nil
}

// UpdateRoutine changes a routine.
func (s *Service) UpdateRoutine(ctx context.Context, owner *store.User, id string, in RoutineInput) (RoutineView, error) {
	r, err := s.ownRoutine(ctx, owner, id)
	if err != nil {
		return RoutineView{}, err
	}
	hadSecret := r.WebhookSecret != ""
	if err := s.applyRoutine(ctx, owner, r, in); err != nil {
		return RoutineView{}, err
	}
	if err := s.Store.Routines().Update(ctx, r); err != nil {
		return RoutineView{}, err
	}
	v := s.routineView(ctx, r)
	if !hadSecret && r.WebhookSecret != "" {
		v.Secret = r.WebhookSecret
	}
	return v, nil
}

// RotateSecret makes a new webhook secret and returns it once.
func (s *Service) RotateSecret(ctx context.Context, owner *store.User, id string) (RoutineView, error) {
	r, err := s.ownRoutine(ctx, owner, id)
	if err != nil {
		return RoutineView{}, err
	}
	if r.Trigger != store.TriggerWebhook {
		return RoutineView{}, &ValidationError{"only webhook routines have a secret"}
	}
	r.WebhookSecret = newSecret()
	if err := s.Store.Routines().Update(ctx, r); err != nil {
		return RoutineView{}, err
	}
	v := s.routineView(ctx, r)
	v.Secret = r.WebhookSecret
	return v, nil
}

// DeleteRoutine removes a routine and its runs.
func (s *Service) DeleteRoutine(ctx context.Context, owner *store.User, id string) error {
	if _, err := s.ownRoutine(ctx, owner, id); err != nil {
		return err
	}
	return s.Store.Routines().Delete(ctx, id)
}

// RoutineRuns returns a routine's newest runs.
func (s *Service) RoutineRuns(ctx context.Context, owner *store.User, id string, limit int) ([]*RunView, error) {
	if _, err := s.ownRoutine(ctx, owner, id); err != nil {
		return nil, err
	}
	runs, err := s.Store.Routines().Runs(ctx, id, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*RunView, 0, len(runs))
	for _, r := range runs {
		out = append(out, runView(r))
	}
	return out, nil
}

// RunRoutineNow runs a routine once (a test run).
func (s *Service) RunRoutineNow(ctx context.Context, owner *store.User, id string) (*RunView, error) {
	r, err := s.ownRoutine(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	run, err := s.fireRoutine(ctx, r, store.RunTest, nil, "", "")
	if err != nil {
		return nil, err
	}
	return runView(run), nil
}

// CalendarEntry is a past run or an upcoming one.
type CalendarEntry struct {
	RoutineID string    `json:"routine_id"`
	At        time.Time `json:"at"`
	Run       *RunView  `json:"run,omitempty"` // nil: upcoming
}

// Calendar lists the runs of [from, to): past ones as they happened and
// upcoming ones from the schedules.
func (s *Service) Calendar(ctx context.Context, owner *store.User, from, to time.Time) ([]CalendarEntry, error) {
	if to.Sub(from) > 62*24*time.Hour {
		to = from.Add(62 * 24 * time.Hour)
	}
	out := []CalendarEntry{}
	runs, err := s.Store.Routines().RunsSince(ctx, owner.ID, from)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.StartedAt.Before(to) {
			at := r.StartedAt
			if r.ScheduledFor != nil {
				at = *r.ScheduledFor
			}
			out = append(out, CalendarEntry{RoutineID: r.RoutineID, At: at, Run: runView(r)})
		}
	}
	rs, err := s.Store.Routines().List(ctx, owner.ID)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	start := from
	if now.After(start) {
		start = now
	}
	for _, r := range rs {
		if !r.Enabled || r.Trigger != store.TriggerSchedule {
			continue
		}
		var sc routine.Schedule
		if json.Unmarshal(r.Schedule, &sc) != nil {
			continue
		}
		for _, at := range sc.Runs(start, to, 200) {
			out = append(out, CalendarEntry{RoutineID: r.ID, At: at})
		}
	}
	return out, nil
}

// ---- firing ----

// RunDueRoutines fires the scheduled routines that are due. The leader
// calls it from its poll loop.
func (s *Service) RunDueRoutines(ctx context.Context) {
	now := s.clock()
	due, err := s.Store.Routines().Due(ctx, now)
	if err != nil {
		s.Log.Warn("cannot list due routines", "err", err)
		return
	}
	for _, r := range due {
		var sc routine.Schedule
		_ = json.Unmarshal(r.Schedule, &sc)
		scheduled := *r.NextRunAt
		// The next run comes from now, so a hub that was down runs a missed
		// routine once, not once for every occurrence it missed.
		next := sc.Next(now)
		var nextPtr *time.Time
		if !next.IsZero() {
			nextPtr = &next
		}
		if err := s.Store.Routines().SetRun(ctx, r.ID, nextPtr, now); err != nil {
			s.Log.Warn("cannot advance a routine", "routine", r.ID, "err", err)
			continue
		}
		if now.Sub(scheduled) > 6*time.Hour {
			// Too late to be useful (the hub was down): record it as missed.
			run := &store.RoutineRun{RoutineID: r.ID, Source: store.RunSchedule, Status: store.RunFailed, Error: "missed: the hub was not running at " + scheduled.Format(time.RFC3339), ScheduledFor: &scheduled, StartedAt: now}
			end := now
			run.EndedAt = &end
			_ = s.Store.Routines().AddRun(ctx, run)
			continue
		}
		if _, err := s.fireRoutine(ctx, r, store.RunSchedule, &scheduled, "", ""); err != nil {
			s.Log.Warn("routine run failed to start", "routine", r.ID, "err", err)
		}
	}
}

// fireRoutine starts one run: an input for an agent, a message for a chat.
func (s *Service) fireRoutine(ctx context.Context, r *store.Routine, source string, scheduled *time.Time, eventType, payload string) (*store.RoutineRun, error) {
	run := &store.RoutineRun{RoutineID: r.ID, Source: source, Status: store.RunQueued, ScheduledFor: scheduled, Payload: payload, StartedAt: s.clock()}
	if err := s.Store.Routines().AddRun(ctx, run); err != nil {
		return nil, err
	}
	fail := func(err error) (*store.RoutineRun, error) {
		run.Status, run.Error = store.RunFailed, err.Error()
		now := s.clock()
		run.EndedAt = &now
		_ = s.Store.Routines().UpdateRun(ctx, run)
		return run, err
	}
	owner, err := s.Store.Users().GetByID(ctx, r.OwnerID)
	if err != nil {
		return fail(err)
	}
	text := routinePrompt(r, source, eventType, payload)
	if r.SessionID != "" {
		sess, err := s.Store.Sessions().Get(ctx, r.SessionID)
		if err != nil {
			return fail(errors.New("the agent no longer exists"))
		}
		if sess.Interface != store.InterfaceConversation {
			return fail(errors.New("the agent runs in a terminal, not as a conversation"))
		}
		extra := map[string]any{"routine": map[string]any{"id": r.ID, "name": r.Name, "run": run.ID, "source": source, "instruction": r.Instruction, "event": eventType}}
		in, err := s.queueInput(ctx, owner.ID, sess, text, convo.SourceRoutine, nil, extra)
		if err != nil {
			return fail(err)
		}
		run.InputID = in.ID
		if err := s.Store.Routines().UpdateRun(ctx, run); err != nil {
			return run, err
		}
		s.wake(ctx, owner, sess)
		return run, nil
	}
	c, err := s.Store.Chats().Get(ctx, r.ChatID)
	if err != nil {
		return fail(errors.New("the chat no longer exists"))
	}
	m, err := s.post(ctx, owner, c, nil, text, 0, nil)
	if err != nil {
		return fail(err)
	}
	run.ChatMessage = &m.ID
	run.Status = store.RunSent
	now := s.clock()
	run.EndedAt = &now
	if err := s.Store.Routines().UpdateRun(ctx, run); err != nil {
		return run, err
	}
	// Stopped members get the message when they run: start them.
	if members, err := s.Store.Chats().Members(ctx, c.ID); err == nil {
		for _, mb := range members {
			if sess, err := s.Store.Sessions().Get(ctx, mb.SessionID); err == nil && !mb.Muted {
				s.wake(ctx, owner, sess)
			}
		}
	}
	return run, nil
}

// wake starts a stopped session so it can take its routine run.
func (s *Service) wake(ctx context.Context, owner *store.User, sess *store.Session) {
	if sess.State != store.StateStopped {
		return
	}
	if _, err := s.Start(ctx, owner, sess.ID); err == nil {
		s.event(ctx, sess.ID, "routine", "started for a routine run")
	}
}

// routinePrompt is what the agent gets for a run.
func routinePrompt(r *store.Routine, source, eventType, payload string) string {
	var b strings.Builder
	kind := map[string]string{store.RunSchedule: "scheduled run", store.RunWebhook: "webhook", store.RunTest: "test run"}[source]
	fmt.Fprintf(&b, "[agents-operator routine] %q (%s)\n\n%s\n", r.Name, kind, r.Instruction)
	if source == store.RunWebhook {
		b.WriteString("\nThe webhook")
		if eventType != "" {
			fmt.Fprintf(&b, " (event %s)", eventType)
		}
		b.WriteString(" sent:\n```\n")
		if len(payload) > maxPayloadInPrompt {
			payload = payload[:maxPayloadInPrompt] + "\n… (cut)"
		}
		b.WriteString(payload)
		b.WriteString("\n```\n")
	}
	if r.SessionID != "" {
		b.WriteString("\nIf there is nothing new worth telling the owner, call the routine_no_update tool instead of answering. If you could not do the task, call routine_report_failure with the reason.")
	}
	return b.String()
}

// ---- runs follow their turns ----

// routineTurnStarted marks the runs of a turn's inputs running.
func (s *Service) routineTurnStarted(ctx context.Context, turn string, inputs []string) {
	for _, id := range inputs {
		run, err := s.Store.Routines().RunByInput(ctx, id)
		if err != nil {
			continue
		}
		run.Status, run.Turn = store.RunRunning, turn
		_ = s.Store.Routines().UpdateRun(ctx, run)
	}
}

// routineTurnEnded ends the runs of a turn; it returns the inputs of runs
// that were quiet (no update), so the conversation can fold their answers.
func (s *Service) routineTurnEnded(ctx context.Context, turn, status, errText string) []string {
	runs, err := s.Store.Routines().RunsByTurn(ctx, turn)
	if err != nil || len(runs) == 0 {
		return nil
	}
	var quiet []string
	now := s.clock()
	for _, run := range runs {
		if run.EndedAt != nil {
			continue
		}
		switch {
		case run.Status == store.RunFailed:
		case status == convo.StatusInterrupted:
			run.Status = store.RunInterrupted
		case status == convo.StatusFailed:
			run.Status, run.Error = store.RunFailed, errText
		default:
			run.Status = store.RunSucceeded
		}
		if run.Quiet && run.Status == store.RunSucceeded && run.Source != store.RunTest {
			quiet = append(quiet, run.InputID)
		}
		run.EndedAt = &now
		_ = s.Store.Routines().UpdateRun(ctx, run)
	}
	return quiet
}

// RoutineReport records what the agent said about the run of input: no
// update, or a failure with reason.
func (s *Service) RoutineReport(ctx context.Context, sess *store.Session, inputID string, failed bool, reason string) error {
	run, err := s.Store.Routines().RunByInput(ctx, inputID)
	if err != nil {
		return &ValidationError{"this input is not a routine run"}
	}
	r, err := s.Store.Routines().Get(ctx, run.RoutineID)
	if err != nil || r.SessionID != sess.ID {
		return &ValidationError{"this input is not a routine run of this agent"}
	}
	if failed {
		run.Status, run.Error = store.RunFailed, strings.TrimSpace(reason)
		if run.Error == "" {
			run.Error = "the agent reported a failure"
		}
		run.Quiet = false
	} else if run.Status != store.RunFailed {
		run.Quiet = true
	}
	return s.Store.Routines().UpdateRun(ctx, run)
}

// ---- webhooks ----

// WebhookRequest is what arrived at a routine's webhook URL.
type WebhookRequest struct {
	RoutineID string
	Body      []byte
	// Signature is the X-Hub-Signature-256 / X-Signature-256 header
	// ("sha256=<hex>"); Token a bearer token or ?token=.
	Signature string
	Token     string
	EventType string
}

// ErrWebhookAuth: the request is not signed with the routine's secret.
var ErrWebhookAuth = errors.New("bad signature")

// Webhook runs a webhook routine for a request; ignored reports a request
// that was valid but did not match the routine's event type or filters.
func (s *Service) Webhook(ctx context.Context, req WebhookRequest) (run *RunView, ignored string, err error) {
	r, err := s.Store.Routines().Get(ctx, req.RoutineID)
	if err != nil || r.Trigger != store.TriggerWebhook || r.WebhookSecret == "" {
		return nil, "", store.ErrNotFound
	}
	if !verifyWebhook(r.WebhookSecret, req) {
		return nil, "", ErrWebhookAuth
	}
	if !r.Enabled {
		return nil, "the routine is paused", nil
	}
	var body any
	_ = json.Unmarshal(req.Body, &body)
	eventType := req.EventType
	if eventType == "" {
		if m, ok := body.(map[string]any); ok {
			for _, k := range []string{"type", "event", "event_type", "action"} {
				if v, ok := m[k].(string); ok && v != "" {
					eventType = v
					break
				}
			}
		}
	}
	if r.EventType != "" && !strings.EqualFold(r.EventType, eventType) {
		return nil, fmt.Sprintf("event %q is not %q", eventType, r.EventType), nil
	}
	var filters []WebhookFilter
	_ = json.Unmarshal(r.Filters, &filters)
	for _, f := range filters {
		if got := jsonPath(body, f.Path); got != f.Value {
			return nil, fmt.Sprintf("%s is %q, not %q", f.Path, got, f.Value), nil
		}
	}
	payload := string(req.Body)
	if body != nil {
		if pretty, err := json.MarshalIndent(body, "", "  "); err == nil {
			payload = string(pretty)
		}
	}
	rr, err := s.fireRoutine(ctx, r, store.RunWebhook, nil, eventType, payload)
	if err != nil && rr == nil {
		return nil, "", err
	}
	return runView(rr), "", nil
}

func verifyWebhook(secret string, req WebhookRequest) bool {
	if req.Token != "" {
		return subtle.ConstantTimeCompare([]byte(req.Token), []byte(secret)) == 1
	}
	sig, ok := strings.CutPrefix(req.Signature, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(req.Body)
	return hmac.Equal(got, mac.Sum(nil))
}

// jsonPath reads a dotted path ("a.b.0.c") from decoded JSON as text.
func jsonPath(v any, path string) string {
	for _, part := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[part]
		case []any:
			var i int
			if _, err := fmt.Sscanf(part, "%d", &i); err != nil || i < 0 || i >= len(x) {
				return ""
			}
			v = x[i]
		default:
			return ""
		}
	}
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
