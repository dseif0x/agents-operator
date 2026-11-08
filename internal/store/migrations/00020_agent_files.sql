-- +goose Up
-- Files the agent shares or reads (images in its tool results) belong to no
-- input of the owner.
ALTER TABLE conversation_files ALTER COLUMN input_id DROP NOT NULL;

-- +goose Down
DELETE FROM conversation_files WHERE input_id IS NULL;
ALTER TABLE conversation_files ALTER COLUMN input_id SET NOT NULL;
