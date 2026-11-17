package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/media"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
	"photography-workflow/server/internal/storage"
)

const (
	MaxBatchFiles   = 500
	DefaultPartSize = 16 << 20
	MinPartSize     = 5 << 20
	MaxPartSize     = 5 << 30
	MaxParts        = 10000
	// Keep browser-compressed 50 MB uploads on the simpler single-PUT path.
	singlePutLimit  = 64 << 20
	presignTTL      = time.Hour
)

var (
	ErrNotFound          = errors.New("ingest resource not found")
	ErrProjectNotFound   = errors.New("project not found")
	ErrProjectClosed     = errors.New("project is cancelled or archived")
	ErrBatchClosed       = errors.New("ingest batch is not open")
	ErrInvalidFile       = errors.New("invalid file entry")
	ErrSizeMismatch      = errors.New("file size mismatch")
	ErrUploadNotStarted  = errors.New("upload was not started")
	ErrUploadNotReceived = errors.New("uploaded object not found")
	ErrAssetImmutable    = errors.New("asset content is already stored")

	ErrDerivativeKind     = errors.New("unsupported derivative kind")
	ErrDerivativeTooLarge = errors.New("derivative exceeds the size limit")
	ErrDerivativeMissing  = errors.New("uploaded derivative not found")
	ErrDerivativeHash     = errors.New("derivative content hash mismatch")
)

// MaxGalleryPreviewBytes bounds an agent-uploaded gallery preview. A 2560px
// JPEG lands far below this; the cap protects the server-side hash check.
const MaxGalleryPreviewBytes = 16 << 20

const derivativePresignTTL = 15 * time.Minute

type Batch struct {
	ID              uuid.UUID  `json:"id"`
	ProjectID       uuid.UUID  `json:"project_id"`
	DeviceID        string     `json:"device_id"`
	CardFingerprint string     `json:"card_fingerprint"`
	FileCount       int        `json:"file_count"`
	Bytes           int64      `json:"bytes"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

type CreateBatchInput struct {
	ProjectID       uuid.UUID
	DeviceID        string
	CardFingerprint string
}

type FileInput struct {
	RelPath    string          `json:"rel_path"`
	Size       int64           `json:"size"`
	XXH64      string          `json:"xxh64"`
	SHA256     string          `json:"sha256"`
	Mime       string          `json:"mime"`
	Width      int             `json:"width"`
	Height     int             `json:"height"`
	CapturedAt *time.Time      `json:"captured_at"`
	Camera     string          `json:"camera"`
	Lens       string          `json:"lens"`
	Exif       json.RawMessage `json:"exif"`
	IsVideo    bool            `json:"is_video"`
}

type AssetResult struct {
	RelPath    string    `json:"rel_path"`
	AssetID    uuid.UUID `json:"asset_id"`
	Duplicated bool      `json:"duplicated"`
	// Status is the asset's current ingest state. On a duplicated result the
	// agent uses it to skip uploads for content that is already stored.
	Status string `json:"status"`
}

// AssetCounts summarizes one ingest batch. Total counts every asset currently
// linked to the batch; the per-status fields use exact match, so assets that
// later advanced past verification are only reflected in Total.
type AssetCounts struct {
	Total      int `json:"total"`
	Registered int `json:"registered"`
	Uploaded   int `json:"uploaded"`
	Verified   int `json:"verified"`
	Missing    int `json:"missing"`
	Corrupt    int `json:"corrupt"`
}

// BatchStatus is the reconciliation view an agent polls after a restart: it
// combines the batch record, per-status asset counts and the verify job.
type BatchStatus struct {
	Batch     Batch       `json:"batch"`
	Assets    AssetCounts `json:"assets"`
	VerifyJob *jobs.Job   `json:"verify_job,omitempty"`
}

type RegisterResult struct {
	Accepted   int           `json:"accepted"`
	Inserted   int           `json:"inserted"`
	Duplicated int           `json:"duplicated"`
	Assets     []AssetResult `json:"assets"`
}

type Asset struct {
	ID          uuid.UUID `json:"id"`
	ProjectID   uuid.UUID `json:"project_id"`
	Filename    string    `json:"filename"`
	ContentHash string    `json:"content_hash"`
	OriginalKey string    `json:"original_key"`
	SizeBytes   int64     `json:"size_bytes"`
	Mime        string    `json:"mime"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Status      string    `json:"status"`
	IsVideo     bool      `json:"is_video"`
	CreatedAt   time.Time `json:"created_at"`
	// ThumbURL is a short-lived presigned URL of the grid thumbnail; empty when
	// derivations have not run yet. LQIP/Color let pickers show a placeholder.
	ThumbURL string `json:"thumb_url,omitempty"`
	LQIP     string `json:"lqip,omitempty"`
	Color    string `json:"color,omitempty"`
}

type PresignInput struct {
	AssetID  uuid.UUID
	Size     int64
	PartSize int64
}

type UploadPlan struct {
	Mode      string                  `json:"mode"`
	AssetID   uuid.UUID               `json:"asset_id"`
	ObjectKey string                  `json:"object_key"`
	URL       string                  `json:"url,omitempty"`
	UploadID  string                  `json:"upload_id,omitempty"`
	PartSize  int64                   `json:"part_size,omitempty"`
	Parts     []storage.PresignedPart `json:"parts,omitempty"`
	ExpiresAt time.Time               `json:"expires_at"`
}

