package store

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Chats returns the chat aggregate.
func (p *Postgres) Chats() Chats { return pgChats{p.pool} }

type pgChats struct{ pool *pgxpool.Pool }

const chatCols = `id, owner_id, name, description, muted, created_at, updated_at`

func scanChat(row pgx.Row) (*Chat, error) {
	var c Chat
	if err := row.Scan(&c.ID, &c.OwnerID, &c.Name, &c.Description, &c.Muted, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &c, nil
}

func (r pgChats) Create(ctx context.Context, c *Chat) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	return mapErr(r.pool.QueryRow(ctx, `INSERT INTO chats (id, owner_id, name, description, muted) VALUES ($1,$2,$3,$4,$5)
		RETURNING created_at, updated_at`, c.ID, c.OwnerID, c.Name, c.Description, c.Muted).Scan(&c.CreatedAt, &c.UpdatedAt))
}

func (r pgChats) Get(ctx context.Context, id string) (*Chat, error) {
	return scanChat(r.pool.QueryRow(ctx, `SELECT `+chatCols+` FROM chats WHERE id::text=$1`, id))
}

func (r pgChats) List(ctx context.Context, ownerID string) ([]*Chat, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+chatCols+` FROM chats WHERE owner_id=$1 ORDER BY created_at`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, mapErr(rows.Err())
}

func (r pgChats) Update(ctx context.Context, c *Chat) error {
	got, err := scanChat(r.pool.QueryRow(ctx, `UPDATE chats SET name=$2, description=$3, muted=$4, updated_at=now() WHERE id=$1 RETURNING `+chatCols,
		c.ID, c.Name, c.Description, c.Muted))
	if err != nil {
		return err
	}
	*c = *got
	return nil
}

func (r pgChats) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM chats WHERE id::text=$1`, id)
	return mapErr(err)
}

func (r pgChats) AddMember(ctx context.Context, chatID, sessionID string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO chat_members (chat_id, session_id) VALUES ($1,$2)`, chatID, sessionID)
	return mapErr(err)
}

