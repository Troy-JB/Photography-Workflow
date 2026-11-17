-- +goose Up

-- M6 integration and notification queue. Email recipients are resolved when the
-- message is queued so the worker never needs read access to clients; the body
-- carries a one-time proof link and is cleared once delivery succeeds.

ALTER TABLE clients ADD COLUMN email text NOT NULL DEFAULT '';

CREATE TABLE integration_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  retouch_task_id uuid NOT NULL REFERENCES retouch_tasks(id) ON DELETE CASCADE,
  provider text NOT NULL,
  direction text NOT NULL CHECK (direction IN ('export','import')),
  status text NOT NULL DEFAULT 'queued'
    CHECK (status IN ('queued','running','completed','failed')),
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  attempts int NOT NULL DEFAULT 0,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, retouch_task_id, direction)
);
CREATE INDEX integration_jobs_status_idx
  ON integration_jobs (tenant_id, status, created_at);

CREATE TABLE email_messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  to_email text NOT NULL,
  subject text NOT NULL,
  body_text text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','sent','failed','dead','skipped')),
  attempts int NOT NULL DEFAULT 0,
  last_error text NOT NULL DEFAULT '',
  dedupe_key text NOT NULL DEFAULT '',
  related_type text NOT NULL DEFAULT '',
  related_id uuid,
  -- Non-secret delivery context (proof round + token id) so the worker can
  -- re-check that a link still works before sending. Never holds the plaintext.
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  sent_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_messages_queue_idx
  ON email_messages (tenant_id, status, created_at);
CREATE UNIQUE INDEX email_messages_dedupe_uniq
  ON email_messages (tenant_id, dedupe_key) WHERE dedupe_key <> '';

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete',
                  'gallery_deadline','gallery_submitted',
                  'proof_ready','proof_changes_requested','proof_resubmitted',
                  'proof_approved','proof_deadline','retouch_delivered'));

ALTER TABLE integration_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE integration_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY integration_jobs_tenant ON integration_jobs
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE email_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE email_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY email_messages_tenant ON email_messages
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The worker delivers email and its own proof deadline reminders. Email bodies
-- are cleared on success, so UPDATE is likewise needed; INSERT covers reminders
-- queued by the worker from the round's recipient snapshot.
GRANT SELECT, INSERT, UPDATE ON integration_jobs TO pwf_worker;
CREATE POLICY integration_jobs_worker ON integration_jobs
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON email_messages TO pwf_worker;
CREATE POLICY email_messages_worker ON email_messages
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM integration_jobs)
     OR EXISTS (SELECT 1 FROM email_messages) THEN
    RAISE EXCEPTION 'integration_jobs or email_messages contain rows; archive or delete them before rolling back 0018';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY IF EXISTS email_messages_worker ON email_messages;
REVOKE SELECT, INSERT, UPDATE ON email_messages FROM pwf_worker;
DROP POLICY IF EXISTS integration_jobs_worker ON integration_jobs;
REVOKE SELECT, INSERT, UPDATE ON integration_jobs FROM pwf_worker;

DROP TABLE IF EXISTS email_messages;
DROP TABLE IF EXISTS integration_jobs;

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete',
                  'gallery_deadline','gallery_submitted'));

ALTER TABLE clients DROP COLUMN email;
