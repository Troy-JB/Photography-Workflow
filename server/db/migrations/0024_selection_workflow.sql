-- +goose Up

-- Snapshot package retouch terms when a customer reserves a service.
ALTER TABLE booking_services
  ADD COLUMN included_retouch_count int NOT NULL DEFAULT 0
    CHECK (included_retouch_count BETWEEN 0 AND 10000),
  ADD COLUMN extra_retouch_unit_price_cents bigint NOT NULL DEFAULT 0
    CHECK (extra_retouch_unit_price_cents >= 0);

ALTER TABLE bookings
  ADD COLUMN included_retouch_count int NOT NULL DEFAULT 0
    CHECK (included_retouch_count BETWEEN 0 AND 10000),
  ADD COLUMN extra_retouch_unit_price_cents bigint NOT NULL DEFAULT 0
    CHECK (extra_retouch_unit_price_cents >= 0);

ALTER TABLE galleries
  ADD COLUMN first_submitted_at timestamptz,
  ADD COLUMN selection_edit_until timestamptz,
  ADD COLUMN selection_locked_at timestamptz,
  ADD COLUMN included_retouch_count int NOT NULL DEFAULT 0,
  ADD COLUMN extra_retouch_unit_price_cents bigint NOT NULL DEFAULT 0
    CHECK (extra_retouch_unit_price_cents >= 0);
UPDATE galleries
SET first_submitted_at = submitted_at,
    selection_locked_at = submitted_at
WHERE submitted_at IS NOT NULL;

