package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// Hooks turns the agent CLI's hook events into an Activity. Claude Code
// runs `agent-runner hook` on every lifecycle event (see the runner's
// bootstrap, which installs them in ~/.claude/settings.json); that helper
// forwards the event JSON to this handler on the loopback interface.
// Nothing outside the pod can reach it, and nothing a hook says can change
// what the CLI does: the helper always exits 0.
type Hooks struct {
	Log *slog.Logger

	mu  sync.Mutex
	cur *runner.Activity
}

// hookEvent is the subset of the hook input fields the tracker reads.
type hookEvent struct {
	Event            string         `json:"hook_event_name"`
	ToolName         string         `json:"tool_name"`
	ToolInput        map[string]any `json:"tool_input"`
	NotificationType string         `json:"notification_type"`
	Message          string         `json:"message"`
	LastMessage      string         `json:"last_assistant_message"`
	Reason           string         `json:"reason"`
	Error            string         `json:"error"`
	ErrorType        string         `json:"error_type"`
	// AgentID is set for events from subagents, which run inside a Task
	// tool call of the main agent and must not flip its state.
	AgentID string `json:"agent_id"`
}

// Handler accepts POST /hook with one event per request.
func (h *Hooks) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		var ev hookEvent
		if err := json.Unmarshal(body, &ev); err != nil || ev.Event == "" {
			http.Error(w, "bad event", http.StatusBadRequest)
			return
		}
		h.Apply(ev, time.Now())
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// Current returns a copy of the latest activity, or nil before any event.
func (h *Hooks) Current() *runner.Activity {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		return nil
	}
	c := *h.cur
	return &c
}

// Reset forgets the activity, for when the agent process is relaunched.
func (h *Hooks) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = nil
}

// Apply folds one event into the activity.
func (h *Hooks) Apply(ev hookEvent, now time.Time) {
	if ev.AgentID != "" {
		return
	}
	var next *runner.Activity
	switch ev.Event {
	case "SessionStart":
		next = &runner.Activity{State: runner.ActivityWaitingInput, Detail: "ready"}
	case "UserPromptSubmit":
		next = &runner.Activity{State: runner.ActivityThinking}
	case "PreToolUse":
		next = &runner.Activity{State: runner.ActivityTool, Detail: describeTool(ev.ToolName, ev.ToolInput)}
	case "PostToolUse":
		next = &runner.Activity{State: runner.ActivityThinking}
	case "PostToolUseFailure":
		next = &runner.Activity{State: runner.ActivityThinking, Detail: "after a failed " + ev.ToolName}
	case "PermissionRequest":
		next = &runner.Activity{State: runner.ActivityNeedsPermission, Detail: describeTool(ev.ToolName, ev.ToolInput)}
	case "Notification":
		switch ev.NotificationType {
		case "permission_prompt", "elicitation_dialog":
			next = &runner.Activity{State: runner.ActivityNeedsPermission, Detail: trim(ev.Message, 160)}
		case "idle_prompt":
			next = &runner.Activity{State: runner.ActivityWaitingInput, Detail: "idle"}
		default:
			return
		}
	case "Elicitation":
		next = &runner.Activity{State: runner.ActivityNeedsPermission, Detail: "an MCP server asks for input"}
	case "Stop":
		next = &runner.Activity{State: runner.ActivityWaitingInput, Message: trim(ev.LastMessage, 300)}
	case "StopFailure":
		// Observed from the CLI: {"error":"authentication_failed",
		// "last_assistant_message":"Please run /login · API Error: 401 …"}.
		detail := firstNonEmpty(ev.ErrorType, trim(ev.Error, 160), ev.Reason, "turn failed")
		next = &runner.Activity{State: runner.ActivityError, Detail: detail, Message: trim(ev.LastMessage, 300)}
	case "SessionEnd":
		next = &runner.Activity{State: runner.ActivityExited, Detail: ev.Reason}
	default:
		return // SubagentStart/Stop, compaction, config changes: no state change
	}
	next.Since = now
	h.mu.Lock()
	// Keep the last reply visible while the next turn only thinks.
	if h.cur != nil && next.Message == "" && next.State == runner.ActivityThinking && ev.Event != "UserPromptSubmit" {
		next.Message = h.cur.Message
	}
	h.cur = next
	h.mu.Unlock()
	if h.Log != nil {
		h.Log.Debug("agent activity", "event", ev.Event, "state", next.State, "detail", next.Detail)
	}
}

// describeTool is "Tool: what" for the tools people recognise.
func describeTool(name string, input map[string]any) string {
	str := func(k string) string {
		v, _ := input[k].(string)
		return strings.TrimSpace(v)
	}
	var what string
	switch name {
	case "Bash":
		what = str("description")
		if what == "" {
			what = str("command")
		}
	case "Edit", "Write", "Read", "MultiEdit", "NotebookEdit":
		what = str("file_path")
		if what != "" {
			what = filepath.Base(what)
		}
	case "Glob", "Grep":
		what = str("pattern")
	case "WebFetch":
		what = str("url")
	case "WebSearch":
		what = str("query")
	case "Task", "Agent":
		what = str("description")
	}
	if what == "" {
		return name
	}
	if i := strings.IndexByte(what, '\n'); i >= 0 {
		what = what[:i]
	}
	return name + ": " + trim(what, 120)
}

// trim collapses whitespace and cuts at n runes.
func trim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
