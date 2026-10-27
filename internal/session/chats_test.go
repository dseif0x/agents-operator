package session

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/runner/server"
	"github.com/dseif0x/agents-operator/internal/store"
	"github.com/dseif0x/agents-operator/internal/term"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// chatFixture: an owner with three running agent sessions in one chat.
func chatFixture(t *testing.T) (*Service, *store.User, []*store.Session, *ChatView) {
	t.Helper()
	svc, _, u := newService(t)
	ctx := context.Background()
	var sessions []*store.Session
	for _, name := range []string{"impl", "review", "test"} {
		s, err := svc.Create(ctx, u, CreateRequest{Name: name, Agent: "claude", Persona: "You are " + name + "."})
		if err != nil {
			t.Fatal(err)
		}
		s, _ = svc.Store.Sessions().SetState(ctx, s.ID, store.StateRunning, "")
		sessions = append(sessions, s)
	}
	name := "launch"
	c, err := svc.CreateChat(ctx, u, ChatInput{Name: &name, Members: []string{sessions[0].ID, sessions[1].ID, sessions[2].ID}})
	if err != nil {
		t.Fatal(err)
	}
	return svc, u, sessions, c
}

func queuedFor(t *testing.T, svc *Service, s *store.Session) []Delivery {
	t.Helper()
	ds, err := svc.QueuedDeliveries(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestChatAddressing(t *testing.T) {
	svc, u, ss, c := chatFixture(t)
	ctx := context.Background()
	impl, review, tst := ss[0], ss[1], ss[2]
	if len(c.Members) != 3 || c.Members[1].Handle != review.Handle || c.Members[1].Persona != "You are review." {
		t.Fatalf("chat = %+v", c)
	}

	// The owner, untagged: every agent, to decide for itself.
	m, err := svc.PostChatMessage(ctx, u, c.ID, "Who wants the login bug?", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Deliveries) != 3 || m.Author.Kind != "owner" || m.Author.Name != "alice" {
		t.Fatalf("message = %+v", m)
	}
	for _, s := range ss {
		if ds := queuedFor(t, svc, s); len(ds) != 1 || ds[0].Address != AddressOpen || ds[0].ChatName != "launch" {
			t.Fatalf("%s queue = %+v", s.Name, ds)
		}
		_ = svc.MarkDelivered(ctx, s, []int64{m.ID})
	}

	// A mention: only that agent, addressed. An e-mail address is no mention.
	m, _ = svc.PostChatMessage(ctx, u, c.ID, "@"+review.Handle+" please look; mail dan@"+impl.Handle+".org", 0, nil)
	if len(m.Deliveries) != 1 || m.Deliveries[review.ID] != store.DeliveryQueued || !slices.Equal(m.Mentions, []string{review.ID}) {
		t.Fatalf("mention = %+v", m)
	}
	if ds := queuedFor(t, svc, review); len(ds) != 1 || ds[0].Address != AddressDirect {
		t.Fatalf("review queue = %+v", ds)
	}
	_ = svc.MarkDelivered(ctx, review, []int64{m.ID})

	// An agent's untagged message: everyone else (not the author).
	am, err := svc.AgentPost(ctx, review, "launch", "LGTM with one nit.", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if am.Author.Kind != "session" || am.Author.Handle != review.Handle || len(am.Deliveries) != 2 || am.Deliveries[review.ID] != "" {
		t.Fatalf("agent message = %+v", am)
	}
	_ = svc.MarkDelivered(ctx, impl, []int64{am.ID})
	_ = svc.MarkDelivered(ctx, tst, []int64{am.ID})

	// A reply to an agent's message addresses its author.
	rm, err := svc.PostChatMessage(ctx, u, c.ID, "Which nit?", am.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rm.ReplyTo != am.ID || len(rm.Deliveries) != 1 || rm.Deliveries[review.ID] == "" {
		t.Fatalf("reply = %+v", rm)
	}
	_ = svc.MarkDelivered(ctx, review, []int64{rm.ID})

	// @everyone: everyone, asked to answer.
	em, _ := svc.PostChatMessage(ctx, u, c.ID, "@everyone status?", 0, nil)
	if !em.Everyone || len(em.Deliveries) != 3 {
		t.Fatalf("everyone = %+v", em)
	}
	if ds := queuedFor(t, svc, tst); len(ds) != 1 || ds[0].Address != AddressEveryone {
		t.Fatalf("test queue = %+v", ds)
	}
	for _, s := range ss {
		_ = svc.MarkDelivered(ctx, s, []int64{em.ID})
	}

	// A muted member gets nothing, and its messages reach nobody.
	if _, err := svc.SetChatMemberMuted(ctx, u, c.ID, tst.ID, true); err != nil {
		t.Fatal(err)
	}
	m, _ = svc.PostChatMessage(ctx, u, c.ID, "next", 0, nil)
	if len(m.Deliveries) != 2 || m.Deliveries[tst.ID] != "" {
		t.Fatalf("with a muted member = %+v", m.Deliveries)
	}
	if mm, _ := svc.AgentPost(ctx, tst, c.ID, "hello?", 0, nil); len(mm.Deliveries) != 0 {
		t.Fatalf("muted member's message delivered: %+v", mm.Deliveries)
	}
	// A muted chat delivers nothing.
	muted := true
	if _, err := svc.UpdateChat(ctx, u, c.ID, ChatInput{Muted: &muted}); err != nil {
		t.Fatal(err)
	}
	if m, _ := svc.PostChatMessage(ctx, u, c.ID, "quiet", 0, nil); len(m.Deliveries) != 0 {
		t.Fatalf("muted chat delivered: %+v", m.Deliveries)
	}

	// Unread counts: the owner has read what it wrote, not what agents wrote after.
	list, err := svc.ListChats(ctx, u)
	if err != nil || len(list) != 1 || list[0].LastMessage == nil || list[0].LastMessage.Text != "quiet" || list[0].Unread != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// The agent side: its chats, info, history (marking it read).
	chats, _ := svc.AgentChats(ctx, impl)
	if len(chats) != 1 || chats[0].Unread == 0 {
		t.Fatalf("agent chats = %+v", chats)
	}
	info, err := svc.AgentChatInfo(ctx, impl, "LAUNCH")
	if err != nil || len(info.Members) != 3 {
		t.Fatalf("info = %+v, %v", info, err)
	}
	_, msgs, err := svc.AgentReadChat(ctx, impl, c.ID, 0, 100)
	if err != nil || len(msgs) < 8 {
		t.Fatalf("read = %d, %v", len(msgs), err)
	}
	if chats, _ := svc.AgentChats(ctx, impl); chats[0].Unread != 0 {
		t.Fatalf("unread after reading = %d", chats[0].Unread)
	}
	// A session sees only its own chats.
	other, _ := svc.Create(ctx, u, CreateRequest{Name: "outsider", Agent: "claude"})
	if _, err := svc.AgentPost(ctx, other, "launch", "hi", 0, nil); err == nil {
		t.Fatal("an outsider posted")
	}
}

func TestChatValidation(t *testing.T) {
	svc, u, ss, c := chatFixture(t)
	ctx := context.Background()
	for name, f := range map[string]func() error{
		"empty": func() error { _, err := svc.PostChatMessage(ctx, u, c.ID, "  ", 0, nil); return err },
		"too long": func() error {
			_, err := svc.PostChatMessage(ctx, u, c.ID, strings.Repeat("x", MaxChatText+1), 0, nil)
			return err
		},
		"not image": func() error {
			_, err := svc.PostChatMessage(ctx, u, c.ID, "x", 0, []ChatImage{{Name: "a.txt", Data: []byte("hello")}})
			return err
		},
		"bad reply": func() error { _, err := svc.PostChatMessage(ctx, u, c.ID, "x", 999, nil); return err },
		"no name":   func() error { _, err := svc.CreateChat(ctx, u, ChatInput{}); return err },
		"not my sess": func() error {
			n := "x"
			_, err := svc.CreateChat(ctx, u, ChatInput{Name: &n, Members: []string{store.NewID()}})
			return err
		},
	} {
		if err := f(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Images: stored, served to the owner and to members only.
	m, err := svc.PostChatMessage(ctx, u, c.ID, "", 0, []ChatImage{{Name: "dot.png", Data: pngBytes(t)}})
	if err != nil || len(m.Attachments) != 1 || m.Attachments[0].ContentType != "image/png" {
		t.Fatalf("image message = %+v, %v", m, err)
	}
	a, data, err := svc.ChatAttachment(ctx, u, c.ID, m.Attachments[0].ID)
	if err != nil || a.Name != "dot.png" || !bytes.Equal(data, pngBytes(t)) {
		t.Fatalf("attachment = %+v, %v", a, err)
	}
	if _, _, err := svc.AgentAttachment(ctx, ss[0], m.Attachments[0].ID); err != nil {
		t.Fatalf("member attachment: %v", err)
	}
	other, _ := svc.Create(ctx, u, CreateRequest{Name: "outsider", Agent: "claude"})
	if _, _, err := svc.AgentAttachment(ctx, other, m.Attachments[0].ID); err == nil {
		t.Fatal("an outsider fetched an image")
	}
	// Removing a member drops what it had not received.
	if _, err := svc.RemoveChatMember(ctx, u, c.ID, ss[2].ID); err != nil {
		t.Fatal(err)
	}
	if ds := queuedFor(t, svc, ss[2]); len(ds) != 0 {
		t.Fatalf("removed member still has %d queued", len(ds))
	}
	if err := svc.DeleteChat(ctx, u, c.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.ListChats(ctx, u); len(list) != 0 {
		t.Fatal("chat not deleted")
	}
}

// TestChatUplinkEndToEnd runs the hub's uplink against a real runner Chat
// with `cat` as the agent: a message is typed into the terminal and
// confirmed, and the agent's tool calls reach the hub.
func TestChatUplinkEndToEnd(t *testing.T) {
	svc, u, ss, c := chatFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	root := t.TempDir()
	proc := server.NewProcess(func() server.Command { return server.Command{Path: "cat"} }, log)
	if err := proc.Start(); err != nil {
		t.Skip("cannot start cat:", err)
	}
	defer proc.Close()
	chat := &server.Chat{Proc: proc, Agent: "claude", Home: filepath.Join(root, "home"), Roots: []string{root}, QuietIdle: 200 * time.Millisecond, Log: log}
	srv := &server.Server{Proc: proc, Token: "tok", Agent: "claude", Chat: chat, Log: log}
	runnerHTTP := httptest.NewServer(srv.Handler())
	defer runnerHTTP.Close()
	local := httptest.NewServer(chat.Handler())
	defer local.Close()
	go chat.Run(ctx)

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(runnerHTTP.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	ul := NewUplinks(svc, staticResolver{term.Endpoint{IP: host, Port: port, Token: "tok"}}, log)
	svc.Uplinks = ul
	impl := ss[0]
	ul.Ensure(ctx, []*store.Session{impl})

	// The owner writes; the runner types it in and confirms.
	m, err := svc.PostChatMessage(ctx, u, c.ID, "@"+impl.Handle+" please fix the login bug", 0, []ChatImage{{Name: "shot.png", Data: pngBytes(t)}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ds, _ := svc.Store.Chats().Deliveries(ctx, []int64{m.ID}); len(ds) == 1 && ds[0].State == store.DeliveryDelivered {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ds, _ := svc.Store.Chats().Deliveries(ctx, []int64{m.ID}); len(ds) != 1 || ds[0].State != store.DeliveryDelivered {
		t.Fatalf("not delivered: %+v", ds)
	}
	typed := string(proc.Scrollback())
	for _, want := range []string{"[agents-operator chat]", "please fix the login bug", "addressed to you", "[image: "} {
		if !strings.Contains(typed, want) {
			t.Fatalf("terminal lacks %q:\n%s", want, typed)
		}
	}
	img := filepath.Join(root, "home", "chats", "launch", strconv.FormatInt(m.ID, 10)+"-1.png")
	if b, err := os.ReadFile(img); err != nil || !bytes.Equal(b, pngBytes(t)) {
		t.Fatalf("image file %s: %v", img, err)
	}

	// The agent's tools, through the runner's loopback API.
	call := func(op string, args any) map[string]json.RawMessage {
		t.Helper()
		b, _ := json.Marshal(args)
		resp, err := http.Post(local.URL+"/chat/"+op, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	shot := filepath.Join(root, "fix.png")
	_ = os.WriteFile(shot, pngBytes(t), 0o644)
	out := call(runner.OpChatSend, map[string]any{"chat": "launch", "text": "Fixed on fix/login. @" + ss[1].Handle + " can you review?", "reply_to": m.ID, "images": []string{shot}})
	if out["error"] != nil {
		t.Fatalf("chat_send: %s", out["error"])
	}
	msgs, _ := svc.Store.Chats().Messages(ctx, c.ID, 0, 0, 10)
	last := msgs[len(msgs)-1]
	if last.AuthorSession != impl.ID || last.ReplyTo != m.ID || len(last.Attachments) != 1 || !slices.Contains(last.Mentions, ss[1].ID) {
		t.Fatalf("posted = %+v", last)
	}
	// Files outside the workspace are refused.
	if out := call(runner.OpChatSend, map[string]any{"chat": "launch", "text": "x", "images": []string{"/etc/passwd"}}); out["error"] == nil {
		t.Fatal("a file outside the workspace was sent")
	}
	out = call(runner.OpChatRead, map[string]any{"chat": "launch"})
	if out["error"] != nil || !strings.Contains(string(out["result"]), "Fixed on fix/login") {
		t.Fatalf("chat_read = %s", out)
	}
	out = call(runner.OpChatInfo, map[string]any{"chat": "launch"})
	if !strings.Contains(string(out["result"]), ss[2].Handle) {
		t.Fatalf("chat_info = %s", out)
	}
	if out := call(runner.OpChatsList, nil); !strings.Contains(string(out["result"]), "launch") {
		t.Fatalf("chats_list = %s", out)
	}
	ul.CloseAll()
}
