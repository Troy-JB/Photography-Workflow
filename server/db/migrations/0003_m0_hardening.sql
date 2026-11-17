-- +goose Up

-- Projects carry the project lifecycle only. Payment, contract, asset and proof
-- states live in their own aggregates; these two columns are read models used
-- by transition gates and the project page.
UPDATE projects SET state = CASE state
  WHEN 'contract_sent' THEN 'contract_pending'
  WHEN 'contract_signed' THEN 'contracted'
  WHEN 'deposit_due' THEN 'contracted'
  WHEN 'prep' THEN 'shooting'
  WHEN 'shot' THEN 'shooting'
  WHEN 'culling' THEN 'post_production'
  WHEN 'client_selection' THEN 'post_production'
  WHEN 'retouching' THEN 'post_production'
  WHEN 'proofing' THEN 'post_production'
  WHEN 'revision_required' THEN 'post_production'
  WHEN 'approved' THEN 'completed'
  WHEN 'balance_due' THEN 'completed'
  WHEN 'delivered' THEN 'completed'
  ELSE state
END;

ALTER TABLE projects ADD COLUMN contract_status text NOT NULL DEFAULT 'none'
  CHECK (contract_status IN ('none','draft','rendered','sent','signing','signed','void'));
ALTER TABLE projects ADD COLUMN payment_status text NOT NULL DEFAULT 'unpaid'
  CHECK (payment_status IN ('unpaid','deposit_pending','deposit_paid','balance_pending','paid'));

UPDATE projects SET contract_status = 'signed'
  WHERE state IN ('contracted','booked','shooting','post_production','completed','archived');
UPDATE projects SET payment_status = 'deposit_paid'
  WHERE state IN ('booked','shooting','post_production','completed','archived');
UPDATE projects SET payment_status = 'paid'
  WHERE state IN ('completed','archived');

ALTER TABLE projects ADD CONSTRAINT projects_state_check CHECK (state IN (
  'draft','quoted','contract_pending','contracted','booked','shooting',
  'post_production','completed','archived','on_hold','cancelled'));

-- Asset states follow the ingest/cull/selection/retouch/deliver pipeline.
UPDATE assets SET status = CASE status
  WHEN 'retouch_in_progress' THEN 'retouching'
  WHEN 'retouch_done' THEN 'proofing'
  WHEN 'proofed' THEN 'proofing'
  WHEN 'rejected' THEN 'culled'
  ELSE status
END;

ALTER TABLE assets DROP CONSTRAINT assets_status_check;
ALTER TABLE assets ADD CONSTRAINT assets_status_check CHECK (status IN (
  'registered','uploaded','verified','derivative_ready','culled','selected',
  'retouching','proofing','approved','delivered','archived',
  'missing','corrupt','quarantined','integration_failed'));
ALTER TABLE assets ADD COLUMN verified_at timestamptz;
UPDATE assets SET verified_at = updated_at
  WHERE status IN ('verified','derivative_ready','culled','selected','retouching',
                   'proofing','approved','delivered','archived');

