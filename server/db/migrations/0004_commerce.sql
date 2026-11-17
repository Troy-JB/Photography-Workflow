-- +goose Up

-- Clients gain optimistic locking so edits cannot clobber a concurrent change.
ALTER TABLE clients ADD COLUMN version int NOT NULL DEFAULT 1;
ALTER TABLE clients ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX clients_tenant_created_idx ON clients (tenant_id, created_at DESC);

CREATE TABLE leads (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name text NOT NULL,
  company text NOT NULL DEFAULT '',
  phone text NOT NULL DEFAULT '',
  wechat text NOT NULL DEFAULT '',
  source text NOT NULL DEFAULT '',
  need text NOT NULL DEFAULT '',
  budget_min_cents bigint NOT NULL DEFAULT 0,
  budget_max_cents bigint NOT NULL DEFAULT 0,
  shoot_on date,
  status text NOT NULL DEFAULT 'new'
    CHECK (status IN ('new','contacted','qualified','quoted','won','lost','converted')),
  owner_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  client_id uuid REFERENCES clients(id) ON DELETE SET NULL,
  project_id uuid REFERENCES projects(id) ON DELETE SET NULL,
  lost_reason text NOT NULL DEFAULT '',
  note text NOT NULL DEFAULT '',
  next_follow_up_at timestamptz,
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (budget_min_cents >= 0 AND budget_max_cents >= 0),
  CHECK (budget_max_cents = 0 OR budget_min_cents <= budget_max_cents)
);
CREATE INDEX leads_tenant_status_idx ON leads (tenant_id, status, next_follow_up_at);
CREATE INDEX leads_tenant_created_idx ON leads (tenant_id, created_at DESC);

CREATE TABLE lead_follow_ups (
  id bigserial PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  lead_id uuid NOT NULL REFERENCES leads(id) ON DELETE CASCADE,
  actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  channel text NOT NULL DEFAULT '',
  summary text NOT NULL,
  next_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX lead_follow_ups_lead_idx ON lead_follow_ups (tenant_id, lead_id, created_at DESC);

CREATE TABLE project_briefs (
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  version int NOT NULL DEFAULT 1,
  style text NOT NULL DEFAULT '',
  shot_count int NOT NULL DEFAULT 0 CHECK (shot_count >= 0),
  venue text NOT NULL DEFAULT '',
  crew text NOT NULL DEFAULT '',
  deliverable_spec text NOT NULL DEFAULT '',
  usage_scope text NOT NULL DEFAULT '',
  requirements text NOT NULL DEFAULT '',
  details jsonb NOT NULL DEFAULT '{}'::jsonb,
  updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, project_id)
);

CREATE TABLE quotes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  version int NOT NULL,
  status text NOT NULL DEFAULT 'draft'
    CHECK (status IN ('draft','sent','accepted','rejected','expired','superseded')),
  currency char(3) NOT NULL DEFAULT 'CNY',
  subtotal_cents bigint NOT NULL DEFAULT 0,
  discount_cents bigint NOT NULL DEFAULT 0,
  tax_cents bigint NOT NULL DEFAULT 0,
  total_cents bigint NOT NULL DEFAULT 0,
  deposit_cents bigint NOT NULL DEFAULT 0,
  valid_until date,
  terms text NOT NULL DEFAULT '',
  snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  row_version int NOT NULL DEFAULT 1,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  sent_at timestamptz,
  accepted_at timestamptz,
  rejected_at timestamptz,
  superseded_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, project_id, version),
  CHECK (subtotal_cents >= 0 AND discount_cents >= 0 AND tax_cents >= 0),
  CHECK (total_cents >= 0 AND deposit_cents >= 0),
  CHECK (deposit_cents <= total_cents),
  CHECK (total_cents = subtotal_cents - discount_cents + tax_cents)
);
CREATE INDEX quotes_project_idx ON quotes (tenant_id, project_id, version DESC);

-- A quote can have at most one draft per project; versions are created from a
-- sent/accepted/rejected predecessor, never by editing a frozen quote.
CREATE UNIQUE INDEX quotes_one_draft_uniq ON quotes (tenant_id, project_id) WHERE status = 'draft';

