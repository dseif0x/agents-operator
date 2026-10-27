package store

import (
	"context"
	"time"
)

// Chat is a group of the owner's sessions talking to each other and to the
// owner, like a group chat.
type Chat struct {
	ID          string
	OwnerID     string
	Name        string
	Description string
	// Muted chats deliver nothing to their agents.
	Muted     bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChatMember is a session in a chat.
type ChatMember struct {
	ChatID    string
	SessionID string
	// Muted members get nothing delivered, and their messages are not
	// delivered to other agents.
	Muted   bool
	AddedAt time.Time
}

// ChatMessage is one message. AuthorSession is empty for the owner.
type ChatMessage struct {
	ID            int64
	ChatID        string
	AuthorSession string
	// AuthorName is the author's session name when the message was posted,
	// shown once the session is gone.
	AuthorName string
	Text       string
	// Mentions are the session ids @mentioned (or replied to).
	Mentions []string
	// Everyone is set by @everyone: every agent is asked to answer.
	Everyone    bool
	ReplyTo     int64
	Attachments []ChatAttachment
	CreatedAt   time.Time
}

// ChatAttachment describes an image of a message; the bytes are fetched
// separately (Chats.Attachment).
type ChatAttachment struct {
	ID          string `json:"id"`
	MessageID   int64  `json:"-"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

// NewAttachment is an image to store with a new message.
type NewAttachment struct {
	Name        string
	ContentType string
	Data        []byte
}

// Delivery states of a message to a session.
const (
	DeliveryQueued    = "queued"
	DeliveryDelivered = "delivered"
)

// ChatDelivery is where a message stands for one recipient.
type ChatDelivery struct {
	MessageID int64
	SessionID string
	State     string
}

// OwnerReader is the reader id of the chat's owner in read markers.
const OwnerReader = "owner"

// Chats is the chat aggregate. Ownership is checked by the service.
type Chats interface {
	Create(ctx context.Context, c *Chat) error
	Get(ctx context.Context, id string) (*Chat, error)
	List(ctx context.Context, ownerID string) ([]*Chat, error)
	// Update replaces name, description and muted.
	Update(ctx context.Context, c *Chat) error
	Delete(ctx context.Context, id string) error

	// AddMember is ErrConflict when the session is in the chat already.
	AddMember(ctx context.Context, chatID, sessionID string) error
	RemoveMember(ctx context.Context, chatID, sessionID string) error
	SetMemberMuted(ctx context.Context, chatID, sessionID string, muted bool) error
	Members(ctx context.Context, chatID string) ([]*ChatMember, error)
	// ChatsOf lists the ids of the chats a session is in.
	ChatsOf(ctx context.Context, sessionID string) ([]string, error)

	// Post stores m with its attachments and queues it for every session
	// in deliverTo, in one transaction. It fills m.ID, m.CreatedAt and
	// m.Attachments.
	Post(ctx context.Context, m *ChatMessage, files []NewAttachment, deliverTo []string) error
	// Messages returns up to limit messages, oldest first: the newest ones
	// before id `before` (0: the newest), or with after > 0 the oldest ones
	// after id `after`.
	Messages(ctx context.Context, chatID string, before, after int64, limit int) ([]*ChatMessage, error)
	Message(ctx context.Context, id int64) (*ChatMessage, error)
	// Attachment returns an image's description, chat and bytes.
	Attachment(ctx context.Context, id string) (*ChatAttachment, string, []byte, error)
	// AttachmentBytes is the total size of an owner's images.
	AttachmentBytes(ctx context.Context, ownerID string) (int64, error)

	// Queued lists the messages still to be delivered to a session, oldest first.
	Queued(ctx context.Context, sessionID string) ([]*ChatMessage, error)
	MarkDelivered(ctx context.Context, sessionID string, messageIDs []int64) error
	Deliveries(ctx context.Context, messageIDs []int64) ([]ChatDelivery, error)

	// SetRead moves a reader's marker forward to lastID (never back).
	SetRead(ctx context.Context, chatID, reader string, lastID int64) error
	Read(ctx context.Context, chatID, reader string) (int64, error)
	// Unread counts messages after the reader's marker not written by it
	// (reader OwnerReader or a session id).
	Unread(ctx context.Context, chatID, reader string) (int, error)
}