type CompleteInput struct {
	AssetID  uuid.UUID
	UploadID string
	Parts    []storage.Part
}

type Service struct {
	pool  *pgxpool.Pool
	store storage.Storage
	log   *slog.Logger
	cfg   Config
}

// Config carries the gallery defaults the ingest endpoints need: how long
// presigned thumbnail URLs live and which watermark text derivation jobs use.
type Config struct {
	PresignTTL    time.Duration
	WatermarkText string
}

func NewService(pool *pgxpool.Pool, store storage.Storage, log *slog.Logger, cfg Config) *Service {
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = 10 * time.Minute
	}
	return &Service{pool: pool, store: store, log: log, cfg: cfg}
}

func (s *Service) ensureProjectOperational(ctx context.Context, tenantID, projectID uuid.UUID) error {
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return projectstate.EnsureOperational(ctx, tx, tenantID, projectID)
	})
	if errors.Is(err, projectstate.ErrClosed) {
		return ErrProjectClosed
	}
	if errors.Is(err, projectstate.ErrNotFound) {
		return ErrProjectNotFound
	}
	return err
}

const batchColumns = `id, project_id, device_id, card_fingerprint, file_count, bytes, status, created_at, finished_at`

func (s *Service) CreateBatch(ctx context.Context, tenantID uuid.UUID, in CreateBatchInput) (*Batch, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Batch, error) {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, in.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, ErrProjectNotFound
			}
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, ErrProjectClosed
			}
			return nil, fmt.Errorf("check project: %w", err)
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO ingest_batches (tenant_id, project_id, device_id, card_fingerprint)
			VALUES ($1,$2,$3,$4)
			RETURNING `+batchColumns,
			tenantID, in.ProjectID, strings.TrimSpace(in.DeviceID), strings.TrimSpace(in.CardFingerprint))
		return scanBatch(row)
	})
}

const upsertAssetSQL = `
INSERT INTO assets (tenant_id, project_id, ingest_batch_id, content_hash, xxh64, filename,
	original_key, size_bytes, mime, width, height, captured_at, camera, lens, exif, is_video, status)
	VALUES ($1,$2,$3,$4,$5,$6,'',$7,$8,$9,$10,$11,$12,$13,$14,$15,'registered')
ON CONFLICT (tenant_id, project_id, content_hash) DO UPDATE
SET ingest_batch_id = CASE
		WHEN assets.status IN ('registered','missing','corrupt') THEN EXCLUDED.ingest_batch_id
		ELSE assets.ingest_batch_id
	END,
	xxh64 = CASE WHEN EXCLUDED.xxh64 <> '' THEN EXCLUDED.xxh64 ELSE assets.xxh64 END,
	size_bytes = CASE
		WHEN assets.status IN ('registered','missing','corrupt') THEN EXCLUDED.size_bytes
		ELSE assets.size_bytes
	END,
	mime = CASE WHEN EXCLUDED.mime <> '' THEN EXCLUDED.mime ELSE assets.mime END,
	width = CASE WHEN EXCLUDED.width > 0 THEN EXCLUDED.width ELSE assets.width END,
	height = CASE WHEN EXCLUDED.height > 0 THEN EXCLUDED.height ELSE assets.height END,
	captured_at = COALESCE(EXCLUDED.captured_at, assets.captured_at),
	camera = CASE WHEN EXCLUDED.camera <> '' THEN EXCLUDED.camera ELSE assets.camera END,
	lens = CASE WHEN EXCLUDED.lens <> '' THEN EXCLUDED.lens ELSE assets.lens END,
	exif = CASE WHEN EXCLUDED.exif <> '{}'::jsonb THEN EXCLUDED.exif ELSE assets.exif END,
	updated_at = now()
