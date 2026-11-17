-- +goose Up

-- A change order is a signed amendment on top of the contract. It never edits
-- projects.total_amount_cents: the contract snapshot stays frozen and the
-- applied change amount is projected into its own column.
CREATE TABLE change_orders (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  order_no text NOT NULL,
  status text NOT NULL DEFAULT 'draft'
    CHECK (status IN ('draft','awaiting_signature','signed','awaiting_payment','applied','cancelled','reversed')),
  currency text NOT NULL DEFAULT 'CNY' CHECK (currency ~ '^[A-Z]{3}$'),
  amount_cents bigint NOT NULL CHECK (amount_cents > 0),
  reason text NOT NULL DEFAULT '',
  snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  sign_task_id text NOT NULL DEFAULT '',
  sign_url text NOT NULL DEFAULT '',
  signer_name text NOT NULL DEFAULT '',
  signer_contact text NOT NULL DEFAULT '',
  signed_document_hash text NOT NULL DEFAULT '',
  signed_at timestamptz,
  payment_order_id uuid,
  applied_at timestamptz,
  cancelled_at timestamptz,
  void_reason text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX change_orders_no_uniq ON change_orders (tenant_id, order_no);
-- One open amendment per project: parallel change orders would make the
-- supplement payment ambiguous.
CREATE UNIQUE INDEX change_orders_active_uniq ON change_orders (tenant_id, project_id)
  WHERE status IN ('draft','awaiting_signature','signed','awaiting_payment');
CREATE INDEX change_orders_project_idx ON change_orders (tenant_id, project_id, created_at DESC);

CREATE TABLE change_order_items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  change_order_id uuid NOT NULL REFERENCES change_orders(id) ON DELETE CASCADE,
  kind text NOT NULL
    CHECK (kind IN ('extra_retouch','reshoot','travel','rush','other')),
  description text NOT NULL,
  quantity int NOT NULL CHECK (quantity > 0),
  unit_price_cents bigint NOT NULL CHECK (unit_price_cents >= 0),
  amount_cents bigint NOT NULL CHECK (amount_cents >= 0),
  sort int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX change_order_items_order_idx
  ON change_order_items (tenant_id, change_order_id, sort, id);

-- Applied change orders are projected here so the effective receivable is
-- total_amount_cents + change_amount_cents without touching the contract
-- snapshot. Only the change-order aggregate writes this column.
ALTER TABLE projects ADD COLUMN change_amount_cents bigint NOT NULL DEFAULT 0;
ALTER TABLE projects DROP CONSTRAINT projects_payment_status_check;
ALTER TABLE projects ADD CONSTRAINT projects_payment_status_check
  CHECK (payment_status IN ('unpaid','deposit_pending','deposit_paid','balance_pending','change_pending','paid'));

-- E-sign tasks now route either a contract or a change order (exactly one).
ALTER TABLE esign_tasks ALTER COLUMN contract_id DROP NOT NULL;
ALTER TABLE esign_tasks ADD COLUMN change_order_id uuid
  REFERENCES change_orders(id) ON DELETE CASCADE;
ALTER TABLE esign_tasks ADD CONSTRAINT esign_tasks_subject_check
  CHECK (num_nonnulls(contract_id, change_order_id) = 1);
CREATE INDEX esign_tasks_change_order_idx ON esign_tasks (tenant_id, change_order_id);

-- Change orders are collected through their own payment order. The live
-- uniqueness now includes the change order so a project can hold a core order
-- and a supplement order at the same time.
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_kind_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_kind_check
  CHECK (kind IN ('deposit','balance','full','change'));
ALTER TABLE payment_orders ADD COLUMN change_order_id uuid
  REFERENCES change_orders(id) ON DELETE RESTRICT;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_change_link_check
  CHECK ((kind = 'change') = (change_order_id IS NOT NULL));
DROP INDEX payment_orders_live_uniq;
CREATE UNIQUE INDEX payment_orders_live_uniq
  ON payment_orders (tenant_id, project_id, kind,
    coalesce(change_order_id, '00000000-0000-0000-0000-000000000000'::uuid))
  WHERE status IN ('creating','pending');

ALTER TABLE change_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_orders FORCE ROW LEVEL SECURITY;
CREATE POLICY change_orders_tenant ON change_orders
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE change_order_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_order_items FORCE ROW LEVEL SECURITY;
CREATE POLICY change_order_items_tenant ON change_order_items
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down

-- Change-order money is not reconstructible from the contract snapshot, so a
-- destructive rollback must be a human decision. StatementBegin/End keeps
-- goose from splitting the DO block on its inner semicolons.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM change_orders) THEN
    RAISE EXCEPTION 'change_orders contains rows; archive or delete them before rolling back 0010';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX IF EXISTS payment_orders_live_uniq;
ALTER TABLE payment_orders DROP CONSTRAINT IF EXISTS payment_orders_change_link_check;
ALTER TABLE payment_orders DROP COLUMN IF EXISTS change_order_id;
CREATE UNIQUE INDEX payment_orders_live_uniq
  ON payment_orders (tenant_id, project_id, kind) WHERE status IN ('creating','pending');
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_kind_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_kind_check
  CHECK (kind IN ('deposit','balance','full'));

DROP INDEX IF EXISTS esign_tasks_change_order_idx;
ALTER TABLE esign_tasks DROP CONSTRAINT IF EXISTS esign_tasks_subject_check;
ALTER TABLE esign_tasks DROP COLUMN IF EXISTS change_order_id;
ALTER TABLE esign_tasks ALTER COLUMN contract_id SET NOT NULL;

ALTER TABLE projects DROP CONSTRAINT projects_payment_status_check;
ALTER TABLE projects ADD CONSTRAINT projects_payment_status_check
  CHECK (payment_status IN ('unpaid','deposit_pending','deposit_paid','balance_pending','paid'));
ALTER TABLE projects DROP COLUMN IF EXISTS change_amount_cents;

DROP TABLE IF EXISTS change_order_items;
DROP POLICY IF EXISTS change_orders_tenant ON change_orders;
DROP TABLE IF EXISTS change_orders;
