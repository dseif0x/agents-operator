package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Postgres implements Store on pgx.
type Postgres struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Open connects to Postgres, waits for it to answer, and applies the
// embedded migrations under a session-level advisory lock.
func Open(ctx context.Context, url string, log *slog.Logger) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Retry the first ping: the chart's Postgres may still be starting.
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = pool.Ping(ctx); lastErr == nil {
			break
		}
		log.Warn("postgres not ready", "err", lastErr, "attempt", i+1)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres unreachable: %w", lastErr)
	}
	p := &Postgres{pool: pool, log: log}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(p.pool)
	defer db.Close()
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	prov, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("goose: %w", err)
	}
	results, err := prov.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, r := range results {
		p.log.Info("applied migration", "version", r.Source.Version, "path", r.Source.Path)
	}
	return nil
}

// Ping checks the connection.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Close releases the pool.
func (p *Postgres) Close() { p.pool.Close() }

// Users returns the user aggregate.
func (p *Postgres) Users() Users { return pgUsers{p.pool} }

// Sessions returns the session aggregate.
func (p *Postgres) Sessions() Sessions { return pgSessions{p.pool} }

// Credentials returns the credential aggregate.
func (p *Postgres) Credentials() Credentials { return pgCredentials{p.pool} }

// Events returns the event aggregate.
func (p *Postgres) Events() Events { return pgEvents{p.pool} }

// RepoUsage returns the repo usage aggregate.
func (p *Postgres) RepoUsage() RepoUsage { return pgRepoUsage{p.pool} }

type pgRepoUsage struct{ pool *pgxpool.Pool }

func (r pgRepoUsage) Increment(ctx context.Context, userID, repoKey string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO repo_usage (user_id, repo_key, count, last_used_at) VALUES ($1,$2,1,now())
		ON CONFLICT (user_id, repo_key) DO UPDATE SET count = repo_usage.count + 1, last_used_at = now()`, userID, repoKey)
	return mapErr(err)
}

func (r pgRepoUsage) List(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT repo_key, count FROM repo_usage WHERE user_id=$1`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}

// ---- users ----

type pgUsers struct{ pool *pgxpool.Pool }

const userCols = "id, username, password_hash, created_at, disabled"

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &u.Disabled); err != nil {
		return nil, mapErr(err)
	}
	return &u, nil
}

func (r pgUsers) Create(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO users (`+userCols+`) VALUES ($1,$2,$3,$4,$5)`,
		u.ID, u.Username, u.PasswordHash, u.CreatedAt, u.Disabled)
	return mapErr(err)
}

func (r pgUsers) GetByID(ctx context.Context, id string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (r pgUsers) GetByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username=$1`, username))
}

func (r pgUsers) UpsertPassword(ctx context.Context, username, hash string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `
		INSERT INTO users (id, username, password_hash) VALUES ($1,$2,$3)
		ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash
		RETURNING `+userCols, NewID(), username, hash))
}

// ---- sessions ----

type pgSessions struct{ pool *pgxpool.Pool }

const sessionCols = `id, owner_id, name, agent, repos, image_tag, pvc_size, storage_class, runtime_class,
	resources, node_selector, tolerations, env, autonomous, state, state_reason, generation,
	created_at, updated_at, last_attached_at, last_output_at, deleted_at`

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.OwnerID, &s.Name, &s.Agent, &s.Repos, &s.ImageTag, &s.PVCSize, &s.StorageClass, &s.RuntimeClass,
		&s.Resources, &s.NodeSelector, &s.Tolerations, &s.Env, &s.Autonomous, &s.State, &s.StateReason, &s.Generation,
		&s.CreatedAt, &s.UpdatedAt, &s.LastAttachedAt, &s.LastOutputAt, &s.DeletedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &s, nil
}

func scanSessions(rows pgx.Rows) ([]*Session, error) {
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, mapErr(rows.Err())
}