RETURNING id, status, (xmax = 0) AS inserted`

func (s *Service) RegisterFiles(ctx context.Context, tenantID, batchID uuid.UUID, files []FileInput) (*RegisterResult, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: no files", ErrInvalidFile)
	}
	if len(files) > MaxBatchFiles {
		return nil, fmt.Errorf("%w: at most %d files per request", ErrInvalidFile, MaxBatchFiles)
	}

	prepared := make([]FileInput, len(files))
	for i := range files {
		f, err := normalizeFile(files[i])
		if err != nil {
			return nil, err
		}
		prepared[i] = f
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*RegisterResult, error) {
		var projectID uuid.UUID
		var status string
		err := tx.QueryRow(ctx,
			`SELECT project_id, status FROM ingest_batches WHERE id = $1 AND tenant_id = $2`,
			batchID, tenantID).Scan(&projectID, &status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("load batch: %w", err)
		}
		if status != "open" {
			return nil, ErrBatchClosed
		}
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, projectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, ErrProjectClosed
			}
			return nil, fmt.Errorf("check batch project: %w", err)
		}

		batch := &pgx.Batch{}
		for i := range prepared {
			f := prepared[i]
			exif := "{}"
			if len(f.Exif) > 0 {
				exif = string(f.Exif)
			}
			batch.Queue(upsertAssetSQL,
				tenantID, projectID, batchID, f.SHA256, strings.ToLower(f.XXH64),
				path.Base(f.RelPath), f.Size, f.Mime, f.Width, f.Height,
				f.CapturedAt, f.Camera, f.Lens, exif, f.IsVideo)
		}

		br := tx.SendBatch(ctx, batch)
		out := &RegisterResult{Accepted: len(prepared), Assets: make([]AssetResult, 0, len(prepared))}
		for i := range prepared {
			var (
				id       uuid.UUID
				status   string
				inserted bool
			)
			if err := br.QueryRow().Scan(&id, &status, &inserted); err != nil {
				_ = br.Close()
				return nil, fmt.Errorf("upsert asset: %w", err)
			}
			if inserted {
				out.Inserted++
			} else {
				out.Duplicated++
			}
			out.Assets = append(out.Assets, AssetResult{
				RelPath: prepared[i].RelPath, AssetID: id, Duplicated: !inserted, Status: status,
			})
		}
		if err := br.Close(); err != nil {
			return nil, fmt.Errorf("close batch: %w", err)
		}
		return out, nil
	})
}

func (s *Service) GetJob(ctx context.Context, tenantID, jobID uuid.UUID) (*jobs.Job, error) {
	job, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*jobs.Job, error) {
		return jobs.Get(ctx, tx, tenantID, jobID)
	})
	if errors.Is(err, jobs.ErrNotFound) {
		return nil, ErrNotFound
	}
	return job, err
}

func (s *Service) ListJobs(ctx context.Context, tenantID uuid.UUID, status string, limit int) ([]jobs.Job, error) {
	out, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) ([]jobs.Job, error) {
		return jobs.List(ctx, tx, tenantID, status, limit)
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []jobs.Job{}
	}
	return out, nil
}

// GetBatchStatus is the agent's reconciliation endpoint: after a crash it
// re-reads the batch, asset counts and verify job instead of trusting local
// upload state.
func (s *Service) GetBatchStatus(ctx context.Context, tenantID, batchID uuid.UUID) (*BatchStatus, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*BatchStatus, error) {
		var (
			b           Batch
			verifyJobID *uuid.UUID
		)
		err := tx.QueryRow(ctx, `
			SELECT `+batchColumns+`, verify_job_id FROM ingest_batches
			WHERE id = $1 AND tenant_id = $2`, batchID, tenantID).
			Scan(&b.ID, &b.ProjectID, &b.DeviceID, &b.CardFingerprint,
				&b.FileCount, &b.Bytes, &b.Status, &b.CreatedAt, &b.FinishedAt, &verifyJobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("load batch: %w", err)
		}

		var counts AssetCounts
		if err := tx.QueryRow(ctx, `
			SELECT count(*),
				count(*) FILTER (WHERE status = 'registered'),
				count(*) FILTER (WHERE status = 'uploaded'),
				count(*) FILTER (WHERE status = 'verified'),
				count(*) FILTER (WHERE status = 'missing'),
				count(*) FILTER (WHERE status = 'corrupt')
			FROM assets WHERE tenant_id = $1 AND ingest_batch_id = $2`,
			tenantID, batchID).Scan(&counts.Total, &counts.Registered, &counts.Uploaded,
			&counts.Verified, &counts.Missing, &counts.Corrupt); err != nil {
			return nil, fmt.Errorf("count batch assets: %w", err)
		}

		out := &BatchStatus{Batch: b, Assets: counts}
		if verifyJobID != nil {
			job, err := jobs.Get(ctx, tx, tenantID, *verifyJobID)
			switch {
			case err == nil:
				out.VerifyJob = job
			case errors.Is(err, jobs.ErrNotFound):
				// The job row was pruned; the batch status still tells the truth.
			default:
				return nil, err
			}
		}
		return out, nil
	})
}

// FinalizeBatch seals the batch. It never promotes assets to verified by
// itself: an ingest.verify job re-reads every object, checks size and SHA-256,
// and only then are assets verified. Re-finalizing returns the same job.
func (s *Service) FinalizeBatch(ctx context.Context, tenantID, actor, batchID uuid.UUID, ip string) (*jobs.Job, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*jobs.Job, error) {
		var (
			projectID   uuid.UUID
			status      string
			verifyJobID *uuid.UUID
		)
		err := tx.QueryRow(ctx,
			`SELECT project_id, status, verify_job_id FROM ingest_batches WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			batchID, tenantID).Scan(&projectID, &status, &verifyJobID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock batch: %w", err)
		}
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, projectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, ErrProjectClosed
			}
			return nil, fmt.Errorf("check batch project state: %w", err)
		}
		if status == "failed" && verifyJobID == nil {
			return nil, ErrBatchClosed
		}
		if verifyJobID != nil {
			job, err := jobs.Get(ctx, tx, tenantID, *verifyJobID)
			if err != nil {
				return nil, err
			}
			if job.Status != "failed" {
				return job, nil
			}
			// A failed verification can be explicitly retried. The old failed
			// job remains in the audit trail; the batch gets a new active job.
			verifyJobID = nil
			if _, err := tx.Exec(ctx, `
				UPDATE ingest_batches SET verify_job_id = NULL, status = 'verifying', finished_at = NULL
				WHERE id = $1 AND tenant_id = $2`, batchID, tenantID); err != nil {
				return nil, fmt.Errorf("reset failed verification: %w", err)
			}
		}

		var (
			fileCount int
			bytes     int64
		)
		if err := tx.QueryRow(ctx, `
			SELECT count(*), coalesce(sum(size_bytes),0) FROM assets
			WHERE tenant_id = $1 AND ingest_batch_id = $2`,
			tenantID, batchID).Scan(&fileCount, &bytes); err != nil {
			return nil, fmt.Errorf("summarize batch: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE ingest_batches
			SET status = 'verifying', file_count = $3, bytes = $4, finished_at = NULL
			WHERE id = $1 AND tenant_id = $2`,
			batchID, tenantID, fileCount, bytes); err != nil {
			return nil, fmt.Errorf("complete batch: %w", err)
		}

		job, err := jobs.Enqueue(ctx, tx, tenantID, "ingest.verify", "ingest_batch", &batchID,
			map[string]any{"batch_id": batchID})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ingest_batches SET verify_job_id = $3 WHERE id = $1 AND tenant_id = $2`,
			batchID, tenantID, job.ID); err != nil {
			return nil, fmt.Errorf("link verify job: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "ingest.batch.finalize", "ingest_batch", batchID.String(), ip,
			map[string]any{"file_count": fileCount, "bytes": bytes, "job_id": job.ID}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "ingest_batch", batchID.String(), "ingest.batch.finalized",
			map[string]any{"file_count": fileCount, "bytes": bytes, "job_id": job.ID}); err != nil {
			return nil, err
		}
		return job, nil
	})
}

