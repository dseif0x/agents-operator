-- +goose Up
-- OpenCode replaces OpenClaude for the Unjailed models. Sessions keep their
-- volume, settings, system prompt and agent definition; OpenClaude's state
-- on it (~/.openclaude) is simply no longer read.
UPDATE sessions SET agent = 'opencode' WHERE agent = 'openclaude';

-- +goose Down
SELECT 1;
