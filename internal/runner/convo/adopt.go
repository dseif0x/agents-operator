package convo

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Adoption: a session switched from the terminal to a conversation keeps
// its earlier CLI conversation. On the first start without a conversation
// id the runner looks for the newest one the CLI left in the workspace,
// resumes it, and imports its history so the chat shows it.

// MaxImported bounds how many items an import sends.
const MaxImported = 3000

// AdoptClaude finds Claude Code's newest transcript for dir under home
// (~/.claude/projects/<dir with / as ->/<session id>.jsonl) and returns its
// session id and its history as items. ref is "" when there is none.
func AdoptClaude(home, dir string) (ref string, items []Event) {
	project := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(dir, "/", "-"))
	entries, err := os.ReadDir(project)
	if err != nil {
		return "", nil
	}
	type file struct {
		path string
		mod  int64
	}
	var files []file
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") || strings.HasPrefix(e.Name(), "agent-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		files = append(files, file{filepath.Join(project, e.Name()), info.ModTime().UnixNano()})
	}
	if len(files) == 0 {
		return "", nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod > files[j].mod })
	path := files[0].path
	ref = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return ref, claudeHistory(path)
}

// claudeHistory turns a transcript into items: the owner's messages, the
// answers and the tool calls with their results. Sub-agent lines and
// meta lines are left out.
func claudeHistory(path string) []Event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	type line struct {
		Type        string          `json:"type"`
		UUID        string          `json:"uuid"`
		IsSidechain bool            `json:"isSidechain"`
		IsMeta      bool            `json:"isMeta"`
		Message     json.RawMessage `json:"message"`
	}
	var out []Event
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		raw, err := readLine(r)
		if len(raw) > 0 {
			var l line
			if json.Unmarshal(raw, &l) == nil && !l.IsSidechain && !l.IsMeta && (l.Type == "user" || l.Type == "assistant") {
				out = append(out, claudeLineItems(l.Type, l.UUID, l.Message)...)
			}
		}
		if err != nil {
			break
		}
	}
	if len(out) > MaxImported {
		out = out[len(out)-MaxImported:]
	}
	return out
}

func claudeLineItems(kind, uuid string, msg json.RawMessage) []Event {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return nil
	}
	item := func(key, k, text string, tool *Tool, status string) Event {
		return Event{Type: EventItem, Item: &Item{Key: "imported:" + key, Kind: k, Status: status, Text: text, Tool: tool, Source: "imported"}}
	}
	// A plain string: a message from the owner (or a command echo).
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		text = strings.TrimSpace(text)
		if kind != "user" || text == "" || strings.HasPrefix(text, "<command-") || strings.HasPrefix(text, "<local-command") || strings.HasPrefix(text, "Caveat:") {
			return nil
		}
		return []Event{item(uuid, KindUser, text, nil, StatusDone)}
	}
	var blocks []claudeBlockJSON
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	var out []Event
	for i, b := range blocks {
		key := uuid + ":" + itoa(i)
		switch {
		case kind == "user" && b.Type == "text" && strings.TrimSpace(b.Text) != "" && !strings.HasPrefix(strings.TrimSpace(b.Text), "<"):
			out = append(out, item(key, KindUser, b.Text, nil, StatusDone))
		case kind == "user" && b.Type == "tool_result":
			status := StatusDone
			if b.IsError {
				status = StatusFailed
			}
			out = append(out, Event{Type: EventItem, Item: &Item{Key: "imported:" + b.ToolUseID, Kind: KindTool, Status: status, Tool: &Tool{Output: trim(resultText(b.Content)), Error: b.IsError}}})
		case kind == "assistant" && b.Type == "text" && strings.TrimSpace(b.Text) != "":
			out = append(out, item(key, KindText, b.Text, nil, StatusDone))
		case kind == "assistant" && b.Type == "thinking" && strings.TrimSpace(b.Thinking) != "":
			out = append(out, item(key, KindThinking, b.Thinking, nil, StatusDone))
		case kind == "assistant" && b.Type == "tool_use":
			out = append(out, Event{Type: EventItem, Item: &Item{Key: "imported:" + b.ID, Kind: KindTool, Status: StatusDone, Tool: claudeTool(b.Name, b.Input)}})
		}
	}
	return out
}
