package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dseif0x/agents-operator/internal/store"
)

// Chats: groups of the owner's sessions, like a group chat (see
// docs/design/agent-chats.md). The owner uses them through the API; agents
// through the uplink and `agent-runner mcp`.
//
// Who receives a message, whoever wrote it (never the author):
//   - no mention and no reply: every agent, each deciding for itself
//     whether it has something to add (AddressOpen);
//   - @handle mentions, or a reply to an agent's message: those agents
//     (AddressDirect);
//   - @everyone: every agent, each asked to answer (AddressEveryone).
//
// A muted chat delivers nothing; a muted member gets nothing and its
// messages reach no other agent.

// ChatNotifier is told when a session has new messages queued (the uplink
// manager, which pushes them to the runner).
type ChatNotifier interface {
	Notify(sessionID string)
}

// How a delivered message concerns its recipient.
const (
	AddressOpen     = "open"     // to everyone; answer if you have something to add
	AddressDirect   = "direct"   // mentions or replies to you; answer
	AddressEveryone = "everyone" // @everyone; everyone answers
)

// Limits.
const (
	MaxChatText        = 32 << 10
	MaxChatFiles       = 5
	MaxChatFileBytes   = 10 << 20
	ChatFileQuota      = 2 << 30 // per owner
	MaxChatName        = 80
	MaxChatDescription = 4 << 10
)

// ChatImageTypes are the sniffed types shown inline as images. Files of
// any other type are attachments to download.
var ChatImageTypes = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

// IsChatImage reports whether a stored attachment is shown as an image.
func IsChatImage(contentType string) bool {
	_, ok := ChatImageTypes[contentType]
	return ok
}

// CleanFileName keeps the last path element of an uploaded file's name,
// without control characters, quotes or backslashes, at most 200 bytes.
func CleanFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	for len(name) > 200 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// ChatView is the API shape of a chat.
type ChatView struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Muted       bool           `json:"muted"`
	Members     []MemberView   `json:"members"`
	LastMessage *MessageView   `json:"last_message,omitempty"`
	Unread      int            `json:"unread"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	membersBy   map[string]int // session id -> index in Members
}

// MemberView is a session in a chat.
type MemberView struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Handle    string `json:"handle"`
	Agent     string `json:"agent"`
	Persona   string `json:"persona"`
	State     string `json:"state"`
	// Activity is what the agent is doing right now, when known.
	Activity string `json:"activity,omitempty"`
	Muted    bool   `json:"muted"`
}

// AuthorView is who wrote a message.
type AuthorView struct {
	// Kind is "owner" or "session".
	Kind      string `json:"kind"`
	SessionID string `json:"session_id,omitempty"`
	// Name is the session's current name (the one it had when posting, if
	// it is gone) or the owner's username.
	Name    string `json:"name"`
	Handle  string `json:"handle,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// AttachmentView is an image of a message.
type AttachmentView struct {
	store.ChatAttachment
	URL string `json:"url"`
}

// MessageView is the API shape of a message.
type MessageView struct {
	ID          int64            `json:"id"`
	ChatID      string           `json:"chat_id"`
	Author      AuthorView       `json:"author"`
	Text        string           `json:"text"`
	Mentions    []string         `json:"mentions"`
	Everyone    bool             `json:"everyone"`
	ReplyTo     int64            `json:"reply_to,omitempty"`
	Attachments []AttachmentView `json:"attachments"`
	// Deliveries maps recipient session id to "queued" or "delivered".
	Deliveries map[string]string `json:"deliveries"`
	CreatedAt  time.Time         `json:"created_at"`
}

// ChatInput creates or changes a chat; nil fields are left as they are.
type ChatInput struct {
	Name        *string  `json:"name"`
	Description *string  `json:"description"`
	Muted       *bool    `json:"muted"`
	Members     []string `json:"members"` // session ids, on create
}

// ChatFile is a file (an image or anything else) posted with a message.
type ChatFile struct {
	Name string
	Data []byte
}

// ---- owner side ----

