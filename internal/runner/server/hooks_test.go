package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func TestHooksActivity(t *testing.T) {
	h := &Hooks{}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	post := func(body string) int {
		resp, err := http.Post(srv.URL+"/hook", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if h.Current() != nil {
		t.Fatal("activity before any event")
	}
	if code := post(`not json`); code != http.StatusBadRequest {
		t.Fatalf("bad body accepted: %d", code)
	}

	steps := []struct {
		body   string
		state  string
		detail string
		attn   bool
	}{
		{`{"hook_event_name":"SessionStart","source":"startup"}`, runner.ActivityWaitingInput, "ready", true},
		{`{"hook_event_name":"UserPromptSubmit","prompt":"fix the bug"}`, runner.ActivityThinking, "", false},
		{`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./...\necho done","description":"Run the tests"}}`, runner.ActivityTool, "Bash: Run the tests", false},
		{`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`, runner.ActivityThinking, "", false},
		{`{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/workspace/app/internal/api/api.go"}}`, runner.ActivityTool, "Edit: api.go", false},
		// A subagent's events do not touch the main agent's state.
		{`{"hook_event_name":"Stop","agent_id":"sub-1","last_assistant_message":"done"}`, runner.ActivityTool, "Edit: api.go", false},
		{`{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf build"}}`, runner.ActivityNeedsPermission, "Bash: rm -rf build", true},
		{`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`, runner.ActivityThinking, "", false},
		{`{"hook_event_name":"Notification","notification_type":"auth_success","message":"ok"}`, runner.ActivityThinking, "", false},
		{`{"hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission to use Edit"}`, runner.ActivityNeedsPermission, "Claude needs your permission to use Edit", true},
		{`{"hook_event_name":"Stop","last_assistant_message":"  I fixed the bug in\n\n api.go and added a test.  "}`, runner.ActivityWaitingInput, "", true},
		{`{"hook_event_name":"Notification","notification_type":"idle_prompt","message":"waiting"}`, runner.ActivityWaitingInput, "idle", true},
		{`{"hook_event_name":"StopFailure","error_type":"rate_limit","error":"429"}`, runner.ActivityError, "rate_limit", true},
		{`{"hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`, runner.ActivityExited, "prompt_input_exit", false},
	}
	for i, s := range steps {
		if code := post(s.body); code != http.StatusNoContent {
			t.Fatalf("step %d: status %d", i, code)
		}
		a := h.Current()
		if a == nil || a.State != s.state || a.Detail != s.detail || a.NeedsAttention() != s.attn {
			t.Fatalf("step %d (%s): activity = %+v, want %s %q attention=%v", i, s.body, a, s.state, s.detail, s.attn)
		}
		if a.Since.IsZero() {
			t.Fatalf("step %d: no timestamp", i)
		}
	}
	// The reply was kept whitespace-collapsed.
	h.Apply(hookEvent{Event: "Stop", LastMessage: "  I fixed the bug in\n\n api.go and added a test.  "}, time.Now())
	if a := h.Current(); a.Message != "I fixed the bug in api.go and added a test." {
		t.Fatalf("message = %q", a.Message)
	}
	// It stays visible through the next turn's thinking, until a new prompt.
	h.Apply(hookEvent{Event: "PostToolUse", ToolName: "Read"}, time.Now())
	if a := h.Current(); a.Message == "" {
		t.Fatal("reply dropped while thinking")
	}
	h.Apply(hookEvent{Event: "UserPromptSubmit"}, time.Now())
	if a := h.Current(); a.Message != "" {
		t.Fatal("reply kept past a new prompt")
	}
	// A long reply is cut.
	h.Apply(hookEvent{Event: "Stop", LastMessage: strings.Repeat("x", 500)}, time.Now())
	if a := h.Current(); len([]rune(a.Message)) != 300 || !strings.HasSuffix(a.Message, "…") {
		t.Fatalf("message not trimmed: %d", len(a.Message))
	}
	h.Reset()
	if h.Current() != nil {
		t.Fatal("reset kept the activity")
	}
}

func TestDescribeTool(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "ls -la"}, "Bash: ls -la"},
		{"Bash", map[string]any{"command": "ls", "description": "List files"}, "Bash: List files"},
		{"Write", map[string]any{"file_path": "/a/b/c.ts"}, "Write: c.ts"},
		{"Grep", map[string]any{"pattern": "TODO"}, "Grep: TODO"},
		{"WebSearch", map[string]any{"query": "xterm touch"}, "WebSearch: xterm touch"},
		{"Task", map[string]any{"description": "Explore the repo"}, "Task: Explore the repo"},
		{"mcp__github__get_me", map[string]any{"x": 1}, "mcp__github__get_me"},
		{"Bash", nil, "Bash"},
	}
	for _, c := range cases {
		if got := describeTool(c.name, c.input); got != c.want {
			t.Errorf("describeTool(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}
