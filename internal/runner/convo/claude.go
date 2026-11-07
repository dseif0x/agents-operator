package convo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Claude drives Claude Code in print mode with stream-json on both sides:
// one long-lived process that takes user messages as JSON lines and writes
// every message, partial message and result as JSON lines.
type Claude struct {
	Cmd Command // claude with the session's flags (persona, MCP config, ...)
	Log *slog.Logger

	p      *proc
	events chan Event

	mu          sync.Mutex
	turn        string
	turnSeq     int
	interrupted bool
	ctrlSeq     int
	// Streaming state of the current API message.
	msgID   string
	blocks  map[int]*claudeBlock // by stream index
	matched map[string]int       // msgID -> blocks matched to full messages
	order   map[string][]int     // msgID -> stream indexes in order
	context int64
	window  int64
}

type claudeBlock struct {
	key  string
	kind string
	used bool
}

// Start launches claude; ref resumes an earlier session.
func (c *Claude) Start(ctx context.Context, ref string) error {
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}, c.Cmd.Args...)
	if ref != "" {
		args = append(args, "--resume", ref)
	}
	cmd := c.Cmd
	cmd.Args = args
	p, err := startProc(ctx, cmd, c.Log)
	if err != nil {
		return err
	}
	c.p = p
	c.events = make(chan Event, 256)
	c.blocks = map[int]*claudeBlock{}
	c.matched = map[string]int{}
	c.order = map[string][]int{}
	go c.read()
	return nil
}

func (c *Claude) Events() <-chan Event { return c.events }
func (c *Claude) Wait() error          { return c.p.wait() }
func (c *Claude) Close() {
	if c.p != nil {
		c.p.stop()
	}
}

// Send writes a user message; the turn starts at once.
func (c *Claude) Send(in Input) error {
	c.mu.Lock()
	if c.turn != "" {
		c.mu.Unlock()
		return ErrBusy
	}
	c.turnSeq++
	c.turn = fmt.Sprintf("c%d", c.turnSeq)
	c.interrupted = false
	turn := c.turn
	c.mu.Unlock()

	content := []map[string]any{}
	text := in.Text
	for _, f := range in.Files {
		if block := imageBlock(f); block != nil {
			content = append(content, block)
		} else {
			text += "\n[file: " + f + "]"
		}
	}
	content = append([]map[string]any{{"type": "text", "text": text}}, content...)
	c.emit(Event{Type: EventTurnStarted, Turn: turn, Inputs: []string{in.ID}})
	err := c.p.send(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		c.endTurn(StatusFailed, "cannot write to claude: "+err.Error(), nil)
	}
	return err
}

// imageBlock is an image content block for path, or nil for other files.
func imageBlock(path string) map[string]any {
	typ := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	switch typ {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 5<<20 {
		return nil
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": typ, "data": base64.StdEncoding.EncodeToString(data)}}
}

// Interrupt asks claude to stop the turn.
func (c *Claude) Interrupt() error {
	c.mu.Lock()
	if c.turn == "" {
		c.mu.Unlock()
		return nil
	}
	c.interrupted = true
	c.ctrlSeq++
	id := fmt.Sprintf("int-%d", c.ctrlSeq)
	c.mu.Unlock()
	return c.p.send(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "interrupt"}})
}

func (c *Claude) emit(ev Event) { c.events <- ev }

func (c *Claude) currentTurn() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turn
}

func (c *Claude) endTurn(status, errText string, usage *Usage) {
	c.mu.Lock()
	turn := c.turn
	if c.interrupted && status != StatusDone {
		status = StatusInterrupted
		errText = ""
	}
	c.turn = ""
	c.mu.Unlock()
	if turn != "" {
		c.emit(Event{Type: EventTurnEnded, Turn: turn, Status: status, Error: errText, Usage: usage})
	}
}

type claudeLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Model     string          `json:"model"`
	Parent    *string         `json:"parent_tool_use_id"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	// result
	IsError    bool                       `json:"is_error"`
	Result     string                     `json:"result"`
	Errors     []string                   `json:"errors"`
	Usage      *claudeUsage               `json:"usage"`
	CostUSD    float64                    `json:"total_cost_usd"`
	ModelUsage map[string]claudeModelInfo `json:"modelUsage"`
}

type claudeUsage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
}

type claudeModelInfo struct {
	ContextWindow int64 `json:"contextWindow"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *claudeUsage    `json:"usage"`
}

