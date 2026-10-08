-- +goose Up
-- Kubernetes access became a mode: off, readonly (the old boolean) or
-- namespace (read-only plus write access in the listed namespaces).
ALTER TABLE sessions ADD COLUMN k8s_access text NOT NULL DEFAULT 'off';
ALTER TABLE sessions ADD COLUMN k8s_namespaces jsonb NOT NULL DEFAULT '[]'::jsonb;
UPDATE sessions SET k8s_access = 'readonly' WHERE service_account;
ALTER TABLE sessions DROP COLUMN service_account;

-- +goose Down
ALTER TABLE sessions ADD COLUMN service_account boolean NOT NULL DEFAULT false;
UPDATE sessions SET service_account = true WHERE k8s_access <> 'off';
ALTER TABLE sessions DROP COLUMN k8s_namespaces;
ALTER TABLE sessions DROP COLUMN k8s_access;
