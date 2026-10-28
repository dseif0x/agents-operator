package runner

import "encoding/json"

// The uplink carries chat traffic between the hub and a runner. The hub
// opens it (GET UplinkPath with the runner token), so the network direction
// stays hub -> pod: a session pod never connects to the hub. Over it:
//
//   - the runner sends requests on behalf of its agent (the chat tools of
//     `agent-runner mcp`): {"id":1,"op":"chat_send","args":{...}}, answered
//     with {"id":1,"ok":true,"result":...} or {"id":1,"error":"..."};
//   - the hub pushes messages queued for the agent:
//     {"event":"deliver","deliveries":[...]}. The runner hands them to the
//     agent when it is idle and confirms with the "delivered" op; until
//     then the hub pushes them again on every (re)connect, and the runner
//     ignores ids it already has.
const UplinkPath = "/uplink"

// Uplink ops (runner -> hub).
const (
	OpChatsList  = "chats_list"
	OpChatInfo   = "chat_info"
	OpChatRead   = "chat_read"
	OpChatSend   = "chat_send"
	OpAttachment = "attachment" // {"id":"<attachment id>"} -> UplinkFile
	OpDelivered  = "delivered"  // {"ids":[1,2]}
)

// EventDeliver is the hub's push of queued messages.
const EventDeliver = "deliver"

// UplinkFrame is one JSON text frame of the uplink, in either direction.
type UplinkFrame struct {
	ID     int64           `json:"id,omitempty"`
	Op     string          `json:"op,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`

	Event      string         `json:"event,omitempty"`
	Deliveries []ChatDelivery `json:"deliveries,omitempty"`
}

// ChatAuthor is who wrote a chat message.
type ChatAuthor struct {
	Kind      string `json:"kind"` // "owner" or "session"
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name"`
	Handle    string `json:"handle,omitempty"`
}

// ChatFile describes an image of a chat message. Path is filled in by the
// runner once the file is on disk.
type ChatFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Path        string `json:"path,omitempty"`
}

// ChatMessage is a chat message as the runner and its agent see it.
type ChatMessage struct {
	ID          int64      `json:"id"`
	ChatID      string     `json:"chat_id"`
	Author      ChatAuthor `json:"author"`
	Text        string     `json:"text"`
	Mentions    []string   `json:"mentions,omitempty"`
	Everyone    bool       `json:"everyone,omitempty"`
	ReplyTo     int64      `json:"reply_to,omitempty"`
	Attachments []ChatFile `json:"attachments,omitempty"`
	CreatedAt   string     `json:"created_at"`
}

// ChatDelivery is a queued message for the agent.
type ChatDelivery struct {
	ChatMessage
	ChatName string `json:"chat_name"`
	// Address is "open" (to everyone: answer if you have something to
	// add), "direct" (mentions or replies to the agent) or "everyone"
	// (@everyone: answer).
	Address string `json:"address"`
	// Earlier is how many unread messages of the chat were not delivered.
	Earlier int `json:"earlier"`
}

// UplinkFile is an image's bytes (base64 in JSON).
type UplinkFile struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

// ChatSendArgs are the arguments of OpChatSend.
type ChatSendArgs struct {
	Chat    string `json:"chat"`
	Text    string `json:"text"`
	ReplyTo int64  `json:"reply_to,omitempty"`
	// Images carries the attached files of any type (the wire name is from
	// when only images were allowed).
	Images []UplinkFile `json:"images,omitempty"`
}

// ChatRefArgs name a chat (OpChatInfo) and, for OpChatRead, a window.
type ChatRefArgs struct {
	Chat  string `json:"chat"`
	Since int64  `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// UplinkMaxFrame bounds an uplink frame: up to five 10 MiB images in base64.
const UplinkMaxFrame = 80 << 20
