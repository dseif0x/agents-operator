-- +goose Up
-- Accounts replace the fixed credential slots of user_credentials: any
-- number of AI accounts (each with a kind and a name) and git accounts (one
-- per kind and host). Values stay in the per-user Kubernetes Secret, under
-- acct-<id>. The hub moves the old slots over at start (session/migrate.go)
-- and only then empties user_credentials, which is why it is not dropped here.
CREATE TABLE accounts (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    name       text NOT NULL,
    host       text NOT NULL DEFAULT '',
    base_url   text NOT NULL DEFAULT '',
    username   text NOT NULL DEFAULT '',
    models     jsonb NOT NULL DEFAULT '[]',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX accounts_user ON accounts (user_id, created_at);
CREATE UNIQUE INDEX accounts_git_host ON accounts (user_id, kind, lower(host)) WHERE kind IN ('git_https', 'git_ssh');
CREATE UNIQUE INDEX accounts_ai_name ON accounts (user_id, lower(name)) WHERE kind NOT IN ('git_https', 'git_ssh');

-- Settings that are not credentials: the git identity, and which account
-- Claude Code and Codex sessions use unless they pick one (agent -> id).
CREATE TABLE user_profiles (
    user_id    uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    git_name   text NOT NULL DEFAULT '',
    git_email  text NOT NULL DEFAULT '',
    defaults   jsonb NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The AI account a Claude Code or Codex session signs in with; '' = the
-- owner's default for the agent.
ALTER TABLE sessions ADD COLUMN ai_account text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN ai_account;
DROP TABLE user_profiles;
DROP TABLE accounts;
