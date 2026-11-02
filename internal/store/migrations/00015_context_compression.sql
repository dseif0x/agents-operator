-- +goose Up
-- OpenCode sessions can compress their context with billion-context instead
-- of OpenCode's own compaction.
ALTER TABLE sessions ADD COLUMN context_compression boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN context_compression;
