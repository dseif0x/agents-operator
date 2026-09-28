-- +goose Up
CREATE TABLE users (
    id            uuid PRIMARY KEY,
    username      text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    disabled      boolean NOT NULL DEFAULT false
);

CREATE TABLE user_credentials (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    secret_ref text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, kind)
);

CREATE TABLE sessions (
    id               uuid PRIMARY KEY,
    owner_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name             text NOT NULL,
    agent            text NOT NULL,
    repos            jsonb NOT NULL DEFAULT '[]'::jsonb,
    image_tag        text NOT NULL DEFAULT '',
    pvc_size         text NOT NULL,
    storage_class    text NOT NULL DEFAULT '',
    resources        jsonb NOT NULL DEFAULT '{}'::jsonb,
    node_selector    jsonb NOT NULL DEFAULT '{}'::jsonb,
    tolerations      jsonb NOT NULL DEFAULT '[]'::jsonb,
    env              jsonb NOT NULL DEFAULT '{}'::jsonb,
    autonomous       boolean NOT NULL DEFAULT false,
    state            text NOT NULL,
    state_reason     text NOT NULL DEFAULT '',
    generation       integer NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    last_attached_at timestamptz,
    last_output_at   timestamptz,
    deleted_at       timestamptz
);
CREATE INDEX sessions_owner_idx ON sessions(owner_id);
CREATE INDEX sessions_state_idx ON sessions(state);

CREATE TABLE session_events (
    id         bigserial PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    at         timestamptz NOT NULL DEFAULT now(),
    kind       text NOT NULL,
    message    text NOT NULL DEFAULT ''
);
CREATE INDEX session_events_session_idx ON session_events(session_id, id DESC);

-- +goose Down
DROP TABLE session_events;
DROP TABLE sessions;
DROP TABLE user_credentials;
DROP TABLE users;
