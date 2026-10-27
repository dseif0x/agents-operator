package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// Chat is the runner's side of the chats its session is in. The hub opens
// the uplink (runner.UplinkPath) to it; through that the agent's chat tools
// (`agent-runner mcp`, which calls Chat's loopback API) reach the hub, and
// the hub pushes the messages queued for the agent. Chat hands those to the
// agent when it is idle by typing them into the terminal as a prompt, or,
// for Claude Code, at the end of a turn through the Stop hook.
type Chat struct {
	Proc  *Process
	Hooks *Hooks // nil for agents without hooks
	Agent string
	// Home is where received images are written (~/chats/<chat>/...).
	Home string
	// Roots are the directories the agent may send images from.
	Roots []string
	// QuietIdle is how long an agent without hooks must print nothing to
	// count as idle (default 20s; 3s when its last line looks like a prompt).
	QuietIdle time.Duration
	Log       *slog.Logger

	mu      sync.Mutex
	conn    *websocket.Conn
	connGen uint64
	nextID  int64
	pending map[int64]chan runner.UplinkFrame
	queue   []runner.ChatDelivery
	seen    map[int64]bool
	typedAt time.Time
	wake    chan struct{}
	wmu     sync.Mutex
}

func (c *Chat) init() {
	if c.pending == nil {
		c.pending = map[int64]chan runner.UplinkFrame{}
		c.seen = map[int64]bool{}
		c.wake = make(chan struct{}, 1)
	}
}

// ServeUplink accepts the hub's uplink. A new one replaces the old.
func (c *Chat) ServeUplink(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(runner.UplinkMaxFrame)
	c.mu.Lock()
	c.init()
	old := c.conn
	c.conn = ws
	c.connGen++
	gen := c.connGen
	c.mu.Unlock()
	if old != nil {
		_ = old.Close(websocket.StatusNormalClosure, "replaced")
	}
	c.Log.Info("chat uplink connected")
	defer func() {
		c.mu.Lock()
		if c.connGen == gen {
			c.conn = nil
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
		}
		c.mu.Unlock()
		_ = ws.CloseNow()
	}()
	ctx := r.Context()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var f runner.UplinkFrame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch {
		case f.Event == runner.EventDeliver:
			c.enqueue(f.Deliveries)
		case f.ID != 0:
			c.mu.Lock()
			ch := c.pending[f.ID]
			delete(c.pending, f.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		}
	}
}

func (c *Chat) enqueue(ds []runner.ChatDelivery) {
	c.mu.Lock()
	c.init()
	added := 0
	for _, d := range ds {
		if c.seen[d.ID] {
			continue
		}
		c.seen[d.ID] = true
		c.queue = append(c.queue, d)
		added++
	}
	c.mu.Unlock()
	if added > 0 {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// ErrNoUplink: the hub has not connected (the session is in no chat, or it
// just started).
var ErrNoUplink = errors.New("this session is not in a chat, or the hub has not connected yet; try again in a few seconds")

// Call sends a request to the hub and waits for the answer.
func (c *Chat) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.init()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, ErrNoUplink
	}
	c.nextID++
	id := c.nextID
	ch := make(chan runner.UplinkFrame, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	frame, _ := json.Marshal(runner.UplinkFrame{ID: id, Op: op, Args: b})
	c.wmu.Lock()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = conn.Write(wctx, websocket.MessageText, frame)
	cancel()
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ErrNoUplink
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return nil, ErrNoUplink
		}
		if !f.OK {
			return nil, errors.New(cmpOr(f.Error, "the hub refused"))
		}
		return f.Result, nil
	case <-time.After(60 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, errors.New("the hub did not answer in time")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Run delivers queued messages whenever the agent is idle, until ctx ends.
func (c *Chat) Run(ctx context.Context) {
	c.mu.Lock()
	c.init()
	wake := c.wake
	c.mu.Unlock()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
		if !c.hasQueue() || !c.idle(time.Now()) {
			continue
		}
		ds := c.take()
		if len(ds) == 0 {
			continue
		}
		prompt := c.prompt(ctx, ds)
		if err := c.typeIn(prompt); err != nil {
			c.Log.Warn("cannot type chat messages into the terminal", "err", err)
			c.requeue(ds)
			continue
		}
		c.mu.Lock()
		c.typedAt = time.Now()
		c.mu.Unlock()
		c.confirm(ctx, ds)
	}
}

func (c *Chat) hasQueue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue) > 0
}

func (c *Chat) take() []runner.ChatDelivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	ds := c.queue
	c.queue = nil
	return ds
}

func (c *Chat) requeue(ds []runner.ChatDelivery) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(ds, c.queue...)
}

