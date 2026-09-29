-- +goose Up
-- A per-session runtimeClassName; empty means the chart-wide default.
ALTER TABLE sessions ADD COLUMN runtime_class text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN runtime_class;
