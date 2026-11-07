package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Routines returns the routine aggregate.
func (p *Postgres) Routines() Routines { return pgRoutines{p.pool} }

type pgRoutines struct{ pool *pgxpool.Pool }

const routineCols = `id, owner_id, COALESCE(session_id::text, ''), COALESCE(chat_id::text, ''), name, instruction, trigger, schedule, enabled,
	webhook_secret, event_type, filters, next_run_at, last_run_at, created_at, updated_at`

func scanRoutine(row pgx.Row) (*Routine, error) {
	var r Routine
	if err := row.Scan(&r.ID, &r.OwnerID, &r.SessionID, &r.ChatID, &r.Name, &r.Instruction, &r.Trigger, &r.Schedule, &r.Enabled,
		&r.WebhookSecret, &r.EventType, &r.Filters, &r.NextRunAt, &r.LastRunAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func scanRoutines(rows pgx.Rows, err error) ([]*Routine, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Routine
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, mapErr(rows.Err())
}

func nullID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func normaliseRoutine(r *Routine) {
	if len(r.Schedule) == 0 {
		r.Schedule = []byte(`{}`)
	}
	if len(r.Filters) == 0 {
		r.Filters = []byte(`[]`)
	}
}

func (s pgRoutines) Create(ctx context.Context, r *Routine) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	normaliseRoutine(r)
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO routines (id, owner_id, session_id, chat_id, name, instruction, trigger, schedule, enabled, webhook_secret, event_type, filters, next_run_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING created_at, updated_at`,
		r.ID, r.OwnerID, nullID(r.SessionID), nullID(r.ChatID), r.Name, r.Instruction, r.Trigger, r.Schedule, r.Enabled, r.WebhookSecret, r.EventType, r.Filters, r.NextRunAt).
		Scan(&r.CreatedAt, &r.UpdatedAt))
}

func (s pgRoutines) Update(ctx context.Context, r *Routine) error {
	normaliseRoutine(r)
	tag, err := s.pool.Exec(ctx, `UPDATE routines SET name=$2, instruction=$3, trigger=$4, schedule=$5, enabled=$6, webhook_secret=$7, event_type=$8, filters=$9, next_run_at=$10, updated_at=now() WHERE id=$1`,
		r.ID, r.Name, r.Instruction, r.Trigger, r.Schedule, r.Enabled, r.WebhookSecret, r.EventType, r.Filters, r.NextRunAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s pgRoutines) Get(ctx context.Context, id string) (*Routine, error) {
	return scanRoutine(s.pool.QueryRow(ctx, `SELECT `+routineCols+` FROM routines WHERE id::text=$1`, id))
}

func (s pgRoutines) List(ctx context.Context, ownerID string) ([]*Routine, error) {
	return scanRoutines(s.pool.Query(ctx, `SELECT `+routineCols+` FROM routines WHERE owner_id=$1 ORDER BY created_at, id`, ownerID))
}

func (s pgRoutines) Delete(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM routines WHERE id::text=$1`, id)
	return mapErr(err)
}

func (s pgRoutines) Due(ctx context.Context, now time.Time) ([]*Routine, error) {
	return scanRoutines(s.pool.Query(ctx, `SELECT `+routineCols+` FROM routines WHERE enabled AND trigger='schedule' AND next_run_at IS NOT NULL AND next_run_at <= $1 ORDER BY next_run_at`, now))
}

func (s pgRoutines) SetRun(ctx context.Context, id string, next *time.Time, last time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE routines SET next_run_at=$2, last_run_at=$3 WHERE id::text=$1`, id, next, last)
	return mapErr(err)
}

const runCols = `id, routine_id, source, status, input_id, turn, chat_message, quiet, error, payload, scheduled_for, started_at, ended_at`

func scanRun(row pgx.Row) (*RoutineRun, error) {
	var r RoutineRun
	if err := row.Scan(&r.ID, &r.RoutineID, &r.Source, &r.Status, &r.InputID, &r.Turn, &r.ChatMessage, &r.Quiet, &r.Error, &r.Payload, &r.ScheduledFor, &r.StartedAt, &r.EndedAt); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func scanRuns(rows pgx.Rows, err error) ([]*RoutineRun, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*RoutineRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, mapErr(rows.Err())
}

func (s pgRoutines) AddRun(ctx context.Context, r *RoutineRun) error {
	var started any
	if !r.StartedAt.IsZero() {
		started = r.StartedAt
	}
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO routine_runs (routine_id, source, status, input_id, turn, chat_message, quiet, error, payload, scheduled_for, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,COALESCE($11::timestamptz, now())) RETURNING id, started_at`,
		r.RoutineID, r.Source, r.Status, r.InputID, r.Turn, r.ChatMessage, r.Quiet, r.Error, r.Payload, r.ScheduledFor, started).Scan(&r.ID, &r.StartedAt))
}

func (s pgRoutines) UpdateRun(ctx context.Context, r *RoutineRun) error {
	_, err := s.pool.Exec(ctx, `UPDATE routine_runs SET status=$2, input_id=$3, turn=$4, chat_message=$5, quiet=$6, error=$7, ended_at=$8 WHERE id=$1`,
		r.ID, r.Status, r.InputID, r.Turn, r.ChatMessage, r.Quiet, r.Error, r.EndedAt)
	return mapErr(err)
}

func (s pgRoutines) Runs(ctx context.Context, routineID string, limit int) ([]*RoutineRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	return scanRuns(s.pool.Query(ctx, `SELECT `+runCols+` FROM routine_runs WHERE routine_id::text=$1 ORDER BY id DESC LIMIT $2`, routineID, limit))
}

func (s pgRoutines) RunsSince(ctx context.Context, ownerID string, since time.Time) ([]*RoutineRun, error) {
	return scanRuns(s.pool.Query(ctx, `SELECT `+runCols+` FROM routine_runs WHERE started_at >= $2 AND routine_id IN (SELECT id FROM routines WHERE owner_id=$1) ORDER BY id`, ownerID, since))
}

func (s pgRoutines) RunByInput(ctx context.Context, inputID string) (*RoutineRun, error) {
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM routine_runs WHERE input_id=$1 ORDER BY id DESC LIMIT 1`, inputID))
}

func (s pgRoutines) RunsByTurn(ctx context.Context, turn string) ([]*RoutineRun, error) {
	return scanRuns(s.pool.Query(ctx, `SELECT `+runCols+` FROM routine_runs WHERE turn=$1 ORDER BY id`, turn))
}