func (r pgSessions) Create(ctx context.Context, s *Session) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	now := time.Now()
	s.CreatedAt, s.UpdatedAt = now, now
	if s.Generation == 0 {
		s.Generation = 1
	}
	if s.Repos == nil {
		s.Repos = []Repo{}
	}
	if s.NodeSelector == nil {
		s.NodeSelector = map[string]string{}
	}
	if s.Tolerations == nil {
		s.Tolerations = []config.Toleration{}
	}
	if s.Env == nil {
		s.Env = map[string]string{}
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO sessions (`+sessionCols+`) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		s.ID, s.OwnerID, s.Name, s.Agent, s.Repos, s.ImageTag, s.PVCSize, s.StorageClass, s.RuntimeClass,
		s.Resources, s.NodeSelector, s.Tolerations, s.Env, s.Autonomous, s.State, s.StateReason, s.Generation,
		s.CreatedAt, s.UpdatedAt, s.LastAttachedAt, s.LastOutputAt, s.DeletedAt)
	return mapErr(err)
}

func (r pgSessions) Get(ctx context.Context, id string) (*Session, error) {
	return scanSession(r.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id=$1`, id))
}

func (r pgSessions) List(ctx context.Context, ownerID string) ([]*Session, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+sessionCols+` FROM sessions WHERE owner_id=$1 ORDER BY created_at DESC`, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanSessions(rows)
}

func (r pgSessions) ListAll(ctx context.Context) ([]*Session, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+sessionCols+` FROM sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanSessions(rows)
}

func (r pgSessions) SetState(ctx context.Context, id, state, reason string) (*Session, error) {
	return scanSession(r.pool.QueryRow(ctx, `UPDATE sessions SET state=$2, state_reason=$3, updated_at=now(),
		deleted_at = CASE WHEN $2='deleting' THEN now() ELSE deleted_at END
		WHERE id=$1 RETURNING `+sessionCols, id, state, reason))
}

func (r pgSessions) Bump(ctx context.Context, id, state string) (*Session, error) {
	return scanSession(r.pool.QueryRow(ctx, `UPDATE sessions SET state=$2, state_reason='', generation=generation+1, updated_at=now()
		WHERE id=$1 RETURNING `+sessionCols, id, state))
}

func (r pgSessions) TouchAttached(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE sessions SET last_attached_at=$2 WHERE id=$1`, id, at)
	return mapErr(err)
}

func (r pgSessions) TouchOutput(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE sessions SET last_output_at=$2 WHERE id=$1 AND (last_output_at IS NULL OR last_output_at < $2)`, id, at)
	return mapErr(err)
}

func (r pgSessions) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, id)
	return mapErr(err)
}

func (r pgSessions) CountByState(ctx context.Context) (map[string]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT state, count(*) FROM sessions GROUP BY state`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ---- credentials ----

type pgCredentials struct{ pool *pgxpool.Pool }

func (r pgCredentials) Upsert(ctx context.Context, c *Credential) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO user_credentials (user_id, kind, secret_ref, updated_at) VALUES ($1,$2,$3,now())
		ON CONFLICT (user_id, kind) DO UPDATE SET secret_ref=EXCLUDED.secret_ref, updated_at=now()`,
		c.UserID, c.Kind, c.SecretRef)
	return mapErr(err)
}

func (r pgCredentials) Delete(ctx context.Context, userID, kind string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM user_credentials WHERE user_id=$1 AND kind=$2`, userID, kind)
	return mapErr(err)
}

func (r pgCredentials) List(ctx context.Context, userID string) ([]*Credential, error) {
	rows, err := r.pool.Query(ctx, `SELECT user_id, kind, secret_ref, updated_at FROM user_credentials WHERE user_id=$1 ORDER BY kind`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Credential
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.UserID, &c.Kind, &c.SecretRef, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ---- events ----

type pgEvents struct{ pool *pgxpool.Pool }

func (r pgEvents) Add(ctx context.Context, sessionID, kind, message string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO session_events (session_id, kind, message) VALUES ($1,$2,$3)`, sessionID, kind, message)
	return mapErr(err)
}

func (r pgEvents) List(ctx context.Context, sessionID string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = EventsKeep
	}
	rows, err := r.pool.Query(ctx, `SELECT id, session_id, at, kind, message FROM session_events WHERE session_id=$1 ORDER BY id DESC LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.SessionID, &e.At, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (r pgEvents) Prune(ctx context.Context, sessionID string, keep int) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM session_events WHERE session_id=$1 AND id NOT IN
		(SELECT id FROM session_events WHERE session_id=$1 ORDER BY id DESC LIMIT $2)`, sessionID, keep)
	return mapErr(err)
}
