-- +goose Up
-- Personas: the OpenCode-only system prompt becomes every agent's persona,
-- always added to the CLI's own prompt (the "replace" mode is gone).
ALTER TABLE sessions RENAME COLUMN system_prompt TO persona;
ALTER TABLE sessions DROP COLUMN system_prompt_replace;
-- Handles: an immutable Docker-style name per session ("eager_turing"),
-- what chat mentions store. The hub fills it for existing sessions at start.
ALTER TABLE sessions ADD COLUMN handle text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX sessions_owner_handle ON sessions (owner_id, handle) WHERE handle <> '';

-- Chats: groups of the owner's sessions, like a group chat.
CREATE TABLE chats (
    id          uuid PRIMARY KEY,
    owner_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    muted       boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chats_owner ON chats (owner_id);

CREATE TABLE chat_members (
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    muted      boolean NOT NULL DEFAULT false,
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, session_id)
);
CREATE INDEX chat_members_session ON chat_members (session_id);

-- Messages outlive their author's session: author_name keeps what to show.
CREATE TABLE chat_messages (
    id             bigserial PRIMARY KEY,
    chat_id        uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    author_session uuid,                  -- NULL: the owner
    author_name    text NOT NULL DEFAULT '',
    text           text NOT NULL,
    mentions       jsonb NOT NULL DEFAULT '[]', -- session ids
    everyone       boolean NOT NULL DEFAULT false,
    reply_to       bigint,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_messages_chat ON chat_messages (chat_id, id);

CREATE TABLE chat_attachments (
    id           uuid PRIMARY KEY,
    message_id   bigint NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    chat_id      uuid NOT NULL,
    owner_id     uuid NOT NULL,
    name         text NOT NULL DEFAULT '',
    content_type text NOT NULL,
    size         integer NOT NULL,
    data         bytea NOT NULL
);
CREATE INDEX chat_attachments_message ON chat_attachments (message_id);
CREATE INDEX chat_attachments_owner ON chat_attachments (owner_id);

-- Which session still has to get which message.
CREATE TABLE chat_deliveries (
    message_id   bigint NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    session_id   uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    state        text NOT NULL,           -- queued | delivered
    delivered_at timestamptz,
    PRIMARY KEY (message_id, session_id)
);
CREATE INDEX chat_deliveries_queue ON chat_deliveries (session_id, state, message_id);

-- Read markers, for unread counts: reader is "owner" or a session id.
CREATE TABLE chat_reads (
    chat_id         uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    reader          text NOT NULL,
    last_message_id bigint NOT NULL,
    PRIMARY KEY (chat_id, reader)
);

-- +goose Down
DROP TABLE chat_reads;
DROP TABLE chat_deliveries;
DROP TABLE chat_attachments;
DROP TABLE chat_messages;
DROP TABLE chat_members;
DROP TABLE chats;
DROP INDEX sessions_owner_handle;
ALTER TABLE sessions DROP COLUMN handle;
ALTER TABLE sessions ADD COLUMN system_prompt_replace boolean NOT NULL DEFAULT false;
ALTER TABLE sessions RENAME COLUMN persona TO system_prompt;
