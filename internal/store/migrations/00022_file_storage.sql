-- +goose Up
-- Files are served in ranges (a video player asks for several at once):
-- store the bytes uncompressed out of line, so substring() fetches only
-- the TOAST chunks a range covers instead of the whole value. Applies to
-- rows written from now on; media rarely compressed anyway.
ALTER TABLE conversation_files ALTER COLUMN data SET STORAGE EXTERNAL;
ALTER TABLE chat_attachments ALTER COLUMN data SET STORAGE EXTERNAL;

-- +goose Down
ALTER TABLE conversation_files ALTER COLUMN data SET STORAGE EXTENDED;
ALTER TABLE chat_attachments ALTER COLUMN data SET STORAGE EXTENDED;
