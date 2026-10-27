package store

import (
	"context"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests. It is not used in production.
type Memory struct {
	mu          sync.Mutex
	users       map[string]*User
	sessions    map[string]*Session
	credentials map[string]map[string]*Credential
	accounts    []*Account
	profiles    map[string]*Profile
	events      map[string][]*Event
	usage       map[string]map[string]int
	nextEvent   int64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		users:       map[string]*User{},
		sessions:    map[string]*Session{},
		credentials: map[string]map[string]*Credential{},
		profiles:    map[string]*Profile{},
		events:      map[string][]*Event{},
		usage:       map[string]map[string]int{},
	}
}

func (m *Memory) RepoUsage() RepoUsage { return memRepoUsage{m} }

type memRepoUsage struct{ m *Memory }

func (r memRepoUsage) Increment(_ context.Context, userID, repoKey string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.usage[userID] == nil {
		r.m.usage[userID] = map[string]int{}
	}
	r.m.usage[userID][repoKey]++
	return nil
}

func (r memRepoUsage) List(_ context.Context, userID string) (map[string]int, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	out := map[string]int{}
	for k, v := range r.m.usage[userID] {
		out[k] = v
	}
	return out, nil
}

func (m *Memory) Users() Users             { return memUsers{m} }
func (m *Memory) Sessions() Sessions       { return memSessions{m} }
func (m *Memory) Credentials() Credentials { return memCredentials{m} }
func (m *Memory) Accounts() Accounts       { return memAccounts{m} }
func (m *Memory) Profiles() Profiles       { return memProfiles{m} }
func (m *Memory) Events() Events           { return memEvents{m} }
func (m *Memory) Ping(context.Context) error {
	return nil
}
func (m *Memory) Close() {}

func copySession(s *Session) *Session {
	c := *s
	return &c
}

type memUsers struct{ m *Memory }

func (r memUsers) Create(_ context.Context, u *User) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if u.ID == "" {
		u.ID = NewID()
	}
	for _, x := range r.m.users {
		if x.Username == u.Username {
			return ErrConflict
		}
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	c := *u
	r.m.users[u.ID] = &c
	return nil
}