CREATE TABLE selection_charge_batches (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  gallery_id uuid NOT NULL REFERENCES galleries(id) ON DELETE CASCADE,
  quantity int NOT NULL CHECK (quantity > 0),
  unit_price_cents bigint NOT NULL CHECK (unit_price_cents > 0),
  amount_cents bigint NOT NULL CHECK (amount_cents = quantity * unit_price_cents),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','paid','closed')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX selection_charge_batches_gallery_idx
  ON selection_charge_batches (tenant_id, gallery_id, created_at);
CREATE UNIQUE INDEX selection_charge_batches_one_pending_uniq
  ON selection_charge_batches (tenant_id, gallery_id) WHERE status = 'pending';
ALTER TABLE selection_charge_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE selection_charge_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY selection_charge_batches_tenant ON selection_charge_batches
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON selection_charge_batches TO pwf_app;
GRANT SELECT ON selection_charge_batches TO pwf_worker;
CREATE POLICY selection_charge_batches_worker ON selection_charge_batches
  FOR SELECT TO pwf_worker USING (true);

-- The worker only needs photo-selection counts to calculate a delayed refund;
-- it never receives asset bytes, manifests, client identities, or share tokens.
GRANT SELECT ON gallery_assets, gallery_selections TO pwf_worker;
CREATE POLICY gallery_assets_worker_selection_count ON gallery_assets
  FOR SELECT TO pwf_worker USING (true);
CREATE POLICY gallery_selections_worker_selection_count ON gallery_selections
  FOR SELECT TO pwf_worker USING (true);

-- Delayed selection refunds use the existing payment service from the worker.
-- Grants are constrained to the columns it locks or settles.
GRANT SELECT, UPDATE (payment_status) ON projects TO pwf_worker;
CREATE POLICY projects_worker_payment_status ON projects
  FOR UPDATE TO pwf_worker
  USING (EXISTS (SELECT 1 FROM payment_orders o WHERE o.tenant_id=projects.tenant_id AND o.project_id=projects.id AND o.kind='selection_extra'))
  WITH CHECK (EXISTS (SELECT 1 FROM payment_orders o WHERE o.tenant_id=projects.tenant_id AND o.project_id=projects.id AND o.kind='selection_extra'));
GRANT SELECT, UPDATE (id) ON payments TO pwf_worker;
CREATE POLICY payments_worker_selection_refund ON payments
  FOR ALL TO pwf_worker
  USING (EXISTS (SELECT 1 FROM payment_orders o WHERE o.tenant_id=payments.tenant_id AND o.id=payments.order_id AND o.kind='selection_extra'))
  WITH CHECK (EXISTS (SELECT 1 FROM payment_orders o WHERE o.tenant_id=payments.tenant_id AND o.id=payments.order_id AND o.kind='selection_extra'));
GRANT SELECT, INSERT ON refunds TO pwf_worker;
GRANT UPDATE (status, provider_refund_id, failure_reason, updated_at) ON refunds TO pwf_worker;
CREATE POLICY refunds_worker_selection_refund ON refunds
  FOR ALL TO pwf_worker
  USING (EXISTS (SELECT 1 FROM payments p JOIN payment_orders o ON o.tenant_id=p.tenant_id AND o.id=p.order_id
    WHERE p.tenant_id=refunds.tenant_id AND p.id=refunds.payment_id AND o.kind='selection_extra'))
  WITH CHECK (EXISTS (SELECT 1 FROM payments p JOIN payment_orders o ON o.tenant_id=p.tenant_id AND o.id=p.order_id
    WHERE p.tenant_id=refunds.tenant_id AND p.id=refunds.payment_id AND o.kind='selection_extra'));
GRANT INSERT ON payment_refund_routes TO pwf_worker;

ALTER TABLE payment_orders
  DROP CONSTRAINT payment_orders_kind_check,
  ADD CONSTRAINT payment_orders_kind_check
    CHECK (kind IN ('deposit','balance','full','change','selection_extra')),
  ADD COLUMN selection_charge_batch_id uuid
    REFERENCES selection_charge_batches(id) ON DELETE RESTRICT,
  ADD CONSTRAINT payment_orders_selection_link_check
    CHECK ((kind = 'selection_extra') = (selection_charge_batch_id IS NOT NULL));

DROP INDEX payment_orders_live_uniq;
CREATE UNIQUE INDEX payment_orders_live_uniq
  ON payment_orders (tenant_id, project_id, kind,
    coalesce(change_order_id, '00000000-0000-0000-0000-000000000000'::uuid),
    coalesce(selection_charge_batch_id, '00000000-0000-0000-0000-000000000000'::uuid))
  WHERE status IN ('creating','pending');

-- All rows in an extra-photo payment order belong to the separately scoped
-- batch. Disallow contract/change-order links on this kind at the database.
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_selection_charge_isolated
  CHECK (kind <> 'selection_extra' OR (contract_id IS NULL AND change_order_id IS NULL));

-- +goose Down
REVOKE INSERT ON payment_refund_routes FROM pwf_worker;
DROP POLICY IF EXISTS refunds_worker_selection_refund ON refunds;
REVOKE SELECT, INSERT ON refunds FROM pwf_worker;
REVOKE UPDATE (status, provider_refund_id, failure_reason, updated_at) ON refunds FROM pwf_worker;
DROP POLICY IF EXISTS payments_worker_selection_refund ON payments;
REVOKE SELECT ON payments FROM pwf_worker;
REVOKE UPDATE (id) ON payments FROM pwf_worker;
DROP POLICY IF EXISTS projects_worker_payment_status ON projects;
REVOKE UPDATE (payment_status) ON projects FROM pwf_worker;
DROP POLICY IF EXISTS gallery_selections_worker_selection_count ON gallery_selections;
DROP POLICY IF EXISTS gallery_assets_worker_selection_count ON gallery_assets;
REVOKE SELECT ON gallery_assets, gallery_selections FROM pwf_worker;
DROP POLICY IF EXISTS selection_charge_batches_worker ON selection_charge_batches;
REVOKE SELECT ON selection_charge_batches FROM pwf_worker;
REVOKE INSERT, UPDATE, DELETE ON selection_charge_batches FROM pwf_app;
DROP INDEX IF EXISTS payment_orders_live_uniq;
DROP INDEX IF EXISTS selection_charge_batches_one_pending_uniq;
ALTER TABLE payment_orders DROP CONSTRAINT IF EXISTS payment_orders_selection_link_check;
ALTER TABLE payment_orders DROP CONSTRAINT IF EXISTS payment_orders_selection_charge_isolated;
ALTER TABLE payment_orders DROP COLUMN IF EXISTS selection_charge_batch_id;
ALTER TABLE payment_orders DROP CONSTRAINT IF EXISTS payment_orders_kind_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_kind_check CHECK (kind IN ('deposit','balance','full','change'));
CREATE UNIQUE INDEX payment_orders_live_uniq
  ON payment_orders (tenant_id, project_id, kind,
    coalesce(change_order_id, '00000000-0000-0000-0000-000000000000'::uuid))
  WHERE status IN ('creating','pending');
DROP TABLE IF EXISTS selection_charge_batches;
ALTER TABLE galleries DROP COLUMN IF EXISTS first_submitted_at;
ALTER TABLE galleries DROP COLUMN IF EXISTS selection_edit_until;
ALTER TABLE galleries DROP COLUMN IF EXISTS selection_locked_at;
ALTER TABLE galleries DROP COLUMN IF EXISTS included_retouch_count;
ALTER TABLE galleries DROP COLUMN IF EXISTS extra_retouch_unit_price_cents;
ALTER TABLE bookings DROP COLUMN IF EXISTS included_retouch_count;
ALTER TABLE bookings DROP COLUMN IF EXISTS extra_retouch_unit_price_cents;
ALTER TABLE booking_services DROP COLUMN IF EXISTS included_retouch_count;
ALTER TABLE booking_services DROP COLUMN IF EXISTS extra_retouch_unit_price_cents;