type claudeBlockJSON struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		ID string `json:"id"`
	} `json:"message"`
	ContentBlock claudeBlockJSON `json:"content_block"`
	Delta        struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
}

func (c *Claude) read() {
	defer close(c.events)
	for line := range c.p.lines {
		var l claudeLine
		if err := json.Unmarshal(line, &l); err != nil {
			continue
		}
		c.handle(l)
	}
	err := c.p.wait()
	msg := "the agent CLI exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	c.endTurn(StatusFailed, msg, nil)
}

func (c *Claude) handle(l claudeLine) {
	parent := ""
	if l.Parent != nil {
		parent = *l.Parent
	}
	switch l.Type {
	case "system":
		switch l.Subtype {
		case "init":
			c.emit(Event{Type: EventInfo, Info: &Info{Model: l.Model, Ref: l.SessionID}})
		case "compact_boundary":
			if turn := c.currentTurn(); turn != "" {
				c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: "compact-" + randKey(), Kind: KindNotice, Status: StatusDone, Text: "Context compacted"}})
			}
		}
	case "stream_event":
		if parent != "" {
			return // sub-agents show up as full messages
		}
		var ev claudeStreamEvent
		if json.Unmarshal(l.Event, &ev) != nil {
			return
		}
		c.stream(ev)
	case "assistant":
		var m claudeMessage
		if json.Unmarshal(l.Message, &m) != nil {
			return
		}
		c.assistant(m, parent)
	case "user":
		var m claudeMessage
		if json.Unmarshal(l.Message, &m) != nil {
			return
		}
		c.toolResults(m, parent)
	case "result":
		usage := &Usage{CostUSD: l.CostUSD}
		if l.Usage != nil {
			usage.Input = l.Usage.Input + l.Usage.CacheCreation
			usage.Output = l.Usage.Output
			usage.CacheRead = l.Usage.CacheRead
		}
		c.mu.Lock()
		for _, mu := range l.ModelUsage {
			if mu.ContextWindow > c.window {
				c.window = mu.ContextWindow
			}
		}
		usage.Context, usage.ContextWindow = c.context, c.window
		c.mu.Unlock()
		if l.IsError || strings.HasPrefix(l.Subtype, "error") {
			msg := l.Result
			if msg == "" {
				msg = strings.Join(l.Errors, "; ")
			}
			if msg == "" {
				msg = strings.ReplaceAll(l.Subtype, "_", " ")
			}
			c.endTurn(StatusFailed, msg, usage)
			return
		}
		c.endTurn(StatusDone, "", usage)
	}
}

// stream handles a partial message event of the main conversation.
func (c *Claude) stream(ev claudeStreamEvent) {
	turn := c.currentTurn()
	if turn == "" {
		return
	}
	switch ev.Type {
	case "message_start":
		c.mu.Lock()
		c.msgID = ev.Message.ID
		c.blocks = map[int]*claudeBlock{}
		c.mu.Unlock()
	case "content_block_start":
		b := ev.ContentBlock
		c.mu.Lock()
		msg := c.msgID
		var blk *claudeBlock
		switch b.Type {
		case "text":
			blk = &claudeBlock{key: fmt.Sprintf("%s:%d", msg, ev.Index), kind: KindText}
		case "thinking":
			blk = &claudeBlock{key: fmt.Sprintf("%s:%d", msg, ev.Index), kind: KindThinking}
		case "tool_use", "server_tool_use":
			blk = &claudeBlock{key: b.ID, kind: KindTool}
		}
		if blk != nil {
			c.blocks[ev.Index] = blk
			c.order[msg] = append(c.order[msg], ev.Index)
		}
		c.mu.Unlock()
		if blk == nil {
			return
		}
		item := &Item{Key: blk.key, Kind: blk.kind, Status: StatusRunning}
		if blk.kind == KindTool {
			item.Tool = &Tool{Name: b.Name, Kind: claudeToolKind(b.Name)}
		}
		c.emit(Event{Type: EventItem, Turn: turn, Item: item})
	case "content_block_delta":
		c.mu.Lock()
		blk := c.blocks[ev.Index]
		c.mu.Unlock()
		if blk == nil {
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: blk.key, Kind: blk.kind, Status: StatusRunning, Text: ev.Delta.Text, Delta: true}})
		case "thinking_delta":
			c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: blk.key, Kind: blk.kind, Status: StatusRunning, Text: ev.Delta.Thinking, Delta: true}})
		}
	}
}

