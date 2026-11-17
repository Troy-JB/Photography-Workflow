-- +goose Up

-- Customer identities are global to a person; studio links and public booking
-- configuration remain tenant-scoped.
CREATE TABLE customer_accounts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  wechat_openid text NOT NULL UNIQUE,
  name text NOT NULL DEFAULT '',
  phone text NOT NULL DEFAULT '',
  phone_verified_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE customer_studio_links (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  customer_id uuid NOT NULL REFERENCES customer_accounts(id) ON DELETE CASCADE,
  client_id uuid NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, customer_id),
  UNIQUE (tenant_id, client_id)
);
CREATE INDEX customer_studio_links_customer_idx ON customer_studio_links (customer_id, created_at DESC);

CREATE TABLE customer_invites (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  token_hash text NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_invites_tenant_idx ON customer_invites (tenant_id, created_at DESC);

CREATE TABLE booking_services (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
  name text NOT NULL,
  description text NOT NULL DEFAULT '',
  duration_minutes int NOT NULL CHECK (duration_minutes BETWEEN 15 AND 1440),
  price_cents bigint NOT NULL DEFAULT 0 CHECK (price_cents >= 0),
  active boolean NOT NULL DEFAULT true,
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);
CREATE INDEX booking_services_public_idx ON booking_services (tenant_id, active, name);

CREATE TABLE customer_refresh_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  customer_id uuid NOT NULL REFERENCES customer_accounts(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_refresh_tokens_account_idx ON customer_refresh_tokens (customer_id);

ALTER TABLE bookings
  ADD COLUMN customer_id uuid REFERENCES customer_accounts(id) ON DELETE SET NULL,
  ADD COLUMN customer_request_id text,
  ADD COLUMN service_id uuid REFERENCES booking_services(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX bookings_customer_request_uniq
  ON bookings (tenant_id, customer_id, customer_request_id)
  WHERE customer_id IS NOT NULL AND customer_request_id IS NOT NULL;

ALTER TABLE customer_studio_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_studio_links FORCE ROW LEVEL SECURITY;
CREATE POLICY customer_studio_links_tenant ON customer_studio_links
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE customer_invites ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_invites FORCE ROW LEVEL SECURITY;
CREATE POLICY customer_invites_tenant ON customer_invites
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE booking_services ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_services FORCE ROW LEVEL SECURITY;
CREATE POLICY booking_services_tenant ON booking_services
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Public invite resolution returns only tenant/id data. Follow-up queries run
-- under the resolved tenant context and remain subject to RLS.
CREATE FUNCTION customer_invite_lookup(p_hash text)
RETURNS TABLE (invite_id uuid, tenant_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
  SELECT i.id, i.tenant_id FROM public.customer_invites i
  WHERE i.token_hash = p_hash AND i.revoked_at IS NULL AND i.expires_at > now()
$$;
REVOKE ALL ON FUNCTION customer_invite_lookup(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customer_invite_lookup(text) TO pwf_app;

CREATE FUNCTION customer_studio_list(p_customer_id uuid)
RETURNS TABLE (tenant_id uuid, tenant_name text, client_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
  SELECT l.tenant_id, t.name, l.client_id
  FROM public.customer_studio_links l
  JOIN public.tenants t ON t.id = l.tenant_id
  WHERE l.customer_id = p_customer_id
$$;
REVOKE ALL ON FUNCTION customer_studio_list(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION customer_studio_list(uuid) TO pwf_app;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM customer_accounts)
     OR EXISTS (SELECT 1 FROM customer_studio_links)
     OR EXISTS (SELECT 1 FROM customer_invites)
     OR EXISTS (SELECT 1 FROM booking_services)
     OR EXISTS (SELECT 1 FROM bookings WHERE customer_id IS NOT NULL) THEN
    RAISE EXCEPTION 'customer portal contains data; preserve it before rolling back 0021';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX IF EXISTS bookings_customer_request_uniq;
ALTER TABLE bookings DROP COLUMN IF EXISTS service_id;
ALTER TABLE bookings DROP COLUMN IF EXISTS customer_request_id;
ALTER TABLE bookings DROP COLUMN IF EXISTS customer_id;
DROP FUNCTION IF EXISTS customer_invite_lookup(text);
DROP FUNCTION IF EXISTS customer_studio_list(uuid);
DROP TABLE IF EXISTS customer_refresh_tokens;
DROP TABLE IF EXISTS booking_services;
DROP TABLE IF EXISTS customer_invites;
DROP TABLE IF EXISTS customer_studio_links;
DROP TABLE IF EXISTS customer_accounts;
