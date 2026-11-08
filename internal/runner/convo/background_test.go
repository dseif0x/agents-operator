package convo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeBackgroundFromTranscript(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ref1.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"content":"old"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Claude{Cmd: Command{Dir: "/work", Env: []string{"HOME=" + home}}, events: make(chan Event, 10), done: make(chan struct{}),
		background: map[string]string{"b6ab52jzx": "sleep 20", "other1": "watch"}}
	defer close(c.done)
	go c.followTranscript("ref1")
	time.Sleep(2500 * time.Millisecond) // the follower has read up to the end
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"user","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>\n<task-id>b6ab52jzx</task-id>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>"}}` + "\n")
	_ = f.Close()
	select {
	case ev := <-c.events:
		if ev.Type != EventBackground || len(ev.Background) != 1 || ev.Background[0].ID != "other1" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the finished task was not noticed")
	}
}
