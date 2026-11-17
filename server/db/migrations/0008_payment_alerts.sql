-- +goose Up

-- Security alerts are durable, deduplicated records raised by payment and
-- billing code inside the same transaction as the event that caused them.
-- The worker pushes them to the configured webhook and writes delivery state
-- back, so it needs a narrow grant on this table only.
CREATE TABLE payment_alerts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid REFERENCES projects(id) ON DELETE CASCADE,
  subject_type text NOT NULL DEFAULT '',
  subject_id text NOT NULL DEFAULT '',
  code text NOT NULL,
  severity text NOT NULL CHECK (severity IN ('info','warning','critical')),
  status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','acknowledged','resolved')),
  dedupe_key text NOT NULL,
  title text NOT NULL,
  detail jsonb NOT NULL DEFAULT '{}'::jsonb,
  amount_cents bigint,
  occurrences int NOT NULL DEFAULT 1,
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  acknowledged_by uuid REFERENCES users(id) ON DELETE SET NULL,
  acknowledged_at timestamptz,
  resolved_by uuid REFERENCES users(id) ON DELETE SET NULL,
  resolved_at timestamptz,
  resolution_note text NOT NULL DEFAULT '',
  notify_status text NOT NULL DEFAULT 'pending'
    CHECK (notify_status IN ('pending','skipped','sent','failed','dead')),
  notify_attempts int NOT NULL DEFAULT 0,
  last_notify_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Only one live alert per dedupe key; resolved alerts may be raised again.
CREATE UNIQUE INDEX payment_alerts_open_uniq
  ON payment_alerts (tenant_id, dedupe_key) WHERE status IN ('open','acknowledged');
CREATE INDEX payment_alerts_list_idx
  ON payment_alerts (tenant_id, status, severity, created_at DESC);
CREATE INDEX payment_alerts_project_idx
  ON payment_alerts (tenant_id, project_id, created_at DESC);

ALTER TABLE payment_alerts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_alerts FORCE ROW LEVEL SECURITY;
CREATE POLICY payment_alerts_tenant ON payment_alerts
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON payment_alerts TO pwf_worker;
CREATE POLICY payment_alerts_worker ON payment_alerts
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

-- +goose Down

DROP POLICY IF EXISTS payment_alerts_worker ON payment_alerts;
DROP TABLE IF EXISTS payment_alerts;
