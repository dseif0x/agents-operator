package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Conversations returns the conversation aggregate.
func (p *Postgres) Conversations() Conversations { return pgConversations{p.pool} }

type pgConversations struct{ pool *pgxpool.Pool }

const itemCols = `id, session_id, thread, turn, key, kind, status, data, created_at, updated_at`

func scanItem(row pgx.Row) (*ConvItem, error) {
	var it ConvItem
	if err := row.Scan(&it.ID, &it.SessionID, &it.Thread, &it.Turn, &it.Key, &it.Kind, &it.Status, &it.Data, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &it, nil
}

func scanItems(rows pgx.Rows) ([]*ConvItem, error) {
	defer rows.Close()
	var out []*ConvItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, mapErr(rows.Err())
}

func (r pgConversations) UpsertItem(ctx context.Context, it *ConvItem) (*ConvItem, error) {
	if len(it.Data) == 0 {
		it.Data = []byte(`{}`)
	}
	return scanItem(r.pool.QueryRow(ctx, `INSERT INTO conversation_items (session_id, thread, turn, key, kind, status, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (session_id, thread, key) DO UPDATE SET turn=EXCLUDED.turn, kind=EXCLUDED.kind, status=EXCLUDED.status, data=EXCLUDED.data, updated_at=now()
		RETURNING `+itemCols, it.SessionID, it.Thread, it.Turn, it.Key, it.Kind, it.Status, it.Data))
}

func (r pgConversations) GetItem(ctx context.Context, sessionID, thread, key string) (*ConvItem, error) {
	return scanItem(r.pool.QueryRow(ctx, `SELECT `+itemCols+` FROM conversation_items WHERE session_id=$1 AND thread=$2 AND key=$3`, sessionID, thread, key))
}

func (r pgConversations) DeleteItem(ctx context.Context, sessionID, thread, key string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM conversation_items WHERE session_id=$1 AND thread=$2 AND key=$3`, sessionID, thread, key)
	return mapErr(err)
}

func (r pgConversations) ListItems(ctx context.Context, sessionID, thread string, before int64, limit int) ([]*ConvItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + itemCols + ` FROM (SELECT * FROM conversation_items WHERE session_id=$1 AND thread=$2 AND ($3 = 0 OR id < $3)
		ORDER BY id DESC LIMIT $4) page ORDER BY id`
	rows, err := r.pool.Query(ctx, q, sessionID, thread, before, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanItems(rows)
}

func (r pgConversations) TurnItems(ctx context.Context, sessionID, thread, turn string) ([]*ConvItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+itemCols+` FROM conversation_items WHERE session_id=$1 AND thread=$2 AND turn=$3 ORDER BY id`, sessionID, thread, turn)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanItems(rows)
}

func (r pgConversations) RunningItems(ctx context.Context, sessionID, thread, turn string) ([]*ConvItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+itemCols+` FROM conversation_items WHERE session_id=$1 AND thread=$2 AND turn=$3 AND status='running' ORDER BY id`, sessionID, thread, turn)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanItems(rows)
}

func (r pgConversations) AddInput(ctx context.Context, in *ConvInput) error {
	if in.ID == "" {
		in.ID = NewID()
	}
	if in.Thread == "" {
		in.Thread = MainThread
	}
	if in.State == "" {
		in.State = InputQueued
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `INSERT INTO conversation_inputs (id, session_id, thread, text, source, state) VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`,
		in.ID, in.SessionID, in.Thread, in.Text, in.Source, in.State).Scan(&in.CreatedAt); err != nil {
		return mapErr(err)
	}
	for i := range in.Files {
		f := &in.Files[i]
		if f.ID == "" {
			f.ID = NewID()
		}
		f.InputID, f.SessionID = in.ID, in.SessionID
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_files (id, input_id, session_id, owner_id, name, content_type, size, data) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			f.ID, f.InputID, f.SessionID, f.OwnerID, f.Name, f.ContentType, f.Size, f.Data); err != nil {
			return mapErr(err)
		}
	}
	return mapErr(tx.Commit(ctx))
}

const inputCols = `id, session_id, thread, text, source, state, created_at, sent_at`

func scanInput(row pgx.Row) (*ConvInput, error) {
	var in ConvInput
	if err := row.Scan(&in.ID, &in.SessionID, &in.Thread, &in.Text, &in.Source, &in.State, &in.CreatedAt, &in.SentAt); err != nil {
		return nil, mapErr(err)
	}
	return &in, nil
}

func (r pgConversations) files(ctx context.Context, ins []*ConvInput) error {
	if len(ins) == 0 {
		return nil
	}
	ids := make([]string, len(ins))
	byID := map[string]*ConvInput{}
	for i, in := range ins {
		ids[i] = in.ID
		byID[in.ID] = in
	}
	rows, err := r.pool.Query(ctx, `SELECT id, input_id::text, name, content_type, size FROM conversation_files WHERE input_id::text = ANY($1) ORDER BY created_at, id`, ids)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var f ConvFile
		if err := rows.Scan(&f.ID, &f.InputID, &f.Name, &f.ContentType, &f.Size); err != nil {
			return mapErr(err)
		}
		if in := byID[f.InputID]; in != nil {
			in.Files = append(in.Files, f)
		}
	}
	return mapErr(rows.Err())
}

func (r pgConversations) GetInput(ctx context.Context, sessionID, id string) (*ConvInput, error) {
	in, err := scanInput(r.pool.QueryRow(ctx, `SELECT `+inputCols+` FROM conversation_inputs WHERE session_id=$1 AND id::text=$2`, sessionID, id))
	if err != nil {
		return nil, err
	}
	return in, r.files(ctx, []*ConvInput{in})
}

func (r pgConversations) QueuedInputs(ctx context.Context, sessionID string) ([]*ConvInput, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+inputCols+` FROM conversation_inputs WHERE session_id=$1 AND state='queued' ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*ConvInput
	for rows.Next() {
		in, err := scanInput(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, r.files(ctx, out)
}

func (r pgConversations) UpdateInput(ctx context.Context, sessionID, id, text string) (*ConvInput, error) {
	in, err := scanInput(r.pool.QueryRow(ctx, `UPDATE conversation_inputs SET text=$3 WHERE session_id=$1 AND id::text=$2 AND state='queued' RETURNING `+inputCols, sessionID, id, text))
	if err == ErrNotFound {
		if _, gerr := r.GetInput(ctx, sessionID, id); gerr == nil {
			return nil, ErrConflict
		}
	}
	if err != nil {
		return nil, err
	}
	return in, r.files(ctx, []*ConvInput{in})
}

func (r pgConversations) SetInputState(ctx context.Context, sessionID, id, state string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE conversation_inputs SET state=$3, sent_at=CASE WHEN $3='sent' THEN now() ELSE sent_at END
		WHERE session_id=$1 AND id::text=$2 AND state='queued'`, sessionID, id, state)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r pgConversations) AddFile(ctx context.Context, f *ConvFile) error {
	if f.ID == "" {
		f.ID = NewID()
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO conversation_files (id, session_id, owner_id, name, content_type, size, data) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		f.ID, f.SessionID, f.OwnerID, f.Name, f.ContentType, f.Size, f.Data)
	return mapErr(err)
}

func (r pgConversations) GetFile(ctx context.Context, sessionID, id string) (*ConvFile, error) {
	var f ConvFile
	err := r.pool.QueryRow(ctx, `SELECT id, COALESCE(input_id::text, ''), session_id::text, owner_id::text, name, content_type, size, data FROM conversation_files WHERE session_id=$1 AND id::text=$2`, sessionID, id).
		Scan(&f.ID, &f.InputID, &f.SessionID, &f.OwnerID, &f.Name, &f.ContentType, &f.Size, &f.Data)
	if err != nil {
		return nil, mapErr(err)
	}
	return &f, nil
}
