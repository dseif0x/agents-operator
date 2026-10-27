package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/runner"
)

// `agent-runner mcp` is a stdio MCP server with the chat tools. The agent
// CLI starts it (the runner registers it as the "chat" server for Claude
// Code, Codex and OpenCode); each tool call goes to the runner's loopback
// API (/chat/<op>), and from there over the uplink to the hub.

const mcpServerName = "chat"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var chatToolDefs = []map[string]any{
	{
		"name":        runner.OpChatsList,
		"description": "List the chats you are in, with their number of unread messages.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        runner.OpChatInfo,
		"description": "Show a chat's description (its goal, who does what) and its members: session name, @handle (what to write to mention them), persona and what they are doing right now.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"chat"}, "properties": map[string]any{
			"chat": map[string]any{"type": "string", "description": "the chat's name or id"},
		}},
	},
	{
		"name":        runner.OpChatRead,
		"description": "Read a chat's messages, oldest first: the latest ones, or with since those after a message id. Images come as file paths you can open.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"chat"}, "properties": map[string]any{
			"chat":  map[string]any{"type": "string", "description": "the chat's name or id"},
			"since": map[string]any{"type": "integer", "description": "only messages after this message id"},
			"limit": map[string]any{"type": "integer", "description": "at most this many messages (default 30, max 100)"},
		}},
	},
	{
		"name": runner.OpChatSend,
		"description": "Post a message (Markdown) to a chat. Mention members with @handle to address them (chat_info lists handles); a message without mentions goes to everyone, @everyone asks everyone to answer. " +
			"Use reply_to to answer a specific message. Attach images by giving paths of PNG, JPEG, GIF or WebP files in the workspace.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"chat", "text"}, "properties": map[string]any{
			"chat":     map[string]any{"type": "string", "description": "the chat's name or id"},
			"text":     map[string]any{"type": "string", "description": "the message, Markdown"},
			"reply_to": map[string]any{"type": "integer", "description": "the id of the message this replies to"},
			"images":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "paths of images to attach"},
		}},
	},
}

const mcpInstructions = "Chats connect you with other agent sessions and with your owner. Messages for you arrive as prompts starting with [agents-operator chat]. " +
	"Answer with chat_send when a message is addressed to you; for messages to everyone, only post when you add something. chat_info shows who is in a chat."

// mcpMain serves MCP on stdin/stdout until stdin closes.
func mcpMain(in io.Reader, out io.Writer, chatURL string) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			resp.Result = map[string]any{
				"protocolVersion": firstNonEmptyStr(p.ProtocolVersion, "2025-06-18"),
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "agents-operator-chat", "version": "1"},
				"instructions":    mcpInstructions,
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			resp.Result = map[string]any{"tools": chatToolDefs}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
				break
			}
			text, isErr := callChatTool(chatURL, p.Name, p.Arguments)
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
		}
		_ = enc.Encode(resp)
	}
	return 0
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// chatURL is the runner's loopback chat API, next to its hook endpoint.
func chatURL() string {
	return strings.TrimSuffix(hookURL(), "/hook") + "/chat/"
}

// callChatTool runs one tool through the runner and formats the answer.
func callChatTool(base, name string, args json.RawMessage) (string, bool) {
	known := false
	for _, t := range chatToolDefs {
		if t["name"] == name {
			known = true
		}
	}
	if !known {
		return "unknown tool " + name, true
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+name, bytes.NewReader(args))
	if err != nil {
		return err.Error(), true
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "the chat service in this pod is not reachable: " + err.Error(), true
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return "bad answer from the chat service", true
	}
	if out.Error != "" {
		return out.Error, true
	}
	return formatChatResult(name, out.Result), false
}

// formatChatResult renders a tool's result as text for the model.
func formatChatResult(name string, raw json.RawMessage) string {
	var b strings.Builder
	switch name {
	case runner.OpChatsList:
		var chats []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Unread int    `json:"unread"`
		}
		if json.Unmarshal(raw, &chats) != nil {
			return string(raw)
		}
		if len(chats) == 0 {
			return "You are in no chat."
		}
		for _, c := range chats {
			fmt.Fprintf(&b, "- %q (id %s): %d unread\n", c.Name, c.ID, c.Unread)
		}
	case runner.OpChatInfo:
		var c struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Members     []struct {
				Name     string `json:"name"`
				Handle   string `json:"handle"`
				Agent    string `json:"agent"`
				Persona  string `json:"persona"`
				State    string `json:"state"`
				Activity string `json:"activity"`
				Muted    bool   `json:"muted"`
			} `json:"members"`
		}
		if json.Unmarshal(raw, &c) != nil {
			return string(raw)
		}
		fmt.Fprintf(&b, "Chat %q\n", c.Name)
		if c.Description != "" {
			b.WriteString("\nDescription:\n" + c.Description + "\n")
		}
		b.WriteString("\nMembers (the owner is always in the chat too):\n")
		for _, m := range c.Members {
			state := m.State
			if m.Activity != "" {
				state += ", " + m.Activity
			}
			if m.Muted {
				state += ", muted"
			}
			fmt.Fprintf(&b, "- %s (@%s, %s, %s)\n", m.Name, m.Handle, m.Agent, state)
			if p := strings.TrimSpace(m.Persona); p != "" {
				b.WriteString("  persona: " + strings.ReplaceAll(p, "\n", "\n  ") + "\n")
			}
		}
	case runner.OpChatRead:
		var res struct {
			Chat struct {
				Name string `json:"name"`
			} `json:"chat"`
			Messages []runner.ChatMessage `json:"messages"`
		}
		if json.Unmarshal(raw, &res) != nil {
			return string(raw)
		}
		if len(res.Messages) == 0 {
			return fmt.Sprintf("No messages in %q.", res.Chat.Name)
		}
		fmt.Fprintf(&b, "Chat %q, oldest first:\n", res.Chat.Name)
		for _, m := range res.Messages {
			who := m.Author.Name
			if m.Author.Kind == "owner" {
				who += " (owner)"
			} else if m.Author.Handle != "" {
				who += " (@" + m.Author.Handle + ")"
			}
			fmt.Fprintf(&b, "\n#%d %s · %s", m.ID, m.CreatedAt, who)
			if m.ReplyTo > 0 {
				fmt.Fprintf(&b, " · reply to #%d", m.ReplyTo)
			}
			b.WriteString("\n")
			if m.Text != "" {
				b.WriteString(m.Text + "\n")
			}
			for _, a := range m.Attachments {
				if a.Path != "" {
					b.WriteString("[image: " + a.Path + "]\n")
				} else {
					b.WriteString("[image " + a.Name + ", not fetched]\n")
				}
			}
		}
	case runner.OpChatSend:
		var m struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return string(raw)
		}
		fmt.Fprintf(&b, "Posted as message #%d.", m.ID)
	default:
		return string(raw)
	}
	return b.String()
}

// mcpConfigFile writes the Claude Code MCP configuration that starts this
// server and returns its path ("" on failure).
func mcpConfigFile(home string) string {
	cfg := map[string]any{"mcpServers": map[string]any{mcpServerName: map[string]any{"type": "stdio", "command": "agent-runner", "args": []string{"mcp"}}}}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	path := writeSessionFileIn(home, "mcp.json", string(b))
	return path
}

// writeSessionFileIn writes a runner file under ~/.config/agents-operator.
func writeSessionFileIn(home, name, content string) string {
	if home == "" {
		return ""
	}
	dir := home + "/.config/agents-operator"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		return ""
	}
	return path
}
