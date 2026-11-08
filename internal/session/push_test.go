package session

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/dseif0x/agents-operator/internal/push"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/runner/convo"
)

type fakePusher struct {
	mu   sync.Mutex
	sent []push.Notification
}

func (f *fakePusher) Notify(_ string, n push.Notification) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
}

func TestChatPush(t *testing.T) {
	svc, u, ss, c := chatFixture(t)
	fp := &fakePusher{}
	svc.Push = fp
	ctx := context.Background()
	if _, err := svc.PostChatMessage(ctx, u, c.ID, "owner speaking", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AgentPost(ctx, ss[1], c.ID, "## Done\n**LGTM**, merged `fix/x`.", 0, nil, ""); err != nil {
		t.Fatal(err)
	}
	if len(fp.sent) != 1 {
		t.Fatalf("sent = %+v", fp.sent)
	}
	n := fp.sent[0]
	if n.Title != "launch · review" || n.Body != "Done LGTM, merged fix/x." || n.URL != "/chats/"+c.ID || n.Tag != "chat-"+c.ID {
		t.Fatalf("notification = %+v", n)
	}
}

func TestActivityPush(t *testing.T) {
	svc, _, u := newService(t)
	fp := &fakePusher{}
	svc.Push = fp
	sess, _ := svc.Create(context.Background(), u, CreateRequest{Name: "impl", Agent: "claude"})
	live := func(state, detail string) *Live {
		return &Live{Status: runner.Status{Activity: &runner.Activity{State: state, Detail: detail}}}
	}
	svc.notifyActivity(sess, live(runner.ActivityThinking, ""), live(runner.ActivityNeedsPermission, "Bash: rm -rf build"))
	svc.notifyActivity(sess, live(runner.ActivityNeedsPermission, ""), live(runner.ActivityNeedsPermission, "Bash: rm -rf build")) // no change
	svc.notifyActivity(sess, live(runner.ActivityTool, ""), live(runner.ActivityWaitingInput, ""))                                 // not notified
	svc.notifyActivity(sess, &Live{}, live(runner.ActivityError, "authentication_failed"))
	if len(fp.sent) != 2 || fp.sent[0].Title != "impl needs you" || fp.sent[0].Body != "Bash: rm -rf build" || fp.sent[0].URL != "/sessions/"+sess.ID ||
		fp.sent[1].Title != "impl hit an error" {
		t.Fatalf("sent = %+v", fp.sent)
	}
}

func TestConversationPush(t *testing.T) {
	svc, _, u := newService(t)
	fp := &fakePusher{}
	svc.Push = fp
	ctx := context.Background()
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "impl", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(evs ...convo.Event) {
		t.Helper()
		if err := svc.ApplyEvents(ctx, sess, evs); err != nil {
			t.Fatal(err)
		}
	}
	// A turn that ends: its last answer.
	apply(convo.Event{Type: convo.EventTurnStarted, Turn: "t1"},
		convo.Event{Type: convo.EventItem, Turn: "t1", Item: &convo.Item{Key: "a", Kind: convo.KindText, Status: convo.StatusDone, Text: "working on it"}},
		convo.Event{Type: convo.EventItem, Turn: "t1", Item: &convo.Item{Key: "sub", Kind: convo.KindText, Status: convo.StatusDone, Text: "sub-agent text", Parent: "tool1"}})
	apply(convo.Event{Type: convo.EventItem, Turn: "t1", Item: &convo.Item{Key: "b", Kind: convo.KindText, Status: convo.StatusDone, Text: "**Done**: tests pass."}})
	apply(convo.Event{Type: convo.EventTurnEnded, Turn: "t1", Status: convo.StatusDone})
	// A question, once even when the runner repeats it.
	req := &convo.Request{ID: "r1", Kind: "question", Questions: []convo.Question{{ID: "q", Question: "Which branch?"}}}
	apply(convo.Event{Type: convo.EventTurnStarted, Turn: "t2"}, convo.Event{Type: convo.EventRequest, Turn: "t2", Request: req})
	apply(convo.Event{Type: convo.EventRequest, Turn: "t2", Request: req})
	// Stopped by the owner: nothing.
	apply(convo.Event{Type: convo.EventTurnEnded, Turn: "t2", Status: convo.StatusInterrupted})
	// A turn for a chat message: the chat notifies.
	apply(convo.Event{Type: convo.EventTurnStarted, Turn: "t3", Inputs: []string{"chat-msg-7"}},
		convo.Event{Type: convo.EventItem, Turn: "t3", Item: &convo.Item{Key: "c", Kind: convo.KindText, Status: convo.StatusDone, Text: "replied in the chat"}},
		convo.Event{Type: convo.EventTurnEnded, Turn: "t3", Status: convo.StatusDone})
	// A failure.
	apply(convo.Event{Type: convo.EventTurnStarted, Turn: "t4"}, convo.Event{Type: convo.EventTurnEnded, Turn: "t4", Status: convo.StatusFailed, Error: "rate limited"})

	want := []push.Notification{
		{Title: "impl", Body: "Done: tests pass."},
		{Title: "impl needs you", Body: "Which branch?"},
		{Title: "impl hit an error", Body: "rate limited"},
	}
	if len(fp.sent) != len(want) {
		t.Fatalf("sent = %+v", fp.sent)
	}
	for i, w := range want {
		n := fp.sent[i]
		if n.Title != w.Title || n.Body != w.Body || n.URL != "/sessions/"+sess.ID || n.Tag != "session-"+sess.ID {
			t.Fatalf("notification %d = %+v, want %+v", i, n, w)
		}
	}
}

func TestAgentFiles(t *testing.T) {
	svc, _, u := newService(t)
	ctx := context.Background()
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "impl", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	// A picture the agent read shows on its step.
	png := []byte("\x89PNG\r\n\x1a\nfake")
	if err := svc.ApplyEvents(ctx, sess, []convo.Event{{Type: convo.EventTurnStarted, Turn: "t1"},
		{Type: convo.EventItem, Turn: "t1", Item: &convo.Item{Key: "tool1", Kind: convo.KindTool, Status: convo.StatusDone, Tool: &convo.Tool{Output: "[image]"}, Files: []convo.File{{Name: "home.png", ContentType: "image/png", Data: png}}}}}); err != nil {
		t.Fatal(err)
	}
	it, err := svc.Store.Conversations().GetItem(ctx, sess.ID, "main", "tool1")
	if err != nil {
		t.Fatal(err)
	}
	var d ItemData
	_ = json.Unmarshal(it.Data, &d)
	if len(d.Files) != 1 || d.Files[0].Name != "home.png" {
		t.Fatalf("tool files = %+v", d.Files)
	}
	f, err := svc.OwnerInputFile(ctx, u.ID, sess.ID, d.Files[0].ID)
	if err != nil || string(f.Data) != string(png) {
		t.Fatalf("file = %+v, %v", f, err)
	}
	// Files the agent shares: an item of their own in the turn.
	res, err := svc.AgentShareFiles(ctx, sess, runner.ShareFileArgs{Turn: "t1", Text: "the build log", Files: []runner.UplinkFile{{Name: "build.log", ContentType: "text/plain; charset=utf-8", Data: []byte("ok\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["shared"].([]string)[0] != "build.log" {
		t.Fatalf("result = %+v", res)
	}
	items, _ := svc.Store.Conversations().TurnItems(ctx, sess.ID, "main", "t1")
	var shared *ItemData
	for _, it := range items {
		if it.Kind == KindFiles {
			var d ItemData
			_ = json.Unmarshal(it.Data, &d)
			shared = &d
		}
	}
	if shared == nil || shared.Text != "the build log" || len(shared.Files) != 1 || shared.Files[0].ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("shared = %+v", shared)
	}
	if _, err := svc.AgentShareFiles(ctx, sess, runner.ShareFileArgs{Files: []runner.UplinkFile{{Name: "empty"}}}); err == nil {
		t.Fatal("an empty file was shared")
	}
}
