package convo

import (
	"encoding/base64"
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

func TestClaudeBackgroundTasksChanged(t *testing.T) {
	c := &Claude{events: make(chan Event, 10), background: map[string]string{"stale": "old"}}
	c.handle(claudeLine{Type: "system", Subtype: "background_tasks_changed", Tasks: []claudeTask{{TaskID: "bdbqydcky", Description: "Sleep 90 seconds"}}})
	ev := <-c.events
	if ev.Type != EventBackground || len(ev.Background) != 1 || ev.Background[0].ID != "bdbqydcky" || ev.Background[0].Title != "Sleep 90 seconds" {
		t.Fatalf("event = %+v", ev)
	}
	c.handle(claudeLine{Type: "system", Subtype: "background_tasks_changed"})
	if ev := <-c.events; len(ev.Background) != 0 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestClaudeManualBackgroundResult(t *testing.T) {
	out := "Command was manually backgrounded by user with ID: bdbqydcky. Output is being written to: /tmp/x.output."
	if m := bgStartRE.FindStringSubmatch(out); m == nil || m[1] != "bdbqydcky" {
		t.Fatalf("match = %v", m)
	}
}

func TestClaudeResultImages(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	raw := []byte(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(png) + `"}},{"type":"text","text":"x"}]`)
	files := resultImages(raw, "/workspace/shots/home.png")
	if len(files) != 1 || files[0].Name != "home.png" || files[0].ContentType != "image/png" || string(files[0].Data) != string(png) {
		t.Fatalf("files = %+v", files)
	}
	if resultImages([]byte(`"plain text"`), "") != nil {
		t.Fatal("a text result has images")
	}
}