func (r memUsers) GetByID(_ context.Context, id string) (*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if u, ok := r.m.users[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, ErrNotFound
}

func (r memUsers) GetByUsername(_ context.Context, username string) (*User, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, u := range r.m.users {
		if u.Username == username {
			c := *u
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (r memUsers) UpsertPassword(ctx context.Context, username, hash string) (*User, error) {
	r.m.mu.Lock()
	for _, u := range r.m.users {
		if u.Username == username {
			u.PasswordHash = hash
			c := *u
			r.m.mu.Unlock()
			return &c, nil
		}
	}
	r.m.mu.Unlock()
	u := &User{Username: username, PasswordHash: hash}
	if err := r.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

type memSessions struct{ m *Memory }

func (r memSessions) Create(_ context.Context, s *Session) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if s.ID == "" {
		s.ID = NewID()
	}
	if s.Generation == 0 {
		s.Generation = 1
	}
	if s.Handle != "" && r.handleTaken(s.OwnerID, s.Handle) {
		return ErrConflict
	}
	now := time.Now()
	s.CreatedAt, s.UpdatedAt = now, now
	normaliseK8s(s)
	r.m.sessions[s.ID] = copySession(s)
	return nil
}

// handleTaken mirrors the unique index on (owner_id, handle).
func (r memSessions) handleTaken(ownerID, handle string) bool {
	for _, o := range r.m.sessions {
		if o.OwnerID == ownerID && o.Handle == handle {
			return true
		}
	}
	return false
}

func (r memSessions) SetHandle(_ context.Context, id, handle string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	s, ok := r.m.sessions[id]
	if !ok || s.Handle != "" {
		return ErrNotFound
	}
	if r.handleTaken(s.OwnerID, handle) {
		return ErrConflict
	}
	s.Handle = handle
	return nil
}

func (r memSessions) Get(_ context.Context, id string) (*Session, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if s, ok := r.m.sessions[id]; ok {
		return copySession(s), nil
	}
	return nil, ErrNotFound
}

func (r memSessions) list(filter func(*Session) bool) []*Session {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []*Session
	for _, s := range r.m.sessions {
		if filter(s) {
			out = append(out, copySession(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (r memSessions) List(_ context.Context, ownerID string) ([]*Session, error) {
	return r.list(func(s *Session) bool { return s.OwnerID == ownerID }), nil
}

func (r memSessions) ListAll(context.Context) ([]*Session, error) {
	return r.list(func(*Session) bool { return true }), nil
}

func (r memSessions) Update(_ context.Context, s *Session) (*Session, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	cur, ok := r.m.sessions[s.ID]
	if !ok {
		return nil, ErrNotFound
	}
	cur.Name, cur.Repos, cur.ImageTag, cur.RuntimeClass = s.Name, s.Repos, s.ImageTag, s.RuntimeClass
	cur.K8sAccess, cur.K8sNamespaces = s.K8sAccess, append([]string(nil), s.K8sNamespaces...)
	normaliseK8s(cur)
	cur.Resources, cur.NodeSelector, cur.Tolerations, cur.Env, cur.Autonomous = s.Resources, s.NodeSelector, s.Tolerations, s.Env, s.Autonomous
	cur.NoGitCredentials = s.NoGitCredentials
	cur.Egress = append([]string{}, s.Egress...)
	cur.Persona, cur.AgentDefinition = s.Persona, s.AgentDefinition
	cur.AIAccount = s.AIAccount
	cur.UpdatedAt = time.Now()
	return copySession(cur), nil
}

func (r memSessions) SetState(_ context.Context, id, state, reason string) (*Session, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	s, ok := r.m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	s.State, s.StateReason, s.UpdatedAt = state, reason, time.Now()
	if state == StateDeleting {
		t := time.Now()
		s.DeletedAt = &t
	}
	return copySession(s), nil
}

func (r memSessions) Bump(_ context.Context, id, state string) (*Session, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	s, ok := r.m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	s.State, s.StateReason, s.UpdatedAt = state, "", time.Now()
	s.Generation++
	return copySession(s), nil
}

func (r memSessions) TouchAttached(_ context.Context, id string, at time.Time) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if s, ok := r.m.sessions[id]; ok {
		s.LastAttachedAt = &at
		return nil
	}
	return ErrNotFound
}

func (r memSessions) TouchOutput(_ context.Context, id string, at time.Time) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if s, ok := r.m.sessions[id]; ok {
		if s.LastOutputAt == nil || s.LastOutputAt.Before(at) {
			s.LastOutputAt = &at
		}
		return nil
	}
	return ErrNotFound
}

func (r memSessions) Delete(_ context.Context, id string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	delete(r.m.sessions, id)
	delete(r.m.events, id)
	return nil
}

func (r memSessions) CountByState(context.Context) (map[string]int, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	out := map[string]int{}
	for _, s := range r.m.sessions {
		out[s.State]++
	}
	return out, nil
}

type memCredentials struct{ m *Memory }

func (r memCredentials) Upsert(_ context.Context, c *Credential) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.credentials[c.UserID] == nil {
		r.m.credentials[c.UserID] = map[string]*Credential{}
	}
	cc := *c
	cc.UpdatedAt = time.Now()
	r.m.credentials[c.UserID][c.Kind] = &cc
	return nil
}

func (r memCredentials) Delete(_ context.Context, userID, kind string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if m := r.m.credentials[userID]; m != nil {
		delete(m, kind)
	}
	return nil
}

func (r memCredentials) List(_ context.Context, userID string) ([]*Credential, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []*Credential
	for _, c := range r.m.credentials[userID] {
		cc := *c
		out = append(out, &cc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}

func (r memCredentials) UserIDs(context.Context) ([]string, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []string
	for id, m := range r.m.credentials {
		if len(m) > 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

type memAccounts struct{ m *Memory }

func copyAccount(a *Account) *Account {
	c := *a
	c.Models = append([]string{}, a.Models...)
	return &c
}

// conflicts mirrors the unique indexes: one git account per user, kind and
// host; AI account names unique per user.
func (r memAccounts) conflicts(a *Account) bool {
	for _, o := range r.m.accounts {
		if o.UserID != a.UserID || o.ID == a.ID {
			continue
		}
		if GitAccountKind(a.Kind) && o.Kind == a.Kind && strings.EqualFold(o.Host, a.Host) {
			return true
		}
		if !GitAccountKind(a.Kind) && !GitAccountKind(o.Kind) && strings.EqualFold(o.Name, a.Name) {
			return true
		}
	}
	return false
}

func (r memAccounts) Create(_ context.Context, a *Account) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if a.ID == "" {
		a.ID = NewID()
	}
	if r.conflicts(a) {
		return ErrConflict
	}
	// Strictly increasing, so List keeps creation order within a test.
	now := time.Now()
	if n := len(r.m.accounts); n > 0 && !now.After(r.m.accounts[n-1].CreatedAt) {
		now = r.m.accounts[n-1].CreatedAt.Add(time.Microsecond)
	}
	a.CreatedAt, a.UpdatedAt = now, now
	r.m.accounts = append(r.m.accounts, copyAccount(a))
	return nil
}

func (r memAccounts) Update(_ context.Context, a *Account) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, o := range r.m.accounts {
		if o.UserID == a.UserID && o.ID == a.ID {
			if r.conflicts(&Account{ID: a.ID, UserID: a.UserID, Kind: o.Kind, Name: a.Name, Host: a.Host}) {
				return ErrConflict
			}
			o.Name, o.Host, o.BaseURL, o.Username, o.Models = a.Name, a.Host, a.BaseURL, a.Username, append([]string{}, a.Models...)
			o.UpdatedAt = time.Now()
			if !o.UpdatedAt.After(o.CreatedAt) {
				o.UpdatedAt = o.CreatedAt.Add(time.Microsecond)
			}
			*a = *copyAccount(o)
			return nil
		}
	}
	return ErrNotFound
}

func (r memAccounts) Get(_ context.Context, userID, id string) (*Account, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, o := range r.m.accounts {
		if o.UserID == userID && o.ID == id {
			return copyAccount(o), nil
		}
	}
	return nil, ErrNotFound
}

func (r memAccounts) List(_ context.Context, userID string) ([]*Account, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []*Account
	for _, o := range r.m.accounts {
		if o.UserID == userID {
			out = append(out, copyAccount(o))
		}
	}
	return out, nil
}

func (r memAccounts) Delete(_ context.Context, userID, id string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for i, o := range r.m.accounts {
		if o.UserID == userID && o.ID == id {
			r.m.accounts = append(r.m.accounts[:i], r.m.accounts[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

type memProfiles struct{ m *Memory }

func (r memProfiles) Get(_ context.Context, userID string) (*Profile, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	p := Profile{UserID: userID, Defaults: map[string]string{}}
	if cur := r.m.profiles[userID]; cur != nil {
		p = *cur
		p.Defaults = maps.Clone(cur.Defaults)
	}
	return &p, nil
}

func (r memProfiles) Put(_ context.Context, p *Profile) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	c := *p
	c.Defaults = maps.Clone(p.Defaults)
	if c.Defaults == nil {
		c.Defaults = map[string]string{}
	}
	c.UpdatedAt = time.Now()
	r.m.profiles[p.UserID] = &c
	return nil
}

type memEvents struct{ m *Memory }

func (r memEvents) Add(_ context.Context, sessionID, kind, message string) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.m.nextEvent++
	r.m.events[sessionID] = append(r.m.events[sessionID], &Event{ID: r.m.nextEvent, SessionID: sessionID, At: time.Now(), Kind: kind, Message: message})
	return nil
}

func (r memEvents) List(_ context.Context, sessionID string, limit int) ([]*Event, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	evs := r.m.events[sessionID]
	if limit <= 0 {
		limit = EventsKeep
	}
	var out []*Event
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		e := *evs[i]
		out = append(out, &e)
	}
	return out, nil
}

func (r memEvents) Prune(_ context.Context, sessionID string, keep int) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	evs := r.m.events[sessionID]
	if len(evs) > keep {
		r.m.events[sessionID] = evs[len(evs)-keep:]
	}
	return nil
}

var _ Store = (*Memory)(nil)
var _ Store = (*Postgres)(nil)
