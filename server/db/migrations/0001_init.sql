-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE tenants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug text NOT NULL UNIQUE,
  name text NOT NULL,
  plan text NOT NULL DEFAULT 'solo',
  storage_prefix text NOT NULL DEFAULT '',
  cdn_config jsonb NOT NULL DEFAULT '{}'::jsonb,
  settings jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email text NOT NULL,
  password_hash text NOT NULL,
  name text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

CREATE TABLE memberships (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role text NOT NULL CHECK (role IN ('owner','staff','retoucher','assistant')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE refresh_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

CREATE TABLE clients (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name text NOT NULL,
  company text NOT NULL DEFAULT '',
  phone text NOT NULL DEFAULT '',
  wechat text NOT NULL DEFAULT '',
  source text NOT NULL DEFAULT '',
  tags text[] NOT NULL DEFAULT '{}',
  note text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX clients_tenant_name_idx ON clients (tenant_id, name);

CREATE TABLE projects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  code text NOT NULL,
  title text NOT NULL,
  client_id uuid REFERENCES clients(id) ON DELETE SET NULL,
  state text NOT NULL DEFAULT 'draft',
  shoot_start_at timestamptz,
  shoot_end_at timestamptz,
  owner_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  currency char(3) NOT NULL DEFAULT 'CNY',
  total_amount_cents bigint NOT NULL DEFAULT 0,
  deposit_cents bigint NOT NULL DEFAULT 0,
  balance_cents bigint NOT NULL DEFAULT 0,
  selection_deadline date,
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, code)
);
CREATE INDEX projects_tenant_state_idx ON projects (tenant_id, state, shoot_start_at);

CREATE TABLE project_state_logs (
  id bigserial PRIMARY KEY,
  tenant_id uuid NOT NULL,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  from_state text NOT NULL,
  to_state text NOT NULL,
  reason text NOT NULL DEFAULT '',
  actor_user_id uuid,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX project_state_logs_project_idx ON project_state_logs (project_id, created_at);

CREATE TABLE ingest_batches (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  device_id text NOT NULL DEFAULT '',
  card_fingerprint text NOT NULL DEFAULT '',
  file_count int NOT NULL DEFAULT 0,
  bytes bigint NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','completed','failed')),
  manifest_key text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX ingest_batches_project_idx ON ingest_batches (tenant_id, project_id, created_at DESC);

CREATE TABLE assets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  ingest_batch_id uuid REFERENCES ingest_batches(id) ON DELETE SET NULL,
  content_hash text NOT NULL,
  xxh64 text NOT NULL DEFAULT '',
  filename text NOT NULL,
  original_key text NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  mime text NOT NULL DEFAULT '',
  width int NOT NULL DEFAULT 0,
  height int NOT NULL DEFAULT 0,
  captured_at timestamptz,
  camera text NOT NULL DEFAULT '',
  lens text NOT NULL DEFAULT '',
  exif jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'registered'
    CHECK (status IN ('registered','uploaded','verified','derivative_ready','culled',
                      'selected','rejected','retouch_in_progress','retouch_done',
                      'proofed','approved','delivered','archived',
                      'missing','corrupt','quarantined')),
  is_video boolean NOT NULL DEFAULT false,
  blur_score double precision,
  eyes_closed_score double precision,
  dup_group text NOT NULL DEFAULT '',
  burst_group text NOT NULL DEFAULT '',
  best_pick boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, project_id, content_hash)
);
CREATE INDEX assets_project_status_idx ON assets (tenant_id, project_id, status);
CREATE INDEX assets_batch_idx ON assets (ingest_batch_id);
CREATE INDEX assets_dup_group_idx ON assets (tenant_id, project_id, dup_group) WHERE dup_group <> '';

CREATE TABLE upload_sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  upload_id text NOT NULL,
  object_key text NOT NULL,
  part_size int NOT NULL,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','completed','aborted')),
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  UNIQUE (upload_id)
);
CREATE INDEX upload_sessions_asset_idx ON upload_sessions (asset_id);

CREATE TABLE asset_derivatives (
  tenant_id uuid NOT NULL,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  kind text NOT NULL,
  is_watermarked boolean NOT NULL DEFAULT false,
  object_key text NOT NULL,
  width int NOT NULL DEFAULT 0,
  height int NOT NULL DEFAULT 0,
  bytes bigint NOT NULL DEFAULT 0,
  content_hash text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (asset_id, kind, is_watermarked)
);

CREATE TABLE galleries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  title text NOT NULL,
  status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','published','expired','revoked')),
  round_no int NOT NULL DEFAULT 1,
  manifest_version int NOT NULL DEFAULT 1,
  selection_limit int,
  download_allowed boolean NOT NULL DEFAULT false,
  expires_at timestamptz,
  published_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX galleries_project_idx ON galleries (tenant_id, project_id, status);

CREATE TABLE idempotency_keys (
  tenant_id uuid NOT NULL,
  endpoint text NOT NULL,
  key text NOT NULL,
  request_hash text NOT NULL,
  status int NOT NULL,
  response_body jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, endpoint, key)
);

CREATE TABLE audit_logs (
  id bigserial PRIMARY KEY,
  tenant_id uuid NOT NULL,
  actor_user_id uuid,
  action text NOT NULL,
  object_type text NOT NULL DEFAULT '',
  object_id text NOT NULL DEFAULT '',
  ip text NOT NULL DEFAULT '',
  detail jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_tenant_idx ON audit_logs (tenant_id, created_at DESC);

-- Row level security: fail closed when app.tenant_id is not set.
ALTER TABLE clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE clients FORCE ROW LEVEL SECURITY;
CREATE POLICY clients_tenant ON clients
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY projects_tenant ON projects
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE project_state_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_state_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY project_state_logs_tenant ON project_state_logs
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE ingest_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE ingest_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY ingest_batches_tenant ON ingest_batches
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE assets FORCE ROW LEVEL SECURITY;
CREATE POLICY assets_tenant ON assets
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE upload_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE upload_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY upload_sessions_tenant ON upload_sessions
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE asset_derivatives ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_derivatives FORCE ROW LEVEL SECURITY;
CREATE POLICY asset_derivatives_tenant ON asset_derivatives
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE galleries ENABLE ROW LEVEL SECURITY;
ALTER TABLE galleries FORCE ROW LEVEL SECURITY;
CREATE POLICY galleries_tenant ON galleries
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant ON idempotency_keys
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_logs_tenant ON audit_logs
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS galleries;
DROP TABLE IF EXISTS asset_derivatives;
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS assets;
DROP TABLE IF EXISTS ingest_batches;
DROP TABLE IF EXISTS project_state_logs;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS clients;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
