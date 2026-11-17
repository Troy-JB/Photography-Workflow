-- +goose Up

-- Device ledger for the Windows capture agent. Registration is idempotent on
-- (tenant_id, fingerprint): the agent derives the fingerprint from a stable
-- machine/installation identity, never from a drive letter or hostname alone,
-- so reinstalling under a different path maps to the same device row.
--
-- public_key is the agent's ECDSA P-256 signing key (base64 PKIX DER). It is
-- recorded now to reserve the device-token upgrade path; M4 still authenticates
-- with the user's JWT.

CREATE TABLE agent_devices (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  -- Last user who registered this device; kept when a later registration comes
  -- from a different staff member on a shared workstation.
  user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
  fingerprint text NOT NULL CHECK (char_length(fingerprint) BETWEEN 1 AND 256),
  platform text NOT NULL DEFAULT '' CHECK (char_length(platform) <= 32),
  agent_version text NOT NULL DEFAULT '' CHECK (char_length(agent_version) <= 64),
  public_key text NOT NULL CHECK (char_length(public_key) BETWEEN 1 AND 4096),
  status text NOT NULL DEFAULT 'active'
    CHECK (status IN ('active','disabled')),
  last_seen_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, fingerprint)
);
CREATE INDEX agent_devices_seen_idx ON agent_devices (tenant_id, last_seen_at DESC);

ALTER TABLE agent_devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_devices FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_devices_tenant ON agent_devices
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The worker has no business with the device ledger. No grants are issued, so
-- an ingest worker that is compromised cannot enumerate or disable devices.

-- +goose Down

-- A disabled device is a security control; dropping the ledger silently would
-- re-admit it on the next registration. Refuse while rows exist.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM agent_devices) THEN
    RAISE EXCEPTION 'agent_devices contains rows; archive or delete them before rolling back 0013';
  END IF;
END $$;
-- +goose StatementEnd

DROP POLICY IF EXISTS agent_devices_tenant ON agent_devices;
DROP TABLE IF EXISTS agent_devices;
