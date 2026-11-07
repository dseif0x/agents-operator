package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/routine"
	"github.com/dseif0x/agents-operator/internal/runner/convo"
	"github.com/dseif0x/agents-operator/internal/store"
	"k8s.io/utils/ptr"
)

func TestRoutineSchedule(t *testing.T) {
	ctx := context.Background()
	svc, _, u := newService(t)
	now := time.Date(2026, 11, 9, 6, 59, 0, 0, time.UTC) // a Monday
	svc.now = func() time.Time { return now }
	sess, _ := svc.Create(ctx, u, CreateRequest{Name: "chief", Agent: "claude"})
	stopped, _ := svc.Store.Sessions().SetState(ctx, sess.ID, store.StateStopped, "")

	v, err := svc.CreateRoutine(ctx, u, RoutineInput{SessionID: sess.ID, Name: ptr.To("Morning brief"), Instruction: ptr.To("Summarise the open PRs."),
		Trigger: ptr.To(store.TriggerSchedule), Schedule: &routine.Schedule{Kind: routine.Weekdays, At: "07:00"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.NextRunAt == nil || !v.NextRunAt.Equal(time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)) || v.Describe != "Weekdays at 07:00" {
		t.Fatalf("routine = %+v", v)
	}
	// Not due yet.
	svc.RunDueRoutines(ctx)
	if runs, _ := svc.RoutineRuns(ctx, u, v.ID, 10); len(runs) != 0 {
		t.Fatalf("ran early: %+v", runs)
	}
	// Due: a queued input for the agent, which is started.
	now = now.Add(2 * time.Minute)
	svc.RunDueRoutines(ctx)
	runs, _ := svc.RoutineRuns(ctx, u, v.ID, 10)
	if len(runs) != 1 || runs[0].Status != store.RunQueued || runs[0].InputID == "" || runs[0].Source != store.RunSchedule {
		t.Fatalf("runs = %+v", runs)
	}
	ins, _ := svc.Store.Conversations().QueuedInputs(ctx, sess.ID)
	if len(ins) != 1 || ins[0].Source != convo.SourceRoutine || !strings.Contains(ins[0].Text, "Summarise the open PRs.") || !strings.Contains(ins[0].Text, "routine_no_update") {
		t.Fatalf("queued = %+v", ins)
	}
	if s, _ := svc.Store.Sessions().Get(ctx, stopped.ID); s.State != store.StateCreating {
		t.Fatalf("the stopped agent was not started: %s", s.State)
	}
	r, _ := svc.Store.Routines().Get(ctx, v.ID)
	if r.NextRunAt == nil || !r.NextRunAt.Equal(time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("next run = %v", r.NextRunAt)
	}

	// The run follows its turn; the agent said there was no news.
	runID := runs[0].InputID
	svc.routineTurnStarted(ctx, "t1", []string{runID})
	if err := svc.RoutineReport(ctx, sess, runID, false, ""); err != nil {
		t.Fatal(err)
	}
	quiet := svc.routineTurnEnded(ctx, "t1", convo.StatusDone, "")
	if len(quiet) != 1 || quiet[0] != runID {
		t.Fatalf("quiet inputs = %v", quiet)
	}
	runs, _ = svc.RoutineRuns(ctx, u, v.ID, 10)
	if runs[0].Status != store.RunSucceeded || !runs[0].Quiet || runs[0].EndedAt == nil {
		t.Fatalf("run = %+v", runs[0])
	}

	// A hub that was down for a day records the missed run instead of
	// running it late.
	now = time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	svc.RunDueRoutines(ctx)
	runs, _ = svc.RoutineRuns(ctx, u, v.ID, 10)
	if len(runs) != 2 || runs[0].Status != store.RunFailed || !strings.Contains(runs[0].Error, "missed") {
		t.Fatalf("runs = %+v", runs)
	}

	// A test run, a failure report, pausing.
	run, err := svc.RunRoutineNow(ctx, u, v.ID)
	if err != nil || run.Source != store.RunTest {
		t.Fatalf("test run = %+v, %v", run, err)
	}
	svc.routineTurnStarted(ctx, "t2", []string{run.InputID})
	_ = svc.RoutineReport(ctx, sess, run.InputID, true, "the API was down")
	svc.routineTurnEnded(ctx, "t2", convo.StatusDone, "")
	runs, _ = svc.RoutineRuns(ctx, u, v.ID, 1)
	if runs[0].Status != store.RunFailed || runs[0].Error != "the API was down" {
		t.Fatalf("failed run = %+v", runs[0])
	}
	v, err = svc.UpdateRoutine(ctx, u, v.ID, RoutineInput{Enabled: ptr.To(false)})
	if err != nil || v.Enabled || v.NextRunAt != nil {
		t.Fatalf("paused = %+v, %v", v, err)
	}

	// Validation.
	if _, err := svc.CreateRoutine(ctx, u, RoutineInput{SessionID: sess.ID, Name: ptr.To("x"), Instruction: ptr.To("y"), Trigger: ptr.To(store.TriggerSchedule), Schedule: &routine.Schedule{Kind: routine.Weekly}}); err == nil {
		t.Fatal("a weekly routine without days was accepted")
	}
	term, _ := svc.Create(ctx, u, CreateRequest{Name: "old", Agent: "claude", Interface: ptr.To(store.InterfaceTerminal)})
	if _, err := svc.CreateRoutine(ctx, u, RoutineInput{SessionID: term.ID, Name: ptr.To("x"), Instruction: ptr.To("y"), Trigger: ptr.To(store.TriggerSchedule), Schedule: &routine.Schedule{Kind: routine.Daily}}); err == nil {
		t.Fatal("a routine for a terminal session was accepted")
	}

	// Calendar: the past runs and the upcoming ones.
	_, _ = svc.UpdateRoutine(ctx, u, v.ID, RoutineInput{Enabled: ptr.To(true)})
	entries, err := svc.Calendar(ctx, u, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	past, upcoming := 0, 0
	for _, e := range entries {
		if e.Run != nil {
			past++
		} else {
			upcoming++
		}
	}
	// Past: Monday's run, the missed one, the test run; upcoming from "now"
	// (Tuesday evening): Wednesday to Friday.
	if past != 3 || upcoming != 3 {
		t.Fatalf("calendar: %d past, %d upcoming", past, upcoming)
	}
}

func TestRoutineWebhookAndChat(t *testing.T) {
	ctx := context.Background()
	svc, u, ss, c := chatFixture(t)
	v, err := svc.CreateRoutine(ctx, u, RoutineInput{ChatID: c.ID, Name: ptr.To("Deploy review"), Instruction: ptr.To("Review the deployment."),
		Trigger: ptr.To(store.TriggerWebhook), EventType: ptr.To("deployment.created"), Filters: &[]WebhookFilter{{Path: "deployment.env", Value: "prod"}}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Secret == "" || v.WebhookPath != "/api/v1/hooks/"+v.ID {
		t.Fatalf("webhook routine = %+v", v)
	}
	sign := func(body string) string {
		mac := hmac.New(sha256.New, []byte(v.Secret))
		mac.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	body := `{"type":"deployment.created","deployment":{"env":"prod","sha":"abc123"}}`
	// Wrong signature.
	if _, _, err := svc.Webhook(ctx, WebhookRequest{RoutineID: v.ID, Body: []byte(body), Signature: "sha256=00"}); err != ErrWebhookAuth {
		t.Fatalf("bad signature err = %v", err)
	}
	// Not matching the filter.
	other := `{"type":"deployment.created","deployment":{"env":"staging"}}`
	if _, ignored, err := svc.Webhook(ctx, WebhookRequest{RoutineID: v.ID, Body: []byte(other), Signature: sign(other)}); err != nil || ignored == "" {
		t.Fatalf("filtered request: ignored=%q err=%v", ignored, err)
	}
	// A match posts to the chat, with the payload.
	run, ignored, err := svc.Webhook(ctx, WebhookRequest{RoutineID: v.ID, Body: []byte(body), Signature: sign(body)})
	if err != nil || ignored != "" || run.Status != store.RunSent {
		t.Fatalf("run = %+v ignored=%q err=%v", run, ignored, err)
	}
	msgs, _ := svc.Store.Chats().Messages(ctx, c.ID, 0, 0, 10)
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Text, "Review the deployment.") || !strings.Contains(last.Text, "abc123") || last.AuthorSession != "" {
		t.Fatalf("chat message = %+v", last)
	}
	if len(queuedFor(t, svc, ss[0])) == 0 {
		t.Fatal("the chat's agents got nothing")
	}
	// A bearer token works too; a rotated secret replaces the old one.
	if _, _, err := svc.Webhook(ctx, WebhookRequest{RoutineID: v.ID, Body: []byte(body), Token: v.Secret}); err != nil {
		t.Fatal(err)
	}
	rot, _ := svc.RotateSecret(ctx, u, v.ID)
	if _, _, err := svc.Webhook(ctx, WebhookRequest{RoutineID: v.ID, Body: []byte(body), Token: v.Secret}); err != ErrWebhookAuth {
		t.Fatalf("old secret err = %v", err)
	}
	if _, _, err := svc.Webhook(ctx, WebhookRequest{RoutineID: v.ID, Body: []byte(body), Token: rot.Secret}); err != nil {
		t.Fatal(err)
	}
}
