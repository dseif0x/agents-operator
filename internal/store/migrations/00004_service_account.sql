-- +goose Up
-- Whether the session pod mounts the chart's read-only runner ServiceAccount.
ALTER TABLE sessions ADD COLUMN service_account boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN service_account;
