package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRoutineStores(t *testing.T) {
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) { exerciseRoutines(t, st) })
	}
}

func exerciseRoutines(t *testing.T, st Store) {
	ctx := context.Background()
	u := &User{Username: "routine-owner", PasswordHash: "h"}
	if err := st.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	s := &Session{OwnerID: u.ID, Name: "chief", Agent: "claude", PVCSize: "1Gi", State: StateRunning, Interface: InterfaceConversation}
	if err := st.Sessions().Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-time.Minute)
	later := now.Add(time.Hour)
	r1 := &Routine{OwnerID: u.ID, SessionID: s.ID, Name: "brief", Instruction: "do it", Trigger: TriggerSchedule, Schedule: json.RawMessage(`{"kind":"daily","at":"07:00"}`), Enabled: true, NextRunAt: &due}
	r2 := &Routine{OwnerID: u.ID, SessionID: s.ID, Name: "later", Instruction: "x", Trigger: TriggerSchedule, Enabled: true, NextRunAt: &later}
	r3 := &Routine{OwnerID: u.ID, SessionID: s.ID, Name: "hook", Instruction: "y", Trigger: TriggerWebhook, Enabled: true, WebhookSecret: "s3cret", Filters: json.RawMessage(`[{"path":"a","value":"b"}]`)}
	for _, r := range []*Routine{r1, r2, r3} {
		if err := st.Routines().Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Routines().Get(ctx, r3.ID)
	if err != nil || got.SessionID != s.ID || got.ChatID != "" || got.WebhookSecret != "s3cret" || got.NextRunAt != nil {
		t.Fatalf("get = %+v, %v", got, err)
	}
	dueList, err := st.Routines().Due(ctx, now)
	if err != nil || len(dueList) != 1 || dueList[0].ID != r1.ID {
		t.Fatalf("due = %v, %v", dueList, err)
	}
	if err := st.Routines().SetRun(ctx, r1.ID, &later, now); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.Routines().Due(ctx, now); len(d) != 0 {
		t.Fatalf("still due: %v", d)
	}
	r2.Enabled = false
	r2.Name = "later, paused"
	if err := st.Routines().Update(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.Routines().Get(ctx, r2.ID); g.Enabled || g.Name != "later, paused" {
		t.Fatalf("updated = %+v", g)
	}
	if all, _ := st.Routines().List(ctx, u.ID); len(all) != 3 {
		t.Fatalf("list = %d", len(all))
	}

	// Runs.
	run := &RoutineRun{RoutineID: r1.ID, Source: RunSchedule, Status: RunQueued, InputID: "in-1", ScheduledFor: &due}
	if err := st.Routines().AddRun(ctx, run); err != nil || run.ID == 0 {
		t.Fatalf("add run = %+v, %v", run, err)
	}
	run.Status, run.Turn = RunRunning, "turn-1"
	if err := st.Routines().UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if g, err := st.Routines().RunByInput(ctx, "in-1"); err != nil || g.ID != run.ID || g.Turn != "turn-1" {
		t.Fatalf("by input = %+v, %v", g, err)
	}
	if rs, _ := st.Routines().RunsByTurn(ctx, "turn-1"); len(rs) != 1 {
		t.Fatalf("by turn = %v", rs)
	}
	if rs, _ := st.Routines().RunsSince(ctx, u.ID, now.Add(-time.Hour)); len(rs) != 1 {
		t.Fatalf("since = %v", rs)
	}
	if rs, _ := st.Routines().Runs(ctx, r1.ID, 10); len(rs) != 1 || rs[0].Status != RunRunning {
		t.Fatalf("runs = %v", rs)
	}
	if err := st.Routines().Delete(ctx, r1.ID); err != nil {
		t.Fatal(err)
	}
	if rs, _ := st.Routines().Runs(ctx, r1.ID, 10); len(rs) != 0 {
		t.Fatal("runs survived their routine")
	}
	// A session's routines go with it.
	if err := st.Sessions().Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
}