func (s *Service) PresignUpload(ctx context.Context, tenantID uuid.UUID, in PresignInput) (*UploadPlan, error) {
	if in.Size <= 0 {
		return nil, fmt.Errorf("%w: size must be positive", ErrInvalidFile)
	}

	asset, err := s.getAsset(ctx, tenantID, in.AssetID)
	if err != nil {
		return nil, err
	}
	if err := s.ensureProjectOperational(ctx, tenantID, asset.ProjectID); err != nil {
		return nil, err
	}
	switch asset.Status {
	case "registered", "missing", "corrupt":
	default:
		return nil, fmt.Errorf("%w: asset status is %s", ErrAssetImmutable, asset.Status)
	}
	if asset.SizeBytes > 0 && asset.SizeBytes != in.Size {
		return nil, fmt.Errorf("%w: registered %d bytes, got %d", ErrSizeMismatch, asset.SizeBytes, in.Size)
	}
	sealed, err := s.batchSealed(ctx, tenantID, in.AssetID)
	if err != nil {
		return nil, err
	}
	if sealed {
		return nil, fmt.Errorf("%w: batch is sealed, open a new batch to re-upload", ErrBatchClosed)
	}
	key := asset.OriginalKey
	if key == "" {
		key = objectKey(tenantID, asset.ProjectID, asset.ContentHash, asset.Filename)
		if err := s.setAssetKey(ctx, tenantID, in.AssetID, key, in.Size); err != nil {
			return nil, err
		}
	}

	expires := time.Now().Add(presignTTL)
	if in.Size < singlePutLimit {
		url, err := s.store.PresignPut(ctx, key, presignTTL)
		if err != nil {
			return nil, err
		}
		return &UploadPlan{
			Mode:      "single",
			AssetID:   in.AssetID,
			ObjectKey: key,
			URL:       url,
			ExpiresAt: expires,
		}, nil
	}

	// Reuse an active multipart session so a retried presign does not upload the
	// same bytes twice or leak abandoned uploads.
	sess, err := s.activeUploadSession(ctx, tenantID, in.AssetID)
	if err != nil {
		return nil, err
	}
	if sess != nil && sess.ObjectKey == key &&
		(in.Size+sess.PartSize-1)/sess.PartSize <= MaxParts {
		plan, err := s.multipartPlan(ctx, in.AssetID, key, sess.UploadID, sess.PartSize, in.Size, expires)
		if err == nil {
			return plan, nil
		}
		s.discardSession(ctx, tenantID, sess.UploadID, sess.ObjectKey)
		return nil, err
	}
	if sess != nil {
		s.discardSession(ctx, tenantID, sess.UploadID, sess.ObjectKey)
	}

	partSize, err := choosePartSize(in.Size, in.PartSize)
	if err != nil {
		return nil, err
	}

	uploadID, err := s.store.CreateMultipart(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := s.createUploadSession(ctx, tenantID, in.AssetID, key, uploadID, partSize); err != nil {
		_ = s.store.AbortMultipart(ctx, key, uploadID)
		return nil, err
	}

	plan, err := s.multipartPlan(ctx, in.AssetID, key, uploadID, partSize, in.Size, expires)
	if err != nil {
		s.discardSession(ctx, tenantID, uploadID, key)
		return nil, err
	}
	return plan, nil
}

func (s *Service) multipartPlan(ctx context.Context, assetID uuid.UUID, key, uploadID string,
	partSize, size int64, expires time.Time) (*UploadPlan, error) {
	total := int32((size + partSize - 1) / partSize)
	parts := make([]storage.PresignedPart, 0, total)
	for n := int32(1); n <= total; n++ {
		url, err := s.store.PresignPart(ctx, key, uploadID, n, presignTTL)
		if err != nil {
			return nil, err
		}
		parts = append(parts, storage.PresignedPart{PartNumber: n, URL: url})
	}
	return &UploadPlan{
		Mode:      "multipart",
		AssetID:   assetID,
		ObjectKey: key,
		UploadID:  uploadID,
		PartSize:  partSize,
		Parts:     parts,
		ExpiresAt: expires,
	}, nil
}

func (s *Service) CompleteUpload(ctx context.Context, tenantID uuid.UUID, in CompleteInput) (*Asset, error) {
	asset, err := s.getAsset(ctx, tenantID, in.AssetID)
	if err != nil {
		return nil, err
	}
	if err := s.ensureProjectOperational(ctx, tenantID, asset.ProjectID); err != nil {
		return nil, err
	}
	if asset.OriginalKey == "" {
		return nil, ErrUploadNotStarted
	}
	if asset.Status != "uploaded" && asset.Status != "verified" {
		sealed, err := s.batchSealed(ctx, tenantID, in.AssetID)
		if err != nil {
			return nil, err
		}
		if sealed {
			return nil, fmt.Errorf("%w: batch is sealed, open a new batch to upload", ErrBatchClosed)
		}
	}

	if in.UploadID != "" {
		var sessionKey, sessionStatus string
		err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT object_key, status FROM upload_sessions
				WHERE upload_id = $1 AND asset_id = $2 AND tenant_id = $3`,
				in.UploadID, in.AssetID, tenantID).Scan(&sessionKey, &sessionStatus)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("load upload session: %w", err)
		}
		if sessionStatus == "active" {
			if err := s.store.CompleteMultipart(ctx, sessionKey, in.UploadID, in.Parts); err != nil {
				// A retry after a lost response finds the upload already completed.
				if _, statErr := s.store.Stat(ctx, sessionKey); statErr != nil {
					s.discardSession(ctx, tenantID, in.UploadID, sessionKey)
					return nil, err
				}
			}
			if err := s.completeSession(ctx, tenantID, in.UploadID); err != nil {
				return nil, fmt.Errorf("mark upload session: %w", err)
			}
		}
	}

	got, err := s.store.Stat(ctx, asset.OriginalKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrUploadNotReceived
		}
		return nil, err
	}
	if asset.SizeBytes > 0 && got != asset.SizeBytes {
		return nil, fmt.Errorf("%w: expected %d bytes, object has %d", ErrSizeMismatch, asset.SizeBytes, got)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Asset, error) {
		row := tx.QueryRow(ctx, `
			UPDATE assets SET status = 'uploaded', size_bytes = $3, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND status IN ('registered','missing','corrupt')
			RETURNING `+assetColumns,
			in.AssetID, tenantID, got)
		a, err := scanAsset(row)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("mark asset uploaded: %w", err)
		}
		// Already uploaded or beyond: return the current row so retries stay idempotent.
		row = tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets WHERE id = $1 AND tenant_id = $2`,
			in.AssetID, tenantID)
		return scanAsset(row)
	})
}

