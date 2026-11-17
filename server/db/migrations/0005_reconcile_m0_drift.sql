-- +goose Up

-- Repair databases initialised before 0003 gained the idempotency response
-- columns and the extended ingest batch statuses. All statements are safe to
-- run against an already-current schema, so fresh installs are unaffected.

ALTER TABLE idempotency_keys
  ADD COLUMN IF NOT EXISTS response_content_type text NOT NULL DEFAULT 'application/json';
ALTER TABLE idempotency_keys
  ADD COLUMN IF NOT EXISTS response_headers jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE idempotency_keys
  ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

ALTER TABLE ingest_batches DROP CONSTRAINT IF EXISTS ingest_batches_status_check;
ALTER TABLE ingest_batches ADD CONSTRAINT ingest_batches_status_check
  CHECK (status IN ('open','verifying','completed','partial','failed'));

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS lease_token text NOT NULL DEFAULT '';

-- +goose Down

-- Intentionally empty: the columns and constraint belong to 0003.
