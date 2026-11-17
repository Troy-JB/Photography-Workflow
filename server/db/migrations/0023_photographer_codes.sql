-- +goose Up
ALTER TABLE users ADD COLUMN photographer_code text;

UPDATE users
SET photographer_code = 'PH' || upper(substr(replace(id::text, '-', ''), 1, 10));

ALTER TABLE users ALTER COLUMN photographer_code SET NOT NULL;
CREATE UNIQUE INDEX users_photographer_code_key ON users (photographer_code);

ALTER TABLE customer_accounts ALTER COLUMN wechat_openid DROP NOT NULL;

CREATE TABLE customer_login_codes (
  phone text PRIMARY KEY,
  code_hash bytea NOT NULL,
  expires_at timestamptz NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE customer_login_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_login_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY customer_login_codes_app ON customer_login_codes USING (true) WITH CHECK (true);
GRANT SELECT, INSERT, UPDATE, DELETE ON customer_login_codes TO pwf_app;

-- +goose Down
DROP TABLE IF EXISTS customer_login_codes;
ALTER TABLE customer_accounts ALTER COLUMN wechat_openid SET NOT NULL;
DROP INDEX IF EXISTS users_photographer_code_key;
ALTER TABLE users DROP COLUMN IF EXISTS photographer_code;
