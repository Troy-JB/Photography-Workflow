package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/storage"
)

// ErrNoSource means the asset has nothing the server can decode: a RAW file
// without the preview the Windows agent is expected to upload.
var ErrNoSource = errors.New("asset has no gallery preview source")

// JobKind is the queue entry that renders thumb/display for one asset.
const JobKind = "media.derive"

// maxSourceBytes bounds how much a single derivative job reads into memory.
const maxSourceBytes = 80 << 20

type Config struct {
	// WatermarkText is drawn on display previews; the API config defaults it to Lumeo.
	WatermarkText string
	// WatermarkFont is a TTF/OTF/TTC path; empty uses the Latin fallback.
	WatermarkFont string
}

// JobPayload rides in jobs.payload for media.derive jobs. The watermark is
// resolved where the tenant name is readable, so the worker never needs to
// touch the tenants table.
type JobPayload struct {
	AssetID   uuid.UUID `json:"asset_id"`
	Watermark string    `json:"watermark,omitempty"`
	Force     bool      `json:"force,omitempty"`
}

type Service struct {
	pool        *pgxpool.Pool
	store       storage.Storage
	wm          *Watermark
	defaultText string
	log         *slog.Logger
}

func NewService(pool *pgxpool.Pool, store storage.Storage, cfg Config, log *slog.Logger) (*Service, error) {
	wm, err := NewWatermark(cfg.WatermarkFont)
	if err != nil {
		return nil, err
	}
	return &Service{
		pool:        pool,
		store:       store,
		wm:          wm,
		defaultText: strings.TrimSpace(cfg.WatermarkText),
		log:         log,
	}, nil
}

// EnqueueDerive queues a derivation for one asset inside the caller's
// transaction. A queued/running job for the same asset is reused; force only
// affects the job that ends up running.
func EnqueueDerive(ctx context.Context, tx pgx.Tx, tenantID, assetID uuid.UUID, watermark string, force bool) (*jobs.Job, error) {
	return jobs.Enqueue(ctx, tx, tenantID, JobKind, "asset", &assetID,
		JobPayload{AssetID: assetID, Watermark: watermark, Force: force})
}

// ResolveWatermark picks the configured preview watermark, falling back to the
// tenant name for callers that do not use the API's Lumeo default.
func ResolveWatermark(ctx context.Context, tx pgx.Tx, configured string, tenantID uuid.UUID) (string, error) {
	if text := strings.TrimSpace(configured); text != "" {
		return text, nil
	}
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM tenants WHERE id = $1`, tenantID).Scan(&name); err != nil {
		return "", fmt.Errorf("load tenant name: %w", err)
	}
	return name, nil
}

// Result reports what a derivation produced. Skipped means every derivative
// already existed and nothing was rewritten.
type Result struct {
	AssetID       uuid.UUID `json:"asset_id"`
	Skipped       bool      `json:"skipped"`
	Source        string    `json:"source"`
	ThumbKey      string    `json:"thumb_key,omitempty"`
	DisplayKey    string    `json:"display_key,omitempty"`
	DisplayWidth  int       `json:"display_width,omitempty"`
	DisplayHeight int       `json:"display_height,omitempty"`
}

// Derive renders thumb/display/LQIP for one asset. It runs in the worker with
// the worker role, so it writes through a permissive policy rather than a
// tenant setting.
func (s *Service) Derive(ctx context.Context, tenantID, assetID uuid.UUID, watermark string, force bool) (*Result, error) {
	var (
		projectID   uuid.UUID
		contentHash string
		originalKey string
		mime        string
		filename    string
		status      string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT project_id, content_hash, original_key, mime, filename, status
		FROM assets WHERE tenant_id = $1 AND id = $2`, tenantID, assetID).
		Scan(&projectID, &contentHash, &originalKey, &mime, &filename, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("asset %s no longer exists", assetID)
		}
		return nil, fmt.Errorf("load asset: %w", err)
	}

	if !force {
		var complete int
		if err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM asset_derivatives
			WHERE tenant_id = $1 AND asset_id = $2 AND kind IN ('thumb','display')
			  AND object_key <> '' AND meta->>'complete' = 'true'`,
			tenantID, assetID).Scan(&complete); err != nil {
			return nil, fmt.Errorf("check derivatives: %w", err)
		}
		if complete >= 2 {
			return &Result{AssetID: assetID, Skipped: true}, nil
		}
	}

	sourceKey, source, err := s.pickSource(ctx, tenantID, assetID, originalKey, mime, filename)
	if err != nil {
		s.recordFailure(ctx, tenantID, assetID, err)
		return nil, err
	}
	raw, err := s.readSource(ctx, sourceKey)
	if err != nil {
		s.recordFailure(ctx, tenantID, assetID, err)
		return nil, err
	}

	text := strings.TrimSpace(watermark)
	if text == "" {
		text = s.defaultText
	}
	rendered, err := s.Render(raw, text)
	if err != nil {
		s.recordFailure(ctx, tenantID, assetID, err)
		return nil, err
	}

	thumbKey := DerivativeObjectKey(tenantID, projectID, contentHash, KindThumb)
	displayKey := DerivativeObjectKey(tenantID, projectID, contentHash, KindDisplay)
	if err := s.store.Put(ctx, thumbKey, bytes.NewReader(rendered.Thumb), int64(len(rendered.Thumb)), "image/jpeg"); err != nil {
		s.recordFailure(ctx, tenantID, assetID, err)
		return nil, fmt.Errorf("store thumb: %w", err)
	}
	if err := s.store.Put(ctx, displayKey, bytes.NewReader(rendered.Display), int64(len(rendered.Display)), "image/jpeg"); err != nil {
		s.recordFailure(ctx, tenantID, assetID, err)
		return nil, fmt.Errorf("store display: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin derivative tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	thumbMeta, _ := json.Marshal(map[string]any{
		"complete": true, "source": source, "lqip": rendered.LQIP, "color": rendered.Color,
	})
	displayMeta, _ := json.Marshal(map[string]any{
		"complete": true, "source": source, "watermark": text,
	})
	if err := upsertDerivative(ctx, tx, tenantID, assetID, KindThumb, false,
		thumbKey, rendered.ThumbWidth, rendered.ThumbHeight, rendered.Thumb, thumbMeta); err != nil {
		return nil, err
	}
	if err := upsertDerivative(ctx, tx, tenantID, assetID, KindDisplay, true,
		displayKey, rendered.DisplayWidth, rendered.DisplayHeight, rendered.Display, displayMeta); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE assets SET status = 'derivative_ready',
			width = CASE WHEN width = 0 THEN $3 ELSE width END,
			height = CASE WHEN height = 0 THEN $4 ELSE height END,
			updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status IN ('verified','derivative_ready')`,
		tenantID, assetID, rendered.SourceWidth, rendered.SourceHeight); err != nil {
		return nil, fmt.Errorf("promote asset: %w", err)
	}
	if err := outbox.Emit(ctx, tx, tenantID, "asset", assetID.String(), "asset.derivative_ready",
		map[string]any{"project_id": projectID, "thumb_key": thumbKey, "display_key": displayKey}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit derivative: %w", err)
	}
	return &Result{
		AssetID: assetID, Source: source,
		ThumbKey: thumbKey, DisplayKey: displayKey,
		DisplayWidth: rendered.DisplayWidth, DisplayHeight: rendered.DisplayHeight,
	}, nil
}

