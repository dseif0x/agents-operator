package convo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ACP drives an agent that speaks the Agent Client Protocol
// (agentclientprotocol.com) over stdio: OpenCode (`opencode acp`).
type ACP struct {
	Cmd Command // the full command, "acp" included
	Dir string
	// Mode is the session mode to switch to (OpenCode: an agent definition).
	Mode string
	Log  *slog.Logger

	p      *proc
	events chan Event

	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan rpcResponse
	session   string
	canImage  bool
	turn      string
	turnSeq   int
	cancelled bool
	loading   bool // session/load replays history: ignore it
	textKey   string
	thinkKey  string
	keySeq    int
	tools     map[string]*Tool
	streamed  map[string]string // key -> kind of text streamed this turn
	usage     *Usage
}

// Start launches the agent and creates or loads the session.
func (a *ACP) Start(ctx context.Context, ref string) error {
	p, err := startProc(ctx, a.Cmd, a.Log)
	if err != nil {
		return err
	}
	a.p = p
	a.events = make(chan Event, 256)
	a.pending = map[int64]chan rpcResponse{}
	a.tools = map[string]*Tool{}
	a.streamed = map[string]string{}
	go a.read()

	ictx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := a.call(ictx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]any{"name": "agents-operator", "version": "1"},
	})
	if err != nil {
		a.Close()
		return fmt.Errorf("acp initialize: %w", err)
	}
	var init struct {
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(res, &init)
	a.canImage = init.AgentCapabilities.PromptCapabilities.Image

	if ref != "" && init.AgentCapabilities.LoadSession {
		a.mu.Lock()
		a.loading = true
		a.mu.Unlock()
		_, err = a.call(ictx, "session/load", map[string]any{"sessionId": ref, "cwd": a.Dir, "mcpServers": []any{}})
		a.mu.Lock()
		a.loading = false
		a.mu.Unlock()
		if err == nil {
			a.mu.Lock()
			a.session = ref
			a.mu.Unlock()
			a.setMode(ictx)
			a.emit(Event{Type: EventInfo, Info: &Info{Ref: ref}})
			return nil
		}
		a.Log.Warn("cannot load the ACP session; starting a new one", "session", ref, "err", err)
	}
	res, err = a.call(ictx, "session/new", map[string]any{"cwd": a.Dir, "mcpServers": []any{}})
	if err != nil {
		a.Close()
		return fmt.Errorf("acp session/new: %w", err)
	}
	var s struct {
		SessionID string `json:"sessionId"`
		Models    *struct {
			CurrentModelID string `json:"currentModelId"`
		} `json:"models"`
	}
	_ = json.Unmarshal(res, &s)
	a.mu.Lock()
	a.session = s.SessionID
	a.mu.Unlock()
	a.setMode(ictx)
	info := &Info{Ref: s.SessionID}
	if s.Models != nil {
		info.Model = s.Models.CurrentModelID
	}
	a.emit(Event{Type: EventInfo, Info: info})
	return nil
}

// setMode switches the session to Mode, if set.
func (a *ACP) setMode(ctx context.Context) {
	if a.Mode == "" {
		return
	}
	if _, err := a.call(ctx, "session/set_mode", map[string]any{"sessionId": a.session, "modeId": a.Mode}); err != nil {
		a.Log.Warn("cannot switch the session to the agent definition", "mode", a.Mode, "err", err)
	}
}

func (a *ACP) Events() <-chan Event { return a.events }
func (a *ACP) Wait() error          { return a.p.wait() }
func (a *ACP) Close() {
	if a.p != nil {
		a.p.stop()
	}
}

func (a *ACP) emit(ev Event) { a.events <- ev }

