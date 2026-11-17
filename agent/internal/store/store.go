// Package store persists the agent's import state in a local SQLite database.
// All timestamps are fixed-width UTC text so ordering is stable, and every
// state change is a compare-and-swap against the expected previous states.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"photography-workflow/agent/internal/model"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var ErrNotFound = errors.New("store: not found")

// timeLayout keeps a fixed nine-digit fraction so text ordering matches time
// ordering (RFC3339Nano trims zeros and breaks lexicographic comparisons).
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}

func formatTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func parseTimePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

const fileColumns = `id, import_id, rel_path, size, mtime_ns, kind, role, pair_key,
	sha256, mime, width, height, captured_at, camera, lens, exif_json, state,
	server_asset_id, error, remote_batch_id`

const importColumns = `id, project_id, project_code, server_batch_id, card_id,
	source_root, shoot_date, dest_a, dest_b, require_backup, status, pause_reason,
	created_at, completed_at`

// Store owns the single write connection to the local database. Callers may
// read concurrently through the same pool, but writes are serialized by
// MaxOpenConns(1) so SQLite never sees a busy writer in normal operation.
type Store struct {
	db   *sql.DB
	path string
}

func dsn(path string) string {
	p := path
	if path != ":memory:" {
		if abs, err := filepath.Abs(path); err == nil {
			p = filepath.ToSlash(abs)
		}
		p = "file:" + p
	} else {
		p = "file::memory:"
	}
	return p + "?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"
}

// Open creates (if needed) and migrates the database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.Up(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Path reports the on-disk location, which is useful for logs and support.
func (s *Store) Path() string { return s.path }