// keyFor finds the streamed block a block of a full message belongs to.
func (c *Claude) keyFor(msg string, kind string, pos int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg == c.msgID {
		for _, idx := range c.order[msg] {
			b := c.blocks[idx]
			if b != nil && !b.used && b.kind == kind {
				b.used = true
				return b.key
			}
		}
	}
	return fmt.Sprintf("%s:f%d", msg, pos)
}

// assistant handles a full assistant message (Claude Code writes one per
// content block, with the API message's id).
func (c *Claude) assistant(m claudeMessage, parent string) {
	turn := c.currentTurn()
	if turn == "" {
		return
	}
	if parent == "" && m.Usage != nil {
		c.mu.Lock()
		c.context = m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheCreation + m.Usage.Output
		c.mu.Unlock()
	}
	var blocks []claudeBlockJSON
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		c.mu.Lock()
		c.matched[m.ID]++
		pos := c.matched[m.ID]
		c.mu.Unlock()
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			key := fmt.Sprintf("%s:p%d", m.ID, pos)
			if parent == "" {
				key = c.keyFor(m.ID, KindText, pos)
			}
			c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: key, Kind: KindText, Status: StatusDone, Text: b.Text, Parent: parent}})
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			key := fmt.Sprintf("%s:p%d", m.ID, pos)
			if parent == "" {
				key = c.keyFor(m.ID, KindThinking, pos)
			}
			c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: key, Kind: KindThinking, Status: StatusDone, Text: b.Thinking, Parent: parent}})
		case "tool_use", "server_tool_use":
			if parent == "" {
				c.keyFor(m.ID, KindTool, pos) // mark the streamed block used
			}
			c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: b.ID, Kind: KindTool, Status: StatusRunning, Parent: parent, Tool: claudeTool(b.Name, b.Input)}})
		}
	}
}

// toolResults handles tool results (a "user" message from the CLI).
func (c *Claude) toolResults(m claudeMessage, parent string) {
	turn := c.currentTurn()
	if turn == "" {
		return
	}
	var blocks []claudeBlockJSON
	if json.Unmarshal(m.Content, &blocks) != nil {
		return // a plain string: the CLI echoing a prompt
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		status := StatusDone
		if b.IsError {
			status = StatusFailed
		}
		c.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: b.ToolUseID, Kind: KindTool, Status: status, Parent: parent, Tool: &Tool{Output: trim(resultText(b.Content)), Error: b.IsError}}})
	}
}

// resultText flattens a tool result's content (a string or blocks).
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeBlockJSON
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

func claudeToolKind(name string) string {
	switch name {
	case "Bash", "BashOutput", "KillShell", "KillBash":
		return ToolCommand
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return ToolEdit
	case "Read", "LS", "NotebookRead":
		return ToolRead
	case "Grep", "Glob", "WebSearch", "ToolSearch":
		return ToolSearch
	case "WebFetch":
		return ToolWeb
	case "Task", "Agent", "TodoWrite":
		return ToolTask
	}
	if strings.HasPrefix(name, "mcp__") {
		return ToolMCP
	}
	return ToolOther
}

// claudeTool describes a tool call from its name and input.
func claudeTool(name string, input json.RawMessage) *Tool {
	t := &Tool{Name: name, Kind: claudeToolKind(name), Input: trim(string(input))}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch name {
	case "Bash":
		t.Title, t.Input = str("command"), trim(str("command"))
	case "Edit", "MultiEdit", "Write", "Read", "NotebookEdit":
		t.Title = firstNonEmpty(str("file_path"), str("notebook_path"))
	case "Grep", "Glob":
		t.Title = strings.TrimSpace(str("pattern") + " " + str("path"))
	case "WebFetch":
		t.Title = str("url")
	case "WebSearch":
		t.Title = str("query")
	case "Task", "Agent":
		t.Title = firstNonEmpty(str("description"), str("subagent_type"))
	case "TodoWrite":
		t.Title = "Updated the task list"
	default:
		if strings.HasPrefix(name, "mcp__") {
			parts := strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)
			if len(parts) == 2 {
				t.Title = parts[0] + ": " + parts[1]
			}
		}
	}
	return t
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
