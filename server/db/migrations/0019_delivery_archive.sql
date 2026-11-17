-- +goose Up

-- M7 delivery and archive. A delivery is bound to one approved proof round and
-- owns the web/original packages plus the download links handed to the
-- customer. An archive snapshots every stored object of a finished project,
-- verifies it by SHA-256 and tracks the tenant retention policy. Nothing is
-- deleted automatically: deleting objects is a separate, owner-approved action.

CREATE TABLE deliveries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  proof_round_id uuid NOT NULL REFERENCES proof_rounds(id) ON DELETE CASCADE,
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','building','ready','failed','blocked')),
  build_version int NOT NULL DEFAULT 1 CHECK (build_version > 0),
  item_count int NOT NULL DEFAULT 0,
  total_bytes bigint NOT NULL DEFAULT 0,
  blocked_reason text NOT NULL DEFAULT '',
  last_error text NOT NULL DEFAULT '',
  ready_at timestamptz,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- One delivery per approved round; rebuilding bumps build_version instead of
  -- creating a second row.
  UNIQUE (tenant_id, proof_round_id)
);
CREATE INDEX deliveries_project_idx ON deliveries (tenant_id, project_id, created_at DESC);
CREATE INDEX deliveries_status_idx ON deliveries (tenant_id, status, created_at DESC);

CREATE TABLE delivery_items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  delivery_id uuid NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  position int NOT NULL CHECK (position > 0),
  -- Final file for the customer: the newest ready revision when retouching
  -- happened, otherwise the original upload. Snapshotted so a later revision
  -- cannot silently change what an issued link serves.
  original_object_key text NOT NULL DEFAULT '',
  original_name text NOT NULL DEFAULT '',
  original_mime text NOT NULL DEFAULT '',
  original_size bigint NOT NULL DEFAULT 0,
  original_sha256 text NOT NULL DEFAULT '',
  revision_id uuid REFERENCES asset_revisions(id) ON DELETE SET NULL,
  revision_no int NOT NULL DEFAULT 0,
  web_object_key text NOT NULL DEFAULT '',
  web_name text NOT NULL DEFAULT '',
  web_size bigint NOT NULL DEFAULT 0,
  web_sha256 text NOT NULL DEFAULT '',
  web_width int NOT NULL DEFAULT 0,
  web_height int NOT NULL DEFAULT 0,
  web_status text NOT NULL DEFAULT 'pending'
    CHECK (web_status IN ('pending','ready','skipped')),
  web_warning_code text NOT NULL DEFAULT '',
  web_warning text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (delivery_id, asset_id),
  UNIQUE (delivery_id, position)
);
CREATE INDEX delivery_items_web_state_idx
  ON delivery_items (tenant_id, delivery_id, web_status);

CREATE TABLE delivery_packages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  delivery_id uuid NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
  spec text NOT NULL CHECK (spec IN ('web','original')),
  version int NOT NULL CHECK (version > 0),
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','building','ready','failed')),
  object_key text NOT NULL DEFAULT '',
  file_name text NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  sha256 text NOT NULL DEFAULT '',
  entry_count int NOT NULL DEFAULT 0,
  manifest jsonb NOT NULL DEFAULT '{}'::jsonb,
  last_error text NOT NULL DEFAULT '',
  built_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (delivery_id, spec, version)
);

-- Download links follow the gallery/proof token pattern: only the SHA-256 is
-- stored and the plaintext is returned once.
CREATE TABLE download_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  delivery_id uuid NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
  token_hash text NOT NULL UNIQUE,
  label text NOT NULL DEFAULT '',
  recipient text NOT NULL DEFAULT '',
  max_downloads int NOT NULL DEFAULT 20 CHECK (max_downloads > 0),
  downloads int NOT NULL DEFAULT 0 CHECK (downloads >= 0),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  revoked_by uuid REFERENCES users(id) ON DELETE SET NULL,
  last_used_at timestamptz,
  last_used_ip text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX download_tokens_delivery_idx
  ON download_tokens (tenant_id, delivery_id, created_at DESC);