// Up applies all pending migrations.
func (s *Store) Up(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, s.db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// DownTo rolls migrations back to target (0 removes everything). It exists for
// tests and operator recovery; the agent itself never calls it.
func (s *Store) DownTo(ctx context.Context, target int64) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.DownToContext(ctx, s.db, "migrations", target); err != nil {
		return fmt.Errorf("roll back migrations: %w", err)
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// ---------- cards ----------

// UpsertCard records a card by fingerprint and refreshes its mutable fields.
// The original first_seen is preserved across re-insertion.
func (s *Store) UpsertCard(ctx context.Context, c model.Card) (*model.Card, error) {
	if c.ID == "" || c.Fingerprint == "" {
		return nil, errors.New("store: card id and fingerprint are required")
	}
	if c.FirstSeen.IsZero() {
		c.FirstSeen = time.Now()
	}
	if c.LastSeen.IsZero() {
		c.LastSeen = c.FirstSeen
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO cards (id, fingerprint, volume_guid, label, capacity, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET
			volume_guid = CASE WHEN excluded.volume_guid <> '' THEN excluded.volume_guid ELSE cards.volume_guid END,
			label = CASE WHEN excluded.label <> '' THEN excluded.label ELSE cards.label END,
			capacity = CASE WHEN excluded.capacity > 0 THEN excluded.capacity ELSE cards.capacity END,
			last_seen = excluded.last_seen
		RETURNING id, fingerprint, volume_guid, label, capacity, first_seen, last_seen`,
		c.ID, c.Fingerprint, c.VolumeGUID, c.Label, c.Capacity,
		formatTime(c.FirstSeen), formatTime(c.LastSeen))
	return scanCard(row)
}

func (s *Store) CardByFingerprint(ctx context.Context, fingerprint string) (*model.Card, error) {
	return scanCard(s.db.QueryRowContext(ctx, `
		SELECT id, fingerprint, volume_guid, label, capacity, first_seen, last_seen
		FROM cards WHERE fingerprint = ?`, fingerprint))
}

func (s *Store) Card(ctx context.Context, id string) (*model.Card, error) {
	return scanCard(s.db.QueryRowContext(ctx, `
		SELECT id, fingerprint, volume_guid, label, capacity, first_seen, last_seen
		FROM cards WHERE id = ?`, id))
}

type scanner interface {
	Scan(dest ...any) error
}

func scanCard(row scanner) (*model.Card, error) {
	var (
		c         model.Card
		firstSeen string
		lastSeen  string
	)
	if err := row.Scan(&c.ID, &c.Fingerprint, &c.VolumeGUID, &c.Label,
		&c.Capacity, &firstSeen, &lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan card: %w", err)
	}
	first, err := parseTime(firstSeen)
	if err != nil {
		return nil, err
	}
	c.FirstSeen = first
	last, err := parseTime(lastSeen)
	if err != nil {
		return nil, err
	}
	c.LastSeen = last
	return &c, nil
}

// ---------- imports ----------

func (s *Store) CreateImport(ctx context.Context, imp model.Import) error {
	if imp.Status == "" {
		imp.Status = model.ImportOpen
	}
	if imp.CreatedAt.IsZero() {
		imp.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO imports (id, project_id, project_code, server_batch_id, card_id,
			source_root, shoot_date, dest_a, dest_b, require_backup, status,
			pause_reason, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		imp.ID, imp.ProjectID, imp.ProjectCode, imp.ServerBatchID, imp.CardID,
		imp.SourceRoot, imp.ShootDate, imp.DestA, imp.DestB, boolInt(imp.RequireBackup),
		string(imp.Status), imp.PauseReason, formatTime(imp.CreatedAt),
		formatTimePtr(imp.CompletedAt))
	if err != nil {
		return fmt.Errorf("insert import: %w", err)
	}
	return nil
}

func (s *Store) Import(ctx context.Context, id string) (*model.Import, error) {
	return scanImport(s.db.QueryRowContext(ctx,
		`SELECT `+importColumns+` FROM imports WHERE id = ?`, id))
}

// ActiveImportForCard returns the most recent import for a card that has not
// reached a terminal state, or ErrNotFound.
func (s *Store) ActiveImportForCard(ctx context.Context, cardID string) (*model.Import, error) {
	return scanImport(s.db.QueryRowContext(ctx, `
		SELECT `+importColumns+` FROM imports
		WHERE card_id = ? AND status <> 'complete'
		ORDER BY created_at DESC LIMIT 1`, cardID))
}

func (s *Store) Imports(ctx context.Context, limit int) ([]model.Import, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+importColumns+` FROM imports ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list imports: %w", err)
	}
	defer rows.Close()
	var out []model.Import
	for rows.Next() {
		imp, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *imp)
	}
	return out, rows.Err()
}

// SetImportStatus moves an import from one of the from states to to. The
// completed_at stamp is set only when reaching complete.
func (s *Store) SetImportStatus(ctx context.Context, id string, from []model.ImportStatus, to model.ImportStatus) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("store: at least one previous status is required")
	}
	args := []any{string(to), string(to), formatTime(time.Now()), id}
	args = append(args, statusArgs(from)...)
	res, err := s.db.ExecContext(ctx, `
		UPDATE imports SET status = ?,
			completed_at = CASE WHEN ? = 'complete' THEN ? ELSE completed_at END
		WHERE id = ? AND status IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("update import status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update import status: %w", err)
	}
	return n > 0, nil
}

func (s *Store) SetPauseReason(ctx context.Context, id, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE imports SET pause_reason = ? WHERE id = ?`, reason, id)
	if err != nil {
		return fmt.Errorf("set pause reason: %w", err)
	}
	return nil
}

func scanImport(row scanner) (*model.Import, error) {
	var (
		imp         model.Import
		requireBak  int
		createdAt   string
		completedAt sql.NullString
	)
	if err := row.Scan(&imp.ID, &imp.ProjectID, &imp.ProjectCode, &imp.ServerBatchID,
		&imp.CardID, &imp.SourceRoot, &imp.ShootDate, &imp.DestA, &imp.DestB,
		&requireBak, &imp.Status, &imp.PauseReason, &createdAt, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan import: %w", err)
	}
	imp.RequireBackup = requireBak != 0
	var err error
	if imp.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if imp.CompletedAt, err = parseTimePtr(completedAt); err != nil {
		return nil, err
	}
	return &imp, nil
}

// ---------- files ----------