func (a *ACP) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	ch := make(chan rpcResponse, 1)
	a.pending[id] = ch
	a.mu.Unlock()
	if err := a.p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrNotRunning
		}
		return r.result, r.err
	case <-ctx.Done():
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Send starts a turn: session/prompt answers when the turn ends.
func (a *ACP) Send(in Input) error {
	a.mu.Lock()
	if a.turn != "" {
		a.mu.Unlock()
		return ErrBusy
	}
	a.turnSeq++
	a.turn = fmt.Sprintf("a%d", a.turnSeq)
	a.cancelled = false
	a.textKey, a.thinkKey = "", ""
	a.usage = nil
	turn, session, canImage := a.turn, a.session, a.canImage
	a.mu.Unlock()

	text := in.Text
	var blocks []map[string]any
	for _, f := range in.Files {
		typ := mime.TypeByExtension(strings.ToLower(filepath.Ext(f)))
		if canImage && strings.HasPrefix(typ, "image/") {
			if data, err := os.ReadFile(f); err == nil && len(data) <= 5<<20 {
				blocks = append(blocks, map[string]any{"type": "image", "mimeType": typ, "data": base64.StdEncoding.EncodeToString(data)})
				continue
			}
		}
		text += "\n[file: " + f + "]"
	}
	blocks = append([]map[string]any{{"type": "text", "text": text}}, blocks...)
	a.emit(Event{Type: EventTurnStarted, Turn: turn, Inputs: []string{in.ID}})
	go func() {
		res, err := a.call(context.Background(), "session/prompt", map[string]any{"sessionId": session, "prompt": blocks})
		if err != nil {
			a.endTurn(turn, StatusFailed, err.Error())
			return
		}
		var r struct {
			StopReason string `json:"stopReason"`
			Usage      *struct {
				Input  int64 `json:"inputTokens"`
				Output int64 `json:"outputTokens"`
				Cached int64 `json:"cachedReadTokens"`
				Total  int64 `json:"totalTokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(res, &r)
		if r.Usage != nil {
			a.mu.Lock()
			if a.usage == nil {
				a.usage = &Usage{}
			}
			a.usage.Input, a.usage.Output, a.usage.CacheRead = r.Usage.Input, r.Usage.Output, r.Usage.Cached
			a.mu.Unlock()
		}
		switch r.StopReason {
		case "cancelled":
			a.endTurn(turn, StatusInterrupted, "")
		case "refusal":
			a.endTurn(turn, StatusFailed, "the model refused")
		case "max_tokens":
			a.endTurn(turn, StatusFailed, "the answer hit the output limit")
		default:
			a.endTurn(turn, StatusDone, "")
		}
	}()
	return nil
}

// Interrupt sends session/cancel; the prompt then ends with "cancelled".
func (a *ACP) Interrupt() error {
	a.mu.Lock()
	session, turn := a.session, a.turn
	a.cancelled = true
	a.mu.Unlock()
	if turn == "" {
		return nil
	}
	return a.p.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": session}})
}

func (a *ACP) endTurn(turn, status, errText string) {
	a.mu.Lock()
	if a.turn != turn {
		a.mu.Unlock()
		return
	}
	if a.cancelled && status == StatusFailed {
		status, errText = StatusInterrupted, ""
	}
	a.turn = ""
	usage := a.usage
	// Tools still running when the turn ends are done or stopped.
	var open []Event
	for key, t := range a.tools {
		st := StatusDone
		if status != StatusDone {
			st = StatusInterrupted
		}
		open = append(open, Event{Type: EventItem, Turn: turn, Item: &Item{Key: key, Kind: KindTool, Status: st, Tool: t}})
		delete(a.tools, key)
	}
	for key, kind := range a.streamed {
		open = append(open, Event{Type: EventItem, Turn: turn, Item: &Item{Key: key, Kind: kind, Status: StatusDone}})
		delete(a.streamed, key)
	}
	a.mu.Unlock()
	for _, ev := range open {
		a.emit(ev)
	}
	a.emit(Event{Type: EventTurnEnded, Turn: turn, Status: status, Error: errText, Usage: usage})
}

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (a *ACP) read() {
	defer close(a.events)
	for line := range a.p.lines {
		var m acpMessage
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			a.clientRequest(m)
		case m.Method == "session/update":
			a.update(m.Params)
		case len(m.ID) > 0 && m.Method == "":
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			a.mu.Lock()
			ch := a.pending[id]
			delete(a.pending, id)
			a.mu.Unlock()
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
	a.mu.Lock()
	for id, ch := range a.pending {
		close(ch)
		delete(a.pending, id)
	}
	turn := a.turn
	a.mu.Unlock()
	err := a.p.wait()
	msg := "the agent CLI exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	if turn != "" {
		a.endTurn(turn, StatusFailed, msg)
	}
}

// clientRequest answers the agent's requests: permissions are granted
// (sessions run autonomously; phase 4 shows them as cards).
func (a *ACP) clientRequest(m acpMessage) {
	if m.Method == "session/request_permission" {
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(m.Params, &p)
		choice := ""
		for _, kind := range []string{"allow_always", "allow_once"} {
			for _, o := range p.Options {
				if o.Kind == kind && choice == "" {
					choice = o.OptionID
				}
			}
		}
		outcome := map[string]any{"outcome": "cancelled"}
		if choice != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": choice}
		}
		_ = a.p.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"outcome": outcome}})
		return
	}
	_ = a.p.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "not supported by agents-operator"}})
}

type acpContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Path    string `json:"path"`
	NewText string `json:"newText"`
	Content *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type acpUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	RawInput      json.RawMessage `json:"rawInput"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	Locations     []struct {
		Path string `json:"path"`
	} `json:"locations"`
	Entries []struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	} `json:"entries"`
	Used *int64 `json:"used"`
	Size *int64 `json:"size"`
}

func (a *ACP) update(raw json.RawMessage) {
	var p struct {
		Update acpUpdate `json:"update"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	u := p.Update
	a.mu.Lock()
	turn, loading := a.turn, a.loading
	a.mu.Unlock()
	if loading || turn == "" {
		return
	}
	switch u.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		var c acpContent
		if json.Unmarshal(u.Content, &c) != nil || c.Type != "text" || c.Text == "" {
			return
		}
		kind := KindText
		a.mu.Lock()
		key := &a.textKey
		if u.SessionUpdate == "agent_thought_chunk" {
			kind, key = KindThinking, &a.thinkKey
		}
		if *key == "" {
			a.keySeq++
			*key = fmt.Sprintf("%s-%s%d", turn, kind[:2], a.keySeq)
		}
		k := *key
		a.streamed[k] = kind
		// A thought ends the answer text before it and vice versa.
		if kind == KindText {
			a.thinkKey = ""
		} else {
			a.textKey = ""
		}
		a.mu.Unlock()
		a.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: k, Kind: kind, Status: StatusRunning, Text: c.Text, Delta: true}})
	case "tool_call", "tool_call_update":
		a.mu.Lock()
		a.textKey, a.thinkKey = "", ""
		t := a.tools[u.ToolCallID]
		if t == nil {
			t = &Tool{Name: u.Kind, Kind: acpToolKind(u.Kind)}
			a.tools[u.ToolCallID] = t
		}
		if u.Title != "" {
			t.Title = u.Title
		}
		if u.Kind != "" {
			t.Name, t.Kind = u.Kind, acpToolKind(u.Kind)
		}
		if len(u.RawInput) > 0 && string(u.RawInput) != "{}" && string(u.RawInput) != "null" {
			t.Input = trim(string(u.RawInput))
		}
		if out := acpToolOutput(u.Content); out != "" {
			t.Output = trim(out)
		} else if len(u.RawOutput) > 0 && string(u.RawOutput) != "null" {
			t.Output = trim(rawOutputText(u.RawOutput))
		}
		status := StatusRunning
		switch u.Status {
		case "completed":
			status = StatusDone
		case "failed":
			status, t.Error = StatusFailed, true
		}
		cp := *t
		if status != StatusRunning {
			delete(a.tools, u.ToolCallID)
		}
		a.mu.Unlock()
		a.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: u.ToolCallID, Kind: KindTool, Status: status, Tool: &cp}})
	case "plan":
		var b strings.Builder
		for _, e := range u.Entries {
			mark := "[ ]"
			switch e.Status {
			case "completed":
				mark = "[x]"
			case "in_progress":
				mark = "[~]"
			}
			fmt.Fprintf(&b, "%s %s\n", mark, e.Content)
		}
		a.emit(Event{Type: EventItem, Turn: turn, Item: &Item{Key: turn + "-plan", Kind: KindTool, Status: StatusDone, Tool: &Tool{Name: "plan", Kind: ToolTask, Title: "Updated the plan", Output: b.String()}}})
	case "usage_update":
		if u.Used != nil {
			a.mu.Lock()
			if a.usage == nil {
				a.usage = &Usage{}
			}
			a.usage.Context = *u.Used
			if u.Size != nil {
				a.usage.ContextWindow = *u.Size
			}
			a.mu.Unlock()
		}
	}
}

func acpToolKind(kind string) string {
	switch kind {
	case "execute":
		return ToolCommand
	case "edit", "delete", "move":
		return ToolEdit
	case "read":
		return ToolRead
	case "search":
		return ToolSearch
	case "fetch":
		return ToolWeb
	case "think":
		return ToolTask
	}
	return ToolOther
}

// acpToolOutput is the text of a tool call's content.
func acpToolOutput(raw json.RawMessage) string {
	var cs []acpContent
	if json.Unmarshal(raw, &cs) != nil {
		return ""
	}
	var parts []string
	for _, c := range cs {
		switch c.Type {
		case "content":
			if c.Content != nil && c.Content.Type == "text" {
				parts = append(parts, c.Content.Text)
			}
		case "diff":
			parts = append(parts, "--- "+c.Path+"\n"+c.NewText)
		}
	}
	return strings.Join(parts, "\n")
}

// rawOutputText makes a tool's raw output readable: its "output" field
// when it has one, else the JSON.
func rawOutputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"output", "stdout", "result", "text"} {
			if v, ok := m[k].(string); ok {
				return v
			}
		}
	}
	return string(raw)
}
