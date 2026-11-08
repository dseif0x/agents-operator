package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Skills returns the skill library aggregate.
func (p *Postgres) Skills() Skills { return pgSkills{p.pool} }

type pgSkills struct{ pool *pgxpool.Pool }

const skillCols = `id, user_id, name, description, instructions, created_at, updated_at`

func scanSkill(row pgx.Row) (*Skill, error) {
	var s Skill
	if err := row.Scan(&s.ID, &s.UserID, &s.Name, &s.Description, &s.Instructions, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &s, nil
}

func (r pgSkills) List(ctx context.Context, userID string) ([]*Skill, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+skillCols+` FROM skills WHERE user_id=$1 ORDER BY name`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, mapErr(rows.Err())
}

func (r pgSkills) Get(ctx context.Context, userID, id string) (*Skill, error) {
	return scanSkill(r.pool.QueryRow(ctx, `SELECT `+skillCols+` FROM skills WHERE user_id=$1 AND id::text=$2`, userID, id))
}

func (r pgSkills) Create(ctx context.Context, s *Skill) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	return mapErr(r.pool.QueryRow(ctx, `INSERT INTO skills (id, user_id, name, description, instructions) VALUES ($1,$2,$3,$4,$5) RETURNING created_at, updated_at`,
		s.ID, s.UserID, s.Name, s.Description, s.Instructions).Scan(&s.CreatedAt, &s.UpdatedAt))
}

func (r pgSkills) Update(ctx context.Context, s *Skill) (*Skill, error) {
	return scanSkill(r.pool.QueryRow(ctx, `UPDATE skills SET name=$3, description=$4, instructions=$5, updated_at=now() WHERE user_id=$1 AND id::text=$2 RETURNING `+skillCols,
		s.UserID, s.ID, s.Name, s.Description, s.Instructions))
}

func (r pgSkills) Delete(ctx context.Context, userID, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM skills WHERE user_id=$1 AND id::text=$2`, userID, id)
	return mapErr(err)
}
