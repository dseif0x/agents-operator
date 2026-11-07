package convo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Codex drives `codex app-server`: JSON-RPC 2.0 over stdio (without the
// "jsonrpc" member), one thread per conversation.
type Codex struct {
	Cmd Command // codex with the session's -c overrides; "app-server" is added
	Dir string  // the thread's working directory
	Log *slog.Logger

	p      *proc
	events chan Event

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan rpcResponse
	thread   string
	turn     string // our turn id
	codexTID string // codex's id of the running turn
	turnSeq  int
	window   int64
	context  int64
	usage    *Usage
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

// Start launches the app server, initializes it and starts or resumes the
// thread.
func (c *Codex) Start(ctx context.Context, ref string) error {
	cmd := c.Cmd
	cmd.Args = append(append([]string{}, c.Cmd.Args...), "app-server")
	p, err := startProc(ctx, cmd, c.Log)
	if err != nil {
		return err
	}
	c.p = p
	c.events = make(chan Event, 256)
	c.pending = map[int64]chan rpcResponse{}
	go c.read()

	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := c.call(ictx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agents-operator", "title": "agents-operator", "version": "1"},
		"capabilities": nil,
	}); err != nil {
		c.Close()
		return fmt.Errorf("codex initialize: %w", err)
	}
	_ = c.p.send(map[string]any{"method": "initialized"})

	params := map[string]any{"cwd": c.Dir, "approvalPolicy": "never", "sandbox": "danger-full-access"}
	var res json.RawMessage
	if ref != "" {
		params["threadId"] = ref
		params["excludeTurns"] = true
		res, err = c.call(ictx, "thread/resume", params)
		if err != nil {
			c.Log.Warn("cannot resume the codex thread; starting a new one", "thread", ref, "err", err)
			delete(params, "threadId")
			delete(params, "excludeTurns")
		}
	}
	if ref == "" || err != nil {
		res, err = c.call(ictx, "thread/start", params)
		if err != nil {
			c.Close()
			return fmt.Errorf("codex thread/start: %w", err)
		}
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	_ = json.Unmarshal(res, &started)
	c.mu.Lock()
	c.thread = started.Thread.ID
	c.mu.Unlock()
	c.emit(Event{Type: EventInfo, Info: &Info{Model: started.Model, Ref: started.Thread.ID}})
	return nil
}

func (c *Codex) Events() <-chan Event { return c.events }
func (c *Codex) Wait() error          { return c.p.wait() }
func (c *Codex) Close() {
	if c.p != nil {
		c.p.stop()
	}
}

func (c *Codex) emit(ev Event) { c.events <- ev }

// call sends a request and waits for its response.
func (c *Codex) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.p.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrNotRunning
		}
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Send starts a turn.
func (c *Codex) Send(in Input) error {
	c.mu.Lock()
	if c.turn != "" {
		c.mu.Unlock()
		return ErrBusy
	}
	c.turnSeq++
	c.turn = fmt.Sprintf("x%d", c.turnSeq)
	c.codexTID = ""
	c.usage = nil
	turn, thread := c.turn, c.thread
	c.mu.Unlock()

	text := in.Text
	input := []map[string]any{}
	for _, f := range in.Files {
		switch mime.TypeByExtension(strings.ToLower(filepath.Ext(f))) {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			input = append(input, map[string]any{"type": "localImage", "path": f})
		default:
			text += "\n[file: " + f + "]"
		}
	}
	input = append([]map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}, input...)
	c.emit(Event{Type: EventTurnStarted, Turn: turn, Inputs: []string{in.ID}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.call(ctx, "turn/start", map[string]any{"threadId": thread, "input": input})
	if err != nil {
		c.endTurn(StatusFailed, "codex refused the message: "+err.Error())
		return err
	}
	var r struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(res, &r) == nil && r.Turn.ID != "" {
		c.mu.Lock()
		if c.turn == turn && c.codexTID == "" {
			c.codexTID = r.Turn.ID
		}
		c.mu.Unlock()
	}
	return nil
}

// Interrupt stops the running turn.
func (c *Codex) Interrupt() error {
	c.mu.Lock()
	thread, tid, turn := c.thread, c.codexTID, c.turn
	c.mu.Unlock()
	if turn == "" || tid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": tid})
	return err
}

func (c *Codex) currentTurn() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turn
}

func (c *Codex) endTurn(status, errText string) {
	c.mu.Lock()
	turn := c.turn
	c.turn, c.codexTID = "", ""
	usage := c.usage
	if usage == nil {
		usage = &Usage{}
	}
	usage.Context, usage.ContextWindow = c.context, c.window
	c.mu.Unlock()
	if turn != "" {
		c.emit(Event{Type: EventTurnEnded, Turn: turn, Status: status, Error: errText, Usage: usage})
	}
}

func (c *Codex) read() {
	defer close(c.events)
	for line := range c.p.lines {
		var m rpcMessage
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			c.serverRequest(m)
		case m.Method != "":
			c.notification(m.Method, m.Params)
		case len(m.ID) > 0:
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if m.Error != nil {
				ch <- rpcResponse{err: errors.New(m.Error.Message)}
			} else {
				ch <- rpcResponse{result: m.Result}
			}
		}
	}
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	err := c.p.wait()
	msg := "the agent CLI exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	c.endTurn(StatusFailed, msg)
}

