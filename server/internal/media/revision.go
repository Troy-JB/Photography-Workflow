package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/outbox"
)

// RevisionJobKind renders previews for a retouched revision. It is separate
// from asset derivatives so a revision never overwrites the original gallery
// previews that other rounds may still point at.
const RevisionJobKind = "media.derive_revision"

type RevisionPayload struct {
	RevisionID uuid.UUID `json:"revision_id"`
	Watermark  string    `json:"watermark,omitempty"`
}

func EnqueueRevisionDerive(ctx context.Context, tx pgx.Tx, tenantID, revisionID uuid.UUID, watermark string) (*jobs.Job, error) {
	return jobs.Enqueue(ctx, tx, tenantID, RevisionJobKind, "asset_revision", &revisionID,
		RevisionPayload{RevisionID: revisionID, Watermark: watermark})
}

// RevisionObjectKey is content-addressed by revision number under the asset, so
// a failed derivation can be retried without colliding with another version.
func RevisionObjectKey(tenantID, assetID uuid.UUID, revisionNo int, name string) string {
	return fmt.Sprintf("t/%s/revisions/%s/v%d/%s.jpg", tenantID, assetID, revisionNo, name)
}

type RevisionResult struct {
	RevisionID uuid.UUID `json:"revision_id"`
	AssetID    uuid.UUID `json:"asset_id"`
	RevisionNo int       `json:"revision_no"`
	Skipped    bool      `json:"skipped"`
	ThumbKey   string    `json:"thumb_key,omitempty"`
	DisplayKey string    `json:"display_key,omitempty"`
}

// DeriveRevision renders thumb/display/LQIP for one retouched version. The
// watermark text is resolved where the gallery snapshot is readable and passed
// in the job payload, so the worker still never reads galleries or tenants.
func (s *Service) DeriveRevision(ctx context.Context, tenantID, revisionID uuid.UUID, watermark string) (*RevisionResult, error) {
	var (
		projectID  uuid.UUID
		assetID    uuid.UUID
		revisionNo int
		sourceKey  string
		status     string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT project_id, asset_id, revision_no, source_key, status
		FROM asset_revisions WHERE tenant_id = $1 AND id = $2`, tenantID, revisionID).
		Scan(&projectID, &assetID, &revisionNo, &sourceKey, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("revision %s no longer exists", revisionID)
		}
		return nil, fmt.Errorf("load revision: %w", err)
	}
	if status == "derivative_ready" {
		return &RevisionResult{RevisionID: revisionID, AssetID: assetID, RevisionNo: revisionNo, Skipped: true}, nil
	}

	raw, err := s.readObject(ctx, sourceKey)
	if err != nil {
		s.markRevisionFailed(ctx, tenantID, revisionID)
		return nil, err
	}
	text := watermark
	if text == "" {
		text = s.defaultText
	}
	rendered, err := s.Render(raw, text)
	if err != nil {
		s.markRevisionFailed(ctx, tenantID, revisionID)
		return nil, err
	}

	thumbKey := RevisionObjectKey(tenantID, assetID, revisionNo, KindThumb)
	displayKey := RevisionObjectKey(tenantID, assetID, revisionNo, KindDisplay)
	if err := s.store.Put(ctx, thumbKey, bytes.NewReader(rendered.Thumb), int64(len(rendered.Thumb)), "image/jpeg"); err != nil {
		s.markRevisionFailed(ctx, tenantID, revisionID)
		return nil, fmt.Errorf("store revision thumb: %w", err)
	}
	if err := s.store.Put(ctx, displayKey, bytes.NewReader(rendered.Display), int64(len(rendered.Display)), "image/jpeg"); err != nil {
		s.markRevisionFailed(ctx, tenantID, revisionID)
		return nil, fmt.Errorf("store revision display: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin revision tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := upsertRevisionDerivative(ctx, tx, revisionID, KindThumb, thumbKey, rendered.ThumbWidth, rendered.ThumbHeight,
		map[string]any{"lqip": rendered.LQIP, "color": rendered.Color, "complete": true}); err != nil {
		return nil, err
	}
	if err := upsertRevisionDerivative(ctx, tx, revisionID, KindDisplay, displayKey, rendered.DisplayWidth, rendered.DisplayHeight,
		map[string]any{"watermark": text, "complete": true}); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_revisions
		SET status = 'derivative_ready',
			width = CASE WHEN width = 0 THEN $3 ELSE width END,
			height = CASE WHEN height = 0 THEN $4 ELSE height END
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, revisionID, rendered.SourceWidth, rendered.SourceHeight); err != nil {
		return nil, fmt.Errorf("promote revision: %w", err)
	}
	if err := outbox.Emit(ctx, tx, tenantID, "asset_revision", revisionID.String(), "asset.revision_ready",
		map[string]any{
			"asset_id": assetID, "project_id": projectID, "revision_no": revisionNo,
			"thumb_key": thumbKey, "display_key": displayKey,
		}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit revision derivative: %w", err)
	}
	return &RevisionResult{
		RevisionID: revisionID, AssetID: assetID, RevisionNo: revisionNo,
		ThumbKey: thumbKey, DisplayKey: displayKey,
	}, nil
}

func (s *Service) markRevisionFailed(ctx context.Context, tenantID, revisionID uuid.UUID) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE asset_revisions SET status = 'failed'
		WHERE tenant_id = $1 AND id = $2 AND status <> 'derivative_ready'`,
		tenantID, revisionID); err != nil {
		s.log.Error("mark revision failed", "revision_id", revisionID, "err", err)
	}
}

func upsertRevisionDerivative(ctx context.Context, tx pgx.Tx, revisionID uuid.UUID, kind, key string,
	width, height int, meta map[string]any) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal revision meta: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO asset_revision_derivatives (revision_id, kind, object_key, width, height, meta)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (revision_id, kind) DO UPDATE
		SET object_key = EXCLUDED.object_key, width = EXCLUDED.width,
			height = EXCLUDED.height, meta = EXCLUDED.meta`,
		revisionID, kind, key, width, height, body); err != nil {
		return fmt.Errorf("store revision %s derivative: %w", kind, err)
	}
	return nil
}
