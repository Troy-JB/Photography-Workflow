-- +goose Up
-- 16MiB default parts stay under int range, but choosePartSize can grow part
-- sizes for very large objects, so store them in bigint.
ALTER TABLE upload_sessions ALTER COLUMN part_size TYPE bigint;

-- Matches the asset listing order: created_at DESC, filename, id.
CREATE INDEX assets_project_created_idx
  ON assets (tenant_id, project_id, created_at DESC, filename, id);

-- +goose Down
DROP INDEX IF EXISTS assets_project_created_idx;
ALTER TABLE upload_sessions ALTER COLUMN part_size TYPE int;
