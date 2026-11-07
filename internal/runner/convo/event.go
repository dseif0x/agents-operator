// Package convo drives an agent CLI through its structured protocol instead
// of a terminal (docs/design/conversations.md): Claude Code's stream-json,
// Codex's app-server and OpenCode's Agent Client Protocol. Every driver
// turns its protocol into the same events, which the runner sends to the
// hub.
package convo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// Event types.
const (
	EventTurnStarted = "turn_started"
	EventItem        = "item"
	EventTurnEnded   = "turn_ended"
	EventInfo        = "info"
)

// Item kinds.
const (
	KindText     = "text"     // the agent's answer
	KindThinking = "thinking" // its reasoning, when the CLI shows it
	KindTool     = "tool"     // a command, file change or other tool call
	KindError    = "error"
	KindNotice   = "notice" // compaction, a model change, a retry
	KindUser     = "user"   // what the agent was given
)

// Item and turn states.
const (
	StatusRunning     = "running"
	StatusDone        = "done"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// Tool kinds, for the icon and how the UI shows input and output.
const (
	ToolCommand = "command" // a shell command: Input is the command line
	ToolEdit    = "edit"    // a file written or changed: Input is the path
	ToolRead    = "read"    // a file read or listed
	ToolSearch  = "search"  // grep, glob, web search
	ToolWeb     = "web"     // a fetched page
	ToolTask    = "task"    // a sub-agent or a task list update
	ToolMCP     = "mcp"     // an MCP tool
	ToolOther   = "other"
)

// Event is one thing that happened in the conversation.
type Event struct {
	Type string `json:"type"`
	// Turn is the turn's id, set on everything but info.
	Turn string `json:"turn,omitempty"`
	// Inputs are the ids of the inputs a turn started with (turn_started).
	Inputs []string `json:"inputs,omitempty"`
	Item   *Item    `json:"item,omitempty"`
	// Status and Error end a turn (turn_ended).
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
	Usage  *Usage `json:"usage,omitempty"`
	Info   *Info  `json:"info,omitempty"`
}

// Item is one part of an answer, sent when it starts, while it grows and
// when it ends. The hub keeps the latest state per Key.
type Item struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// Text is the item's text; with Delta it is appended to what came
	// before (streaming), else it replaces it.
	Text  string `json:"text,omitempty"`
	Delta bool   `json:"delta,omitempty"`
	// Parent is the key of the tool (a sub-agent) this item belongs to.
	Parent string `json:"parent,omitempty"`
	Tool   *Tool  `json:"tool,omitempty"`
	// Source is where a user item came from (SourceChat, ...).
	Source string `json:"source,omitempty"`
}

// Tool describes a tool call.
type Tool struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Title is a one-line summary for the steps list ("npm test",
	// "src/app.ts", "grep TODO").
	Title string `json:"title,omitempty"`
	// Input is the call's arguments, as JSON or as the command line.
	Input string `json:"input,omitempty"`
	// Output is the result, trimmed to MaxOutput.
	Output string `json:"output,omitempty"`
	Error  bool   `json:"error,omitempty"`
}

// MaxOutput bounds a tool's output and an item's input as sent to the hub.
const MaxOutput = 16 << 10

// Usage is a turn's token count and the context in use after it.
type Usage struct {
	Input         int64   `json:"input,omitempty"`
	Output        int64   `json:"output,omitempty"`
	CacheRead     int64   `json:"cache_read,omitempty"`
	Context       int64   `json:"context,omitempty"`
	ContextWindow int64   `json:"context_window,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
}

// Info is what the CLI told about itself.
type Info struct {
	Model         string `json:"model,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	// Ref is the CLI's own id of the conversation, used to resume it.
	Ref string `json:"ref,omitempty"`
}

// Input is something for the agent to work on.
type Input struct {
	ID   string
	Text string
	// Files are paths of files that come with the message; images are
	// passed to CLIs that take them, the rest are named in the text.
	Files []string
}

// Driver speaks one CLI's protocol.
type Driver interface {
	// Start launches the CLI. ref resumes an earlier conversation; "" starts
	// a new one.
	Start(ctx context.Context, ref string) error
	// Send starts a turn. Only call it between turns.
	Send(in Input) error
	// Interrupt stops the running turn; it ends with StatusInterrupted.
	Interrupt() error
	// Events delivers the conversation; it is closed when the CLI exits.
	Events() <-chan Event
	// Wait waits for the CLI to exit.
	Wait() error
	// Close stops the CLI.
	Close()
}

// trim cuts s to MaxOutput, keeping the start and the end.
func trim(s string) string {
	if len(s) <= MaxOutput {
		return s
	}
	half := MaxOutput/2 - 32
	return s[:half] + "\n… (" + itoa(len(s)-2*half) + " bytes left out) …\n" + s[len(s)-half:]
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// randKey is a short random id for items the CLI gives none.
func randKey() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
