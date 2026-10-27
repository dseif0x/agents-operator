package store

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"
)

// memChatData is the in-memory chat state, kept beside Memory's maps.
type memChatData struct {
	mu          sync.Mutex
	chats       map[string]*Chat
	members     map[string]map[string]*ChatMember // chat -> session -> member
	messages    []*ChatMessage
	attachments map[string]memAttachment
	deliveries  map[int64]map[string]string // message -> session -> state
	reads       map[string]map[string]int64 // chat -> reader -> last id
	nextID      int64
}

type memAttachment struct {
	ChatAttachment
	chatID  string
	ownerID string
	data    []byte
}

func newMemChatData() *memChatData {
	return &memChatData{
		chats: map[string]*Chat{}, members: map[string]map[string]*ChatMember{}, attachments: map[string]memAttachment{},
		deliveries: map[int64]map[string]string{}, reads: map[string]map[string]int64{},
	}
}

// Chats returns the chat aggregate.
func (m *Memory) Chats() Chats { return memChats{m: m, d: m.chat} }

type memChats struct {
	m *Memory
	d *memChatData
}

func copyMessage(msg *ChatMessage) *ChatMessage {
	c := *msg
	c.Mentions = append([]string{}, msg.Mentions...)
	c.Attachments = append([]ChatAttachment(nil), msg.Attachments...)
	return &c
}

func (r memChats) Create(_ context.Context, c *Chat) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if c.ID == "" {
		c.ID = NewID()
	}
	c.CreatedAt, c.UpdatedAt = time.Now(), time.Now()
	cc := *c
	r.d.chats[c.ID] = &cc
	return nil
}

func (r memChats) Get(_ context.Context, id string) (*Chat, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	c, ok := r.d.chats[id]
	if !ok {
		return nil, ErrNotFound
	}
	cc := *c
	return &cc, nil
}

