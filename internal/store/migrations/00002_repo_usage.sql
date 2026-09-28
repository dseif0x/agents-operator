-- +goose Up
-- How often a user has started a session with a repository. Survives session
-- deletion so the "browse repositories" list can rank by real use.
CREATE TABLE repo_usage (
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_key     text NOT NULL,
    count        integer NOT NULL DEFAULT 0,
    last_used_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, repo_key)
);

-- +goose Down
DROP TABLE repo_usage;