func (c *Chat) confirm(ctx context.Context, ds []runner.ChatDelivery) {
	ids := make([]int64, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	if _, err := c.Call(ctx, runner.OpDelivered, map[string]any{"ids": ids}); err != nil {
		// The hub pushes them again on reconnect; seen keeps them from
		// being typed twice while this runner lives.
		c.Log.Warn("cannot confirm chat delivery", "err", err)
	}
}

// idle reports whether a prompt can be typed now: the agent runs, is
// waiting for input (its hooks say so, or the terminal has gone quiet on a
// prompt), and is not still on the last prompt typed.
func (c *Chat) idle(now time.Time) bool {
	quiet, running := c.Proc.Quiet(now)
	if !running {
		return false
	}
	c.mu.Lock()
	typedAt := c.typedAt
	c.mu.Unlock()
	if now.Sub(typedAt) < 8*time.Second {
		return false
	}
	if c.Hooks != nil {
		if a := c.Hooks.Current(); a != nil {
			if a.State != runner.ActivityWaitingInput {
				return false
			}
			// The agent must have taken a turn on what was typed last, or
			// that prompt never got through: then try again after a while.
			if !typedAt.IsZero() && !a.Since.After(typedAt) && now.Sub(typedAt) < 2*time.Minute {
				return false
			}
			return quiet > 1500*time.Millisecond
		}
	}
	long := c.QuietIdle
	if long <= 0 {
		long = 20 * time.Second
	}
	if quiet > long {
		return true
	}
	return quiet > min(3*time.Second, long) && c.Proc.Status(c.Agent, 0).NeedsAttention
}

// controlRE matches what must never be typed into the terminal from a
// chat: escape sequences and other control characters but newline and tab.
var controlRE = regexp.MustCompile("[\x00-\x08\x0b-\x1f\x7f]")

func sanitize(s string) string { return controlRE.ReplaceAllString(s, "") }

// typeIn types text into the agent's terminal and submits it: as one
// bracketed paste where the agent supports that (line breaks survive),
// else on one line.
func (c *Chat) typeIn(text string) error {
	text = sanitize(text)
	if c.Proc.BracketedPaste() {
		if err := c.Proc.Write([]byte("\x1b[200~" + text + "\x1b[201~")); err != nil {
			return err
		}
	} else if err := c.Proc.Write([]byte(strings.Join(strings.Fields(text), " "))); err != nil {
		return err
	}
	// TUIs take a moment to fold the paste in before Enter submits it.
	time.Sleep(300 * time.Millisecond)
	return c.Proc.Write([]byte("\r"))
}

// StopHook is called on Claude Code's Stop event: with messages queued, it
// returns the reason to block the stop with, so the agent continues with
// them right away instead of going idle.
func (c *Chat) StopHook() string {
	if !c.hasQueue() {
		return ""
	}
	ds := c.take()
	if len(ds) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	prompt := c.prompt(ctx, ds)
	c.mu.Lock()
	c.typedAt = time.Now()
	c.mu.Unlock()
	go func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		c.confirm(cctx, ds)
	}()
	return prompt
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// fetchFiles writes a message's images under ~/chats/<chat>/ (once) and
// fills in their paths.
func (c *Chat) fetchFiles(ctx context.Context, chatName string, m *runner.ChatMessage) {
	dir := filepath.Join(c.Home, "chats", strings.Trim(unsafeName.ReplaceAllString(chatName, "-"), "-"))
	for i := range m.Attachments {
		a := &m.Attachments[i]
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[a.ContentType]
		path := filepath.Join(dir, fmt.Sprintf("%d-%d%s", m.ID, i+1, ext))
		if _, err := os.Stat(path); err == nil {
			a.Path = path
			continue
		}
		raw, err := c.Call(ctx, runner.OpAttachment, map[string]string{"id": a.ID})
		if err != nil {
			c.Log.Warn("cannot fetch a chat image", "err", err)
			continue
		}
		var f runner.UplinkFile
		if json.Unmarshal(raw, &f) != nil {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			continue
		}
		a.Path = path
	}
}

// prompt is the text typed for a batch of deliveries.
func (c *Chat) prompt(ctx context.Context, ds []runner.ChatDelivery) string {
	var b strings.Builder
	b.WriteString("[agents-operator chat] ")
	if len(ds) == 1 {
		b.WriteString("A new chat message:\n")
	} else {
		fmt.Fprintf(&b, "%d new chat messages:\n", len(ds))
	}
	earlier := map[string]int{}
	answer := false
	for i := range ds {
		d := &ds[i]
		c.fetchFiles(ctx, d.ChatName, &d.ChatMessage)
		who := d.Author.Name
		if d.Author.Kind == "owner" {
			who += " (owner)"
		} else if d.Author.Handle != "" {
			who += " (@" + d.Author.Handle + ")"
		}
		var to string
		switch d.Address {
		case "direct":
			to = "addressed to you: please answer"
			answer = true
		case "everyone":
			to = "@everyone: everyone is asked to answer"
			answer = true
		default:
			to = "to everyone: answer only if you have something to add"
		}
		fmt.Fprintf(&b, "\n--- chat %q · message #%d · from %s · %s", d.ChatName, d.ID, who, to)
		if d.ReplyTo > 0 {
			fmt.Fprintf(&b, " · in reply to #%d", d.ReplyTo)
		}
		b.WriteString("\n")
		if d.Text != "" {
			b.WriteString(d.Text + "\n")
		}
		for _, a := range d.Attachments {
			if a.Path != "" {
				b.WriteString("[image: " + a.Path + "]\n")
			} else {
				b.WriteString("[an image that could not be fetched]\n")
			}
		}
		if d.Earlier > earlier[d.ChatName] {
			earlier[d.ChatName] = d.Earlier
		}
	}
	b.WriteString("\n")
	if answer {
		b.WriteString("Answer in the chat with the chat_send tool (reply_to the message id to reply to it). ")
	} else {
		b.WriteString("If you have something to add, post it with the chat_send tool; otherwise just carry on, there is no need to answer. ")
	}
	for name, n := range earlier {
		if n > 0 {
			fmt.Fprintf(&b, "%d earlier message(s) in %q were not sent to you; chat_read has them. ", n, name)
		}
	}
	b.WriteString("chat_info shows the chat's description and who is in it.")
	return b.String()
}

// ---- loopback API for `agent-runner mcp` ----

// Handler serves POST /chat/{op} on the loopback listener: the chat
// tools' requests, forwarded over the uplink.
func (c *Chat) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/{op}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		result, err := c.local(r.Context(), r.PathValue("op"), body)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{"result": result})
	})
	return mux
}

