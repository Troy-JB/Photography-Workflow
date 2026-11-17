-- Dev-only bootstrap: the API connects as a non-superuser so RLS is enforced.
-- Migrations and seed use the owner role (pwf) instead.
CREATE ROLE pwf_app LOGIN PASSWORD 'pwf-app'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;

-- Worker processes all tenants, but is separate from the migration owner and
-- does not bypass RLS: migration 0007 grants it only jobs/assets/ingest_batches/
-- outbox_events with permissive policies scoped to its role.
-- Production must replace this development password with a secret-managed one.
CREATE ROLE pwf_worker LOGIN PASSWORD 'pwf-worker'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;

GRANT USAGE ON SCHEMA public TO pwf_app;

ALTER DEFAULT PRIVILEGES FOR ROLE pwf IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pwf_app;
ALTER DEFAULT PRIVILEGES FOR ROLE pwf IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO pwf_app;

GRANT USAGE ON SCHEMA public TO pwf_worker;

