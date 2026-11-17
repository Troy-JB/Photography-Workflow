-- +goose Up

-- M5 galleries. The immutable manifest lives in object storage; these tables
-- hold the mutable draft that becomes it, the customer tokens and the client's
-- decisions. expires_at doubles as the access and selection deadline.
ALTER TABLE galleries
  ADD COLUMN version int NOT NULL DEFAULT 1,
  ADD COLUMN selection_version int NOT NULL DEFAULT 0,
  ADD COLUMN submitted_at timestamptz,
  ADD COLUMN revoked_at timestamptz,
  ADD COLUMN manifest_key text NOT NULL DEFAULT '',
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX galleries_expiry_idx ON galleries (expires_at)
  WHERE status = 'published';
CREATE INDEX galleries_project_created_idx ON galleries (tenant_id, project_id, created_at DESC);

CREATE TABLE gallery_assets (
  tenant_id uuid NOT NULL,
  gallery_id uuid NOT NULL REFERENCES galleries(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  position int NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (gallery_id, asset_id),
  UNIQUE (gallery_id, position)
);
CREATE INDEX gallery_assets_asset_idx ON gallery_assets (tenant_id, asset_id);

-- Only the hash of a share token is stored. The plaintext is shown once at
-- creation and can never be recovered from the database.
CREATE TABLE gallery_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  gallery_id uuid NOT NULL REFERENCES galleries(id) ON DELETE CASCADE,
  token_hash text NOT NULL UNIQUE,
  label text NOT NULL DEFAULT '',
  max_visits int NOT NULL DEFAULT 0 CHECK (max_visits >= 0),
  visits int NOT NULL DEFAULT 0,
  expires_at timestamptz,
  revoked_at timestamptz,
  last_used_at timestamptz,
  created_by uuid,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX gallery_tokens_gallery_idx ON gallery_tokens (tenant_id, gallery_id);

CREATE TABLE gallery_selections (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  gallery_id uuid NOT NULL REFERENCES galleries(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL,
  decision text NOT NULL CHECK (decision IN ('selected','rejected')),
  decided_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (gallery_id, asset_id),
  FOREIGN KEY (gallery_id, asset_id) REFERENCES gallery_assets (gallery_id, asset_id) ON DELETE CASCADE
);
CREATE INDEX gallery_selections_gallery_idx ON gallery_selections (tenant_id, gallery_id, decision);

-- Rendering metadata (LQIP data URI, dominant colour, source size, failure
-- reason) rides along with each derivative row instead of a second table.
ALTER TABLE asset_derivatives ADD COLUMN meta jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE gallery_assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE gallery_assets FORCE ROW LEVEL SECURITY;
CREATE POLICY gallery_assets_tenant ON gallery_assets
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE gallery_selections ENABLE ROW LEVEL SECURITY;
ALTER TABLE gallery_selections FORCE ROW LEVEL SECURITY;
CREATE POLICY gallery_selections_tenant ON gallery_selections
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- gallery_tokens deliberately does NOT use FORCE ROW LEVEL SECURITY. The
-- customer API must resolve a token hash to its tenant before any tenant
-- context exists, and it does so through the SECURITY DEFINER function below.
-- FORCE would apply the tenant policy to the function's owner as well and make
-- that lookup impossible. Direct access by pwf_app is still filtered by the
-- policy; only the owner (migrations) bypasses it.
ALTER TABLE gallery_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY gallery_tokens_tenant ON gallery_tokens
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Resolves a share token without a tenant context. Returns only identifiers;
-- callers still load the gallery through db.WithTenant, so RLS applies to every
-- subsequent read. Executable by the app role only.
CREATE FUNCTION gallery_token_lookup(p_hash text)
RETURNS TABLE (token_id uuid, tenant_id uuid, gallery_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
  SELECT t.id, t.tenant_id, t.gallery_id
  FROM public.gallery_tokens t
  WHERE t.token_hash = p_hash
$$;
REVOKE ALL ON FUNCTION gallery_token_lookup(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION gallery_token_lookup(text) TO pwf_app;

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete',
                  'gallery_deadline','gallery_submitted'));

-- Worker least privilege for the M5 pipeline: it derives previews, sweeps
-- expired galleries and plans deadline reminders. It never reads client
-- selections or the gallery asset list.
GRANT SELECT, INSERT, UPDATE ON asset_derivatives TO pwf_worker;
CREATE POLICY asset_derivatives_worker ON asset_derivatives
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, UPDATE ON galleries TO pwf_worker;
CREATE POLICY galleries_worker ON galleries
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, UPDATE ON gallery_tokens TO pwf_worker;
CREATE POLICY gallery_tokens_worker ON gallery_tokens
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT INSERT ON notifications TO pwf_worker;

-- +goose Down

-- Galleries, tokens and selections are customer-facing records; refuse to drop
-- them silently.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM galleries)
     OR EXISTS (SELECT 1 FROM gallery_assets)
     OR EXISTS (SELECT 1 FROM gallery_tokens)
     OR EXISTS (SELECT 1 FROM gallery_selections) THEN
    RAISE EXCEPTION 'gallery tables contain rows; archive or delete them before rolling back 0015';
  END IF;
END $$;
-- +goose StatementEnd

REVOKE INSERT ON notifications FROM pwf_worker;

DROP POLICY IF EXISTS gallery_tokens_worker ON gallery_tokens;
REVOKE SELECT, UPDATE ON gallery_tokens FROM pwf_worker;
DROP POLICY IF EXISTS galleries_worker ON galleries;
REVOKE SELECT, UPDATE ON galleries FROM pwf_worker;
DROP POLICY IF EXISTS asset_derivatives_worker ON asset_derivatives;
REVOKE SELECT, INSERT, UPDATE ON asset_derivatives FROM pwf_worker;

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete'));

DROP FUNCTION IF EXISTS gallery_token_lookup(text);

DROP TABLE IF EXISTS gallery_selections;
DROP TABLE IF EXISTS gallery_tokens;
DROP TABLE IF EXISTS gallery_assets;

ALTER TABLE asset_derivatives DROP COLUMN meta;

DROP INDEX IF EXISTS galleries_project_created_idx;
DROP INDEX IF EXISTS galleries_expiry_idx;
ALTER TABLE galleries
  DROP COLUMN updated_at,
  DROP COLUMN manifest_key,
  DROP COLUMN revoked_at,
  DROP COLUMN submitted_at,
  DROP COLUMN selection_version,
  DROP COLUMN version;
