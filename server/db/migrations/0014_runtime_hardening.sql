-- +goose Up

-- Outbox delivery is at-least-once. A claimed event carries a lease token so a
-- worker that overran its lease can never mark an event sent after another
-- worker reclaimed it. available_at is the lease deadline while processing.
ALTER TABLE outbox_events
  ADD COLUMN locked_by text NOT NULL DEFAULT '',
  ADD COLUMN lease_token text NOT NULL DEFAULT '';
CREATE INDEX outbox_processing_idx ON outbox_events (available_at)
  WHERE status = 'processing';

-- Sending a contract now reserves the state before the vendor call, so a
-- second send cannot emit a duplicate signing task.
ALTER TABLE contracts DROP CONSTRAINT contracts_status_check;
ALTER TABLE contracts ADD CONSTRAINT contracts_status_check
  CHECK (status IN ('draft','rendered','submitting','sent','signing','signed','void'));

-- The worker sweeps reservations that never reached the channel. The partial
-- index keeps that scan cheap as the orders table grows.
CREATE INDEX payment_orders_creating_idx ON payment_orders (created_at)
  WHERE status = 'creating';

-- Worker least privilege for the reservation sweeper: it closes abandoned
-- creating orders, writes the audit trail and raises alerts. It still cannot
-- read tenant business tables.
GRANT SELECT, UPDATE ON payment_orders TO pwf_worker;
CREATE POLICY payment_orders_worker ON payment_orders
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

GRANT INSERT ON audit_logs TO pwf_worker;
GRANT USAGE, SELECT ON SEQUENCE audit_logs_id_seq TO pwf_worker;
CREATE POLICY audit_logs_worker_insert ON audit_logs
  FOR INSERT TO pwf_worker WITH CHECK (true);

-- +goose Down

DROP POLICY IF EXISTS audit_logs_worker_insert ON audit_logs;
REVOKE INSERT ON audit_logs FROM pwf_worker;
REVOKE USAGE, SELECT ON SEQUENCE audit_logs_id_seq FROM pwf_worker;

DROP POLICY IF EXISTS payment_orders_worker ON payment_orders;
REVOKE SELECT, UPDATE ON payment_orders FROM pwf_worker;

DROP INDEX IF EXISTS payment_orders_creating_idx;

UPDATE contracts SET status = 'rendered' WHERE status = 'submitting';
ALTER TABLE contracts DROP CONSTRAINT contracts_status_check;
ALTER TABLE contracts ADD CONSTRAINT contracts_status_check
  CHECK (status IN ('draft','rendered','sent','signing','signed','void'));

DROP INDEX IF EXISTS outbox_processing_idx;
ALTER TABLE outbox_events
  DROP COLUMN lease_token,
  DROP COLUMN locked_by;