CREATE TABLE archives (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','verifying','ready','failed','deleting','deleted')),
  version int NOT NULL DEFAULT 1 CHECK (version > 0),
  object_count int NOT NULL DEFAULT 0,
  verified_count int NOT NULL DEFAULT 0,
  failed_count int NOT NULL DEFAULT 0,
  total_bytes bigint NOT NULL DEFAULT 0,
  keep_days int NOT NULL DEFAULT 365 CHECK (keep_days BETWEEN 1 AND 3650),
  warn_days int NOT NULL DEFAULT 30 CHECK (warn_days >= 0 AND warn_days < keep_days),
  retention_until timestamptz,
  warned_at timestamptz,
  verified_at timestamptz,
  deletion_requested_at timestamptz,
  confirmed_by uuid REFERENCES users(id) ON DELETE SET NULL,
  reason text NOT NULL DEFAULT '',
  deleted_at timestamptz,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, project_id)
);
CREATE INDEX archives_retention_idx ON archives (retention_until)
  WHERE status = 'ready';
CREATE INDEX archives_tenant_status_idx ON archives (tenant_id, status, created_at DESC);

CREATE TABLE archive_items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  archive_id uuid NOT NULL REFERENCES archives(id) ON DELETE CASCADE,
  asset_id uuid REFERENCES assets(id) ON DELETE SET NULL,
  kind text NOT NULL CHECK (kind IN (
    'original','derivative','revision','revision_derivative',
    'delivery_web','delivery_package',
    'contract_document','contract_signed','invoice_file')),
  object_key text NOT NULL,
  expected_sha256 text NOT NULL DEFAULT '',
  expected_size bigint NOT NULL DEFAULT 0,
  actual_sha256 text NOT NULL DEFAULT '',
  actual_size bigint NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','verified','missing','mismatch','deleted')),
  note text NOT NULL DEFAULT '',
  checked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (archive_id, object_key)
);
CREATE INDEX archive_items_status_idx
  ON archive_items (tenant_id, archive_id, status);

CREATE TABLE retention_policies (
  tenant_id uuid PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  keep_days int NOT NULL DEFAULT 365 CHECK (keep_days BETWEEN 1 AND 3650),
  warn_days int NOT NULL DEFAULT 30 CHECK (warn_days >= 0 AND warn_days < keep_days),
  updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO retention_policies (tenant_id)
SELECT id FROM tenants
ON CONFLICT (tenant_id) DO NOTHING;

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete',
                  'gallery_deadline','gallery_submitted',
                  'proof_ready','proof_changes_requested','proof_resubmitted',
                  'proof_approved','proof_deadline','retouch_delivered',
                  'delivery_ready','archive_ready','archive_retention_warning',
                  'archive_deleted'));