func (s *Service) ownChat(ctx context.Context, owner *store.User, id string) (*store.Chat, error) {
	c, err := s.Store.Chats().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.OwnerID != owner.ID {
		return nil, store.ErrNotFound
	}
	return c, nil
}

func cleanChatName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || utf8.RuneCountInString(name) > MaxChatName {
		return "", &ValidationError{fmt.Sprintf("a chat needs a name of up to %d characters", MaxChatName)}
	}
	return name, nil
}

// CreateChat creates a chat with the given members.
func (s *Service) CreateChat(ctx context.Context, owner *store.User, in ChatInput) (*ChatView, error) {
	if in.Name == nil {
		return nil, &ValidationError{"a chat needs a name"}
	}
	name, err := cleanChatName(*in.Name)
	if err != nil {
		return nil, err
	}
	c := &store.Chat{OwnerID: owner.ID, Name: name}
	if in.Description != nil {
		if c.Description, err = cleanDescription(*in.Description); err != nil {
			return nil, err
		}
	}
	for _, sid := range in.Members {
		if _, err := s.Get(ctx, owner.ID, sid); err != nil {
			return nil, &ValidationError{"member " + sid + " is not one of your sessions"}
		}
	}
	if err := s.Store.Chats().Create(ctx, c); err != nil {
		return nil, err
	}
	for _, sid := range in.Members {
		if err := s.Store.Chats().AddMember(ctx, c.ID, sid); err != nil && !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
	}
	return s.chatChanged(ctx, owner, c)
}

func cleanDescription(d string) (string, error) {
	d = strings.TrimSpace(strings.ReplaceAll(d, "\r\n", "\n"))
	if len(d) > MaxChatDescription || !utf8.ValidString(d) {
		return "", &ValidationError{fmt.Sprintf("the description is limited to %d KiB of text", MaxChatDescription>>10)}
	}
	return d, nil
}

// UpdateChat renames a chat, changes its description or mutes it.
func (s *Service) UpdateChat(ctx context.Context, owner *store.User, id string, in ChatInput) (*ChatView, error) {
	c, err := s.ownChat(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		if c.Name, err = cleanChatName(*in.Name); err != nil {
			return nil, err
		}
	}
	if in.Description != nil {
		if c.Description, err = cleanDescription(*in.Description); err != nil {
			return nil, err
		}
	}
	if in.Muted != nil {
		// Muting stops new deliveries; what was queued before still goes out.
		c.Muted = *in.Muted
	}
	if err := s.Store.Chats().Update(ctx, c); err != nil {
		return nil, err
	}
	return s.chatChanged(ctx, owner, c)
}

// DeleteChat deletes a chat with its messages and images.
func (s *Service) DeleteChat(ctx context.Context, owner *store.User, id string) error {
	if _, err := s.ownChat(ctx, owner, id); err != nil {
		return err
	}
	if err := s.Store.Chats().Delete(ctx, id); err != nil {
		return err
	}
	if s.Broker != nil {
		s.Broker.Publish(owner.ID, Event{Type: "chat", ID: id})
	}
	return nil
}

// AddChatMember adds one of the owner's sessions to a chat.
func (s *Service) AddChatMember(ctx context.Context, owner *store.User, chatID, sessionID string) (*ChatView, error) {
	c, err := s.ownChat(ctx, owner, chatID)
	if err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, owner.ID, sessionID); err != nil {
		return nil, err
	}
	if err := s.Store.Chats().AddMember(ctx, chatID, sessionID); err != nil && !errors.Is(err, store.ErrConflict) {
		return nil, err
	}
	// A running session needs an uplink now.
	if s.Uplinks != nil {
		s.Uplinks.Notify(sessionID)
	}
	return s.chatChanged(ctx, owner, c)
}

// RemoveChatMember takes a session out of a chat.
func (s *Service) RemoveChatMember(ctx context.Context, owner *store.User, chatID, sessionID string) (*ChatView, error) {
	c, err := s.ownChat(ctx, owner, chatID)
	if err != nil {
		return nil, err
	}
	if err := s.Store.Chats().RemoveMember(ctx, chatID, sessionID); err != nil {
		return nil, err
	}
	return s.chatChanged(ctx, owner, c)
}