CREATE TABLE quote_items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  quote_id uuid NOT NULL REFERENCES quotes(id) ON DELETE CASCADE,
  kind text NOT NULL DEFAULT 'service'
    CHECK (kind IN ('service','retouch','production','travel','rush','license','other')),
  description text NOT NULL,
  quantity int NOT NULL DEFAULT 1 CHECK (quantity > 0),
  unit_price_cents bigint NOT NULL DEFAULT 0 CHECK (unit_price_cents >= 0),
  amount_cents bigint NOT NULL CHECK (amount_cents >= 0),
  sort int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX quote_items_quote_idx ON quote_items (tenant_id, quote_id, sort, id);

CREATE TABLE contracts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  quote_id uuid NOT NULL REFERENCES quotes(id) ON DELETE RESTRICT,
  quote_version int NOT NULL,
  status text NOT NULL DEFAULT 'rendered'
    CHECK (status IN ('draft','rendered','sent','signing','signed','void')),
  template_version text NOT NULL DEFAULT 'v1',
  snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  document_key text NOT NULL DEFAULT '',
  document_hash text NOT NULL DEFAULT '',
  sign_task_id text NOT NULL DEFAULT '',
  sign_url text NOT NULL DEFAULT '',
  signed_document_key text NOT NULL DEFAULT '',
  signed_document_hash text NOT NULL DEFAULT '',
  signer_name text NOT NULL DEFAULT '',
  signer_contact text NOT NULL DEFAULT '',
  evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
  sent_at timestamptz,
  signed_at timestamptz,
  void_reason text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX contracts_active_project_uniq
  ON contracts (tenant_id, project_id) WHERE status <> 'void';
CREATE INDEX contracts_project_idx ON contracts (tenant_id, project_id, created_at DESC);

-- Callback lookup happens before a tenant is known, so this mapping is an
-- infrastructure table without RLS (like users/refresh_tokens). It stores only
-- the provider task id and the owning tenant/contract.
CREATE TABLE esign_tasks (
  provider text NOT NULL,
  sign_task_id text NOT NULL,
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  contract_id uuid NOT NULL REFERENCES contracts(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, sign_task_id)
);
CREATE INDEX esign_tasks_contract_idx ON esign_tasks (tenant_id, contract_id);

-- Provider events are recorded for replay detection. Signed states are guarded
-- by the contract row itself, so a duplicate event can never sign twice.
CREATE TABLE esign_events (
  id bigserial PRIMARY KEY,
  provider text NOT NULL,
  event_id text NOT NULL,
  sign_task_id text NOT NULL,
  event_type text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  processed_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, event_id)
);

ALTER TABLE leads ENABLE ROW LEVEL SECURITY;
ALTER TABLE leads FORCE ROW LEVEL SECURITY;
CREATE POLICY leads_tenant ON leads
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE lead_follow_ups ENABLE ROW LEVEL SECURITY;
ALTER TABLE lead_follow_ups FORCE ROW LEVEL SECURITY;
CREATE POLICY lead_follow_ups_tenant ON lead_follow_ups
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE project_briefs ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_briefs FORCE ROW LEVEL SECURITY;
CREATE POLICY project_briefs_tenant ON project_briefs
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE quotes ENABLE ROW LEVEL SECURITY;
ALTER TABLE quotes FORCE ROW LEVEL SECURITY;
CREATE POLICY quotes_tenant ON quotes
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE quote_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE quote_items FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_items_tenant ON quote_items
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contracts ENABLE ROW LEVEL SECURITY;
ALTER TABLE contracts FORCE ROW LEVEL SECURITY;
CREATE POLICY contracts_tenant ON contracts
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down

DROP TABLE IF EXISTS esign_events;
DROP TABLE IF EXISTS esign_tasks;
DROP TABLE IF EXISTS contracts;
DROP TABLE IF EXISTS quote_items;
DROP TABLE IF EXISTS quotes;
DROP TABLE IF EXISTS project_briefs;
DROP TABLE IF EXISTS lead_follow_ups;
DROP TABLE IF EXISTS leads;
DROP INDEX IF EXISTS clients_tenant_created_idx;
ALTER TABLE clients DROP COLUMN IF EXISTS updated_at;
ALTER TABLE clients DROP COLUMN IF EXISTS version;