ALTER TABLE deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY deliveries_tenant ON deliveries
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE delivery_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_items FORCE ROW LEVEL SECURITY;
CREATE POLICY delivery_items_tenant ON delivery_items
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE delivery_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_packages FORCE ROW LEVEL SECURITY;
CREATE POLICY delivery_packages_tenant ON delivery_packages
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Same pattern as gallery/proof tokens: a download token is resolved before any
-- tenant context exists, so FORCE would also apply to the SECURITY DEFINER
-- lookup and make session exchange impossible.
ALTER TABLE download_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY download_tokens_tenant ON download_tokens
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE archives ENABLE ROW LEVEL SECURITY;
ALTER TABLE archives FORCE ROW LEVEL SECURITY;
CREATE POLICY archives_tenant ON archives
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE archive_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE archive_items FORCE ROW LEVEL SECURITY;
CREATE POLICY archive_items_tenant ON archive_items
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE retention_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE retention_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY retention_policies_tenant ON retention_policies
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE FUNCTION delivery_token_lookup(p_hash text)
RETURNS TABLE (token_id uuid, tenant_id uuid, delivery_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
  SELECT t.id, t.tenant_id, t.delivery_id
  FROM public.download_tokens t
  WHERE t.token_hash = p_hash
$$;
REVOKE ALL ON FUNCTION delivery_token_lookup(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION delivery_token_lookup(text) TO pwf_app;

-- The worker builds packages, verifies archives and raises retention warnings,
-- so it reads each project's payment/state (the finalize gate re-checks payment
-- inside the worker) and the contract/invoice objects it must verify.
GRANT SELECT ON projects TO pwf_worker;
CREATE POLICY projects_worker ON projects
  FOR SELECT TO pwf_worker USING (true);

-- Finalize re-checks payment under FOR SHARE OF projects, and PostgreSQL
-- requires UPDATE privilege to take that row lock. Scope the grant to the id
-- column: the worker can lock the row but has no writable column.
GRANT UPDATE (id) ON projects TO pwf_worker;

GRANT SELECT ON contracts TO pwf_worker;
CREATE POLICY contracts_worker ON contracts
  FOR SELECT TO pwf_worker USING (true);

GRANT SELECT ON invoices TO pwf_worker;
CREATE POLICY invoices_worker ON invoices
  FOR SELECT TO pwf_worker USING (true);

GRANT SELECT, INSERT, UPDATE ON deliveries TO pwf_worker;
CREATE POLICY deliveries_worker ON deliveries
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

-- A rebuild resets items/packages for the delivery before re-planning, so the
-- worker needs DELETE on both (delete a delivery row itself stays app-only).
GRANT SELECT, INSERT, UPDATE, DELETE ON delivery_items TO pwf_worker;
CREATE POLICY delivery_items_worker ON delivery_items
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON delivery_packages TO pwf_worker;
CREATE POLICY delivery_packages_worker ON delivery_packages
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON download_tokens TO pwf_worker;
CREATE POLICY download_tokens_worker ON download_tokens
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON archives TO pwf_worker;
CREATE POLICY archives_worker ON archives
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON archive_items TO pwf_worker;
CREATE POLICY archive_items_worker ON archive_items
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT ON retention_policies TO pwf_worker;
CREATE POLICY retention_policies_worker ON retention_policies
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM deliveries)
     OR EXISTS (SELECT 1 FROM delivery_items)
     OR EXISTS (SELECT 1 FROM delivery_packages)
     OR EXISTS (SELECT 1 FROM download_tokens)
     OR EXISTS (SELECT 1 FROM archives)
     OR EXISTS (SELECT 1 FROM archive_items) THEN
    RAISE EXCEPTION 'delivery or archive tables contain rows; archive or delete them before rolling back 0019';
  END IF;
  -- The down migration narrows the notification CHECK; rows using the M7
  -- types would make that ALTER fail halfway.
  IF EXISTS (SELECT 1 FROM notifications WHERE type IN (
      'delivery_ready','archive_ready','archive_retention_warning','archive_deleted')) THEN
    RAISE EXCEPTION 'delivery or archive notifications contain rows; delete them before rolling back 0019';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY IF EXISTS retention_policies_worker ON retention_policies;
REVOKE SELECT ON retention_policies FROM pwf_worker;
DROP POLICY IF EXISTS archive_items_worker ON archive_items;
REVOKE SELECT, INSERT, UPDATE ON archive_items FROM pwf_worker;
DROP POLICY IF EXISTS archives_worker ON archives;
REVOKE SELECT, INSERT, UPDATE ON archives FROM pwf_worker;
DROP POLICY IF EXISTS download_tokens_worker ON download_tokens;
REVOKE SELECT, INSERT, UPDATE ON download_tokens FROM pwf_worker;
DROP POLICY IF EXISTS delivery_packages_worker ON delivery_packages;
REVOKE SELECT, INSERT, UPDATE ON delivery_packages FROM pwf_worker;
DROP POLICY IF EXISTS delivery_items_worker ON delivery_items;
REVOKE SELECT, INSERT, UPDATE ON delivery_items FROM pwf_worker;
DROP POLICY IF EXISTS deliveries_worker ON deliveries;
REVOKE SELECT, INSERT, UPDATE ON deliveries FROM pwf_worker;
DROP POLICY IF EXISTS invoices_worker ON invoices;
REVOKE SELECT ON invoices FROM pwf_worker;
DROP POLICY IF EXISTS contracts_worker ON contracts;
REVOKE SELECT ON contracts FROM pwf_worker;
DROP POLICY IF EXISTS projects_worker ON projects;
REVOKE SELECT ON projects FROM pwf_worker;
REVOKE UPDATE (id) ON projects FROM pwf_worker;

DROP FUNCTION IF EXISTS delivery_token_lookup(text);
DROP TABLE IF EXISTS retention_policies;
DROP TABLE IF EXISTS archive_items;
DROP TABLE IF EXISTS archives;
DROP TABLE IF EXISTS download_tokens;
DROP TABLE IF EXISTS delivery_packages;
DROP TABLE IF EXISTS delivery_items;
DROP TABLE IF EXISTS deliveries;

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete',
                  'gallery_deadline','gallery_submitted',
                  'proof_ready','proof_changes_requested','proof_resubmitted',
                  'proof_approved','proof_deadline','retouch_delivered'));
