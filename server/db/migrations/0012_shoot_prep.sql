-- +goose Up

-- Shoot preparation hangs off the booking (a booking is one shoot), so
-- reschedules follow automatically and cancelled/expired bookings keep a
-- read-only history. Equipment uses resources(kind='equipment') as its catalog;
-- shoot_gear is only the per-shoot checklist.

CREATE TABLE shot_lists (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  booking_id uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
  title text NOT NULL DEFAULT '',
  note text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, booking_id)
);

CREATE TABLE shot_items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  shot_list_id uuid NOT NULL REFERENCES shot_lists(id) ON DELETE CASCADE,
  sort_order int NOT NULL DEFAULT 0 CHECK (sort_order >= 0),
  category text NOT NULL DEFAULT '',
  title text NOT NULL,
  description text NOT NULL DEFAULT '',
  reference_url text NOT NULL DEFAULT '',
  priority text NOT NULL DEFAULT 'normal'
    CHECK (priority IN ('low','normal','high')),
  done boolean NOT NULL DEFAULT false,
  done_at timestamptz,
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (NOT done OR done_at IS NOT NULL)
);
CREATE INDEX shot_items_list_idx ON shot_items (tenant_id, shot_list_id, sort_order);

CREATE TABLE shoot_gear (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  booking_id uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
  resource_id uuid REFERENCES resources(id) ON DELETE SET NULL,
  name text NOT NULL,
  category text NOT NULL DEFAULT '',
  quantity int NOT NULL DEFAULT 1 CHECK (quantity BETWEEN 1 AND 100),
  checked boolean NOT NULL DEFAULT false,
  checked_at timestamptz,
  note text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, booking_id, name),
  CHECK (NOT checked OR checked_at IS NOT NULL)
);
CREATE INDEX shoot_gear_booking_idx ON shoot_gear (tenant_id, booking_id);

CREATE TABLE booking_authorizations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  booking_id uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
  kind text NOT NULL
    CHECK (kind IN ('model_release','venue_permit','usage_license','other')),
  subject text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','signed','waived')),
  signed_at timestamptz,
  note text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, booking_id, kind, subject),
  CHECK (status <> 'signed' OR signed_at IS NOT NULL)
);
CREATE INDEX booking_authorizations_booking_idx
  ON booking_authorizations (tenant_id, booking_id, status);

CREATE TABLE call_sheets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  booking_id uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
  version int NOT NULL CHECK (version >= 1),
  -- Snapshot bytes must never be re-serialized: the SHA-256 covers this text.
  snapshot text NOT NULL,
  snapshot_sha256 char(64) NOT NULL CHECK (snapshot_sha256 ~ '^[0-9a-f]{64}$'),
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, booking_id, version)
);

CREATE TABLE notifications (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  type text NOT NULL
    CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete')),
  subject_type text NOT NULL DEFAULT 'booking',
  subject_id uuid NOT NULL,
  dedupe_key text NOT NULL,
  title text NOT NULL,
  body text NOT NULL DEFAULT '',
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','sent','failed','skipped','dead','cancelled')),
  due_at timestamptz NOT NULL,
  sent_at timestamptz,
  read_at timestamptz,
  attempts int NOT NULL DEFAULT 0,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_subject_idx
  ON notifications (tenant_id, subject_type, subject_id, created_at DESC);
CREATE INDEX notifications_due_idx ON notifications (tenant_id, due_at) WHERE status = 'pending';
CREATE INDEX notifications_unread_idx ON notifications (tenant_id, created_at DESC) WHERE read_at IS NULL;
-- Only a pending row occupies a dedupe key, so a future plan can reuse a key
-- after its earlier notification was sent or cancelled. Already-sent keys are
-- still checked explicitly by the planner to avoid duplicate delivery.
CREATE UNIQUE INDEX notifications_pending_dedupe_uniq
  ON notifications (tenant_id, dedupe_key) WHERE status = 'pending';

CREATE TABLE notification_deliveries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  notification_id uuid NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
  channel text NOT NULL DEFAULT 'webhook' CHECK (channel IN ('webhook')),
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','sent','failed','skipped','dead')),
  attempts int NOT NULL DEFAULT 0,
  last_error text NOT NULL DEFAULT '',
  delivered_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, notification_id, channel)
);

ALTER TABLE shot_lists ENABLE ROW LEVEL SECURITY;
ALTER TABLE shot_lists FORCE ROW LEVEL SECURITY;
CREATE POLICY shot_lists_tenant ON shot_lists
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE shot_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE shot_items FORCE ROW LEVEL SECURITY;
CREATE POLICY shot_items_tenant ON shot_items
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE shoot_gear ENABLE ROW LEVEL SECURITY;
ALTER TABLE shoot_gear FORCE ROW LEVEL SECURITY;
CREATE POLICY shoot_gear_tenant ON shoot_gear
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE booking_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_authorizations FORCE ROW LEVEL SECURITY;
CREATE POLICY booking_authorizations_tenant ON booking_authorizations
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE call_sheets ENABLE ROW LEVEL SECURITY;
ALTER TABLE call_sheets FORCE ROW LEVEL SECURITY;
CREATE POLICY call_sheets_tenant ON call_sheets
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications FORCE ROW LEVEL SECURITY;
CREATE POLICY notifications_tenant ON notifications
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE notification_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY notification_deliveries_tenant ON notification_deliveries
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The worker dispatches due reminders and records deliveries. Authorization
-- rows are only read to re-check whether an auth_incomplete reminder is still
-- applicable. All other prep tables stay invisible to the worker.
GRANT SELECT, UPDATE ON notifications TO pwf_worker;
GRANT SELECT, INSERT, UPDATE ON notification_deliveries TO pwf_worker;
GRANT SELECT ON booking_authorizations TO pwf_worker;
CREATE POLICY notifications_worker ON notifications
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);
CREATE POLICY notification_deliveries_worker ON notification_deliveries
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);
CREATE POLICY booking_authorizations_worker ON booking_authorizations
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

-- +goose Down

-- Prep, snapshots and notification history are business records; refusing to
-- roll back over them is safer than silently destroying evidence.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM shot_lists) OR EXISTS (SELECT 1 FROM shot_items)
     OR EXISTS (SELECT 1 FROM shoot_gear) OR EXISTS (SELECT 1 FROM booking_authorizations)
     OR EXISTS (SELECT 1 FROM call_sheets) OR EXISTS (SELECT 1 FROM notifications)
     OR EXISTS (SELECT 1 FROM notification_deliveries) THEN
    RAISE EXCEPTION 'shoot preparation tables contain rows; archive or delete them before rolling back 0012';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY IF EXISTS booking_authorizations_worker ON booking_authorizations;
DROP POLICY IF EXISTS notification_deliveries_worker ON notification_deliveries;
DROP POLICY IF EXISTS notifications_worker ON notifications;
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS call_sheets;
DROP TABLE IF EXISTS booking_authorizations;
DROP TABLE IF EXISTS shoot_gear;
DROP TABLE IF EXISTS shot_items;
DROP TABLE IF EXISTS shot_lists;
