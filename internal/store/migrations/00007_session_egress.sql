-- +goose Up
-- Local network destinations a session's agent may reach beyond the chart's
-- default (an address or CIDR, optionally with a port), in canonical form.
ALTER TABLE sessions ADD COLUMN egress jsonb NOT NULL DEFAULT '[]'::jsonb;

-- +goose Down
ALTER TABLE sessions DROP COLUMN egress;
