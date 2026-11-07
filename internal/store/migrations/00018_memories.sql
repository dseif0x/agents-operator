-- +goose Up
-- Memories: short notes an agent keeps across conversations (its own tools
-- or the owner write them); the runner gives them to the agent.
CREATE TABLE memories (
    id         uuid PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    text       text NOT NULL,
    source     text NOT NULL DEFAULT 'agent', -- agent | owner
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX memories_session ON memories (session_id, created_at);

-- +goose Down
DROP TABLE memories;
