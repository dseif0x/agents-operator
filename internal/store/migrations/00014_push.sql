-- +goose Up
-- Web Push: one row per browser or device that allowed notifications.
CREATE TABLE push_subscriptions (
    endpoint   text PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    p256dh     text NOT NULL,
    auth       text NOT NULL,
    user_agent text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX push_subscriptions_user ON push_subscriptions (user_id);

-- Hub-wide settings that must survive restarts (the VAPID key pair).
CREATE TABLE app_settings (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE app_settings;
DROP TABLE push_subscriptions;
