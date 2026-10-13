-- +goose Up
-- The account-wide OpenClaude model is gone: each session keeps the model
-- picked with /model. A value left in the user Secret is no longer read.
DELETE FROM user_credentials WHERE kind = 'openclaude_model';

-- +goose Down
SELECT 1;
