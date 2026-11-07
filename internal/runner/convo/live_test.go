package convo

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLive runs a real CLI against a model server: CONVO_LIVE=claude,
// codex or opencode; CONVO_HOME is the HOME it runs with (configured for
// the model server), CONVO_DIR its working directory. It prints every
// event and checks that a turn with a tool call ends.
func TestLive(t *testing.T) {
	which := os.Getenv("CONVO_LIVE")
	if which == "" {
		t.Skip("set CONVO_LIVE to run a real CLI")
	}
	home, dir := os.Getenv("CONVO_HOME"), os.Getenv("CONVO_DIR")
	env := append(os.Environ(), "HOME="+home)
	for _, kv := range strings.Split(os.Getenv("CONVO_ENV"), ";") {
		if kv != "" {
			env = append(env, kv)
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var d Driver
	switch which {
	case "claude":
		d = &Claude{Cmd: Command{Path: "claude", Args: strings.Fields(os.Getenv("CONVO_ARGS")), Dir: dir, Env: env}, Log: log}
	case "codex":
		d = &Codex{Cmd: Command{Path: "codex", Args: strings.Fields(os.Getenv("CONVO_ARGS")), Dir: dir, Env: env}, Dir: dir, Log: log}
	case "opencode":
		d = &ACP{Cmd: Command{Path: "opencode", Args: append(strings.Fields(os.Getenv("CONVO_ARGS")), "acp"), Dir: dir, Env: env}, Dir: dir, Log: log}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := d.Start(ctx, os.Getenv("CONVO_REF")); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	prompts := []string{
		"Create a file named hello.txt containing the word hi, using a shell command. Then reply with one short sentence.",
		"What is in hello.txt? Read it and answer in a few words.",
	}
	if p := os.Getenv("CONVO_PROMPT"); p != "" {
		prompts = []string{p}
	}
	for i, p := range prompts {
		if err := d.Send(Input{ID: string(rune('a' + i)), Text: p}); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("CONVO_INTERRUPT") != "" {
			go func() { time.Sleep(3 * time.Second); _ = d.Interrupt() }()
		}
		for ev := range d.Events() {
			b, _ := json.Marshal(ev)
			if ev.Type == EventItem && ev.Item.Delta {
				continue
			}
			t.Logf("%s", b)
			if ev.Type == EventTurnEnded {
				if ev.Status == StatusFailed {
					t.Errorf("turn failed: %s", ev.Error)
				}
				break
			}
		}
	}
}