// DerivativePresignInput asks for an upload slot for a server-rendered source
// (today only the agent's gallery JPEG).
type DerivativePresignInput struct {
	AssetID uuid.UUID
	Kind    string
	Size    int64
	SHA256  string
}

type DerivativePresign struct {
	Key       string    `json:"key"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type DerivativeCompleteInput struct {
	AssetID uuid.UUID
	Kind    string
	Size    int64
	SHA256  string
	Width   int
	Height  int
}

type Derivative struct {
	AssetID uuid.UUID `json:"asset_id"`
	Kind    string    `json:"kind"`
	Key     string    `json:"key"`
	Width   int       `json:"width"`
	Height  int       `json:"height"`
	Bytes   int64     `json:"bytes"`
	Status  string    `json:"status"`
}

// galleryPreviewKey is content addressed by the uploaded preview hash, so the
// same JPEG always maps to one object and repeated uploads overwrite nothing.
func galleryPreviewKey(tenantID, projectID uuid.UUID, hash string) string {
	return "t/" + tenantID.String() + "/p/" + projectID.String() + "/" +
		hash[:2] + "/" + hash + "/gallery.jpg"
}

func validSHA256(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// PresignDerivative signs a single PUT for an agent-rendered gallery preview.
func (s *Service) PresignDerivative(ctx context.Context, tenantID uuid.UUID, in DerivativePresignInput) (*DerivativePresign, error) {
	if in.Kind != media.KindGallery {
		return nil, fmt.Errorf("%w: %q", ErrDerivativeKind, in.Kind)
	}
	if in.Size <= 0 || in.Size > MaxGalleryPreviewBytes {
		return nil, fmt.Errorf("%w: 1..%d bytes", ErrDerivativeTooLarge, MaxGalleryPreviewBytes)
	}
	if !validSHA256(in.SHA256) {
		return nil, fmt.Errorf("%w: sha256 must be 64 lowercase hex characters", ErrInvalidFile)
	}
	var projectID uuid.UUID
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		project, err := s.assetProject(ctx, tx, tenantID, in.AssetID)
		if err != nil {
			return err
		}
		projectID = project
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, projectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return ErrProjectClosed
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	key := galleryPreviewKey(tenantID, projectID, in.SHA256)
	url, err := s.store.PresignPut(ctx, key, derivativePresignTTL)
	if err != nil {
		return nil, fmt.Errorf("presign derivative: %w", err)
	}
	return &DerivativePresign{Key: key, URL: url, ExpiresAt: time.Now().Add(derivativePresignTTL)}, nil
}

// CompleteDerivative verifies the uploaded bytes and registers the preview.
// Only after the SHA-256 matches does it write asset_derivatives and queue the
// thumb/display rendering, so a truncated upload can never become a preview.
func (s *Service) CompleteDerivative(ctx context.Context, tenantID uuid.UUID, in DerivativeCompleteInput) (*Derivative, error) {
	if in.Kind != media.KindGallery {
		return nil, fmt.Errorf("%w: %q", ErrDerivativeKind, in.Kind)
	}
	if in.Size <= 0 || in.Size > MaxGalleryPreviewBytes {
		return nil, fmt.Errorf("%w: 1..%d bytes", ErrDerivativeTooLarge, MaxGalleryPreviewBytes)
	}
	if !validSHA256(in.SHA256) {
		return nil, fmt.Errorf("%w: sha256 must be 64 lowercase hex characters", ErrInvalidFile)
	}
	if in.Width <= 0 || in.Height <= 0 {
		return nil, fmt.Errorf("%w: width and height are required", ErrInvalidFile)
	}

	var projectID uuid.UUID
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		project, err := s.assetProject(ctx, tx, tenantID, in.AssetID)
		if err != nil {
			return err
		}
		projectID = project
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, projectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return ErrProjectClosed
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	key := galleryPreviewKey(tenantID, projectID, in.SHA256)
	got, err := s.store.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrDerivativeMissing
		}
		return nil, fmt.Errorf("stat derivative: %w", err)
	}
	if got != in.Size {
		return nil, fmt.Errorf("%w: expected %d bytes, object has %d", ErrSizeMismatch, in.Size, got)
	}
	if err := verifyObjectHash(ctx, s.store, key, in.SHA256); err != nil {
		return nil, err
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Derivative, error) {
		watermark, err := media.ResolveWatermark(ctx, tx, s.cfg.WatermarkText, tenantID)
		if err != nil {
			return nil, err
		}
		meta, _ := json.Marshal(map[string]any{
			"complete": true, "source": "agent", "kind": media.KindGallery,
		})
		if _, err := tx.Exec(ctx, `
			INSERT INTO asset_derivatives
				(tenant_id, asset_id, kind, is_watermarked, object_key, width, height, bytes, content_hash, meta)
			VALUES ($1,$2,$3,false,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (asset_id, kind, is_watermarked) DO UPDATE
			SET object_key = EXCLUDED.object_key, width = EXCLUDED.width, height = EXCLUDED.height,
				bytes = EXCLUDED.bytes, content_hash = EXCLUDED.content_hash, meta = EXCLUDED.meta`,
			tenantID, in.AssetID, media.KindGallery, key, in.Width, in.Height, in.Size, in.SHA256, meta); err != nil {
			return nil, fmt.Errorf("store gallery derivative: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE assets SET status = 'derivative_ready',
				width = CASE WHEN width = 0 THEN $3 ELSE width END,
				height = CASE WHEN height = 0 THEN $4 ELSE height END,
				updated_at = now()
			WHERE tenant_id = $1 AND id = $2 AND status IN ('verified','derivative_ready')`,
			tenantID, in.AssetID, in.Width, in.Height); err != nil {
			return nil, fmt.Errorf("promote asset: %w", err)
		}
		if _, err := media.EnqueueDerive(ctx, tx, tenantID, in.AssetID, watermark, false); err != nil {
			return nil, fmt.Errorf("queue derive: %w", err)
		}
		if err := outbox.Emit(ctx, tx, tenantID, "asset", in.AssetID.String(), "asset.gallery_uploaded",
			map[string]any{"project_id": projectID, "bytes": in.Size}); err != nil {
			return nil, err
		}
		return &Derivative{
			AssetID: in.AssetID, Kind: media.KindGallery, Key: key,
			Width: in.Width, Height: in.Height, Bytes: in.Size, Status: "derivative_ready",
		}, nil
	})
}