func (s *Service) pickSource(ctx context.Context, tenantID, assetID uuid.UUID, originalKey, mime, filename string) (string, string, error) {
	var galleryKey string
	err := s.pool.QueryRow(ctx, `
		SELECT object_key FROM asset_derivatives
		WHERE tenant_id = $1 AND asset_id = $2 AND kind = 'gallery' AND object_key <> ''
		ORDER BY is_watermarked
		LIMIT 1`, tenantID, assetID).Scan(&galleryKey)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("load gallery preview: %w", err)
	}
	if galleryKey != "" {
		return galleryKey, "gallery", nil
	}
	if originalKey != "" && (derivableMime(mime) || derivableExtension(filename)) {
		return originalKey, "original", nil
	}
	return "", "", fmt.Errorf("%w: %s", ErrNoSource, filename)
}

func (s *Service) readSource(ctx context.Context, key string) ([]byte, error) {
	data, err := s.readObject(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("%w: preview object %s is missing", ErrNoSource, key)
		}
		return nil, err
	}
	return data, nil
}

func (s *Service) readObject(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.store.Open(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("%w: object %s is missing", storage.ErrNotFound, key)
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	defer rc.Close()
	limited := io.LimitReader(rc, maxSourceBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}
	if len(data) > maxSourceBytes {
		return nil, fmt.Errorf("object exceeds %d bytes", maxSourceBytes)
	}
	return data, nil
}

// recordFailure leaves an inspectable marker without clobbering a derivative
// that already succeeded (a forced re-render may fail while the old one is
// still serviceable).
func (s *Service) recordFailure(ctx context.Context, tenantID, assetID uuid.UUID, cause error) {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	meta, _ := json.Marshal(map[string]any{"complete": false, "error": msg})
	_, err := s.pool.Exec(ctx, `
		INSERT INTO asset_derivatives (tenant_id, asset_id, kind, is_watermarked, object_key, meta)
		VALUES ($1, $2, 'thumb', false, '', $3)
		ON CONFLICT (asset_id, kind, is_watermarked) DO UPDATE
		SET meta = EXCLUDED.meta
		WHERE asset_derivatives.meta->>'complete' IS DISTINCT FROM 'true'`,
		tenantID, assetID, meta)
	if err != nil {
		s.log.Error("record derivative failure", "asset_id", assetID, "err", err)
	}
}

func upsertDerivative(ctx context.Context, tx pgx.Tx, tenantID, assetID uuid.UUID, kind string,
	watermarked bool, key string, width, height int, body, meta []byte) error {
	sum := sha256.Sum256(body)
	_, err := tx.Exec(ctx, `
		INSERT INTO asset_derivatives
			(tenant_id, asset_id, kind, is_watermarked, object_key, width, height, bytes, content_hash, meta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (asset_id, kind, is_watermarked) DO UPDATE
		SET object_key = EXCLUDED.object_key, width = EXCLUDED.width, height = EXCLUDED.height,
			bytes = EXCLUDED.bytes, content_hash = EXCLUDED.content_hash, meta = EXCLUDED.meta`,
		tenantID, assetID, kind, watermarked, key, width, height, len(body), hex.EncodeToString(sum[:]), meta)
	if err != nil {
		return fmt.Errorf("store %s derivative: %w", kind, err)
	}
	return nil
}
