package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PushSubscription is a browser's Web Push subscription.
type PushSubscription struct {
	Endpoint  string
	UserID    string
	P256dh    string
	Auth      string
	UserAgent string
	CreatedAt time.Time
}

// PushSubscriptions is the push_subscriptions aggregate.
type PushSubscriptions interface {
	// Save stores s, replacing a subscription with the same endpoint.
	Save(ctx context.Context, s *PushSubscription) error
	List(ctx context.Context, userID string) ([]*PushSubscription, error)
	// Delete removes an endpoint; userID "" deletes it for anyone (a push
	// service said it is gone).
	Delete(ctx context.Context, userID, endpoint string) error
}

// Settings holds hub-wide values that must survive restarts.
type Settings interface {
	// Get returns "" for a key never set.
	Get(ctx context.Context, key string) (string, error)
	// SetIfAbsent stores value unless the key exists, and returns the value
	// now stored (so concurrent hubs agree on the first one written).
	SetIfAbsent(ctx context.Context, key, value string) (string, error)
}

// ---- Postgres ----

// Push returns the push subscription aggregate.
func (p *Postgres) Push() PushSubscriptions { return pgPush{p.pool} }

// Settings returns the settings aggregate.
func (p *Postgres) Settings() Settings { return pgSettings{p.pool} }

type pgPush struct{ pool *pgxpool.Pool }

func (r pgPush) Save(ctx context.Context, s *PushSubscription) error {
	err := r.pool.QueryRow(ctx, `INSERT INTO push_subscriptions (endpoint, user_id, p256dh, auth, user_agent) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (endpoint) DO UPDATE SET user_id=EXCLUDED.user_id, p256dh=EXCLUDED.p256dh, auth=EXCLUDED.auth, user_agent=EXCLUDED.user_agent
		RETURNING created_at`, s.Endpoint, s.UserID, s.P256dh, s.Auth, s.UserAgent).Scan(&s.CreatedAt)
	return mapErr(err)
}

func (r pgPush) List(ctx context.Context, userID string) ([]*PushSubscription, error) {
	rows, err := r.pool.Query(ctx, `SELECT endpoint, user_id, p256dh, auth, user_agent, created_at FROM push_subscriptions WHERE user_id=$1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*PushSubscription
	for rows.Next() {
		var s PushSubscription
		if err := rows.Scan(&s.Endpoint, &s.UserID, &s.P256dh, &s.Auth, &s.UserAgent, &s.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, &s)
	}
	return out, mapErr(rows.Err())
}

func (r pgPush) Delete(ctx context.Context, userID, endpoint string) error {
	var err error
	if userID == "" {
		_, err = r.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint=$1`, endpoint)
	} else {
		_, err = r.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint=$1 AND user_id=$2`, endpoint, userID)
	}
	return mapErr(err)
}

type pgSettings struct{ pool *pgxpool.Pool }

func (r pgSettings) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := r.pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key=$1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, mapErr(err)
}

func (r pgSettings) SetIfAbsent(ctx context.Context, key, value string) (string, error) {
	if _, err := r.pool.Exec(ctx, `INSERT INTO app_settings (key, value) VALUES ($1,$2) ON CONFLICT (key) DO NOTHING`, key, value); err != nil {
		return "", mapErr(err)
	}
	return r.Get(ctx, key)
}

// ---- memory ----

type memPushData struct {
	mu       sync.Mutex
	subs     map[string]*PushSubscription
	settings map[string]string
}

// Push returns the push subscription aggregate.
func (m *Memory) Push() PushSubscriptions { return memPush{m.push} }

// Settings returns the settings aggregate.
func (m *Memory) Settings() Settings { return memSettings{m.push} }

type memPush struct{ d *memPushData }

func (r memPush) Save(_ context.Context, s *PushSubscription) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	c := *s
	if old, ok := r.d.subs[s.Endpoint]; ok {
		c.CreatedAt = old.CreatedAt
	} else {
		c.CreatedAt = time.Now()
	}
	s.CreatedAt = c.CreatedAt
	r.d.subs[s.Endpoint] = &c
	return nil
}

func (r memPush) List(_ context.Context, userID string) ([]*PushSubscription, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	var out []*PushSubscription
	for _, s := range r.d.subs {
		if s.UserID == userID {
			c := *s
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r memPush) Delete(_ context.Context, userID, endpoint string) error {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if s, ok := r.d.subs[endpoint]; ok && (userID == "" || s.UserID == userID) {
		delete(r.d.subs, endpoint)
	}
	return nil
}

type memSettings struct{ d *memPushData }

func (r memSettings) Get(_ context.Context, key string) (string, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	return r.d.settings[key], nil
}

func (r memSettings) SetIfAbsent(_ context.Context, key, value string) (string, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if v, ok := r.d.settings[key]; ok {
		return v, nil
	}
	r.d.settings[key] = value
	return value, nil
}
