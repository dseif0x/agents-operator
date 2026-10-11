-- +goose Up
-- Sessions can keep the owner's git credentials (SSH key, HTTPS and GitHub
-- tokens) from the agent; the runner still uses them to clone.
ALTER TABLE sessions ADD COLUMN no_git_credentials boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN no_git_credentials;
