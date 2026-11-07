package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Memories returns the memory aggregate.
func (p *Postgres) Memories() Memories { return pgMemories{p.pool} }

type pgMemories struct{ pool *pgxpool.Pool }

const memoryCols = `id, session_id, text, source, created_at, updated_at`

func scanMemory(row pgx.Row) (*AgentMemory, error) {
	var m AgentMemory
	if err := row.Scan(&m.ID, &m.SessionID, &m.Text, &m.Source, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &m, nil
}

func (r pgMemories) List(ctx context.Context, sessionID string) ([]*AgentMemory, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+memoryCols+` FROM memories WHERE session_id=$1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*AgentMemory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, mapErr(rows.Err())
}

func (r pgMemories) Add(ctx context.Context, m *AgentMemory) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	return mapErr(r.pool.QueryRow(ctx, `INSERT INTO memories (id, session_id, text, source) VALUES ($1,$2,$3,$4) RETURNING created_at, updated_at`,
		m.ID, m.SessionID, m.Text, m.Source).Scan(&m.CreatedAt, &m.UpdatedAt))
}

func (r pgMemories) Update(ctx context.Context, sessionID, id, text string) (*AgentMemory, error) {
	return scanMemory(r.pool.QueryRow(ctx, `UPDATE memories SET text=$3, updated_at=now() WHERE session_id=$1 AND id::text=$2 RETURNING `+memoryCols, sessionID, id, text))
}

func (r pgMemories) Delete(ctx context.Context, sessionID, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM memories WHERE session_id=$1 AND id::text=$2`, sessionID, id)
	return mapErr(err)
}
