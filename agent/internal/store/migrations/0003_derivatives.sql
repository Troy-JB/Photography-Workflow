-- +goose Up
-- Agent-side state for uploadable derivatives. Today only the gallery JPEG
-- exists, but kind is kept explicit so future previews reuse the same resume
-- logic. A row is keyed by (file_id, kind) and survives restarts.
CREATE TABLE derivatives (
	file_id    TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL,
	path       TEXT NOT NULL DEFAULT '',
	sha256     TEXT NOT NULL DEFAULT '',
	size       INTEGER NOT NULL DEFAULT 0,
	width      INTEGER NOT NULL DEFAULT 0,
	height     INTEGER NOT NULL DEFAULT 0,
	state      TEXT NOT NULL DEFAULT 'pending'
		CHECK (state IN ('pending', 'generating', 'uploading', 'uploaded', 'failed')),
	error      TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	PRIMARY KEY (file_id, kind)
);

CREATE INDEX derivatives_state_idx ON derivatives (state);

-- +goose Down
DROP INDEX derivatives_state_idx;
DROP TABLE derivatives;
