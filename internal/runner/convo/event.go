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
	// EventRequest: the agent waits for the owner (a permission or a
	// question); Respond answers it.
	EventRequest = "request"
	// EventRequestDone: a request was answered or is moot.
	EventRequestDone = "request_done"
	// EventBackground: the work the agent left running in the background
	// (Background is the whole list now).
	EventBackground = "background"
)

// Background is one thing the agent runs in the background: a command, a
// watcher, a sub-agent.
type Background struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

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
	Status  string   `json:"status,omitempty"`
	Error   string   `json:"error,omitempty"`
	Usage   *Usage   `json:"usage,omitempty"`
	Info    *Info    `json:"info,omitempty"`
	Request *Request `json:"request,omitempty"`
	// Background is set (possibly empty) on background events.
	Background []Background `json:"background,omitempty"`
}

// Request kinds.
const (
	RequestPermission = "permission" // may the agent use a tool?
	RequestQuestion   = "question"   // the agent asks the owner something
)

// Request is something the agent waits for the owner to answer.
type Request struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Tool is the tool asked for (permissions); Title a one-line summary.
	Tool  *Tool  `json:"tool,omitempty"`
	Title string `json:"title,omitempty"`
	// Options are the answers to a permission: allow, always allow, deny.
	Options []Option `json:"options,omitempty"`
	// Questions are what the agent asks (questions).
	Questions []Question `json:"questions,omitempty"`
}

// Option is one answer to a permission request.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Kind is allow, allow_always or deny.
	Kind string `json:"kind"`
}

// Question is one question with its choices (none: a free answer).
type Question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header,omitempty"`
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	Multi    bool     `json:"multi,omitempty"`
}

// Response answers a request: an option (permissions) or answers by
// question id (questions). Empty means deny or skip.
type Response struct {
	Option  string            `json:"option,omitempty"`
	Answers map[string]string `json:"answers,omitempty"`
	// Message is a note for the agent with a denial.
	Message string `json:"message,omitempty"`
}

// Standard permission options.
var permissionOptions = []Option{
	{ID: "allow", Label: "Allow", Kind: "allow"},
	{ID: "allow_always", Label: "Always allow", Kind: "allow_always"},
	{ID: "deny", Label: "Deny", Kind: "deny"},
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
	// Files are files the item shows (images in a tool's result); the hub
	// stores them and keeps references.
	Files []File `json:"files,omitempty"`
}

// File is a file of an item, with its bytes.
type File struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

// MaxItemFile bounds a file sent with an item (larger ones are left out).
const MaxItemFile = 10 << 20

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
	// Models are what the owner can switch to.
	Models []Model `json:"models,omitempty"`
}

// Model is a model the CLI can use.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
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
	// Respond answers a request the driver sent.
	Respond(id string, r Response) error
	// SetModel switches the model for the next turns.
	SetModel(model string) error
	// Steer gives the agent a message during the running turn; ErrNoSteer
	// when the CLI cannot take one (the session then stops the turn and
	// sends it as the next).
	Steer(in Input) error
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

// permissionTitle is a permission request's summary: "Run a command".
func permissionTitle(t *Tool) string {
	switch t.Kind {
	case ToolCommand:
		return "Run a command"
	case ToolEdit:
		return "Change a file"
	case ToolRead:
		return "Read a file"
	case ToolWeb:
		return "Open a web page"
	case ToolMCP:
		return "Use " + firstNonEmpty(t.Title, t.Name)
	}
	return "Use " + firstNonEmpty(t.Name, "a tool")
}
