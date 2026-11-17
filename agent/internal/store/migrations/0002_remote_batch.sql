-- +goose Up
-- The ingest batch that currently owns the server-side asset. Registration
-- relinks assets whose previous batch closed with missing/corrupt, so a later
-- verification job can be mapped back to exactly the files it covered.
ALTER TABLE files ADD COLUMN remote_batch_id TEXT NOT NULL DEFAULT '';
CREATE INDEX files_remote_batch_idx ON files (remote_batch_id)
	WHERE remote_batch_id <> '';

-- +goose Down
DROP INDEX files_remote_batch_idx;
ALTER TABLE files DROP COLUMN remote_batch_id;