// assetProject resolves the project of a tenant-owned asset; a missing asset is
// reported with ErrNotFound so the agent marks the derivative failed instead of
// retrying forever.
func (s *Service) assetProject(ctx context.Context, tx pgx.Tx, tenantID, assetID uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT project_id FROM assets WHERE id = $1 AND tenant_id = $2`, assetID, tenantID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("load asset: %w", err)
	}
	return projectID, nil
}

func verifyObjectHash(ctx context.Context, store storage.Storage, key, want string) error {
	rc, err := store.Open(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrDerivativeMissing
		}
		return fmt.Errorf("open derivative: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(rc, MaxGalleryPreviewBytes+1)); err != nil {
		return fmt.Errorf("hash derivative: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("%w: expected %s, object has %s", ErrDerivativeHash, want, got)
	}
	return nil
}

func (s *Service) ListAssets(ctx context.Context, tenantID, projectID uuid.UUID, limit, offset int) ([]Asset, int, error) {
	var (
		items []Asset
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM assets WHERE tenant_id = $1 AND project_id = $2`,
			tenantID, projectID).Scan(&total); err != nil {
			return fmt.Errorf("count assets: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT `+assetColumns+` FROM assets
			WHERE tenant_id = $1 AND project_id = $2
			ORDER BY created_at DESC, filename, id
			LIMIT $3 OFFSET $4`, tenantID, projectID, limit, offset)
		if err != nil {
			return fmt.Errorf("list assets: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAsset(rows)
			if err != nil {
				return fmt.Errorf("scan asset: %w", err)
			}
			items = append(items, *a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Asset{}
	}
	if len(items) > 0 {
		s.attachPreviews(ctx, tenantID, items)
	}
	return items, total, nil
}

// attachPreviews fills thumb_url/lqip/color from ready thumb derivatives. A
// presign failure only drops the URL: the asset list must not fail because one
// preview cannot be signed.
func (s *Service) attachPreviews(ctx context.Context, tenantID uuid.UUID, items []Asset) {
	ids := make([]uuid.UUID, len(items))
	byID := make(map[uuid.UUID]*Asset, len(items))
	for i := range items {
		ids[i] = items[i].ID
		byID[items[i].ID] = &items[i]
	}
	type preview struct {
		assetID   uuid.UUID
		objectKey string
		lqip      string
		color     string
	}
	var previews []preview
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT asset_id, object_key, coalesce(meta->>'lqip',''), coalesce(meta->>'color','')
			FROM asset_derivatives
			WHERE tenant_id = $1 AND kind = 'thumb' AND is_watermarked = false
			  AND object_key <> '' AND meta->>'complete' = 'true'
			  AND asset_id = ANY($2)`, tenantID, ids)
		if err != nil {
			return fmt.Errorf("load thumbs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p preview
			if err := rows.Scan(&p.assetID, &p.objectKey, &p.lqip, &p.color); err != nil {
				return fmt.Errorf("scan thumb: %w", err)
			}
			previews = append(previews, p)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Warn("load asset previews", "err", err)
		return
	}
	for _, p := range previews {
		a, ok := byID[p.assetID]
		if !ok {
			continue
		}
		url, err := s.store.PresignGet(ctx, p.objectKey, s.cfg.PresignTTL)
		if err != nil {
			s.log.Warn("presign asset thumb", "asset_id", p.assetID, "err", err)
			continue
		}
		a.ThumbURL, a.LQIP, a.Color = url, p.lqip, p.color
	}
}

// MaxDeriveBatch caps one backfill request; larger projects call it page by
// page instead of holding a huge transaction.
const MaxDeriveBatch = 2000

type DeriveInput struct {
	AssetIDs []uuid.UUID
	Force    bool
}

// DeriveResult reports how many assets were queued for rendering. Candidates
// counts assets that needed work; the jobs themselves are deduplicated per
// asset, so repeated calls are harmless.
type DeriveResult struct {
	Candidates int    `json:"candidates"`
	Watermark  string `json:"watermark"`
	Force      bool   `json:"force"`
}

// EnqueueDerivations queues thumb/display rendering for a project. Explicit
// asset ids are intersected with the project; without ids every asset that is
// not missing/corrupt and lacks a complete thumb+display pair is selected.
func (s *Service) EnqueueDerivations(ctx context.Context, tenantID, projectID uuid.UUID, in DeriveInput) (*DeriveResult, error) {
	if len(in.AssetIDs) > MaxDeriveBatch {
		return nil, fmt.Errorf("%w: at most %d assets per call", ErrInvalidFile, MaxDeriveBatch)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*DeriveResult, error) {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, projectID); err != nil {
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, ErrProjectNotFound
			}
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, ErrProjectClosed
			}
			return nil, fmt.Errorf("check project: %w", err)
		}
		watermark, err := media.ResolveWatermark(ctx, tx, s.cfg.WatermarkText, tenantID)
		if err != nil {
			return nil, err
		}

		query := `
			SELECT a.id FROM assets a
			WHERE a.tenant_id = $1 AND a.project_id = $2
			  AND a.original_key <> ''
			  AND a.status NOT IN ('registered','missing','corrupt','quarantined')`
		args := []any{tenantID, projectID}
		if len(in.AssetIDs) > 0 {
			args = append(args, in.AssetIDs)
			query += ` AND a.id = ANY($3)`
		}
		if !in.Force {
			query += ` AND (SELECT count(*) FROM asset_derivatives d
				WHERE d.asset_id = a.id AND d.kind IN ('thumb','display')
				  AND d.object_key <> '' AND d.meta->>'complete' = 'true') < 2`
		}
		query += ` ORDER BY a.id LIMIT $` + strconv.Itoa(len(args)+1)
		args = append(args, MaxDeriveBatch)

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("list derivable assets: %w", err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan derivable asset: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("list derivable assets: %w", err)
		}

		for _, id := range ids {
			if _, err := media.EnqueueDerive(ctx, tx, tenantID, id, watermark, in.Force); err != nil {
				return nil, fmt.Errorf("enqueue derive for %s: %w", id, err)
			}
		}
		return &DeriveResult{Candidates: len(ids), Watermark: watermark, Force: in.Force}, nil
	})
}