func (r pgChats) RemoveMember(ctx context.Context, chatID, sessionID string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM chat_members WHERE chat_id=$1 AND session_id::text=$2`, chatID, sessionID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// What it had not received yet is no longer its business.
	_, err = r.pool.Exec(ctx, `DELETE FROM chat_deliveries d USING chat_messages m
		WHERE d.message_id=m.id AND m.chat_id=$1 AND d.session_id::text=$2 AND d.state=$3`, chatID, sessionID, DeliveryQueued)
	return mapErr(err)
}

func (r pgChats) SetMemberMuted(ctx context.Context, chatID, sessionID string, muted bool) error {
	tag, err := r.pool.Exec(ctx, `UPDATE chat_members SET muted=$3 WHERE chat_id=$1 AND session_id::text=$2`, chatID, sessionID, muted)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r pgChats) Members(ctx context.Context, chatID string) ([]*ChatMember, error) {
	rows, err := r.pool.Query(ctx, `SELECT chat_id, session_id, muted, added_at FROM chat_members WHERE chat_id=$1 ORDER BY added_at, session_id`, chatID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*ChatMember
	for rows.Next() {
		var m ChatMember
		if err := rows.Scan(&m.ChatID, &m.SessionID, &m.Muted, &m.AddedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, &m)
	}
	return out, mapErr(rows.Err())
}

func (r pgChats) ChatsOf(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT chat_id::text FROM chat_members WHERE session_id::text=$1 ORDER BY added_at`, sessionID)
	if err != nil {
		return nil, mapErr(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return ids, mapErr(err)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (r pgChats) Post(ctx context.Context, m *ChatMessage, files []NewAttachment, deliverTo []string) error {
	if m.Mentions == nil {
		m.Mentions = []string{}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var replyTo any
	if m.ReplyTo > 0 {
		replyTo = m.ReplyTo
	}
	if err := tx.QueryRow(ctx, `INSERT INTO chat_messages (chat_id, author_session, author_name, text, mentions, everyone, reply_to)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
		m.ChatID, nullable(m.AuthorSession), m.AuthorName, m.Text, m.Mentions, m.Everyone, replyTo).Scan(&m.ID, &m.CreatedAt); err != nil {
		return mapErr(err)
	}
	m.Attachments = nil
	for _, f := range files {
		a := ChatAttachment{ID: NewID(), MessageID: m.ID, Name: f.Name, ContentType: f.ContentType, Size: len(f.Data)}
		if _, err := tx.Exec(ctx, `INSERT INTO chat_attachments (id, message_id, chat_id, owner_id, name, content_type, size, data)
			VALUES ($1,$2,$3,(SELECT owner_id FROM chats WHERE id=$3),$4,$5,$6,$7)`,
			a.ID, m.ID, m.ChatID, a.Name, a.ContentType, a.Size, f.Data); err != nil {
			return mapErr(err)
		}
		m.Attachments = append(m.Attachments, a)
	}
	for _, sid := range deliverTo {
		if _, err := tx.Exec(ctx, `INSERT INTO chat_deliveries (message_id, session_id, state) VALUES ($1,$2,$3)`, m.ID, sid, DeliveryQueued); err != nil {
			return mapErr(err)
		}
	}
	return mapErr(tx.Commit(ctx))
}

const messageCols = `id, chat_id, COALESCE(author_session::text, ''), author_name, text, mentions, everyone, COALESCE(reply_to, 0), created_at`

func (r pgChats) scanMessages(ctx context.Context, rows pgx.Rows) ([]*ChatMessage, error) {
	defer rows.Close()
	var out []*ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.AuthorSession, &m.AuthorName, &m.Text, &m.Mentions, &m.Everyone, &m.ReplyTo, &m.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, r.fillAttachments(ctx, out)
}

func (r pgChats) fillAttachments(ctx context.Context, msgs []*ChatMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]int64, len(msgs))
	byID := map[int64]*ChatMessage{}
	for i, m := range msgs {
		ids[i] = m.ID
		byID[m.ID] = m
	}
	rows, err := r.pool.Query(ctx, `SELECT id, message_id, name, content_type, size FROM chat_attachments WHERE message_id = ANY($1) ORDER BY message_id, id`, ids)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a ChatAttachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Name, &a.ContentType, &a.Size); err != nil {
			return mapErr(err)
		}
		if m := byID[a.MessageID]; m != nil {
			m.Attachments = append(m.Attachments, a)
		}
	}
	return mapErr(rows.Err())
}

func (r pgChats) Messages(ctx context.Context, chatID string, before, after int64, limit int) ([]*ChatMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows pgx.Rows
	var err error
	if after > 0 {
		rows, err = r.pool.Query(ctx, `SELECT `+messageCols+` FROM chat_messages WHERE chat_id::text=$1 AND id>$2 ORDER BY id LIMIT $3`, chatID, after, limit)
	} else {
		if before <= 0 {
			before = 1<<63 - 1
		}
		rows, err = r.pool.Query(ctx, `SELECT `+messageCols+` FROM chat_messages WHERE chat_id::text=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, chatID, before, limit)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := r.scanMessages(ctx, rows)
	if err != nil {
		return nil, err
	}
	if after <= 0 {
		slices.Reverse(out)
	}
	return out, nil
}

func (r pgChats) Message(ctx context.Context, id int64) (*ChatMessage, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+messageCols+` FROM chat_messages WHERE id=$1`, id)
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := r.scanMessages(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out[0], nil
}

func (r pgChats) Attachment(ctx context.Context, id string) (*ChatAttachment, string, []byte, error) {
	var a ChatAttachment
	var chatID string
	var data []byte
	err := r.pool.QueryRow(ctx, `SELECT id, message_id, chat_id::text, name, content_type, size, data FROM chat_attachments WHERE id::text=$1`, id).
		Scan(&a.ID, &a.MessageID, &chatID, &a.Name, &a.ContentType, &a.Size, &data)
	if err != nil {
		return nil, "", nil, mapErr(err)
	}
	return &a, chatID, data, nil
}

func (r pgChats) AttachmentBytes(ctx context.Context, ownerID string) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(size), 0) FROM chat_attachments WHERE owner_id=$1`, ownerID).Scan(&n)
	return n, mapErr(err)
}

func (r pgChats) Queued(ctx context.Context, sessionID string) ([]*ChatMessage, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+messageCols+` FROM chat_messages WHERE id IN
		(SELECT message_id FROM chat_deliveries WHERE session_id::text=$1 AND state=$2) ORDER BY id LIMIT 200`, sessionID, DeliveryQueued)
	if err != nil {
		return nil, mapErr(err)
	}
	return r.scanMessages(ctx, rows)
}

func (r pgChats) MarkDelivered(ctx context.Context, sessionID string, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `UPDATE chat_deliveries SET state=$3, delivered_at=now() WHERE session_id::text=$1 AND message_id = ANY($2) AND state=$4`,
		sessionID, messageIDs, DeliveryDelivered, DeliveryQueued)
	return mapErr(err)
}

func (r pgChats) Deliveries(ctx context.Context, messageIDs []int64) ([]ChatDelivery, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT message_id, session_id::text, state FROM chat_deliveries WHERE message_id = ANY($1) ORDER BY message_id`, messageIDs)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []ChatDelivery
	for rows.Next() {
		var d ChatDelivery
		if err := rows.Scan(&d.MessageID, &d.SessionID, &d.State); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, d)
	}
	return out, mapErr(rows.Err())
}

func (r pgChats) SetRead(ctx context.Context, chatID, reader string, lastID int64) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO chat_reads (chat_id, reader, last_message_id) VALUES ($1,$2,$3)
		ON CONFLICT (chat_id, reader) DO UPDATE SET last_message_id=GREATEST(chat_reads.last_message_id, EXCLUDED.last_message_id)`, chatID, reader, lastID)
	return mapErr(err)
}

func (r pgChats) Read(ctx context.Context, chatID, reader string) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT last_message_id FROM chat_reads WHERE chat_id::text=$1 AND reader=$2`, chatID, reader).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, mapErr(err)
}

func (r pgChats) Unread(ctx context.Context, chatID, reader string) (int, error) {
	last, err := r.Read(ctx, chatID, reader)
	if err != nil {
		return 0, err
	}
	var n int
	if reader == OwnerReader {
		err = r.pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages WHERE chat_id::text=$1 AND id>$2 AND author_session IS NOT NULL`, chatID, last).Scan(&n)
	} else {
		err = r.pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages WHERE chat_id::text=$1 AND id>$2 AND (author_session IS NULL OR author_session::text<>$3)`,
			chatID, last, reader).Scan(&n)
	}
	return n, mapErr(err)
}
