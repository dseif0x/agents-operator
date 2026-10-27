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

// Accounts returns the account aggregate.
func (p *Postgres) Accounts() Accounts { return pgAccounts{p.pool} }

// Profiles returns the profile aggregate.
func (p *Postgres) Profiles() Profiles { return pgProfiles{p.pool} }

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

const sessionCols = `id, owner_id, name, agent, repos, image_tag, pvc_size, storage_class, runtime_class, k8s_access, k8s_namespaces,
	resources, node_selector, tolerations, env, autonomous, state, state_reason, generation,
	created_at, updated_at, last_attached_at, last_output_at, deleted_at, no_git_credentials, egress,
	system_prompt, system_prompt_replace, agent_definition, ai_account`

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.OwnerID, &s.Name, &s.Agent, &s.Repos, &s.ImageTag, &s.PVCSize, &s.StorageClass, &s.RuntimeClass, &s.K8sAccess, &s.K8sNamespaces,
		&s.Resources, &s.NodeSelector, &s.Tolerations, &s.Env, &s.Autonomous, &s.State, &s.StateReason, &s.Generation,
		&s.CreatedAt, &s.UpdatedAt, &s.LastAttachedAt, &s.LastOutputAt, &s.DeletedAt, &s.NoGitCredentials, &s.Egress,
		&s.SystemPrompt, &s.SystemPromptReplace, &s.AgentDefinition, &s.AIAccount)
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
	if s.Egress == nil {
		s.Egress = []string{}
	}
	normaliseK8s(s)
	_, err := r.pool.Exec(ctx, `INSERT INTO sessions (`+sessionCols+`) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)`,
		s.ID, s.OwnerID, s.Name, s.Agent, s.Repos, s.ImageTag, s.PVCSize, s.StorageClass, s.RuntimeClass, s.K8sAccess, s.K8sNamespaces,
		s.Resources, s.NodeSelector, s.Tolerations, s.Env, s.Autonomous, s.State, s.StateReason, s.Generation,
		s.CreatedAt, s.UpdatedAt, s.LastAttachedAt, s.LastOutputAt, s.DeletedAt, s.NoGitCredentials, s.Egress,
		s.SystemPrompt, s.SystemPromptReplace, s.AgentDefinition, s.AIAccount)
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

func (r pgSessions) Update(ctx context.Context, s *Session) (*Session, error) {
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
	if s.Egress == nil {
		s.Egress = []string{}
	}
	normaliseK8s(s)
	return scanSession(r.pool.QueryRow(ctx, `UPDATE sessions SET name=$2, repos=$3, image_tag=$4, runtime_class=$5, k8s_access=$6, k8s_namespaces=$7,
		resources=$8, node_selector=$9, tolerations=$10, env=$11, autonomous=$12, no_git_credentials=$13, egress=$14,
		system_prompt=$15, system_prompt_replace=$16, agent_definition=$17, ai_account=$18, updated_at=now()
		WHERE id=$1 RETURNING `+sessionCols,
		s.ID, s.Name, s.Repos, s.ImageTag, s.RuntimeClass, s.K8sAccess, s.K8sNamespaces, s.Resources, s.NodeSelector, s.Tolerations, s.Env, s.Autonomous,
		s.NoGitCredentials, s.Egress, s.SystemPrompt, s.SystemPromptReplace,
		s.AgentDefinition, s.AIAccount))
}

// normaliseK8s fills the access mode and namespace list so the columns never
// hold NULL or an empty string.
func normaliseK8s(s *Session) {
	if s.K8sAccess == "" {
		s.K8sAccess = K8sAccessOff
	}
	if s.K8sNamespaces == nil {
		s.K8sNamespaces = []string{}
	}
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

func (r pgCredentials) UserIDs(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT user_id::text FROM user_credentials`)
	if err != nil {
		return nil, mapErr(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return ids, mapErr(err)
}

// ---- accounts ----

type pgAccounts struct{ pool *pgxpool.Pool }

const accountCols = `id, user_id, kind, name, host, base_url, username, models, created_at, updated_at`

func scanAccount(row pgx.Row) (*Account, error) {
	var a Account
	if err := row.Scan(&a.ID, &a.UserID, &a.Kind, &a.Name, &a.Host, &a.BaseURL, &a.Username, &a.Models, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (r pgAccounts) Create(ctx context.Context, a *Account) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.Models == nil {
		a.Models = []string{}
	}
	// The database's clock, like Update's: updated_at is compared with it.
	err := r.pool.QueryRow(ctx, `INSERT INTO accounts (id, user_id, kind, name, host, base_url, username, models) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING created_at, updated_at`,
		a.ID, a.UserID, a.Kind, a.Name, a.Host, a.BaseURL, a.Username, a.Models).Scan(&a.CreatedAt, &a.UpdatedAt)
	return mapErr(err)
}

func (r pgAccounts) Update(ctx context.Context, a *Account) error {
	if a.Models == nil {
		a.Models = []string{}
	}
	got, err := scanAccount(r.pool.QueryRow(ctx, `UPDATE accounts SET name=$3, host=$4, base_url=$5, username=$6, models=$7, updated_at=now()
		WHERE user_id=$1 AND id=$2 RETURNING `+accountCols, a.UserID, a.ID, a.Name, a.Host, a.BaseURL, a.Username, a.Models))
	if err != nil {
		return err
	}
	*a = *got
	return nil
}

func (r pgAccounts) Get(ctx context.Context, userID, id string) (*Account, error) {
	return scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE user_id=$1 AND id::text=$2`, userID, id))
}

func (r pgAccounts) List(ctx context.Context, userID string) ([]*Account, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+accountCols+` FROM accounts WHERE user_id=$1 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapErr(rows.Err())
}

func (r pgAccounts) Delete(ctx context.Context, userID, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM accounts WHERE user_id=$1 AND id::text=$2`, userID, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- profiles ----

type pgProfiles struct{ pool *pgxpool.Pool }

func (r pgProfiles) Get(ctx context.Context, userID string) (*Profile, error) {
	p := Profile{UserID: userID}
	err := r.pool.QueryRow(ctx, `SELECT git_name, git_email, defaults, updated_at FROM user_profiles WHERE user_id=$1`, userID).
		Scan(&p.GitName, &p.GitEmail, &p.Defaults, &p.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, mapErr(err)
	}
	if p.Defaults == nil {
		p.Defaults = map[string]string{}
	}
	return &p, nil
}

func (r pgProfiles) Put(ctx context.Context, p *Profile) error {
	if p.Defaults == nil {
		p.Defaults = map[string]string{}
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO user_profiles (user_id, git_name, git_email, defaults, updated_at) VALUES ($1,$2,$3,$4,now())
		ON CONFLICT (user_id) DO UPDATE SET git_name=EXCLUDED.git_name, git_email=EXCLUDED.git_email, defaults=EXCLUDED.defaults, updated_at=now()`,
		p.UserID, p.GitName, p.GitEmail, p.Defaults)
	return mapErr(err)
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