// batchSealed reports whether the asset's current batch has been finalized. A
// sealed batch accepts no new uploads; re-uploading requires a new batch.
func (s *Service) batchSealed(ctx context.Context, tenantID, assetID uuid.UUID) (bool, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (bool, error) {
		var sealed bool
		err := tx.QueryRow(ctx, `
			SELECT b.status <> 'open'
			FROM assets a
			JOIN ingest_batches b ON b.id = a.ingest_batch_id AND b.tenant_id = a.tenant_id
			WHERE a.id = $1 AND a.tenant_id = $2`, assetID, tenantID).Scan(&sealed)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("load batch seal state: %w", err)
		}
		return sealed, nil
	})
}

func (s *Service) getAsset(ctx context.Context, tenantID, assetID uuid.UUID) (*Asset, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Asset, error) {
		row := tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets WHERE id = $1 AND tenant_id = $2`,
			assetID, tenantID)
		a, err := scanAsset(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("load asset: %w", err)
		}
		return a, nil
	})
}

func (s *Service) setAssetKey(ctx context.Context, tenantID, assetID uuid.UUID, key string, size int64) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE assets SET original_key = $3,
				size_bytes = CASE WHEN size_bytes = 0 THEN $4 ELSE size_bytes END,
				updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND original_key = ''`,
			assetID, tenantID, key, size)
		return err
	})
}