// local handles one tool call: the chat ops pass through, with images
// read from and written to disk here.
func (c *Chat) local(ctx context.Context, op string, body []byte) (json.RawMessage, error) {
	switch op {
	case runner.OpChatsList, runner.OpChatInfo:
		var args json.RawMessage = body
		if len(body) == 0 {
			args = json.RawMessage(`{}`)
		}
		return c.Call(ctx, op, args)
	case runner.OpChatRead:
		var a runner.ChatRefArgs
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, errors.New("bad arguments")
		}
		raw, err := c.Call(ctx, op, a)
		if err != nil {
			return nil, err
		}
		var res struct {
			Chat     json.RawMessage      `json:"chat"`
			Messages []runner.ChatMessage `json:"messages"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return raw, nil
		}
		var chat struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(res.Chat, &chat)
		for i := range res.Messages {
			if len(res.Messages[i].Attachments) > 0 {
				c.fetchFiles(ctx, chat.Name, &res.Messages[i])
			}
		}
		return json.Marshal(res)
	case runner.OpChatSend:
		var a struct {
			Chat    string   `json:"chat"`
			Text    string   `json:"text"`
			ReplyTo int64    `json:"reply_to"`
			Images  []string `json:"images"`
		}
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, errors.New("bad arguments")
		}
		send := runner.ChatSendArgs{Chat: a.Chat, Text: a.Text, ReplyTo: a.ReplyTo}
		for _, p := range a.Images {
			f, err := c.readImage(p)
			if err != nil {
				return nil, err
			}
			send.Images = append(send.Images, f)
		}
		return c.Call(ctx, op, send)
	default:
		return nil, fmt.Errorf("unknown chat operation %q", op)
	}
}

// readImage reads an image the agent wants to send: a file under one of
// Roots (after resolving links), at most 10 MiB, of an accepted type.
func (c *Chat) readImage(path string) (runner.UplinkFile, error) {
	var f runner.UplinkFile
	abs, err := filepath.Abs(path)
	if err != nil {
		return f, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return f, fmt.Errorf("cannot read %s: %w", path, err)
	}
	inside := false
	for _, root := range c.Roots {
		r, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(r, real); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			inside = true
			break
		}
	}
	if !inside {
		return f, fmt.Errorf("%s is outside the workspace; only files under %s can be sent", path, strings.Join(c.Roots, " or "))
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return f, fmt.Errorf("%s is not a file", path)
	}
	if st.Size() > 10<<20 {
		return f, fmt.Errorf("%s is larger than 10 MiB", path)
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return f, err
	}
	ct := http.DetectContentType(data)
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return f, fmt.Errorf("%s is %s; only PNG, JPEG, GIF and WebP images can be sent", path, ct)
	}
	return runner.UplinkFile{Name: filepath.Base(real), ContentType: ct, Data: data}, nil
}