func (r memChats) List(_ context.Context, ownerID string) ([]*Chat, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*Chat
	for _, c := range r.d.chats {
		if c.OwnerID == ownerID {
			cc := *c
			out = append(out, &cc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r memChats) Update(_ context.Context, c *Chat) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	cur, ok := r.d.chats[c.ID]
	if !ok {
		return ErrNotFound
	}
	cur.Name, cur.Description, cur.Muted, cur.UpdatedAt = c.Name, c.Description, c.Muted, time.Now()
	*c = *cur
	return nil
}

func (r memChats) Delete(_ context.Context, id string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	delete(r.d.chats, id)
	delete(r.d.members, id)
	delete(r.d.reads, id)
	r.d.messages = slices.DeleteFunc(r.d.messages, func(m *ChatMessage) bool {
		if m.ChatID == id {
			delete(r.d.deliveries, m.ID)
			return true
		}
		return false
	})
	for k, a := range r.d.attachments {
		if a.chatID == id {
			delete(r.d.attachments, k)
		}
	}
	return nil
}

func (r memChats) AddMember(_ context.Context, chatID, sessionID string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if r.d.members[chatID] == nil {
		r.d.members[chatID] = map[string]*ChatMember{}
	}
	if _, ok := r.d.members[chatID][sessionID]; ok {
		return ErrConflict
	}
	r.d.members[chatID][sessionID] = &ChatMember{ChatID: chatID, SessionID: sessionID, AddedAt: time.Now()}
	return nil
}

func (r memChats) RemoveMember(_ context.Context, chatID, sessionID string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if _, ok := r.d.members[chatID][sessionID]; !ok {
		return ErrNotFound
	}
	delete(r.d.members[chatID], sessionID)
	for _, m := range r.d.messages {
		if m.ChatID == chatID && r.d.deliveries[m.ID][sessionID] == DeliveryQueued {
			delete(r.d.deliveries[m.ID], sessionID)
		}
	}
	return nil
}

func (r memChats) SetMemberMuted(_ context.Context, chatID, sessionID string, muted bool) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	m, ok := r.d.members[chatID][sessionID]
	if !ok {
		return ErrNotFound
	}
	m.Muted = muted
	return nil
}

// sessionExists: members whose session is gone are left out, as the
// Postgres foreign key cascades.
func (r memChats) sessionExists(id string) bool {
	_, err := r.m.Sessions().Get(context.Background(), id)
	return err == nil
}

func (r memChats) Members(_ context.Context, chatID string) ([]*ChatMember, error) {
	r.d.mu.Lock()
	var out []*ChatMember
	for _, m := range r.d.members[chatID] {
		mm := *m
		out = append(out, &mm)
	}
	r.d.mu.Unlock()
	out = slices.DeleteFunc(out, func(m *ChatMember) bool { return !r.sessionExists(m.SessionID) })
	sort.Slice(out, func(i, j int) bool {
		if !out[i].AddedAt.Equal(out[j].AddedAt) {
			return out[i].AddedAt.Before(out[j].AddedAt)
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}

func (r memChats) ChatsOf(_ context.Context, sessionID string) ([]string, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []string
	for chatID, ms := range r.d.members {
		if _, ok := ms[sessionID]; ok {
			out = append(out, chatID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r memChats) Post(_ context.Context, m *ChatMessage, files []NewAttachment, deliverTo []string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	c, ok := r.d.chats[m.ChatID]
	if !ok {
		return ErrNotFound
	}
	r.d.nextID++
	m.ID, m.CreatedAt = r.d.nextID, time.Now()
	if m.Mentions == nil {
		m.Mentions = []string{}
	}
	m.Attachments = nil
	for _, f := range files {
		a := ChatAttachment{ID: NewID(), MessageID: m.ID, Name: f.Name, ContentType: f.ContentType, Size: len(f.Data)}
		r.d.attachments[a.ID] = memAttachment{ChatAttachment: a, chatID: m.ChatID, ownerID: c.OwnerID, data: append([]byte(nil), f.Data...)}
		m.Attachments = append(m.Attachments, a)
	}
	r.d.messages = append(r.d.messages, copyMessage(m))
	for _, sid := range deliverTo {
		if r.d.deliveries[m.ID] == nil {
			r.d.deliveries[m.ID] = map[string]string{}
		}
		r.d.deliveries[m.ID][sid] = DeliveryQueued
	}
	return nil
}

func (r memChats) Messages(_ context.Context, chatID string, before, after int64, limit int) ([]*ChatMessage, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var all []*ChatMessage
	for _, m := range r.d.messages {
		if m.ChatID != chatID {
			continue
		}
		if after > 0 && m.ID <= after || after <= 0 && before > 0 && m.ID >= before {
			continue
		}
		all = append(all, copyMessage(m))
	}
	if after > 0 {
		if len(all) > limit {
			all = all[:limit]
		}
		return all, nil
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func (r memChats) Message(_ context.Context, id int64) (*ChatMessage, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	for _, m := range r.d.messages {
		if m.ID == id {
			return copyMessage(m), nil
		}
	}
	return nil, ErrNotFound
}

func (r memChats) Attachment(_ context.Context, id string) (*ChatAttachment, string, []byte, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	a, ok := r.d.attachments[id]
	if !ok {
		return nil, "", nil, ErrNotFound
	}
	ca := a.ChatAttachment
	return &ca, a.chatID, append([]byte(nil), a.data...), nil
}

func (r memChats) AttachmentBytes(_ context.Context, ownerID string) (int64, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var n int64
	for _, a := range r.d.attachments {
		if a.ownerID == ownerID {
			n += int64(a.Size)
		}
	}
	return n, nil
}

func (r memChats) Queued(_ context.Context, sessionID string) ([]*ChatMessage, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*ChatMessage
	for _, m := range r.d.messages {
		if r.d.deliveries[m.ID][sessionID] == DeliveryQueued {
			out = append(out, copyMessage(m))
		}
	}
	return out, nil
}

func (r memChats) MarkDelivered(_ context.Context, sessionID string, ids []int64) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	for _, id := range ids {
		if r.d.deliveries[id][sessionID] == DeliveryQueued {
			r.d.deliveries[id][sessionID] = DeliveryDelivered
		}
	}
	return nil
}

func (r memChats) Deliveries(_ context.Context, ids []int64) ([]ChatDelivery, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []ChatDelivery
	for _, id := range ids {
		var sids []string
		for sid := range r.d.deliveries[id] {
			sids = append(sids, sid)
		}
		sort.Strings(sids)
		for _, sid := range sids {
			out = append(out, ChatDelivery{MessageID: id, SessionID: sid, State: r.d.deliveries[id][sid]})
		}
	}
	return out, nil
}

func (r memChats) SetRead(_ context.Context, chatID, reader string, lastID int64) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if r.d.reads[chatID] == nil {
		r.d.reads[chatID] = map[string]int64{}
	}
	if lastID > r.d.reads[chatID][reader] {
		r.d.reads[chatID][reader] = lastID
	}
	return nil
}

func (r memChats) Read(_ context.Context, chatID, reader string) (int64, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	return r.d.reads[chatID][reader], nil
}

func (r memChats) Unread(_ context.Context, chatID, reader string) (int, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	last := r.d.reads[chatID][reader]
	n := 0
	for _, m := range r.d.messages {
		if m.ChatID != chatID || m.ID <= last {
			continue
		}
		if reader == OwnerReader && m.AuthorSession == "" || reader != OwnerReader && m.AuthorSession == reader {
			continue
		}
		n++
	}
	return n, nil
}
