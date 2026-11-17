-- +goose Up

-- M6 proofing: rounds snapshot the customer's selected assets, comments carry
-- normalised annotations, revisions are immutable retouched outputs and retouch
-- tasks hand work to an external provider. Each review round may repeat
-- customer feedback and retouch delivery until approval.

ALTER TABLE contracts ADD COLUMN included_rounds int NOT NULL DEFAULT 2
  CHECK (included_rounds >= 0);

CREATE TABLE proof_rounds (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  gallery_id uuid NOT NULL REFERENCES galleries(id) ON DELETE CASCADE,
  round_no int NOT NULL CHECK (round_no > 0),
  status text NOT NULL DEFAULT 'open'
    CHECK (status IN ('open','in_review','changes_requested','retouching','resubmitted','approved')),
  due_at timestamptz NOT NULL,
  viewed_at timestamptz,
  submitted_at timestamptz,
  approved_at timestamptz,
  change_order_id uuid REFERENCES change_orders(id) ON DELETE RESTRICT,
  override_by uuid REFERENCES users(id) ON DELETE SET NULL,
  override_reason text NOT NULL DEFAULT '',
  -- Recipient snapshot so the worker can queue deadline mail without reading
  -- clients (it has no grant on that table). Refreshed on explicit resend.
  client_email text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, project_id, round_no)
);
CREATE INDEX proof_rounds_project_idx
  ON proof_rounds (tenant_id, project_id, created_at DESC);
CREATE INDEX proof_rounds_due_idx ON proof_rounds (due_at)
  WHERE status IN ('open','in_review','changes_requested','resubmitted');
-- A supplement amendment funds exactly one extra round.
CREATE UNIQUE INDEX proof_rounds_change_order_uniq
  ON proof_rounds (tenant_id, change_order_id) WHERE change_order_id IS NOT NULL;

-- The service validates the gate with a row lock; this trigger is the last line
-- of defence so a future endpoint or manual fix-up cannot attach an unpaid or
-- unrelated amendment.
-- +goose StatementBegin
CREATE FUNCTION proof_round_gate() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  co_status text;
  co_project uuid;
  has_item boolean;
BEGIN
  IF NEW.change_order_id IS NULL THEN
    RETURN NEW;
  END IF;
  SELECT status, project_id INTO co_status, co_project
  FROM public.change_orders
  WHERE tenant_id = NEW.tenant_id AND id = NEW.change_order_id;
  IF co_project IS NULL THEN
    RAISE EXCEPTION 'extra_round_gate: change order % does not exist', NEW.change_order_id;
  END IF;
  IF co_project <> NEW.project_id THEN
    RAISE EXCEPTION 'extra_round_gate: change order belongs to another project';
  END IF;
  IF co_status <> 'applied' THEN
    RAISE EXCEPTION 'extra_round_gate: change order is %, not applied', co_status;
  END IF;
  SELECT EXISTS (
    SELECT 1 FROM public.change_order_items
    WHERE tenant_id = NEW.tenant_id AND change_order_id = NEW.change_order_id
      AND kind = 'extra_retouch'
  ) INTO has_item;
  IF NOT has_item THEN
    RAISE EXCEPTION 'extra_round_gate: change order has no extra_retouch item';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER proof_rounds_gate
  BEFORE INSERT OR UPDATE OF change_order_id ON proof_rounds
  FOR EACH ROW EXECUTE FUNCTION proof_round_gate();

