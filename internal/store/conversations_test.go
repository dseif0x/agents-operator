package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestConversationStores(t *testing.T) {
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) { exerciseConversations(t, st) })
	}
}

func exerciseConversations(t *testing.T, st Store) {
	ctx := context.Background()
	u := &User{Username: "conv-owner", PasswordHash: "h"}
	if err := st.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	s := &Session{OwnerID: u.ID, Name: "talker", Agent: "claude", PVCSize: "1Gi", State: StateCreating, Interface: InterfaceConversation}
	if err := st.Sessions().Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := st.Sessions().Get(ctx, s.ID)
	if err != nil || got.Interface != InterfaceConversation || string(got.Conversation) != "{}" {
		t.Fatalf("session = %+v, %v", got, err)
	}
	if err := st.Sessions().SetConversation(ctx, s.ID, json.RawMessage(`{"model":"m","context":5}`)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Sessions().Get(ctx, s.ID)
	var sum map[string]any
	_ = json.Unmarshal(got.Conversation, &sum)
	if sum["model"] != "m" {
		t.Fatalf("conversation = %s", got.Conversation)
	}
	got.Interface = InterfaceTerminal
	if up, err := st.Sessions().Update(ctx, got); err != nil || up.Interface != InterfaceTerminal {
		t.Fatalf("update interface = %+v, %v", up, err)
	}

	c := st.Conversations()
	// Items: upsert by key, pages oldest first, running items of a turn.
	var ids []int64
	for i, key := range []string{"a", "b", "c", "d"} {
		it, err := c.UpsertItem(ctx, &ConvItem{SessionID: s.ID, Thread: MainThread, Turn: "t1", Key: key, Kind: "text", Status: "running", Data: json.RawMessage(`{"text":"` + key + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && it.ID <= ids[len(ids)-1] {
			t.Fatalf("ids not increasing: %d after %v", it.ID, ids)
		}
		ids = append(ids, it.ID)
	}
	it, err := c.UpsertItem(ctx, &ConvItem{SessionID: s.ID, Thread: MainThread, Turn: "t1", Key: "b", Kind: "text", Status: "done", Data: json.RawMessage(`{"text":"bee"}`)})
	if err != nil || it.ID != ids[1] || it.Status != "done" {
		t.Fatalf("upsert existing = %+v, %v", it, err)
	}
	page, err := c.ListItems(ctx, s.ID, MainThread, 0, 2)
	if err != nil || len(page) != 2 || page[0].Key != "c" || page[1].Key != "d" {
		t.Fatalf("newest page = %v, %v", page, err)
	}
	page, _ = c.ListItems(ctx, s.ID, MainThread, page[0].ID, 10)
	if len(page) != 2 || page[0].Key != "a" || page[1].Key != "b" {
		t.Fatalf("older page = %v", page)
	}
	run, _ := c.RunningItems(ctx, s.ID, MainThread, "t1")
	if len(run) != 3 {
		t.Fatalf("running = %d", len(run))
	}
	if g, err := c.GetItem(ctx, s.ID, MainThread, "b"); err != nil || string(g.Data) != `{"text": "bee"}` && string(g.Data) != `{"text":"bee"}` {
		t.Fatalf("get = %+v, %v", g, err)
	}
	if err := c.DeleteItem(ctx, s.ID, MainThread, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetItem(ctx, s.ID, MainThread, "a"); err != ErrNotFound {
		t.Fatalf("deleted item err = %v", err)
	}

	// Inputs: queue, edit, claim once, files.
	in := &ConvInput{SessionID: s.ID, Text: "hi", Source: "owner", Files: []ConvFile{{OwnerID: u.ID, Name: "a.txt", ContentType: "text/plain", Size: 3, Data: []byte("abc")}}}
	if err := c.AddInput(ctx, in); err != nil {
		t.Fatal(err)
	}
	q, err := c.QueuedInputs(ctx, s.ID)
	if err != nil || len(q) != 1 || len(q[0].Files) != 1 || q[0].Files[0].Data != nil || q[0].Files[0].Name != "a.txt" {
		t.Fatalf("queued = %+v, %v", q, err)
	}
	if e, err := c.UpdateInput(ctx, s.ID, in.ID, "hello"); err != nil || e.Text != "hello" {
		t.Fatalf("update = %+v, %v", e, err)
	}
	f, err := c.GetFile(ctx, s.ID, in.Files[0].ID)
	if err != nil || string(f.Data) != "abc" {
		t.Fatalf("file = %+v, %v", f, err)
	}
	if ok, err := c.SetInputState(ctx, s.ID, in.ID, InputSent); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if ok, _ := c.SetInputState(ctx, s.ID, in.ID, InputSent); ok {
		t.Fatal("claimed twice")
	}
	if _, err := c.UpdateInput(ctx, s.ID, in.ID, "late"); err != ErrConflict {
		t.Fatalf("edit after send err = %v", err)
	}
	if q, _ := c.QueuedInputs(ctx, s.ID); len(q) != 0 {
		t.Fatalf("still queued: %+v", q)
	}
	if g, err := c.GetInput(ctx, s.ID, in.ID); err != nil || g.State != InputSent || g.SentAt == nil {
		t.Fatalf("sent input = %+v, %v", g, err)
	}
	// Deleting the session takes its conversation with it.
	if err := st.Sessions().Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
}
