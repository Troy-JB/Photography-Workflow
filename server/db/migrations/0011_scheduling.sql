-- +goose Up

-- Scheduling core: resources, weekly availability rules, bookings and the
-- single source of truth for occupancy. Every confirmed booking, temporary
-- hold and manual block is one row in resource_blocks; the GiST exclusion
-- constraint below is the only arbiter of double booking.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE resources (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  kind text NOT NULL
    CHECK (kind IN ('photographer','studio','makeup','assistant','equipment')),
  name text NOT NULL,
  active boolean NOT NULL DEFAULT true,
  buffer_before_minutes int NOT NULL DEFAULT 0
    CHECK (buffer_before_minutes BETWEEN 0 AND 1440),
  buffer_after_minutes int NOT NULL DEFAULT 0
    CHECK (buffer_after_minutes BETWEEN 0 AND 1440),
  notes text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, kind, name)
);
CREATE INDEX resources_tenant_kind_idx ON resources (tenant_id, kind, active);

CREATE TABLE availability_rules (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  weekday int NOT NULL CHECK (weekday BETWEEN 1 AND 7),
  start_minute int NOT NULL CHECK (start_minute BETWEEN 0 AND 1439),
  end_minute int NOT NULL CHECK (end_minute BETWEEN 1 AND 1440),
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (start_minute < end_minute)
);
CREATE INDEX availability_rules_resource_idx
  ON availability_rules (tenant_id, resource_id, weekday, start_minute);

CREATE TABLE bookings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  status text NOT NULL DEFAULT 'held'
    CHECK (status IN ('held','confirmed','cancelled','expired')),
  starts_at timestamptz NOT NULL,
  ends_at timestamptz NOT NULL,
  note text NOT NULL DEFAULT '',
  hold_expires_at timestamptz,
  confirmed_at timestamptz,
  cancelled_at timestamptz,
  cancel_reason text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (ends_at > starts_at),
  CHECK (status <> 'held' OR hold_expires_at IS NOT NULL)
);
-- One active booking per project keeps projects.shoot_start_at unambiguous.
CREATE UNIQUE INDEX bookings_active_project_uniq ON bookings (tenant_id, project_id)
  WHERE status IN ('held','confirmed');
CREATE INDEX bookings_status_idx ON bookings (tenant_id, status, starts_at);
CREATE INDEX bookings_project_idx ON bookings (tenant_id, project_id, created_at DESC);
CREATE INDEX bookings_expiry_idx ON bookings (tenant_id, hold_expires_at) WHERE status = 'held';

CREATE TABLE booking_resources (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  booking_id uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
  resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
  PRIMARY KEY (booking_id, resource_id)
);
CREATE INDEX booking_resources_resource_idx ON booking_resources (tenant_id, resource_id);

CREATE TABLE resource_blocks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  booking_id uuid REFERENCES bookings(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('hold','booking','manual','closure')),
  during tstzrange NOT NULL CHECK (NOT isempty(during)),
  expires_at timestamptz,
  note text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'hold') = (expires_at IS NOT NULL)),
  CONSTRAINT resource_blocks_no_overlap EXCLUDE USING gist
    (tenant_id WITH =, resource_id WITH =, during WITH &&)
);
CREATE INDEX resource_blocks_resource_idx ON resource_blocks (tenant_id, resource_id, during);
CREATE INDEX resource_blocks_booking_idx ON resource_blocks (tenant_id, booking_id);
CREATE INDEX resource_blocks_expiry_idx ON resource_blocks (tenant_id, expires_at) WHERE kind = 'hold';

ALTER TABLE resources ENABLE ROW LEVEL SECURITY;
ALTER TABLE resources FORCE ROW LEVEL SECURITY;
CREATE POLICY resources_tenant ON resources
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE availability_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE availability_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY availability_rules_tenant ON availability_rules
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE bookings ENABLE ROW LEVEL SECURITY;
ALTER TABLE bookings FORCE ROW LEVEL SECURITY;
CREATE POLICY bookings_tenant ON bookings
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE booking_resources ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_resources FORCE ROW LEVEL SECURITY;
CREATE POLICY booking_resources_tenant ON booking_resources
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE resource_blocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE resource_blocks FORCE ROW LEVEL SECURITY;
CREATE POLICY resource_blocks_tenant ON resource_blocks
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The worker expires lapsed holds: it updates bookings and deletes their hold
-- blocks. It is not granted access to resources, availability rules or
-- booking_resources.
GRANT SELECT, UPDATE ON bookings TO pwf_worker;
GRANT SELECT, DELETE ON resource_blocks TO pwf_worker;
CREATE POLICY bookings_worker ON bookings
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);
CREATE POLICY resource_blocks_worker ON resource_blocks
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

-- +goose Down

-- Occupancy is business data; dropping it silently would erase booked slots.
-- StatementBegin/End keeps goose from splitting the DO block on its inner
-- semicolons.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM bookings) OR EXISTS (SELECT 1 FROM resources)
     OR EXISTS (SELECT 1 FROM resource_blocks) THEN
    RAISE EXCEPTION 'scheduling tables contain rows; archive or delete them before rolling back 0011';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY IF EXISTS resource_blocks_worker ON resource_blocks;
DROP POLICY IF EXISTS bookings_worker ON bookings;
DROP TABLE IF EXISTS resource_blocks;
DROP TABLE IF EXISTS booking_resources;
DROP TABLE IF EXISTS bookings;
DROP TABLE IF EXISTS availability_rules;
DROP TABLE IF EXISTS resources;
