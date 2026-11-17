-- +goose Up

-- Creating is the reservation written before the provider is called, so a
-- channel order can never exist without a local row that can be reconciled.
ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_status_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_status_check
  CHECK (status IN ('creating','pending','paid','closed'));

-- The live-order uniqueness must also cover reservations, otherwise a retry
-- could reserve a second row while the first provider call is still in flight.
DROP INDEX payment_orders_pending_uniq;
CREATE UNIQUE INDEX payment_orders_live_uniq
  ON payment_orders (tenant_id, project_id, kind) WHERE status IN ('creating','pending');

-- Authentic payments for orders that were closed locally are flagged for
-- refund/manual resolution instead of being silently credited.
ALTER TABLE payment_events DROP CONSTRAINT payment_events_outcome_check;
ALTER TABLE payment_events ADD CONSTRAINT payment_events_outcome_check
  CHECK (outcome IN ('applied','duplicate','rejected','flagged'));

-- Worker least privilege: it handles all tenants, but only jobs, the outbox,
-- assets and ingest batches. It no longer bypasses RLS; four permissive
-- policies scoped to the worker role give it exactly those tables.
ALTER ROLE pwf_worker NOBYPASSRLS;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM pwf_worker;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM pwf_worker;
GRANT SELECT, INSERT, UPDATE, DELETE ON jobs, assets, ingest_batches, outbox_events TO pwf_worker;
GRANT USAGE, SELECT ON SEQUENCE outbox_events_id_seq TO pwf_worker;

CREATE POLICY jobs_worker ON jobs
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);
CREATE POLICY assets_worker ON assets
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);
CREATE POLICY ingest_batches_worker ON ingest_batches
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);
CREATE POLICY outbox_events_worker ON outbox_events
  FOR ALL TO pwf_worker USING (true) WITH CHECK (true);

ALTER DEFAULT PRIVILEGES FOR ROLE pwf IN SCHEMA public
  REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM pwf_worker;
ALTER DEFAULT PRIVILEGES FOR ROLE pwf IN SCHEMA public
  REVOKE USAGE, SELECT ON SEQUENCES FROM pwf_worker;

-- +goose Down

DROP POLICY IF EXISTS outbox_events_worker ON outbox_events;
DROP POLICY IF EXISTS ingest_batches_worker ON ingest_batches;
DROP POLICY IF EXISTS assets_worker ON assets;
DROP POLICY IF EXISTS jobs_worker ON jobs;

-- Rolling back must not re-arm the worker as a database-wide super-reader.
-- The pre-0007 baseline (deploy/postgres/init/01-app-role.sql) is NOBYPASSRLS
-- with no table grants; restore exactly that. A v6 binary rolled back together
-- with this migration needs explicit grants provisioned by the operator.
ALTER DEFAULT PRIVILEGES FOR ROLE pwf IN SCHEMA public
  REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM pwf_worker;
ALTER DEFAULT PRIVILEGES FOR ROLE pwf IN SCHEMA public
  REVOKE USAGE, SELECT ON SEQUENCES FROM pwf_worker;

REVOKE ALL ON ALL TABLES IN SCHEMA public FROM pwf_worker;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM pwf_worker;
ALTER ROLE pwf_worker NOBYPASSRLS;

ALTER TABLE payment_events DROP CONSTRAINT payment_events_outcome_check;
ALTER TABLE payment_events ADD CONSTRAINT payment_events_outcome_check
  CHECK (outcome IN ('applied','duplicate','rejected'));

DROP INDEX payment_orders_live_uniq;
CREATE UNIQUE INDEX payment_orders_pending_uniq
  ON payment_orders (tenant_id, project_id, kind) WHERE status = 'pending';

ALTER TABLE payment_orders DROP CONSTRAINT payment_orders_status_check;
ALTER TABLE payment_orders ADD CONSTRAINT payment_orders_status_check
  CHECK (status IN ('pending','paid','closed'));
