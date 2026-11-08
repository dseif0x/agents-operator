package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/runner/convo"
	"github.com/dseif0x/agents-operator/internal/runner/server"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
)

// fakeDriver answers every input with a tool call and a streamed answer;
// an input containing "slow" keeps the turn open until Interrupt.
type fakeDriver struct {
	events  chan convo.Event
	mu      sync.Mutex
	got     []convo.Input
	turn    string
	n       int
	answers []string
}

func (f *fakeDriver) Start(context.Context, string) error {
	f.events = make(chan convo.Event, 64)
	f.events <- convo.Event{Type: convo.EventInfo, Info: &convo.Info{Model: "fake-1", Ref: "ref-1"}}
	return nil
}

func (f *fakeDriver) Send(in convo.Input) error {
	f.mu.Lock()
	f.got = append(f.got, in)
	f.n++
	turn := fmt.Sprintf("t%d", f.n)
	f.turn = turn
	f.mu.Unlock()
	f.events <- convo.Event{Type: convo.EventTurnStarted, Turn: turn, Inputs: []string{in.ID}}
	f.events <- convo.Event{Type: convo.EventItem, Turn: turn, Item: &convo.Item{Key: turn + "-tool", Kind: convo.KindTool, Status: convo.StatusRunning, Tool: &convo.Tool{Name: "Bash", Kind: convo.ToolCommand, Title: "ls"}}}
	f.events <- convo.Event{Type: convo.EventItem, Turn: turn, Item: &convo.Item{Key: turn + "-tool", Kind: convo.KindTool, Status: convo.StatusDone, Tool: &convo.Tool{Output: "a.txt"}}}
	if strings.Contains(in.Text, "ask") {
		f.events <- convo.Event{Type: convo.EventRequest, Turn: turn, Request: &convo.Request{ID: "req-" + turn, Kind: convo.RequestPermission, Title: "Run a command",
			Tool: &convo.Tool{Name: "Bash", Kind: convo.ToolCommand, Title: "rm -rf build"}, Options: []convo.Option{{ID: "allow", Label: "Allow", Kind: "allow"}, {ID: "deny", Label: "Deny", Kind: "deny"}}}}
		return nil
	}
	if strings.Contains(in.Text, "slow") {
		f.events <- convo.Event{Type: convo.EventItem, Turn: turn, Item: &convo.Item{Key: turn + "-text", Kind: convo.KindText, Status: convo.StatusRunning, Text: "Working on ", Delta: true}}
		return nil
	}
	for _, part := range []string{"Got ", "it: ", in.Text} {
		f.events <- convo.Event{Type: convo.EventItem, Turn: turn, Item: &convo.Item{Key: turn + "-text", Kind: convo.KindText, Status: convo.StatusRunning, Text: part, Delta: true}}
	}
	f.events <- convo.Event{Type: convo.EventItem, Turn: turn, Item: &convo.Item{Key: turn + "-text", Kind: convo.KindText, Status: convo.StatusDone}}
	f.end(convo.StatusDone)
	return nil
}

func (f *fakeDriver) end(status string) {
	f.mu.Lock()
	turn := f.turn
	f.turn = ""
	f.mu.Unlock()
	if turn != "" {
		f.events <- convo.Event{Type: convo.EventTurnEnded, Turn: turn, Status: status, Usage: &convo.Usage{Input: 10, Output: 5, Context: 1234, ContextWindow: 200000}}
	}
}

func (f *fakeDriver) Interrupt() error        { f.end(convo.StatusInterrupted); return nil }
func (f *fakeDriver) Steer(convo.Input) error { return convo.ErrNoSteer }
func (f *fakeDriver) Respond(id string, r convo.Response) error {
	f.mu.Lock()
	turn := f.turn
	f.answers = append(f.answers, r.Option)
	f.mu.Unlock()
	f.events <- convo.Event{Type: convo.EventRequestDone, Turn: turn, Request: &convo.Request{ID: id}}
	f.events <- convo.Event{Type: convo.EventItem, Turn: turn, Item: &convo.Item{Key: turn + "-text", Kind: convo.KindText, Status: convo.StatusDone, Text: "answer: " + r.Option}}
	f.end(convo.StatusDone)
	return nil
}
func (f *fakeDriver) SetModel(string) error      { return nil }
func (f *fakeDriver) Events() <-chan convo.Event { return f.events }
func (f *fakeDriver) Wait() error                { select {} }
func (f *fakeDriver) Close()                     {}

func (f *fakeDriver) inputs() []convo.Input {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]convo.Input(nil), f.got...)
}

