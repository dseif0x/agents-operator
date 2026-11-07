-- +goose Up
-- Conversations (docs/design/conversations.md): the runner drives the agent
-- CLI through its structured protocol; the hub keeps what it did.
ALTER TABLE sessions ADD COLUMN interface text NOT NULL DEFAULT 'terminal';
-- The latest model, context use and turn state, for lists and headers.
ALTER TABLE sessions ADD COLUMN conversation jsonb NOT NULL DEFAULT '{}';

-- One row per item of a conversation: a message given to the agent, a
-- piece of its answer, a tool call, an error. The runner's events update
-- the row of their key.
CREATE TABLE conversation_items (
    id         bigserial PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    thread     text NOT NULL DEFAULT 'main',
    turn       text NOT NULL DEFAULT '',
    key        text NOT NULL,
    kind       text NOT NULL,
    status     text NOT NULL,
    data       jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX conversation_items_key ON conversation_items (session_id, thread, key);
CREATE INDEX conversation_items_page ON conversation_items (session_id, thread, id);

-- The owner's messages until the agent takes them.
CREATE TABLE conversation_inputs (
    id         uuid PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    thread     text NOT NULL DEFAULT 'main',
    text       text NOT NULL,
    source     text NOT NULL DEFAULT 'owner',
    state      text NOT NULL DEFAULT 'queued',
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz
);
CREATE INDEX conversation_inputs_queue ON conversation_inputs (session_id, state, created_at);

-- Files that come with an input.
CREATE TABLE conversation_files (
    id           uuid PRIMARY KEY,
    input_id     uuid NOT NULL REFERENCES conversation_inputs(id) ON DELETE CASCADE,
    session_id   uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    owner_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text NOT NULL,
    content_type text NOT NULL,
    size         integer NOT NULL,
    data         bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX conversation_files_input ON conversation_files (input_id);

-- +goose Down
DROP TABLE conversation_files;
DROP TABLE conversation_inputs;
DROP TABLE conversation_items;
ALTER TABLE sessions DROP COLUMN conversation;
ALTER TABLE sessions DROP COLUMN interface;
