-- +goose Up
-- OpenClaude replaces the Unjailed CLI. Sessions keep their volume; the
-- Unjailed CLI's state on it (~/.unjailed) is simply no longer read.
UPDATE sessions SET agent = 'openclaude' WHERE agent = 'unjailed';

-- +goose Down
UPDATE sessions SET agent = 'unjailed' WHERE agent = 'openclaude';