CREATE TABLE proof_round_items (
  tenant_id uuid NOT NULL,
  proof_round_id uuid NOT NULL REFERENCES proof_rounds(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  position int NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (proof_round_id, asset_id),
  UNIQUE (proof_round_id, position)
);

-- Proof links are independent of the selection deadline and of each other.
-- Only the hash is stored; the plaintext is returned once.
CREATE TABLE proof_round_tokens (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  proof_round_id uuid NOT NULL REFERENCES proof_rounds(id) ON DELETE CASCADE,
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
CREATE INDEX proof_round_tokens_round_idx ON proof_round_tokens (tenant_id, proof_round_id);

CREATE TABLE comments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  proof_round_id uuid NOT NULL REFERENCES proof_rounds(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  parent_id uuid REFERENCES comments(id) ON DELETE CASCADE,
  author_type text NOT NULL CHECK (author_type IN ('client','staff')),
  author_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  client_ref text NOT NULL DEFAULT '',
  body text NOT NULL DEFAULT '',
  annotation jsonb,
  resolved_at timestamptz,
  resolved_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (body <> '' OR annotation IS NOT NULL),
  CHECK (octet_length(coalesce(annotation::text, '')) <= 16384)
);
CREATE INDEX comments_round_asset_idx
  ON comments (tenant_id, proof_round_id, asset_id, created_at);
CREATE UNIQUE INDEX comments_client_ref_uniq
  ON comments (tenant_id, proof_round_id, client_ref) WHERE client_ref <> '';

CREATE TABLE retouch_tasks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  proof_round_id uuid NOT NULL REFERENCES proof_rounds(id) ON DELETE CASCADE,
  status text NOT NULL DEFAULT 'queued'
    CHECK (status IN ('queued','in_progress','delivered','cancelled')),
  provider text NOT NULL DEFAULT 'manual',
  external_ref text NOT NULL DEFAULT '',
  due_at timestamptz,
  started_at timestamptz,
  delivered_at timestamptz,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX retouch_tasks_active_uniq
  ON retouch_tasks (tenant_id, proof_round_id)
  WHERE status IN ('queued','in_progress');
CREATE INDEX retouch_tasks_due_idx ON retouch_tasks (due_at)
  WHERE status IN ('queued','in_progress');

CREATE TABLE retouch_task_items (
  tenant_id uuid NOT NULL,
  retouch_task_id uuid NOT NULL REFERENCES retouch_tasks(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  position int NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (retouch_task_id, asset_id),
  UNIQUE (retouch_task_id, position)
);

-- A revision is an immutable retouched output. Checksum uniqueness makes a
-- repeated FolderWatch scan or retried upload a no-op instead of a new version.
CREATE TABLE asset_revisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  revision_no int NOT NULL CHECK (revision_no > 0),
  source_key text NOT NULL,
  sha256 text NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes > 0),
  width int NOT NULL DEFAULT 0,
  height int NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'uploaded'
    CHECK (status IN ('uploaded','derivative_ready','failed')),
  note text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (asset_id, revision_no),
  UNIQUE (tenant_id, asset_id, sha256)
);
CREATE INDEX asset_revisions_asset_idx
  ON asset_revisions (tenant_id, asset_id, revision_no DESC);

CREATE TABLE asset_revision_derivatives (
  revision_id uuid NOT NULL REFERENCES asset_revisions(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('thumb','display')),
  object_key text NOT NULL,
  width int NOT NULL,
  height int NOT NULL,
  meta jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (revision_id, kind)
);

ALTER TABLE proof_rounds ENABLE ROW LEVEL SECURITY;
ALTER TABLE proof_rounds FORCE ROW LEVEL SECURITY;
CREATE POLICY proof_rounds_tenant ON proof_rounds
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE proof_round_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE proof_round_items FORCE ROW LEVEL SECURITY;
CREATE POLICY proof_round_items_tenant ON proof_round_items
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Same pattern as gallery_tokens: a proof token is resolved before any tenant
-- context exists, so FORCE would also apply to the SECURITY DEFINER lookup and
-- make session exchange impossible.
ALTER TABLE proof_round_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY proof_round_tokens_tenant ON proof_round_tokens
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE comments FORCE ROW LEVEL SECURITY;
CREATE POLICY comments_tenant ON comments
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE retouch_tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE retouch_tasks FORCE ROW LEVEL SECURITY;
CREATE POLICY retouch_tasks_tenant ON retouch_tasks
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE retouch_task_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE retouch_task_items FORCE ROW LEVEL SECURITY;
CREATE POLICY retouch_task_items_tenant ON retouch_task_items
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE asset_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY asset_revisions_tenant ON asset_revisions
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE asset_revision_derivatives ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_revision_derivatives FORCE ROW LEVEL SECURITY;
CREATE POLICY asset_revision_derivatives_tenant ON asset_revision_derivatives
  USING (revision_id IN (
    SELECT id FROM asset_revisions
    WHERE tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid))
  WITH CHECK (revision_id IN (
    SELECT id FROM asset_revisions
    WHERE tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));

CREATE FUNCTION proof_token_lookup(p_hash text)
RETURNS TABLE (token_id uuid, tenant_id uuid, proof_round_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
  SELECT t.id, t.tenant_id, t.proof_round_id
  FROM public.proof_round_tokens t
  WHERE t.token_hash = p_hash
$$;
REVOKE ALL ON FUNCTION proof_token_lookup(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION proof_token_lookup(text) TO pwf_app;

-- Worker pipeline: derive revisions, sweep deadlines and poll the retouch
-- provider. It cannot read comments, but it needs the token row to validate a
-- link before mail is sent and to mint the deadline-reminder link. The tenant
-- name is the last-resort watermark when the gallery and config leave it empty,
-- so the worker may read just that column.
GRANT SELECT (id, name) ON tenants TO pwf_worker;

GRANT SELECT, INSERT ON proof_round_tokens TO pwf_worker;
CREATE POLICY proof_round_tokens_worker ON proof_round_tokens
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON proof_rounds TO pwf_worker;
CREATE POLICY proof_rounds_worker ON proof_rounds
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT ON proof_round_items TO pwf_worker;
CREATE POLICY proof_round_items_worker ON proof_round_items
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON retouch_tasks TO pwf_worker;
CREATE POLICY retouch_tasks_worker ON retouch_tasks
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON retouch_task_items TO pwf_worker;
CREATE POLICY retouch_task_items_worker ON retouch_task_items
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON asset_revisions TO pwf_worker;
CREATE POLICY asset_revisions_worker ON asset_revisions
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON asset_revision_derivatives TO pwf_worker;
CREATE POLICY asset_revision_derivatives_worker ON asset_revision_derivatives
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM proof_rounds)
     OR EXISTS (SELECT 1 FROM proof_round_items)
     OR EXISTS (SELECT 1 FROM proof_round_tokens)
     OR EXISTS (SELECT 1 FROM comments)
     OR EXISTS (SELECT 1 FROM retouch_tasks)
     OR EXISTS (SELECT 1 FROM retouch_task_items)
     OR EXISTS (SELECT 1 FROM asset_revisions)
     OR EXISTS (SELECT 1 FROM asset_revision_derivatives) THEN
    RAISE EXCEPTION 'proof tables contain rows; archive or delete them before rolling back 0017';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY IF EXISTS asset_revision_derivatives_worker ON asset_revision_derivatives;
REVOKE SELECT, INSERT, UPDATE ON asset_revision_derivatives FROM pwf_worker;
DROP POLICY IF EXISTS asset_revisions_worker ON asset_revisions;
REVOKE SELECT, INSERT, UPDATE ON asset_revisions FROM pwf_worker;
DROP POLICY IF EXISTS retouch_task_items_worker ON retouch_task_items;
REVOKE SELECT, INSERT, UPDATE ON retouch_task_items FROM pwf_worker;
DROP POLICY IF EXISTS retouch_tasks_worker ON retouch_tasks;
REVOKE SELECT, INSERT, UPDATE ON retouch_tasks FROM pwf_worker;
DROP POLICY IF EXISTS proof_round_items_worker ON proof_round_items;
REVOKE SELECT ON proof_round_items FROM pwf_worker;
DROP POLICY IF EXISTS proof_round_tokens_worker ON proof_round_tokens;
REVOKE SELECT, INSERT ON proof_round_tokens FROM pwf_worker;
DROP POLICY IF EXISTS proof_rounds_worker ON proof_rounds;
REVOKE SELECT, INSERT, UPDATE ON proof_rounds FROM pwf_worker;
REVOKE SELECT (id, name) ON tenants FROM pwf_worker;

DROP FUNCTION IF EXISTS proof_token_lookup(text);
DROP TABLE IF EXISTS asset_revision_derivatives;
DROP TABLE IF EXISTS asset_revisions;
DROP TABLE IF EXISTS retouch_task_items;
DROP TABLE IF EXISTS retouch_tasks;
DROP TABLE IF EXISTS comments;
DROP TABLE IF EXISTS proof_round_tokens;
DROP TABLE IF EXISTS proof_round_items;
DROP TRIGGER IF EXISTS proof_rounds_gate ON proof_rounds;
DROP TABLE IF EXISTS proof_rounds;
DROP FUNCTION IF EXISTS proof_round_gate();

ALTER TABLE contracts DROP COLUMN included_rounds;