// SetChatMemberMuted mutes or unmutes a member.
func (s *Service) SetChatMemberMuted(ctx context.Context, owner *store.User, chatID, sessionID string, muted bool) (*ChatView, error) {
	c, err := s.ownChat(ctx, owner, chatID)
	if err != nil {
		return nil, err
	}
	if err := s.Store.Chats().SetMemberMuted(ctx, chatID, sessionID, muted); err != nil {
		return nil, err
	}
	return s.chatChanged(ctx, owner, c)
}

func (s *Service) chatChanged(ctx context.Context, owner *store.User, c *store.Chat) (*ChatView, error) {
	v, err := s.chatView(ctx, owner, c)
	if err != nil {
		return nil, err
	}
	if s.Broker != nil {
		s.Broker.Publish(owner.ID, Event{Type: "chat", ID: c.ID, Chat: v})
	}
	return v, nil
}

// ListChats lists the owner's chats with members, last message and unread count.
func (s *Service) ListChats(ctx context.Context, owner *store.User) ([]*ChatView, error) {
	chats, err := s.Store.Chats().List(ctx, owner.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*ChatView, 0, len(chats))
	for _, c := range chats {
		v, err := s.chatView(ctx, owner, c)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// GetChat returns one of the owner's chats.
func (s *Service) GetChat(ctx context.Context, owner *store.User, id string) (*ChatView, error) {
	c, err := s.ownChat(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	return s.chatView(ctx, owner, c)
}

func (s *Service) chatView(ctx context.Context, owner *store.User, c *store.Chat) (*ChatView, error) {
	v := &ChatView{ID: c.ID, Name: c.Name, Description: c.Description, Muted: c.Muted, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		Members: []MemberView{}, membersBy: map[string]int{}}
	members, err := s.Store.Chats().Members(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		sess, err := s.Store.Sessions().Get(ctx, m.SessionID)
		if err != nil {
			continue
		}
		mv := MemberView{SessionID: sess.ID, Name: sess.Name, Handle: sess.Handle, Agent: sess.Agent, Persona: sess.Persona, State: sess.State, Muted: m.Muted}
		if live, ok := s.Live(sess.ID); ok && live.Status.Activity != nil && sess.State == store.StateRunning {
			mv.Activity = live.Status.Activity.State
		}
		v.membersBy[sess.ID] = len(v.Members)
		v.Members = append(v.Members, mv)
	}
	last, err := s.Store.Chats().Messages(ctx, c.ID, 0, 0, 1)
	if err != nil {
		return nil, err
	}
	if len(last) > 0 {
		views, err := s.messageViews(ctx, owner, v, last)
		if err != nil {
			return nil, err
		}
		v.LastMessage = &views[0]
	}
	if v.Unread, err = s.Store.Chats().Unread(ctx, c.ID, store.OwnerReader); err != nil {
		return nil, err
	}
	return v, nil
}

// messageViews turns stored messages into API shapes, with authors and
// delivery states. owner may be nil (the agent side), then the owner shows
// as "owner".
func (s *Service) messageViews(ctx context.Context, owner *store.User, chat *ChatView, msgs []*store.ChatMessage) ([]MessageView, error) {
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	deliveries, err := s.Store.Chats().Deliveries(ctx, ids)
	if err != nil {
		return nil, err
	}
	byMsg := map[int64]map[string]string{}
	for _, d := range deliveries {
		if byMsg[d.MessageID] == nil {
			byMsg[d.MessageID] = map[string]string{}
		}
		byMsg[d.MessageID][d.SessionID] = d.State
	}
	ownerName := "owner"
	if owner != nil {
		ownerName = owner.Username
	}
	out := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		mv := MessageView{ID: m.ID, ChatID: m.ChatID, Text: m.Text, Mentions: m.Mentions, Everyone: m.Everyone, ReplyTo: m.ReplyTo,
			Attachments: []AttachmentView{}, Deliveries: byMsg[m.ID], CreatedAt: m.CreatedAt}
		if mv.Mentions == nil {
			mv.Mentions = []string{}
		}
		if mv.Deliveries == nil {
			mv.Deliveries = map[string]string{}
		}
		for _, a := range m.Attachments {
			mv.Attachments = append(mv.Attachments, AttachmentView{ChatAttachment: a, URL: "/api/v1/chats/" + m.ChatID + "/attachments/" + a.ID})
		}
		if m.AuthorSession == "" {
			mv.Author = AuthorView{Kind: "owner", Name: ownerName}
		} else {
			mv.Author = AuthorView{Kind: "session", SessionID: m.AuthorSession, Name: m.AuthorName}
			if i, ok := chat.membersBy[m.AuthorSession]; ok {
				mv.Author.Name, mv.Author.Handle = chat.Members[i].Name, chat.Members[i].Handle
			} else if sess, err := s.Store.Sessions().Get(ctx, m.AuthorSession); err == nil {
				mv.Author.Name, mv.Author.Handle = sess.Name, sess.Handle
			} else {
				mv.Author.Deleted = true
			}
		}
		out = append(out, mv)
	}
	return out, nil
}

// ChatMessages pages through a chat's messages (see store.Chats.Messages).
func (s *Service) ChatMessages(ctx context.Context, owner *store.User, chatID string, before, after int64, limit int) ([]MessageView, error) {
	c, err := s.ownChat(ctx, owner, chatID)
	if err != nil {
		return nil, err
	}
	v, err := s.chatView(ctx, owner, c)
	if err != nil {
		return nil, err
	}
	msgs, err := s.Store.Chats().Messages(ctx, chatID, before, after, limit)
	if err != nil {
		return nil, err
	}
	return s.messageViews(ctx, owner, v, msgs)
}

// MarkChatRead moves the owner's read marker.
func (s *Service) MarkChatRead(ctx context.Context, owner *store.User, chatID string, lastID int64) error {
	if _, err := s.ownChat(ctx, owner, chatID); err != nil {
		return err
	}
	return s.Store.Chats().SetRead(ctx, chatID, store.OwnerReader, lastID)
}

// PostChatMessage posts as the owner.
func (s *Service) PostChatMessage(ctx context.Context, owner *store.User, chatID, text string, replyTo int64, images []ChatFile) (*MessageView, error) {
	c, err := s.ownChat(ctx, owner, chatID)
	if err != nil {
		return nil, err
	}
	m, err := s.post(ctx, owner, c, nil, text, replyTo, images)
	if err != nil {
		return nil, err
	}
	// Writing counts as having read everything up to here.
	_ = s.Store.Chats().SetRead(ctx, chatID, store.OwnerReader, m.ID)
	return m, nil
}

// ChatAttachment returns an image of one of the owner's chats.
func (s *Service) ChatAttachment(ctx context.Context, owner *store.User, chatID, id string) (*store.ChatAttachment, []byte, error) {
	if _, err := s.ownChat(ctx, owner, chatID); err != nil {
		return nil, nil, err
	}
	a, inChat, data, err := s.Store.Chats().Attachment(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if inChat != chatID {
		return nil, nil, store.ErrNotFound
	}
	return a, data, nil
}

// ---- posting and addressing ----

// mentionRE finds @handle and @everyone; an @ inside a word (e-mail
// addresses) is not a mention.
var mentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_@./])@([a-z]+_[a-z]+(?:_[0-9]+)?|everyone)\b`)

// parseMentions resolves the @handles in text against the chat's members.
// Unknown handles stay plain text.
func parseMentions(text string, members []MemberView) (ids []string, everyone bool) {
	for _, m := range mentionRE.FindAllStringSubmatch(text, -1) {
		if m[1] == "everyone" {
			everyone = true
			continue
		}
		for _, mem := range members {
			if mem.Handle == m[1] && !slices.Contains(ids, mem.SessionID) {
				ids = append(ids, mem.SessionID)
			}
		}
	}
	return ids, everyone
}

// recipients are the members a message is delivered to.
func recipients(chat *store.Chat, members []MemberView, author string, mentions []string, everyone bool) []string {
	if chat.Muted {
		return nil
	}
	for _, m := range members {
		if author != "" && m.SessionID == author && m.Muted {
			return nil // a muted member's messages reach nobody
		}
	}
	var out []string
	for _, m := range members {
		if m.SessionID == author || m.Muted {
			continue
		}
		if !everyone && len(mentions) > 0 && !slices.Contains(mentions, m.SessionID) {
			continue
		}
		out = append(out, m.SessionID)
	}
	return out
}

// Address is how a message concerns recipient.
func Address(m *store.ChatMessage, recipient string) string {
	switch {
	case m.Everyone:
		return AddressEveryone
	case slices.Contains(m.Mentions, recipient):
		return AddressDirect
	default:
		return AddressOpen
	}
}

// post validates and stores a message by the owner (author nil) or a
// session, queues it for its recipients and tells everyone.
func (s *Service) post(ctx context.Context, owner *store.User, c *store.Chat, author *store.Session, text string, replyTo int64, images []ChatFile) (*MessageView, error) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return nil, &ValidationError{"messages are plain UTF-8 text"}
	}
	if len(text) > MaxChatText {
		return nil, &ValidationError{fmt.Sprintf("messages are limited to %d KiB", MaxChatText>>10)}
	}
	if text == "" && len(images) == 0 {
		return nil, &ValidationError{"an empty message"}
	}
	if len(images) > MaxChatFiles {
		return nil, &ValidationError{fmt.Sprintf("at most %d files per message", MaxChatFiles)}
	}
	var files []store.NewAttachment
	var total int64
	for _, img := range images {
		if len(img.Data) == 0 || len(img.Data) > MaxChatFileBytes {
			return nil, &ValidationError{fmt.Sprintf("files are limited to %d MiB (and cannot be empty)", MaxChatFileBytes>>20)}
		}
		// The type is sniffed, never taken from the uploader. Images are
		// shown inline; anything else is only ever served as a download.
		ct := http.DetectContentType(img.Data)
		name := CleanFileName(img.Name)
		if name == "" {
			name = "file"
			if ext, ok := ChatImageTypes[ct]; ok {
				name = "image." + ext
			}
		}
		files = append(files, store.NewAttachment{Name: name, ContentType: ct, Data: img.Data})
		total += int64(len(img.Data))
	}
	if total > 0 {
		used, err := s.Store.Chats().AttachmentBytes(ctx, c.OwnerID)
		if err != nil {
			return nil, err
		}
		if used+total > ChatFileQuota {
			return nil, &ValidationError{"the file storage of your chats is full; delete old chats to make room"}
		}
	}
	view, err := s.chatView(ctx, owner, c)
	if err != nil {
		return nil, err
	}
	m := &store.ChatMessage{ChatID: c.ID, Text: text}
	if author != nil {
		m.AuthorSession, m.AuthorName = author.ID, author.Name
	}
	m.Mentions, m.Everyone = parseMentions(text, view.Members)
	if replyTo > 0 {
		parent, err := s.Store.Chats().Message(ctx, replyTo)
		if err != nil || parent.ChatID != c.ID {
			return nil, &ValidationError{"the message replied to is not in this chat"}
		}
		m.ReplyTo = replyTo
		if parent.AuthorSession != "" && !slices.Contains(m.Mentions, parent.AuthorSession) {
			if _, ok := view.membersBy[parent.AuthorSession]; ok {
				m.Mentions = append(m.Mentions, parent.AuthorSession)
			}
		}
	}
	authorID := ""
	if author != nil {
		authorID = author.ID
	}
	to := recipients(c, view.Members, authorID, m.Mentions, m.Everyone)
	if err := s.Store.Chats().Post(ctx, m, files, to); err != nil {
		return nil, err
	}
	if author != nil {
		_ = s.Store.Chats().SetRead(ctx, c.ID, author.ID, m.ID)
	}
	views, err := s.messageViews(ctx, owner, view, []*store.ChatMessage{m})
	if err != nil {
		return nil, err
	}
	mv := &views[0]
	if s.Broker != nil {
		s.Broker.Publish(c.OwnerID, Event{Type: "chat_message", ID: c.ID, Message: mv})
	}
	if s.Uplinks != nil {
		for _, sid := range to {
			s.Uplinks.Notify(sid)
		}
	}
	return mv, nil
}

// ---- agent side (through the uplink) ----

// AgentChat is a chat as an agent sees it.
type AgentChat struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Unread      int          `json:"unread"`
	Members     []MemberView `json:"members,omitempty"`
}

// agentChat resolves ref (a chat id or name) among the session's chats.
func (s *Service) agentChat(ctx context.Context, sess *store.Session, ref string) (*store.Chat, error) {
	ids, err := s.Store.Chats().ChatsOf(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	var byName *store.Chat
	for _, id := range ids {
		c, err := s.Store.Chats().Get(ctx, id)
		if err != nil {
			continue
		}
		if c.ID == ref {
			return c, nil
		}
		if strings.EqualFold(c.Name, ref) {
			byName = c
		}
	}
	if byName != nil {
		return byName, nil
	}
	return nil, &ValidationError{"you are not in a chat called " + ref + "; chats_list shows yours"}
}

func (s *Service) owner(ctx context.Context, sess *store.Session) *store.User {
	u, err := s.Store.Users().GetByID(ctx, sess.OwnerID)
	if err != nil {
		return &store.User{ID: sess.OwnerID, Username: "owner"}
	}
	return u
}

// AgentChats lists the chats a session is in, with unread counts.
func (s *Service) AgentChats(ctx context.Context, sess *store.Session) ([]AgentChat, error) {
	ids, err := s.Store.Chats().ChatsOf(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	out := []AgentChat{}
	for _, id := range ids {
		c, err := s.Store.Chats().Get(ctx, id)
		if err != nil {
			continue
		}
		n, err := s.Store.Chats().Unread(ctx, id, sess.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, AgentChat{ID: c.ID, Name: c.Name, Unread: n})
	}
	return out, nil
}

// AgentChatInfo is a chat's description and members, for chat_info.
func (s *Service) AgentChatInfo(ctx context.Context, sess *store.Session, ref string) (*AgentChat, error) {
	c, err := s.agentChat(ctx, sess, ref)
	if err != nil {
		return nil, err
	}
	v, err := s.chatView(ctx, s.owner(ctx, sess), c)
	if err != nil {
		return nil, err
	}
	n, _ := s.Store.Chats().Unread(ctx, c.ID, sess.ID)
	return &AgentChat{ID: c.ID, Name: c.Name, Description: c.Description, Unread: n, Members: v.Members}, nil
}

// AgentReadChat returns messages for chat_read and marks them read: the
// newest limit, or with since > 0 those after it.
func (s *Service) AgentReadChat(ctx context.Context, sess *store.Session, ref string, since int64, limit int) (*AgentChat, []MessageView, error) {
	c, err := s.agentChat(ctx, sess, ref)
	if err != nil {
		return nil, nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	owner := s.owner(ctx, sess)
	v, err := s.chatView(ctx, owner, c)
	if err != nil {
		return nil, nil, err
	}
	msgs, err := s.Store.Chats().Messages(ctx, c.ID, 0, since, limit)
	if err != nil {
		return nil, nil, err
	}
	views, err := s.messageViews(ctx, owner, v, msgs)
	if err != nil {
		return nil, nil, err
	}
	if len(msgs) > 0 {
		_ = s.Store.Chats().SetRead(ctx, c.ID, sess.ID, msgs[len(msgs)-1].ID)
	}
	return &AgentChat{ID: c.ID, Name: c.Name}, views, nil
}

// AgentPost posts as a session.
func (s *Service) AgentPost(ctx context.Context, sess *store.Session, ref, text string, replyTo int64, images []ChatFile) (*MessageView, error) {
	c, err := s.agentChat(ctx, sess, ref)
	if err != nil {
		return nil, err
	}
	return s.post(ctx, s.owner(ctx, sess), c, sess, text, replyTo, images)
}

// AgentAttachment returns an image of a chat the session is in.
func (s *Service) AgentAttachment(ctx context.Context, sess *store.Session, id string) (*store.ChatAttachment, []byte, error) {
	a, chatID, data, err := s.Store.Chats().Attachment(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	ids, err := s.Store.Chats().ChatsOf(ctx, sess.ID)
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(ids, chatID) {
		return nil, nil, store.ErrNotFound
	}
	return a, data, nil
}

// Delivery is a queued message prepared for a runner.
type Delivery struct {
	MessageView
	ChatName string `json:"chat_name"`
	// Address is AddressOpen, AddressDirect or AddressEveryone.
	Address string `json:"address"`
	// Earlier is how many unread messages of the chat were not delivered
	// to the session (others talking among themselves), for "read them if
	// you need context".
	Earlier int `json:"earlier"`
}

// QueuedDeliveries lists what a session still has to receive.
func (s *Service) QueuedDeliveries(ctx context.Context, sess *store.Session) ([]Delivery, error) {
	msgs, err := s.Store.Chats().Queued(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	owner := s.owner(ctx, sess)
	views := map[string]*ChatView{}
	queuedIn := map[string]int{}
	for _, m := range msgs {
		queuedIn[m.ChatID]++
	}
	earlier := map[string]int{}
	var out []Delivery
	for _, m := range msgs {
		v := views[m.ChatID]
		if v == nil {
			c, err := s.Store.Chats().Get(ctx, m.ChatID)
			if err != nil {
				continue
			}
			if v, err = s.chatView(ctx, owner, c); err != nil {
				return nil, err
			}
			views[m.ChatID] = v
		}
		mvs, err := s.messageViews(ctx, owner, v, []*store.ChatMessage{m})
		if err != nil {
			return nil, err
		}
		n, ok := earlier[m.ChatID]
		if !ok {
			unread, _ := s.Store.Chats().Unread(ctx, m.ChatID, sess.ID)
			n = max(0, unread-queuedIn[m.ChatID])
			earlier[m.ChatID] = n
		}
		out = append(out, Delivery{MessageView: mvs[0], ChatName: v.Name, Address: Address(m, sess.ID), Earlier: n})
	}
	return out, nil
}

// MarkDelivered records that the runner handed messages to its agent and
// tells the owner's UI.
func (s *Service) MarkDelivered(ctx context.Context, sess *store.Session, ids []int64) error {
	if err := s.Store.Chats().MarkDelivered(ctx, sess.ID, ids); err != nil {
		return err
	}
	owner := s.owner(ctx, sess)
	for _, id := range ids {
		m, err := s.Store.Chats().Message(ctx, id)
		if err != nil {
			continue
		}
		// The agent was handed the message (and told how many came before).
		_ = s.Store.Chats().SetRead(ctx, m.ChatID, sess.ID, m.ID)
		if s.Broker == nil {
			continue
		}
		c, err := s.Store.Chats().Get(ctx, m.ChatID)
		if err != nil {
			continue
		}
		v, err := s.chatView(ctx, owner, c)
		if err != nil {
			continue
		}
		if mvs, err := s.messageViews(ctx, owner, v, []*store.ChatMessage{m}); err == nil {
			s.Broker.Publish(sess.OwnerID, Event{Type: "chat_message", ID: c.ID, Message: &mvs[0]})
		}
	}
	return nil
}

// InChats reports whether a session is a member of any chat (it then
// needs an uplink while running).
func (s *Service) InChats(ctx context.Context, sessionID string) bool {
	ids, err := s.Store.Chats().ChatsOf(ctx, sessionID)
	return err == nil && len(ids) > 0
}