// serverRequest answers what the app server asks: sessions run
// autonomously, so approvals are granted (phase 4 shows them as cards).
func (c *Codex) serverRequest(m rpcMessage) {
	var result any
	switch m.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "applyPatchApproval", "execCommandApproval":
		result = map[string]any{"decision": "accept"}
	case "item/tool/requestUserInput":
		result = map[string]any{"answers": map[string]any{}}
	default:
		_ = c.p.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "not supported by agents-operator"}})
		return
	}
	_ = c.p.send(map[string]any{"id": m.ID, "result": result})
}

type codexItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Text             string          `json:"text"`
	Summary          []string        `json:"summary"`
	Content          []string        `json:"content"`
	Command          string          `json:"command"`
	Status           string          `json:"status"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Changes          []codexChange   `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query string `json:"query"`
}

type codexChange struct {
	Path string          `json:"path"`
	Kind json.RawMessage `json:"kind"`
	Diff string          `json:"diff"`
}

func (c *Codex) notification(method string, params json.RawMessage) {
	turn := c.currentTurn()
	switch method {
	case "turn/started":
		var n struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &n)
		c.mu.Lock()
		if c.turn != "" {
			c.codexTID = n.Turn.ID
		}
		c.mu.Unlock()
	case "turn/completed":
		var n struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &n)
		switch n.Turn.Status {
		case "interrupted":
			c.endTurn(StatusInterrupted, "")
		case "failed":
			msg := "the turn failed"
			if n.Turn.Error != nil && n.Turn.Error.Message != "" {
				msg = n.Turn.Error.Message
			}
			c.endTurn(StatusFailed, msg)
		default:
			c.endTurn(StatusDone, "")
		}
	case "thread/tokenUsage/updated":
		var n struct {
			TokenUsage struct {
				Last struct {
					Total      int64 `json:"totalTokens"`
					Input      int64 `json:"inputTokens"`
					Cached     int64 `json:"cachedInputTokens"`
					CacheWrite int64 `json:"cacheWriteInputTokens"`
					Output     int64 `json:"outputTokens"`
				} `json:"last"`
				Window *int64 `json:"modelContextWindow"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &n) != nil {
			return
		}
		l := n.TokenUsage.Last
		c.mu.Lock()
		if n.TokenUsage.Window != nil {
			c.window = *n.TokenUsage.Window
		}
		c.context = l.Total
		if c.usage == nil {
			c.usage = &Usage{}
		}
		c.usage.Input += l.Input - l.Cached
		c.usage.CacheRead += l.Cached
		c.usage.Output += l.Output
		c.mu.Unlock()
	case "item/agentMessage/delta", "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		if turn == "" {
			return
		}
		var n struct {
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(params, &n) != nil {
			return
		}
		kind := KindText
		if method != "item/agentMessage/delta" {
			kind = KindThinking
		}
		c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: n.ItemID, Kind: kind, Status: StatusRunning, Text: n.Delta, Delta: true}})
	case "item/started", "item/completed":
		if turn == "" {
			return
		}
		var n struct {
			Item codexItem `json:"item"`
		}
		if json.Unmarshal(params, &n) != nil {
			return
		}
		if item := codexToItem(n.Item, method == "item/completed"); item != nil {
			c.emit(Event{Type: EventItem, Turn: turn, Item: item})
		}
	case "error":
		var n struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(params, &n) != nil || turn == "" {
			return
		}
		kind := KindError
		if n.WillRetry {
			kind = KindNotice
		}
		c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: "err-" + randKey(), Kind: kind, Status: StatusDone, Text: n.Error.Message}})
	}
}

// codexToItem converts a thread item; nil for items not shown.
func codexToItem(it codexItem, done bool) *Item {
	status := StatusRunning
	if done {
		status = StatusDone
	}
	switch it.Type {
	case "agentMessage":
		if done && strings.TrimSpace(it.Text) == "" {
			return nil
		}
		return &Item{Key: it.ID, Kind: KindText, Status: status, Text: it.Text}
	case "reasoning":
		text := strings.Join(it.Summary, "\n\n")
		if len(it.Content) > 0 {
			text = strings.Join(it.Content, "\n\n")
		}
		if done && strings.TrimSpace(text) == "" {
			return nil
		}
		return &Item{Key: it.ID, Kind: KindThinking, Status: status, Text: text}
	case "plan":
		return &Item{Key: it.ID, Kind: KindTool, Status: status, Tool: &Tool{Name: "plan", Kind: ToolTask, Title: "Updated the plan", Output: trim(it.Text)}}
	case "commandExecution":
		cmd := unwrapShell(it.Command)
		t := &Tool{Name: "shell", Kind: ToolCommand, Title: cmd, Input: trim(cmd)}
		if it.AggregatedOutput != nil {
			t.Output = trim(*it.AggregatedOutput)
		}
		if done && (it.Status == "failed" || it.Status == "declined" || (it.ExitCode != nil && *it.ExitCode != 0)) {
			status, t.Error = StatusFailed, true
		}
		return &Item{Key: it.ID, Kind: KindTool, Status: status, Tool: t}
	case "fileChange":
		paths := make([]string, 0, len(it.Changes))
		var diff strings.Builder
		for _, ch := range it.Changes {
			paths = append(paths, ch.Path)
			diff.WriteString(ch.Diff)
		}
		t := &Tool{Name: "apply_patch", Kind: ToolEdit, Title: strings.Join(paths, ", "), Input: trim(diff.String())}
		if done && (it.Status == "failed" || it.Status == "declined") {
			status, t.Error = StatusFailed, true
		}
		return &Item{Key: it.ID, Kind: KindTool, Status: status, Tool: t}
	case "mcpToolCall", "dynamicToolCall":
		t := &Tool{Name: it.Tool, Kind: ToolMCP, Title: strings.TrimPrefix(it.Server+": "+it.Tool, ": "), Input: trim(string(it.Arguments))}
		if len(it.Result) > 0 && string(it.Result) != "null" {
			t.Output = trim(mcpResultText(it.Result))
		}
		if it.Error != nil {
			status, t.Error, t.Output = StatusFailed, true, it.Error.Message
		}
		return &Item{Key: it.ID, Kind: KindTool, Status: status, Tool: t}
	case "webSearch":
		return &Item{Key: it.ID, Kind: KindTool, Status: status, Tool: &Tool{Name: "web_search", Kind: ToolSearch, Title: it.Query}}
	case "contextCompaction":
		if !done {
			return nil
		}
		return &Item{Key: it.ID, Kind: KindNotice, Status: StatusDone, Text: "Context compacted"}
	}
	return nil
}

// mcpResultText is the text content of an MCP tool result.
func mcpResultText(raw json.RawMessage) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return string(raw)
	}
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "["+c.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// unwrapShell turns "/bin/bash -lc 'cmd'" into "cmd".
func unwrapShell(c string) string {
	for _, prefix := range []string{"/bin/bash -lc ", "bash -lc ", "/bin/sh -c ", "sh -c ", "/usr/bin/bash -lc ", "/bin/zsh -lc "} {
		rest, ok := strings.CutPrefix(c, prefix)
		if !ok {
			continue
		}
		if len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'' {
			return strings.ReplaceAll(rest[1:len(rest)-1], `'\''`, "'")
		}
		if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
			return rest[1 : len(rest)-1]
		}
		return rest
	}
	return c
}
