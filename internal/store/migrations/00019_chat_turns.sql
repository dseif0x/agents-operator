-- +goose Up
-- The conversation turn an agent's chat message came from, so the chat can
-- show the work behind a reply (docs/design/conversations.md).
ALTER TABLE chat_messages ADD COLUMN turn text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE chat_messages DROP COLUMN turn;