func (s *Service) createUploadSession(ctx context.Context, tenantID, assetID uuid.UUID, key, uploadID string, partSize int64) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO upload_sessions (tenant_id, asset_id, upload_id, object_key, part_size)
			VALUES ($1,$2,$3,$4,$5)`,
			tenantID, assetID, uploadID, key, partSize)
		if err != nil {
			return fmt.Errorf("create upload session: %w", err)
		}
		return nil
	})
}

type uploadSession struct {
	UploadID  string
	ObjectKey string
	PartSize  int64
}

func (s *Service) activeUploadSession(ctx context.Context, tenantID, assetID uuid.UUID) (*uploadSession, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*uploadSession, error) {
		var sess uploadSession
		err := tx.QueryRow(ctx, `
			SELECT upload_id, object_key, part_size FROM upload_sessions
			WHERE tenant_id = $1 AND asset_id = $2 AND status = 'active'
			ORDER BY created_at DESC
			LIMIT 1`, tenantID, assetID).
			Scan(&sess.UploadID, &sess.ObjectKey, &sess.PartSize)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil
			}
			return nil, fmt.Errorf("load upload session: %w", err)
		}
		return &sess, nil
	})
}

func (s *Service) completeSession(ctx context.Context, tenantID uuid.UUID, uploadID string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE upload_sessions SET status = 'completed', completed_at = now()
			WHERE upload_id = $1 AND tenant_id = $2`, uploadID, tenantID)
		return err
	})
}

// discardSession retires a broken or abandoned upload so the next presign starts
// a fresh multipart upload instead of handing out dead URLs.
func (s *Service) discardSession(ctx context.Context, tenantID uuid.UUID, uploadID, key string) {
	if err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE upload_sessions SET status = 'aborted'
			WHERE upload_id = $1 AND tenant_id = $2 AND status = 'active'`, uploadID, tenantID)
		return err
	}); err != nil {
		s.log.Warn("mark upload session aborted", "upload_id", uploadID, "err", err)
	}
	if err := s.store.AbortMultipart(ctx, key, uploadID); err != nil {
		s.log.Warn("abort multipart upload", "upload_id", uploadID, "err", err)
	}
}

const assetColumns = `id, project_id, filename, content_hash, original_key, size_bytes,
	mime, width, height, status, is_video, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAsset(row rowScanner) (*Asset, error) {
	var a Asset
	if err := row.Scan(&a.ID, &a.ProjectID, &a.Filename, &a.ContentHash, &a.OriginalKey,
		&a.SizeBytes, &a.Mime, &a.Width, &a.Height, &a.Status, &a.IsVideo, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func scanBatch(row rowScanner) (*Batch, error) {
	var b Batch
	if err := row.Scan(&b.ID, &b.ProjectID, &b.DeviceID, &b.CardFingerprint,
		&b.FileCount, &b.Bytes, &b.Status, &b.CreatedAt, &b.FinishedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

func normalizeFile(f FileInput) (FileInput, error) {
	rel := path.Clean(strings.ReplaceAll(strings.TrimSpace(f.RelPath), "\\", "/"))
	if rel == "." || rel == ".." || rel == "" || strings.HasPrefix(rel, "/") ||
		strings.HasPrefix(rel, "../") || strings.Contains(rel, ":") {
		return FileInput{}, fmt.Errorf("%w: unsafe rel_path %q", ErrInvalidFile, f.RelPath)
	}
	sha := strings.ToLower(strings.TrimSpace(f.SHA256))
	if len(sha) != 64 {
		return FileInput{}, fmt.Errorf("%w: %s: sha256 must be 64 hex chars", ErrInvalidFile, rel)
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return FileInput{}, fmt.Errorf("%w: %s: sha256 is not hex", ErrInvalidFile, rel)
	}
	if f.Size < 0 {
		return FileInput{}, fmt.Errorf("%w: %s: negative size", ErrInvalidFile, rel)
	}
	if len(f.Exif) > 0 && !json.Valid(f.Exif) {
		return FileInput{}, fmt.Errorf("%w: %s: exif is not valid json", ErrInvalidFile, rel)
	}
	if strings.TrimSpace(string(f.Exif)) == "null" {
		f.Exif = nil
	}
	f.RelPath = rel
	f.SHA256 = sha
	return f, nil
}

func choosePartSize(size, requested int64) (int64, error) {
	partSize := requested
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	if partSize < MinPartSize {
		partSize = MinPartSize
	}
	if partSize > MaxPartSize {
		partSize = MaxPartSize
	}
	if parts := (size + partSize - 1) / partSize; parts > MaxParts {
		partSize = (size + MaxParts - 1) / MaxParts
		partSize = ((partSize + MinPartSize - 1) / MinPartSize) * MinPartSize
	}
	if partSize > MaxPartSize {
		return 0, fmt.Errorf("%w: object too large for %d parts", ErrInvalidFile, MaxParts)
	}
	return partSize, nil
}

// objectKey is content addressed: identical bytes always map to the same key.
func objectKey(tenantID, projectID uuid.UUID, hash, filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	if len(ext) > 10 {
		ext = ""
	}
	return "t/" + tenantID.String() + "/p/" + projectID.String() + "/" +
		hash[:2] + "/" + hash + "/original" + ext
}
