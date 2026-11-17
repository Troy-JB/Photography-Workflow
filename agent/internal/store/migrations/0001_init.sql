-- +goose Up
-- All timestamps are fixed-width UTC text (see store.timeLayout) so that
-- lexicographic ordering matches chronological ordering.

CREATE TABLE cards (
	id              TEXT PRIMARY KEY,
	fingerprint     TEXT NOT NULL UNIQUE,
	volume_guid     TEXT NOT NULL DEFAULT '',
	label           TEXT NOT NULL DEFAULT '',
	capacity        INTEGER NOT NULL DEFAULT 0,
	first_seen      TEXT NOT NULL,
	last_seen       TEXT NOT NULL
);

CREATE TABLE imports (
	id              TEXT PRIMARY KEY,
	project_id      TEXT NOT NULL DEFAULT '',
	project_code    TEXT NOT NULL,
	server_batch_id TEXT NOT NULL DEFAULT '',
	card_id         TEXT NOT NULL REFERENCES cards(id),
	source_root     TEXT NOT NULL,
	shoot_date      TEXT NOT NULL,
	dest_a          TEXT NOT NULL,
	dest_b          TEXT NOT NULL DEFAULT '',
	require_backup  INTEGER NOT NULL DEFAULT 1 CHECK (require_backup IN (0, 1)),
	status          TEXT NOT NULL DEFAULT 'open'
		CHECK (status IN ('open', 'local_complete', 'uploading', 'complete', 'failed')),
	pause_reason    TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL,
	completed_at    TEXT
);

CREATE INDEX imports_card_idx ON imports (card_id, created_at DESC);
CREATE INDEX imports_status_idx ON imports (status, created_at);

CREATE TABLE files (
	id              TEXT PRIMARY KEY,
	import_id       TEXT NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
	rel_path        TEXT NOT NULL,
	size            INTEGER NOT NULL,
	mtime_ns        INTEGER NOT NULL,
	kind            TEXT NOT NULL,
	role            TEXT NOT NULL,
	pair_key        TEXT NOT NULL DEFAULT '',
	sha256          TEXT NOT NULL DEFAULT '',
	mime            TEXT NOT NULL DEFAULT '',
	width           INTEGER NOT NULL DEFAULT 0,
	height          INTEGER NOT NULL DEFAULT 0,
	captured_at     TEXT,
	camera          TEXT NOT NULL DEFAULT '',
	lens            TEXT NOT NULL DEFAULT '',
	exif_json       TEXT NOT NULL DEFAULT '',
	state           TEXT NOT NULL DEFAULT 'discovered'
		CHECK (state IN ('discovered', 'copying', 'copied', 'local_verified',
			'registered', 'uploading', 'uploaded', 'remote_verified', 'complete',
			'copy_failed', 'hash_mismatch', 'upload_failed', 'remote_corrupt')),
	server_asset_id TEXT NOT NULL DEFAULT '',
	error           TEXT NOT NULL DEFAULT '',
	UNIQUE (import_id, rel_path)
);

CREATE INDEX files_import_state_idx ON files (import_id, state);
CREATE INDEX files_sha_idx ON files (sha256) WHERE sha256 <> '';

CREATE TABLE copies (
	file_id     TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	dest        TEXT NOT NULL CHECK (dest IN ('A', 'B')),
	final_path  TEXT NOT NULL DEFAULT '',
	temp_path   TEXT NOT NULL DEFAULT '',
	state       TEXT NOT NULL DEFAULT 'pending'
		CHECK (state IN ('pending', 'copying', 'copied', 'verified', 'failed')),
	bytes       INTEGER NOT NULL DEFAULT 0,
	sha256      TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (file_id, dest)
);

CREATE TABLE uploads (
	file_id     TEXT PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
	asset_id    TEXT NOT NULL DEFAULT '',
	mode        TEXT NOT NULL DEFAULT '',
	object_key  TEXT NOT NULL DEFAULT '',
	upload_id   TEXT NOT NULL DEFAULT '',
	part_size   INTEGER NOT NULL DEFAULT 0,
	total_parts INTEGER NOT NULL DEFAULT 0,
	epoch       INTEGER NOT NULL DEFAULT 0,
	expires_at  TEXT,
	state       TEXT NOT NULL DEFAULT 'pending'
);

CREATE TABLE upload_parts (
	file_id     TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	part_number INTEGER NOT NULL,
	offset      INTEGER NOT NULL,
	size        INTEGER NOT NULL,
	etag        TEXT NOT NULL DEFAULT '',
	sha256      TEXT NOT NULL DEFAULT '',
	state       TEXT NOT NULL DEFAULT 'pending',
	PRIMARY KEY (file_id, part_number)
);

CREATE TABLE tasks (
	id          TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	subject_id  TEXT NOT NULL,
	state       TEXT NOT NULL DEFAULT 'pending'
		CHECK (state IN ('pending', 'running', 'done', 'failed')),
	attempts    INTEGER NOT NULL DEFAULT 0,
	next_run_at TEXT NOT NULL,
	lease_until TEXT,
	last_error  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX tasks_due_idx ON tasks (state, next_run_at);

CREATE TABLE idempotency (
	key          TEXT PRIMARY KEY,
	endpoint     TEXT NOT NULL,
	request_hash TEXT NOT NULL,
	status       INTEGER NOT NULL DEFAULT 0,
	response     TEXT NOT NULL DEFAULT '',
	expires_at   TEXT NOT NULL
);

CREATE TABLE events (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	level        TEXT NOT NULL,
	code         TEXT NOT NULL,
	import_id    TEXT NOT NULL DEFAULT '',
	file_id      TEXT NOT NULL DEFAULT '',
	message      TEXT NOT NULL,
	acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged IN (0, 1)),
	created_at   TEXT NOT NULL
);

CREATE INDEX events_unacked_idx ON events (acknowledged, id);

-- +goose Down
DROP TABLE events;
DROP TABLE idempotency;
DROP TABLE tasks;
DROP TABLE upload_parts;
DROP TABLE uploads;
DROP TABLE copies;
DROP TABLE files;
DROP TABLE imports;
DROP TABLE cards;