// InsertFiles adds newly scanned files. Rows that already exist (same import
// and relative path) are left untouched, so resuming a scan is idempotent.
func (s *Store) InsertFiles(ctx context.Context, files []model.File) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}
	added := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO files (id, import_id, rel_path, size, mtime_ns, kind, role,
				pair_key, sha256, mime, width, height, captured_at, camera, lens,
				exif_json, state, server_asset_id, error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (import_id, rel_path) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("prepare insert file: %w", err)
		}
		defer stmt.Close()
		for _, f := range files {
			if f.State == "" {
				f.State = model.StateDiscovered
			}
			if f.Kind == "" {
				f.Kind = model.KindOther
			}
			if f.Role == "" {
				f.Role = model.RoleOther
			}
			res, err := stmt.ExecContext(ctx, f.ID, f.ImportID, f.RelPath, f.Size,
				f.MtimeNS, string(f.Kind), string(f.Role), f.PairKey, f.SHA256,
				f.Mime, f.Width, f.Height, formatTimePtr(f.CapturedAt), f.Camera,
				f.Lens, f.ExifJSON, string(f.State), f.ServerAssetID, f.Error)
			if err != nil {
				return fmt.Errorf("insert file %q: %w", f.RelPath, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("insert file %q: %w", f.RelPath, err)
			}
			added += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// SetFileMetadata stores the EXIF digest and dimensions for a file. It is a
// separate update so a metadata failure never touches the copy state machine.
func (s *Store) SetFileMetadata(ctx context.Context, id string, md model.FileMetadata) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE files SET mime = ?, width = ?, height = ?, captured_at = ?,
			camera = ?, lens = ?, exif_json = ?
		WHERE id = ?`,
		md.MIME, md.Width, md.Height, formatTimePtr(md.CapturedAt),
		md.Camera, md.Lens, md.ExifJSON, id)
	if err != nil {
		return fmt.Errorf("update file metadata: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update file metadata: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Files(ctx context.Context, importID string) ([]model.File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE import_id = ? ORDER BY rel_path`, importID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	var out []model.File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *Store) File(ctx context.Context, id string) (*model.File, error) {
	return scanFile(s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE id = ?`, id))
}

// SetFileState compares the current state against from before writing to.
func (s *Store) SetFileState(ctx context.Context, id string, from []model.FileState, to model.FileState, errMsg string) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("store: at least one previous state is required")
	}
	args := []any{string(to), errMsg, id}
	for _, f := range from {
		args = append(args, string(f))
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE files SET state = ?, error = ?
		WHERE id = ? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("update file state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update file state: %w", err)
	}
	return n > 0, nil
}

// FinishLocalMove records the source hash and advances the file to a local or
// failure state in one compare-and-swap.
func (s *Store) FinishLocalMove(ctx context.Context, id string, from []model.FileState, to model.FileState, sha, errMsg string) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("store: at least one previous state is required")
	}
	args := []any{string(to), sha, errMsg, id}
	for _, f := range from {
		args = append(args, string(f))
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE files SET state = ?, sha256 = ?, error = ?
		WHERE id = ? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("finish file move: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finish file move: %w", err)
	}
	return n > 0, nil
}

// FileCounts summarizes an import for progress reporting. Uploaded counts
// files the server holds, whether or not verification has finished.
type FileCounts struct {
	Total          int `json:"total"`
	LocalVerified  int `json:"local_verified"`
	Uploaded       int `json:"uploaded"`
	RemoteVerified int `json:"remote_verified"`
	Failed         int `json:"failed"`
}

func (s *Store) FileCounts(ctx context.Context, importID string) (FileCounts, error) {
	var c FileCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*),
			coalesce(sum(CASE WHEN state IN ('local_verified','registered','uploading',
				'uploaded','remote_verified','complete') THEN 1 ELSE 0 END), 0),
			coalesce(sum(CASE WHEN state IN ('uploaded','remote_verified','complete') THEN 1 ELSE 0 END), 0),
			coalesce(sum(CASE WHEN state IN ('remote_verified','complete') THEN 1 ELSE 0 END), 0),
			coalesce(sum(CASE WHEN state IN ('copy_failed','hash_mismatch',
				'upload_failed','remote_corrupt') THEN 1 ELSE 0 END), 0)
		FROM files WHERE import_id = ?`, importID).
		Scan(&c.Total, &c.LocalVerified, &c.Uploaded, &c.RemoteVerified, &c.Failed)
	if err != nil {
		return c, fmt.Errorf("count files: %w", err)
	}
	return c, nil
}

func scanFile(row scanner) (*model.File, error) {
	var (
		f          model.File
		capturedAt sql.NullString
	)
	if err := row.Scan(&f.ID, &f.ImportID, &f.RelPath, &f.Size, &f.MtimeNS,
		&f.Kind, &f.Role, &f.PairKey, &f.SHA256, &f.Mime, &f.Width, &f.Height,
		&capturedAt, &f.Camera, &f.Lens, &f.ExifJSON, &f.State, &f.ServerAssetID,
		&f.Error, &f.RemoteBatchID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan file: %w", err)
	}
	var err error
	if f.CapturedAt, err = parseTimePtr(capturedAt); err != nil {
		return nil, err
	}
	return &f, nil
}

// ---------- copies ----------

// SaveCopy upserts a per-destination copy row. Each (file, dest) pair has a
// single writer, so a blind upsert is safe.
func (s *Store) SaveCopy(ctx context.Context, c model.Copy) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO copies (file_id, dest, final_path, temp_path, state, bytes, sha256, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id, dest) DO UPDATE SET
			final_path = excluded.final_path,
			temp_path = excluded.temp_path,
			state = excluded.state,
			bytes = excluded.bytes,
			sha256 = excluded.sha256,
			error = excluded.error`,
		c.FileID, c.Dest, c.FinalPath, c.TempPath, c.State, c.Bytes, c.SHA256, c.Error)
	if err != nil {
		return fmt.Errorf("save copy: %w", err)
	}
	return nil
}

func (s *Store) Copies(ctx context.Context, fileID string) ([]model.Copy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_id, dest, final_path, temp_path, state, bytes, sha256, error
		FROM copies WHERE file_id = ? ORDER BY dest`, fileID)
	if err != nil {
		return nil, fmt.Errorf("list copies: %w", err)
	}
	defer rows.Close()
	var out []model.Copy
	for rows.Next() {
		var c model.Copy
		if err := rows.Scan(&c.FileID, &c.Dest, &c.FinalPath, &c.TempPath,
			&c.State, &c.Bytes, &c.SHA256, &c.Error); err != nil {
			return nil, fmt.Errorf("scan copy: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- uploads ----------

// SetImportServerBatchID links an import to the ingest batch that carries its
// assets. The batch id is stable across retries because batch creation uses an
// idempotency key derived from the import id.
func (s *Store) SetImportServerBatchID(ctx context.Context, id, batchID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE imports SET server_batch_id = ? WHERE id = ?`, batchID, id)
	if err != nil {
		return fmt.Errorf("set server batch id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set server batch id: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetImportProject links an import to a server project. Uploads are refused
// until this is set; operators can attach the project after the copy started.
func (s *Store) SetImportProject(ctx context.Context, id, projectID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE imports SET project_id = ? WHERE id = ?`, strings.TrimSpace(projectID), id)
	if err != nil {
		return fmt.Errorf("set import project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set import project: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetFileAsset stores the server-side asset id returned by registration.
func (s *Store) SetFileAsset(ctx context.Context, id, assetID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE files SET server_asset_id = ? WHERE id = ?`, assetID, id)
	if err != nil {
		return fmt.Errorf("set file asset: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set file asset: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetFileRemoteBatch records which ingest batch registered the file, so a
// verify job result can be mapped back to exactly the assets it covered.
func (s *Store) SetFileRemoteBatch(ctx context.Context, id, batchID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE files SET remote_batch_id = ? WHERE id = ?`, batchID, id)
	if err != nil {
		return fmt.Errorf("set remote batch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set remote batch: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveUpload upserts the resumable upload plan for one file.
func (s *Store) SaveUpload(ctx context.Context, u model.Upload) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO uploads (file_id, asset_id, mode, object_key, upload_id,
			part_size, total_parts, epoch, expires_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id) DO UPDATE SET
			asset_id = excluded.asset_id,
			mode = excluded.mode,
			object_key = excluded.object_key,
			upload_id = excluded.upload_id,
			part_size = excluded.part_size,
			total_parts = excluded.total_parts,
			epoch = excluded.epoch,
			expires_at = excluded.expires_at,
			state = excluded.state`,
		u.FileID, u.AssetID, u.Mode, u.ObjectKey, u.UploadID, u.PartSize,
		u.TotalParts, u.Epoch, formatTimePtr(u.ExpiresAt), u.State)
	if err != nil {
		return fmt.Errorf("save upload: %w", err)
	}
	return nil
}

func (s *Store) Upload(ctx context.Context, fileID string) (*model.Upload, error) {
	var (
		u         model.Upload
		expiresAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT file_id, asset_id, mode, object_key, upload_id, part_size,
			total_parts, epoch, expires_at, state
		FROM uploads WHERE file_id = ?`, fileID).
		Scan(&u.FileID, &u.AssetID, &u.Mode, &u.ObjectKey, &u.UploadID,
			&u.PartSize, &u.TotalParts, &u.Epoch, &expiresAt, &u.State)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("load upload: %w", err)
	}
	if u.ExpiresAt, err = parseTimePtr(expiresAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// ResetUploadParts forgets every part record for a file. It is called when a
// new multipart session replaces the old one: stale ETags are meaningless then.
func (s *Store) ResetUploadParts(ctx context.Context, fileID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM upload_parts WHERE file_id = ?`, fileID); err != nil {
		return fmt.Errorf("reset upload parts: %w", err)
	}
	return nil
}

// ResetUpload drops the upload plan and every part record. A file that failed
// remote verification must be transferred again even though its previous plan
// claims completion.
func (s *Store) ResetUpload(ctx context.Context, fileID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM upload_parts WHERE file_id = ?`, fileID); err != nil {
			return fmt.Errorf("reset upload parts: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM uploads WHERE file_id = ?`, fileID); err != nil {
			return fmt.Errorf("reset upload: %w", err)
		}
		return nil
	})
}

// SaveUploadPart records the outcome of one part. Rows are keyed by part
// number, so a retried part overwrites its previous ETag.
func (s *Store) SaveUploadPart(ctx context.Context, p model.UploadPart) error {
	if p.State == "" {
		p.State = model.PartUploaded
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO upload_parts (file_id, part_number, offset, size, etag, sha256, state)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id, part_number) DO UPDATE SET
			offset = excluded.offset,
			size = excluded.size,
			etag = excluded.etag,
			sha256 = excluded.sha256,
			state = excluded.state`,
		p.FileID, p.Number, p.Offset, p.Size, p.ETag, p.SHA256, p.State)
	if err != nil {
		return fmt.Errorf("save upload part: %w", err)
	}
	return nil
}

func (s *Store) UploadParts(ctx context.Context, fileID string) ([]model.UploadPart, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_id, part_number, offset, size, etag, sha256, state
		FROM upload_parts WHERE file_id = ? ORDER BY part_number`, fileID)
	if err != nil {
		return nil, fmt.Errorf("list upload parts: %w", err)
	}
	defer rows.Close()
	var out []model.UploadPart
	for rows.Next() {
		var p model.UploadPart
		if err := rows.Scan(&p.FileID, &p.Number, &p.Offset, &p.Size,
			&p.ETag, &p.SHA256, &p.State); err != nil {
			return nil, fmt.Errorf("scan upload part: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- events ----------

func (s *Store) AddEvent(ctx context.Context, e model.Event) (int64, error) {
	if e.Level == "" {
		e.Level = "info"
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	// Informational events are history, not alerts: they never need an
	// acknowledgement and never show up as persistent warnings.
	ack := 0
	if e.Level == "info" {
		ack = 1
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO events (level, code, import_id, file_id, message, acknowledged, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Level, e.Code, e.ImportID, e.FileID, e.Message, ack, formatTime(e.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("insert event: %w", err)
	}
	return res.LastInsertId()
}

// Events returns events newer than sinceID (exclusive), oldest first.
func (s *Store) Events(ctx context.Context, sinceID int64, limit int) ([]model.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return s.queryEvents(ctx, `
		SELECT id, level, code, import_id, file_id, message, acknowledged, created_at
		FROM events WHERE id > ? ORDER BY id LIMIT ?`, sinceID, limit)
}

// UnacknowledgedEvents returns the persistent alerts the UI must keep showing.
func (s *Store) UnacknowledgedEvents(ctx context.Context, limit int) ([]model.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return s.queryEvents(ctx, `
		SELECT id, level, code, import_id, file_id, message, acknowledged, created_at
		FROM events WHERE acknowledged = 0 ORDER BY id LIMIT ?`, limit)
}

// RecentEvents returns the newest events first for the history screen.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]model.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return s.queryEvents(ctx, `
		SELECT id, level, code, import_id, file_id, message, acknowledged, created_at
		FROM events ORDER BY id DESC LIMIT ?`, limit)
}

func (s *Store) queryEvents(ctx context.Context, query string, args ...any) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var (
			e       model.Event
			ack     int
			created string
		)
		if err := rows.Scan(&e.ID, &e.Level, &e.Code, &e.ImportID, &e.FileID,
			&e.Message, &ack, &created); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Acknowledged = ack != 0
		if e.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) AckEvent(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET acknowledged = 1 WHERE id = ? AND acknowledged = 0`, id)
	if err != nil {
		return false, fmt.Errorf("acknowledge event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acknowledge event: %w", err)
	}
	return n > 0, nil
}

// ---------- crash recovery ----------

// ResetInterrupted undoes state left behind by a crash: files stuck in copying
// return to discovered, their partial copies return to pending, and expired
// task leases are released. It returns the temp paths that the caller must
// delete before recopying (rows are cleared here so a stale path is never
// retried twice).
func (s *Store) ResetInterrupted(ctx context.Context) ([]string, error) {
	var temps []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT temp_path FROM copies
			WHERE state = 'copying' AND temp_path <> ''`)
		if err != nil {
			return fmt.Errorf("load interrupted copies: %w", err)
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return fmt.Errorf("scan interrupted copy: %w", err)
			}
			temps = append(temps, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("load interrupted copies: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE copies SET state = 'pending', temp_path = ''
			WHERE state = 'copying'`); err != nil {
			return fmt.Errorf("reset interrupted copies: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE files SET state = 'discovered', error = ''
			WHERE state = 'copying'`); err != nil {
			return fmt.Errorf("reset interrupted files: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'pending', lease_until = NULL
			WHERE state = 'running' AND (lease_until IS NULL OR lease_until < ?)`,
			formatTime(time.Now())); err != nil {
			return fmt.Errorf("release task leases: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return temps, nil
}

// ---------- helpers ----------

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func statusArgs(from []model.ImportStatus) []any {
	out := make([]any, 0, len(from))
	for _, f := range from {
		out = append(out, string(f))
	}
	return out
}

// ---------- derivatives ----------

// SaveDerivative upserts one preview record. The row is the resume state: a
// restart regenerates only what is still pending/failed and reuses uploaded
// previews without touching the source file again.
func (s *Store) SaveDerivative(ctx context.Context, d model.Derivative) error {
	updated := d.UpdatedAt
	if updated == "" {
		updated = formatTime(time.Now())
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO derivatives (file_id, kind, path, sha256, size, width, height, state, error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id, kind) DO UPDATE SET
			path = excluded.path,
			sha256 = excluded.sha256,
			size = excluded.size,
			width = excluded.width,
			height = excluded.height,
			state = excluded.state,
			error = excluded.error,
			updated_at = excluded.updated_at`,
		d.FileID, d.Kind, d.Path, d.SHA256, d.Size, d.Width, d.Height,
		d.State, d.Error, updated)
	if err != nil {
		return fmt.Errorf("save derivative: %w", err)
	}
	return nil
}

func (s *Store) SetDerivativeState(ctx context.Context, fileID, kind string, from []string, to, errMsg string) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("store: at least one previous state is required")
	}
	args := []any{to, errMsg, formatTime(time.Now()), fileID, kind}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE derivatives SET state = ?, error = ?, updated_at = ?
		WHERE file_id = ? AND kind = ? AND state IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("update derivative state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update derivative state: %w", err)
	}
	return n > 0, nil
}

func (s *Store) Derivative(ctx context.Context, fileID, kind string) (*model.Derivative, error) {
	var d model.Derivative
	err := s.db.QueryRowContext(ctx, `
		SELECT file_id, kind, path, sha256, size, width, height, state, error, updated_at
		FROM derivatives WHERE file_id = ? AND kind = ?`, fileID, kind).
		Scan(&d.FileID, &d.Kind, &d.Path, &d.SHA256, &d.Size, &d.Width, &d.Height,
			&d.State, &d.Error, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("load derivative: %w", err)
	}
	return &d, nil
}

// Derivatives lists the preview records of every file in an import.
func (s *Store) Derivatives(ctx context.Context, importID string) ([]model.Derivative, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.file_id, d.kind, d.path, d.sha256, d.size, d.width, d.height,
			d.state, d.error, d.updated_at
		FROM derivatives d
		JOIN files f ON f.id = d.file_id
		WHERE f.import_id = ?
		ORDER BY d.file_id, d.kind`, importID)
	if err != nil {
		return nil, fmt.Errorf("list derivatives: %w", err)
	}
	defer rows.Close()
	var out []model.Derivative
	for rows.Next() {
		var d model.Derivative
		if err := rows.Scan(&d.FileID, &d.Kind, &d.Path, &d.SHA256, &d.Size,
			&d.Width, &d.Height, &d.State, &d.Error, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan derivative: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
