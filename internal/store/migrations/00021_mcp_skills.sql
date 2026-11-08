-- +goose Up
-- MCP servers (accounts of kind mcp_server) and skills a session gets, by
-- id; skills are the owner's library of SKILL.md instructions.
ALTER TABLE sessions ADD COLUMN mcp_servers jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE sessions ADD COLUMN skills jsonb NOT NULL DEFAULT '[]'::jsonb;
CREATE TABLE skills (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text NOT NULL,
    description  text NOT NULL,
    instructions text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

-- +goose Down
DROP TABLE skills;
ALTER TABLE sessions DROP COLUMN skills;
ALTER TABLE sessions DROP COLUMN mcp_servers;