-- Tenant foreign keys that were missing in 0001.
ALTER TABLE project_state_logs
  ADD CONSTRAINT project_state_logs_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE project_state_logs
  ADD CONSTRAINT project_state_logs_actor_fk FOREIGN KEY (actor_user_id) REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE upload_sessions
  ADD CONSTRAINT upload_sessions_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE asset_derivatives
  ADD CONSTRAINT asset_derivatives_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE idempotency_keys
  ADD CONSTRAINT idempotency_keys_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE audit_logs
  ADD CONSTRAINT audit_logs_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE audit_logs
  ADD CONSTRAINT audit_logs_actor_fk FOREIGN KEY (actor_user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE idempotency_keys ADD COLUMN response_content_type text NOT NULL DEFAULT 'application/json';
ALTER TABLE idempotency_keys ADD COLUMN response_headers jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE idempotency_keys ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

ALTER TABLE ingest_batches DROP CONSTRAINT ingest_batches_status_check;
ALTER TABLE ingest_batches ADD CONSTRAINT ingest_batches_status_check
  CHECK (status IN ('open','verifying','completed','partial','failed'));
ALTER TABLE ingest_batches ADD COLUMN verify_job_id uuid;

-- Durable task queue. Asynq can sit in front of this table later; the database
-- row is the source of truth for attempts, retries and results.
CREATE TABLE jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  queue text NOT NULL DEFAULT 'default',
  kind text NOT NULL,
  subject_type text NOT NULL DEFAULT '',
  subject_id uuid,
  status text NOT NULL DEFAULT 'queued'
    CHECK (status IN ('queued','running','succeeded','failed')),
  attempts int NOT NULL DEFAULT 0,
  max_attempts int NOT NULL DEFAULT 10,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  result jsonb NOT NULL DEFAULT '{}'::jsonb,
  last_error text NOT NULL DEFAULT '',
  run_after timestamptz NOT NULL DEFAULT now(),
  locked_at timestamptz,
  locked_by text NOT NULL DEFAULT '',
  lease_token text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX jobs_claim_idx ON jobs (run_after) WHERE status = 'queued';
CREATE INDEX jobs_subject_idx ON jobs (tenant_id, subject_type, subject_id);
CREATE UNIQUE INDEX jobs_active_subject_uniq
  ON jobs (tenant_id, kind, subject_id)
  WHERE status IN ('queued','running') AND subject_id IS NOT NULL;

-- Transactional outbox: business writes and their events commit together.
CREATE TABLE outbox_events (
  id bigserial PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  aggregate_type text NOT NULL,
  aggregate_id text NOT NULL DEFAULT '',
  event_type text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','processing','sent','retrying','dead_letter')),
  attempts int NOT NULL DEFAULT 0,
  available_at timestamptz NOT NULL DEFAULT now(),
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz
);
CREATE INDEX outbox_pending_idx ON outbox_events (available_at) WHERE status IN ('pending','retrying');

ALTER TABLE ingest_batches
  ADD CONSTRAINT ingest_batches_verify_job_fk FOREIGN KEY (verify_job_id) REFERENCES jobs(id) ON DELETE SET NULL;

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY jobs_tenant ON jobs
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant ON outbox_events
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down

DROP TABLE IF EXISTS outbox_events;
ALTER TABLE ingest_batches DROP CONSTRAINT IF EXISTS ingest_batches_verify_job_fk;
DROP TABLE IF EXISTS jobs;

ALTER TABLE ingest_batches DROP COLUMN IF EXISTS verify_job_id;
ALTER TABLE ingest_batches DROP CONSTRAINT IF EXISTS ingest_batches_status_check;
ALTER TABLE ingest_batches ADD CONSTRAINT ingest_batches_status_check
  CHECK (status IN ('open','completed','failed'));

ALTER TABLE idempotency_keys DROP COLUMN IF EXISTS updated_at;
ALTER TABLE idempotency_keys DROP COLUMN IF EXISTS response_headers;
ALTER TABLE idempotency_keys DROP COLUMN IF EXISTS response_content_type;

ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS audit_logs_actor_fk;
ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS audit_logs_tenant_fk;
ALTER TABLE idempotency_keys DROP CONSTRAINT IF EXISTS idempotency_keys_tenant_fk;
ALTER TABLE asset_derivatives DROP CONSTRAINT IF EXISTS asset_derivatives_tenant_fk;
ALTER TABLE upload_sessions DROP CONSTRAINT IF EXISTS upload_sessions_tenant_fk;
ALTER TABLE project_state_logs DROP CONSTRAINT IF EXISTS project_state_logs_actor_fk;
ALTER TABLE project_state_logs DROP CONSTRAINT IF EXISTS project_state_logs_tenant_fk;

ALTER TABLE assets DROP COLUMN IF EXISTS verified_at;
ALTER TABLE assets DROP CONSTRAINT assets_status_check;
UPDATE assets SET status = CASE status
  WHEN 'retouching' THEN 'retouch_in_progress'
  WHEN 'proofing' THEN 'retouch_done'
  ELSE status
END;
ALTER TABLE assets ADD CONSTRAINT assets_status_check CHECK (status IN (
  'registered','uploaded','verified','derivative_ready','culled',
  'selected','rejected','retouch_in_progress','retouch_done',
  'proofed','approved','delivered','archived',
  'missing','corrupt','quarantined'));

ALTER TABLE projects DROP CONSTRAINT projects_state_check;
UPDATE projects SET state = CASE state
  WHEN 'contract_pending' THEN 'contract_sent'
  WHEN 'contracted' THEN 'contract_signed'
  WHEN 'shooting' THEN 'shot'
  WHEN 'post_production' THEN 'culling'
  WHEN 'completed' THEN 'delivered'
  ELSE state
END;
ALTER TABLE projects DROP COLUMN payment_status;
ALTER TABLE projects DROP COLUMN contract_status;
