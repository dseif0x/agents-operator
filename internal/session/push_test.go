package session

import (
	"context"
	"sync"
	"testing"

	"github.com/dseif0x/agents-operator/internal/push"
	"github.com/dseif0x/agents-operator/internal/runner"
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
	if _, err := svc.AgentPost(ctx, ss[1], c.ID, "## Done\n**LGTM**, merged `fix/x`.", 0, nil); err != nil {
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
