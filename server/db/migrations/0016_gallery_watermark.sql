-- +goose Up

-- 0015 predates the per-gallery watermark snapshot. The value is copied from
-- gallery.watermark_text into display derivatives at derive time, so changing
-- it requires a forced re-derive (the publish gate detects the mismatch).
ALTER TABLE galleries ADD COLUMN watermark_text text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE galleries DROP COLUMN watermark_text;
