package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

func TestChatStopHook(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := NewProcess(func() Command { return Command{Path: "cat"} }, log)
	chat := &Chat{Proc: proc, Agent: runner.AgentClaude, Home: t.TempDir(), Log: log}
	hooks := &Hooks{Log: log, OnStop: chat.StopHook}
	srv := httptest.NewServer(hooks.Handler())
	defer srv.Close()
	post := func(ev string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+"/hook", "application/json", strings.NewReader(`{"hook_event_name":"`+ev+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Nothing queued: the stop goes ahead.
	if resp := post("Stop"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty queue: %d", resp.StatusCode)
	}
	chat.enqueue([]runner.ChatDelivery{{ChatMessage: runner.ChatMessage{ID: 5, Author: runner.ChatAuthor{Kind: "owner", Name: "dan"}, Text: "review \x1b[31mthis"}, ChatName: "launch", Address: "direct"}})
	chat.enqueue([]runner.ChatDelivery{{ChatMessage: runner.ChatMessage{ID: 5}}}) // a repeat is ignored
	// Other events never block.
	if resp := post("PostToolUse"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PostToolUse: %d", resp.StatusCode)
	}
	resp := post("Stop")
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out["decision"] != "block" || !strings.Contains(out["reason"], "message #5 · from dan (owner) · addressed to you") {
		t.Fatalf("stop with a message: %d %v", resp.StatusCode, out)
	}
	if chat.hasQueue() {
		t.Fatal("message left queued")
	}
	if resp := post("Stop"); resp.StatusCode != http.StatusNoContent {
		t.Fatal("blocked again without new messages")
	}
}

func TestChatTypesWhenIdle(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc := NewProcess(func() Command { return Command{Path: "cat"} }, log)
	if err := proc.Start(); err != nil {
		t.Skip(err)
	}
	defer proc.Close()
	hooks := &Hooks{Log: log}
	chat := &Chat{Proc: proc, Agent: runner.AgentClaude, Hooks: hooks, Home: t.TempDir(), Log: log}
	// Working: nothing is typed.
	hooks.Apply(hookEvent{Event: "UserPromptSubmit"}, time.Now())
	if chat.idle(time.Now().Add(time.Minute)) {
		t.Fatal("idle while thinking")
	}
	hooks.Apply(hookEvent{Event: "PermissionRequest", ToolName: "Bash"}, time.Now())
	if chat.idle(time.Now().Add(time.Minute)) {
		t.Fatal("idle while asking for permission")
	}
	hooks.Apply(hookEvent{Event: "Stop"}, time.Now())
	if !chat.idle(time.Now().Add(5 * time.Second)) {
		t.Fatal("not idle after the turn ended")
	}
	// Control characters never reach the terminal.
	if err := chat.typeIn("hi \x1b[2Jthere\x07"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if out := string(proc.Scrollback()); !strings.Contains(out, "hi [2Jthere") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("typed %q", out)
	}
}
