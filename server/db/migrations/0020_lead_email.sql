-- +goose Up
ALTER TABLE leads ADD COLUMN email text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE leads DROP COLUMN email;