// convFixture wires a hub service to a runner whose conversation uses d.
func convFixture(t *testing.T, ctx context.Context, d convo.Driver) (*Service, *store.User, *store.Session, *Uplinks, string) {
	t.Helper()
	svc, _, u := newService(t)
	sess, err := svc.Create(ctx, u, CreateRequest{Name: "impl", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Interface != store.InterfaceConversation {
		t.Fatalf("a new Claude Code session runs as %q", sess.Interface)
	}
	sess, _ = svc.Store.Sessions().SetState(ctx, sess.ID, store.StateRunning, "")
	var logOut io.Writer = io.Discard
	if os.Getenv("CONVO_DEBUG") != "" {
		logOut = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
	root := t.TempDir()
	proc := server.NewProcess(func() server.Command { return server.Command{Path: "cat"} }, log)
	if err := proc.Start(); err != nil {
		t.Skip("cannot start cat:", err)
	}
	t.Cleanup(proc.Close)
	chat := &server.Chat{Proc: proc, Agent: "claude", Home: filepath.Join(root, "home"), Roots: []string{root}, Log: log}
	conv := &convo.Session{New: func(string) convo.Driver { return d }, StateFile: filepath.Join(root, "state.json"), Send: chat.SendEvents, ClaimInput: chat.ClaimInput, Log: log}
	chat.Convo = conv
	srv := &server.Server{Proc: proc, Token: "tok", Agent: "claude", Chat: chat, Convo: conv, Log: log}
	runnerHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(runnerHTTP.Close)
	go chat.Run(ctx)
	go conv.Run(ctx)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(runnerHTTP.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	ul := NewUplinks(svc, staticResolver{term.Endpoint{IP: host, Port: port, Token: "tok"}}, log)
	svc.Uplinks = ul
	ul.Ensure(ctx, []*store.Session{sess})
	t.Cleanup(ul.CloseAll)
	return svc, u, sess, ul, root
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func itemsByKey(t *testing.T, svc *Service, u *store.User, id string) map[string]*store.ConvItem {
	t.Helper()
	items, err := svc.Items(context.Background(), u.ID, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*store.ConvItem{}
	for _, it := range items {
		m[it.Key] = it
	}
	return m
}

func itemText(it *store.ConvItem) string {
	var d ItemData
	_ = json.Unmarshal(it.Data, &d)
	return d.Text
}

func TestConversationEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &fakeDriver{}
	svc, u, sess, _, root := convFixture(t, ctx, d)

	// A message with a file reaches the agent, and its answer is stored.
	in, err := svc.SendInput(ctx, u, sess.ID, "list the files", []ChatFile{{Name: "notes.md", Data: []byte("# notes\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != ItemQueued {
		t.Fatalf("new input item: %+v", in)
	}
	waitFor(t, "the answer", 10*time.Second, func() bool {
		it := itemsByKey(t, svc, u, sess.ID)["t1-text"]
		return it != nil && it.Status == convo.StatusDone
	})
	got := d.inputs()
	if len(got) != 1 || got[0].Text != "list the files" || len(got[0].Files) != 1 {
		t.Fatalf("driver got %+v", got)
	}
	if b, err := os.ReadFile(got[0].Files[0]); err != nil || string(b) != "# notes\n" || !strings.HasPrefix(got[0].Files[0], filepath.Join(root, "home", "inputs")) {
		t.Fatalf("input file %s: %q %v", got[0].Files[0], b, err)
	}
	items := itemsByKey(t, svc, u, sess.ID)
	if txt := itemText(items["t1-text"]); txt != "Got it: list the files" {
		t.Fatalf("answer text = %q", txt)
	}
	if items[in.Key].Status != convo.StatusDone {
		t.Fatalf("the input item stays %q", items[in.Key].Status)
	}
	var tool ItemData
	_ = json.Unmarshal(items["t1-tool"].Data, &tool)
	if tool.Tool == nil || tool.Tool.Title != "ls" || tool.Tool.Output != "a.txt" || tool.Tool.Name != "Bash" {
		t.Fatalf("tool item = %s", items["t1-tool"].Data)
	}
	waitFor(t, "the summary", 5*time.Second, func() bool {
		s, _ := svc.Store.Sessions().Get(ctx, sess.ID)
		var sum ConvSummary
		_ = json.Unmarshal(s.Conversation, &sum)
		return sum.Model == "fake-1" && sum.Context == 1234 && sum.Turn == "" && sum.Ref == "ref-1"
	})

	// While a turn runs, further messages queue; one is edited, one
	// cancelled, and Stop ends the turn.
	if _, err := svc.SendInput(ctx, u, sess.ID, "slow job", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the slow turn", 5*time.Second, func() bool { return len(d.inputs()) == 2 })
	second, _ := svc.SendInput(ctx, u, sess.ID, "second", nil)
	third, _ := svc.SendInput(ctx, u, sess.ID, "third", nil)
	var d2, d3 ItemData
	_ = json.Unmarshal(second.Data, &d2)
	_ = json.Unmarshal(third.Data, &d3)
	if _, err := svc.EditInput(ctx, u, sess.ID, d2.Input, "second, edited"); err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelInput(ctx, u, sess.ID, d3.Input); err != nil {
		t.Fatal(err)
	}
	if err := svc.Interrupt(ctx, u, sess.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the edited message", 10*time.Second, func() bool { return len(d.inputs()) >= 3 })
	time.Sleep(300 * time.Millisecond)
	got = d.inputs()
	if len(got) != 3 || got[2].Text != "second, edited" {
		t.Fatalf("driver got %+v", got)
	}
	items = itemsByKey(t, svc, u, sess.ID)
	if items[third.Key] != nil {
		t.Fatal("the cancelled message is still there")
	}
	if items["turn-stopped:"+itemsTurn(items, "t2-text")] == nil {
		t.Fatalf("no stop notice: %v", keys(items))
	}
	if st := items["t2-text"].Status; st != convo.StatusInterrupted {
		t.Fatalf("the interrupted answer is %q", st)
	}
	// Sent messages can no longer be changed.
	if _, err := svc.EditInput(ctx, u, sess.ID, d2.Input, "too late"); err == nil {
		t.Fatal("a sent message was edited")
	}
}

func TestConversationRequestsAndMemories(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &fakeDriver{}
	svc, u, sess, _, _ := convFixture(t, ctx, d)

	// A memory reaches the agent with its next input.
	if _, err := svc.AddMemory(ctx, u, sess.ID, "The owner prefers short answers."); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the push reaches the runner
	if _, err := svc.SendInput(ctx, u, sess.ID, "ask before deleting", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the permission request", 10*time.Second, func() bool {
		it := itemsByKey(t, svc, u, sess.ID)["request:req-t1"]
		return it != nil && it.Status == "pending"
	})
	if got := d.inputs(); len(got) != 1 || !strings.Contains(got[0].Text, "[agents-operator memories]") || !strings.Contains(got[0].Text, "short answers") || !strings.HasSuffix(got[0].Text, "ask before deleting") {
		t.Fatalf("driver got %+v", got)
	}
	it, err := svc.AnswerRequest(ctx, u, sess.ID, "req-t1", runner.ConvoResponse{Option: "deny"})
	if err != nil || it.Status != "answered" {
		t.Fatalf("answer = %+v, %v", it, err)
	}
	waitFor(t, "the answer", 10*time.Second, func() bool {
		a := itemsByKey(t, svc, u, sess.ID)["t1-text"]
		return a != nil && itemText(a) == "answer: deny"
	})
	if it := itemsByKey(t, svc, u, sess.ID)["request:req-t1"]; it.Status != "answered" {
		t.Fatalf("request item after the answer = %s", it.Status)
	}
	if _, err := svc.AnswerRequest(ctx, u, sess.ID, "req-t1", runner.ConvoResponse{Option: "allow"}); err == nil {
		t.Fatal("answered twice")
	}
	// The memory went with the first input only.
	if _, err := svc.SendInput(ctx, u, sess.ID, "next", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second input", 10*time.Second, func() bool { return len(d.inputs()) == 2 })
	if got := d.inputs()[1].Text; got != "next" {
		t.Fatalf("second input = %q", got)
	}
}

func itemsTurn(m map[string]*store.ConvItem, key string) string {
	if it := m[key]; it != nil {
		return it.Turn
	}
	return ""
}

func keys(m map[string]*store.ConvItem) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestConversationLive runs the real Claude Code CLI behind the runner
// (CONVO_E2E=1 with CONVO_HOME and CONVO_ENV as for convo.TestLive).
func TestConversationLive(t *testing.T) {
	if os.Getenv("CONVO_E2E") == "" {
		t.Skip("set CONVO_E2E to run the real CLI")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := append(os.Environ(), "HOME="+os.Getenv("CONVO_HOME"))
	for _, kv := range strings.Split(os.Getenv("CONVO_ENV"), ";") {
		if kv != "" {
			env = append(env, kv)
		}
	}
	dir := t.TempDir()
	d := &convo.Claude{Cmd: convo.Command{Path: "claude", Args: strings.Fields(os.Getenv("CONVO_ARGS")), Dir: dir, Env: env}, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	svc, u, sess, _, _ := convFixture(t, ctx, d)
	if _, err := svc.SendInput(ctx, u, sess.ID, "Run `echo hello > x.txt` with your Bash tool, then answer: done.", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a finished turn", 5*time.Minute, func() bool {
		s, _ := svc.Store.Sessions().Get(ctx, sess.ID)
		var sum ConvSummary
		_ = json.Unmarshal(s.Conversation, &sum)
		return sum.LastTurnAt != nil && sum.Turn == ""
	})
	items, _ := svc.Items(ctx, u.ID, sess.ID, 0, 0)
	for _, it := range items {
		t.Logf("%s %s %s %s", it.Kind, it.Status, it.Key, it.Data)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "x.txt")); err != nil || strings.TrimSpace(string(b)) != "hello" {
		t.Errorf("x.txt: %q %v", b, err)
	}
}
