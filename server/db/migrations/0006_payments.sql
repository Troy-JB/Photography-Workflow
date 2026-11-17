-- +goose Up

-- Payment aggregate: orders, collected payments and refunds. Orders carry the
-- amount asked for; payments are created only from a verified provider
-- callback; refunds are independent rows and never overwrite the payment.
CREATE TABLE payment_orders (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  contract_id uuid REFERENCES contracts(id) ON DELETE SET NULL,
  kind text NOT NULL CHECK (kind IN ('deposit','balance','full')),
  provider text NOT NULL,
  channel text NOT NULL DEFAULT 'sandbox',
  currency text NOT NULL DEFAULT 'CNY' CHECK (currency ~ '^[A-Z]{3}$'),
  amount_cents bigint NOT NULL CHECK (amount_cents > 0),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','paid','closed')),
  out_trade_no text NOT NULL,
  provider_order_id text NOT NULL DEFAULT '',
  pay_url text NOT NULL DEFAULT '',
  client_ip text NOT NULL DEFAULT '',
  expires_at timestamptz NOT NULL,
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, out_trade_no)
);
-- At most one live order per project and kind: double clicks reuse the order.
CREATE UNIQUE INDEX payment_orders_pending_uniq
  ON payment_orders (tenant_id, project_id, kind) WHERE status = 'pending';
CREATE INDEX payment_orders_project_idx ON payment_orders (tenant_id, project_id, created_at DESC);

-- Callback lookup happens before a tenant is known, so routing tables carry no
-- RLS. They store only the provider reference and the owning ids.
CREATE TABLE payment_order_routes (
  provider text NOT NULL,
  out_trade_no text NOT NULL,
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  order_id uuid NOT NULL REFERENCES payment_orders(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, out_trade_no)
);

CREATE TABLE payments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  order_id uuid NOT NULL REFERENCES payment_orders(id) ON DELETE RESTRICT,
  provider text NOT NULL,
  channel text NOT NULL DEFAULT 'sandbox',
  out_trade_no text NOT NULL,
  provider_trade_no text NOT NULL DEFAULT '',
  amount_cents bigint NOT NULL CHECK (amount_cents > 0),
  currency text NOT NULL DEFAULT 'CNY',
  paid_at timestamptz NOT NULL DEFAULT now(),
  event_id text NOT NULL DEFAULT '',
  raw jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- One order can only ever produce one payment, no matter how many times the
  -- provider redelivers the callback.
  UNIQUE (order_id)
);
CREATE UNIQUE INDEX payments_provider_trade_uniq
  ON payments (provider, provider_trade_no) WHERE provider_trade_no <> '';
CREATE INDEX payments_project_idx ON payments (tenant_id, project_id, paid_at DESC);

CREATE TABLE refunds (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  payment_id uuid NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
  provider text NOT NULL,
  out_refund_no text NOT NULL,
  provider_refund_id text NOT NULL DEFAULT '',
  amount_cents bigint NOT NULL CHECK (amount_cents > 0),
  currency text NOT NULL DEFAULT 'CNY',
  reason text NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','succeeded','failed')),
  failure_reason text NOT NULL DEFAULT '',
  created_by uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, out_refund_no)
);
CREATE UNIQUE INDEX refunds_provider_refund_uniq
  ON refunds (provider, provider_refund_id) WHERE provider_refund_id <> '';
CREATE INDEX refunds_payment_idx ON refunds (tenant_id, payment_id, created_at DESC);
CREATE INDEX refunds_project_idx ON refunds (tenant_id, project_id, created_at DESC);

CREATE TABLE payment_refund_routes (
  provider text NOT NULL,
  out_refund_no text NOT NULL,
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  refund_id uuid NOT NULL REFERENCES refunds(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, out_refund_no)
);

-- Provider events are recorded per tenant for replay detection and for the
-- security trail when a callback is rejected (amount/currency mismatch).
CREATE TABLE payment_events (
  id bigserial PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  provider text NOT NULL,
  event_id text NOT NULL,
  event_type text NOT NULL,
  ref_no text NOT NULL DEFAULT '',
  outcome text NOT NULL DEFAULT 'applied'
    CHECK (outcome IN ('applied','duplicate','rejected')),
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, event_id)
);

ALTER TABLE payment_orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_orders FORCE ROW LEVEL SECURITY;
CREATE POLICY payment_orders_tenant ON payment_orders
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments FORCE ROW LEVEL SECURITY;
CREATE POLICY payments_tenant ON payments
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE refunds FORCE ROW LEVEL SECURITY;
CREATE POLICY refunds_tenant ON refunds
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE payment_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_events FORCE ROW LEVEL SECURITY;
CREATE POLICY payment_events_tenant ON payment_events
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down

DROP TABLE IF EXISTS payment_events;
DROP TABLE IF EXISTS payment_refund_routes;
DROP TABLE IF EXISTS refunds;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS payment_order_routes;
DROP TABLE IF EXISTS payment_orders;
