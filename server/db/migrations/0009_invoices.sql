-- +goose Up

-- Invoice request workflow. M2 ships the manual provider: an operator records
-- the invoice numbers issued by the tax system; the file and its hash are
-- verified before the invoice leaves "approved". One payment can have at most
-- one non-voided/non-rejected invoice, and the amount always equals the
-- payment amount (set by the service, never by the client).
CREATE TABLE invoices (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  payment_id uuid NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
  type text NOT NULL DEFAULT 'normal' CHECK (type IN ('normal','special')),
  status text NOT NULL DEFAULT 'requested'
    CHECK (status IN ('requested','approved','rejected','issued','sent','voided')),
  title text NOT NULL,
  tax_no text NOT NULL DEFAULT '',
  email text NOT NULL DEFAULT '',
  address text NOT NULL DEFAULT '',
  phone text NOT NULL DEFAULT '',
  bank_name text NOT NULL DEFAULT '',
  bank_account text NOT NULL DEFAULT '',
  amount_cents bigint NOT NULL CHECK (amount_cents > 0),
  currency text NOT NULL DEFAULT 'CNY' CHECK (currency ~ '^[A-Z]{3}$'),
  remark text NOT NULL DEFAULT '',
  invoice_no text NOT NULL DEFAULT '',
  invoice_code text NOT NULL DEFAULT '',
  file_key text NOT NULL DEFAULT '',
  file_hash text NOT NULL DEFAULT '',
  file_size bigint NOT NULL DEFAULT 0,
  requested_by uuid REFERENCES users(id) ON DELETE SET NULL,
  reviewed_by uuid REFERENCES users(id) ON DELETE SET NULL,
  reviewed_at timestamptz,
  reject_reason text NOT NULL DEFAULT '',
  issued_at timestamptz,
  sent_at timestamptz,
  voided_at timestamptz,
  void_reason text NOT NULL DEFAULT '',
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX invoices_no_uniq
  ON invoices (tenant_id, invoice_no) WHERE invoice_no <> '';
CREATE UNIQUE INDEX invoices_active_payment_uniq
  ON invoices (tenant_id, payment_id) WHERE status NOT IN ('voided','rejected');
CREATE INDEX invoices_list_idx ON invoices (tenant_id, status, created_at DESC);
CREATE INDEX invoices_project_idx ON invoices (tenant_id, project_id, created_at DESC);

-- Red-flush (credit-note) work items generated when a refund settles against an
-- issued invoice. refund_id is unique so a redelivered refund callback can
-- never queue the same red flush twice.
CREATE TABLE invoice_red_flushes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  invoice_id uuid NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
  refund_id uuid NOT NULL UNIQUE REFERENCES refunds(id) ON DELETE RESTRICT,
  amount_cents bigint NOT NULL CHECK (amount_cents > 0),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','issued')),
  red_flush_no text NOT NULL DEFAULT '',
  red_flush_code text NOT NULL DEFAULT '',
  file_key text NOT NULL DEFAULT '',
  file_hash text NOT NULL DEFAULT '',
  file_size bigint NOT NULL DEFAULT 0,
  reason text NOT NULL DEFAULT '',
  issued_by uuid REFERENCES users(id) ON DELETE SET NULL,
  issued_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX invoice_red_flushes_no_uniq
  ON invoice_red_flushes (tenant_id, red_flush_no) WHERE red_flush_no <> '';
CREATE INDEX invoice_red_flushes_invoice_idx
  ON invoice_red_flushes (tenant_id, invoice_id, created_at);

ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE ROW LEVEL SECURITY;
CREATE POLICY invoices_tenant ON invoices
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE invoice_red_flushes ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_red_flushes FORCE ROW LEVEL SECURITY;
CREATE POLICY invoice_red_flushes_tenant ON invoice_red_flushes
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down

DROP TABLE IF EXISTS invoice_red_flushes;
DROP TABLE IF EXISTS invoices;
