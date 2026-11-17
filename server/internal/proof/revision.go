package proof

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/media"
	"photography-workflow/server/internal/outbox"
)

// Revision is one immutable retouched output of an asset. revision_no increases
// per asset; the checksum uniqueness makes a re-sent file a no-op.
type Revision struct {
	ID         uuid.UUID  `json:"id"`
	ProjectID  uuid.UUID  `json:"project_id"`
	AssetID    uuid.UUID  `json:"asset_id"`
	RevisionNo int        `json:"revision_no"`
	SHA256     string     `json:"sha256"`
	SizeBytes  int64      `json:"size_bytes"`
	Width      int        `json:"width"`
	Height     int        `json:"height"`
	Status     string     `json:"status"`
	Note       string     `json:"note,omitempty"`
	CreatedBy  *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

const revisionColumns = `id, project_id, asset_id, revision_no, sha256, size_bytes,
	width, height, status, note, created_by, created_at`

func scanRevision(row scanner) (*Revision, error) {
	var r Revision
	if err := row.Scan(&r.ID, &r.ProjectID, &r.AssetID, &r.RevisionNo, &r.SHA256, &r.SizeBytes,
		&r.Width, &r.Height, &r.Status, &r.Note, &r.CreatedBy, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

type RevisionInput struct {
	AssetID  uuid.UUID
	Filename string
	SHA256   string
	Size     int64
	Width    int
	Height   int
	Note     string
}

// ImportedRevision reports whether the upload created a version or matched an
// existing checksum (duplicate, returned with 200 so retries are idempotent).
type ImportedRevision struct {
	Revision
	Duplicate bool `json:"duplicate"`
}

// revisionSourceKey keeps the uploaded bytes separate from the derived previews
// written by media.RevisionObjectKey.
func revisionSourceKey(tenantID, assetID uuid.UUID, revisionNo int, ext string) string {
	return fmt.Sprintf("t/%s/revisions/%s/v%d/source.%s", tenantID, assetID, revisionNo, ext)
}

// ImportRevision streams one retouched file to a temporary copy, verifies the
// declared SHA-256 and size, then stores it as the asset's next version. It must
// be called while the round is retouching; the round item check rejects assets
// the customer never saw. actor is nil for files imported by the worker on
// behalf of an external retoucher.
func (s *Service) ImportRevision(ctx context.Context, tenantID, roundID uuid.UUID, actor *uuid.UUID, ip string,
	in RevisionInput, content io.Reader) (*ImportedRevision, error) {
	sha, err := normalizeSHA256(in.SHA256)
	if err != nil {
		return nil, err
	}
	if in.Size <= 0 || in.Size > s.cfg.RevisionMaxBytes {
		return nil, fmt.Errorf("%w: size must be between 1 and %d bytes", ErrInvalidInput, s.cfg.RevisionMaxBytes)
	}
	if in.Width < 0 || in.Height < 0 || in.Width > 100000 || in.Height > 100000 {
		return nil, fmt.Errorf("%w: invalid declared dimensions", ErrInvalidInput)
	}
	note := strings.TrimSpace(in.Note)
	if len(note) > 500 {
		return nil, fmt.Errorf("%w: note must be at most 500 characters", ErrInvalidInput)
	}

	spool, err := spoolRevision(content, s.cfg.RevisionMaxBytes)
	if err != nil {
		return nil, err
	}
	defer spool.close()

	if spool.size != in.Size {
		return nil, fmt.Errorf("%w: declared %d bytes, received %d", ErrChecksumMismatch, in.Size, spool.size)
	}
	if spool.sha != sha {
		return nil, fmt.Errorf("%w: declared %s, received %s", ErrChecksumMismatch, sha, spool.sha)
	}

	var storedKey string
	imported, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ImportedRevision, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, true)
		if err != nil {
			return nil, err
		}
		if round.Status != StatusRetouching {
			return nil, fmt.Errorf("%w: revisions can only be uploaded while retouching", ErrInvalidState)
		}
		var inRound int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM proof_round_items
			WHERE tenant_id = $1 AND proof_round_id = $2 AND asset_id = $3`,
			tenantID, roundID, in.AssetID).Scan(&inRound); err != nil {
			return nil, fmt.Errorf("check round item: %w", err)
		}
		if inRound == 0 {
			return nil, ErrUnknownAsset
		}

		// Serialise per asset so concurrent uploads cannot pick the same
		// revision_no; the row also tells us the project for the new version.
		var projectID uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT project_id FROM assets WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, in.AssetID).Scan(&projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrUnknownAsset
			}
			return nil, fmt.Errorf("lock asset: %w", err)
		}
		if projectID != round.ProjectID {
			return nil, ErrUnknownAsset
		}

		if existing, err := scanRevision(tx.QueryRow(ctx, `
			SELECT `+revisionColumns+` FROM asset_revisions
			WHERE tenant_id = $1 AND asset_id = $2 AND sha256 = $3`,
			tenantID, in.AssetID, sha)); err == nil {
			return &ImportedRevision{Revision: *existing, Duplicate: true}, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("check revision checksum: %w", err)
		}

		var next int
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(max(revision_no), 0) + 1 FROM asset_revisions
			WHERE tenant_id = $1 AND asset_id = $2`, tenantID, in.AssetID).Scan(&next); err != nil {
			return nil, fmt.Errorf("next revision number: %w", err)
		}
		key := revisionSourceKey(tenantID, in.AssetID, next, spool.ext)

		rev, err := scanRevision(tx.QueryRow(ctx, `
			INSERT INTO asset_revisions
				(tenant_id, project_id, asset_id, revision_no, source_key, sha256, size_bytes,
				 width, height, status, note, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'uploaded',$10,$11)
			RETURNING `+revisionColumns,
			tenantID, projectID, in.AssetID, next, key, sha, spool.size,
			in.Width, in.Height, note, actor))
		if err != nil {
			return nil, fmt.Errorf("insert revision: %w", err)
		}

		if _, err := spool.seekStart(); err != nil {
			return nil, fmt.Errorf("rewind revision upload: %w", err)
		}
		if err := s.store.Put(ctx, key, spool.file, spool.size, spool.contentType); err != nil {
			return nil, fmt.Errorf("store revision source: %w", err)
		}
		storedKey = key

		watermark, err := galleryWatermark(ctx, tx, tenantID, round.GalleryID, s.cfg.DefaultWatermark)
		if err != nil {
			return nil, err
		}
		if _, err := media.EnqueueRevisionDerive(ctx, tx, tenantID, rev.ID, watermark); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, actor, "proof.revision.uploaded", "asset_revision", rev.ID.String(), ip,
			map[string]any{
				"proof_round_id": roundID, "asset_id": in.AssetID, "revision_no": rev.RevisionNo,
				"sha256": sha, "size_bytes": spool.size,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "proof_round", roundID.String(), "proof.revision.uploaded",
			map[string]any{
				"proof_round_id": roundID, "asset_id": in.AssetID,
				"revision_id": rev.ID, "revision_no": rev.RevisionNo,
			}); err != nil {
			return nil, err
		}
		return &ImportedRevision{Revision: *rev}, nil
	})
	if err != nil {
		s.discardRevisionSource(ctx, storedKey)
		return nil, err
	}
	return imported, nil
}

// discardRevisionSource removes a source object whose transaction did not
// commit, so a failed import cannot leave orphan bytes in storage. The cleanup
// runs even if the request context was cancelled and is best-effort: a failure
// is reported but never masks the original error.
func (s *Service) discardRevisionSource(ctx context.Context, key string) {
	if key == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.Delete(cleanupCtx, key); err != nil {
		s.log.Warn("revision source cleanup failed", "revision_key", key, "error", err)
	}
}

// latestRevisions returns the newest derivative_ready revision per asset, keyed
// by asset. Assets without a retouched version are absent, so the manifest falls
// back to the M5 original previews.
func latestRevisions(ctx context.Context, tx pgx.Tx, tenantID, roundID uuid.UUID) (map[uuid.UUID]*Revision, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (r.asset_id) `+revisionColumns+`
		FROM asset_revisions r
		JOIN proof_round_items i
			ON i.tenant_id = r.tenant_id AND i.asset_id = r.asset_id AND i.proof_round_id = $2
		WHERE r.tenant_id = $1 AND r.status = 'derivative_ready'
		ORDER BY r.asset_id, r.revision_no DESC`, tenantID, roundID)
	if err != nil {
		return nil, fmt.Errorf("load revisions: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]*Revision)
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("scan revision: %w", err)
		}
		out[rev.AssetID] = rev
	}
	return out, rows.Err()
}

func normalizeSHA256(raw string) (string, error) {
	sha := strings.ToLower(strings.TrimSpace(raw))
	if len(sha) != 64 {
		return "", fmt.Errorf("%w: sha256 must be 64 hexadecimal characters", ErrInvalidInput)
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", fmt.Errorf("%w: sha256 must be hexadecimal", ErrInvalidInput)
	}
	return sha, nil
}

var (
	ErrRevisionTooLarge = errors.New("revision upload is too large")
	ErrRevisionFormat   = errors.New("revision must be a JPEG, PNG or WebP image")
)

// uploadSpool is the verified temporary copy of an upload. The caller closes it
// after the source has been written to storage.
type uploadSpool struct {
	file        *os.File
	path        string
	size        int64
	sha         string
	ext         string
	contentType string
}

func (u *uploadSpool) seekStart() (*os.File, error) {
	if _, err := u.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return u.file, nil
}

func (u *uploadSpool) close() {
	if u.file != nil {
		_ = u.file.Close()
	}
	if u.path != "" {
		_ = os.Remove(u.path)
	}
}

// spoolRevision copies the upload to a temporary file while hashing it, so the
// declared checksum can be verified before a single byte reaches storage.
func spoolRevision(src io.Reader, max int64) (*uploadSpool, error) {
	f, err := os.CreateTemp("", "pwf-revision-*")
	if err != nil {
		return nil, fmt.Errorf("create revision spool: %w", err)
	}
	spool := &uploadSpool{file: f, path: f.Name()}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, max+1))
	if err != nil {
		spool.close()
		return nil, fmt.Errorf("read revision upload: %w", err)
	}
	if n > max {
		spool.close()
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrRevisionTooLarge, max)
	}
	if n == 0 {
		spool.close()
		return nil, fmt.Errorf("%w: upload is empty", ErrRevisionFormat)
	}
	spool.size = n
	spool.sha = hex.EncodeToString(h.Sum(nil))
	spool.ext, spool.contentType, err = sniffImage(f)
	if err != nil {
		spool.close()
		return nil, err
	}
	return spool, nil
}

// sniffImage identifies the supported formats from their magic bytes. Raw and
// layered formats are rejected before occupying storage or a derivation slot.
func sniffImage(f *os.File) (ext, contentType string, err error) {
	head := make([]byte, 12)
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return "", "", fmt.Errorf("read revision header: %w", err)
	}
	head = head[:n]
	switch {
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "jpg", "image/jpeg", nil
	case len(head) >= 8 && head[0] == 0x89 && string(head[1:4]) == "PNG" && head[4] == 0x0D && head[5] == 0x0A && head[6] == 0x1A && head[7] == 0x0A:
		return "png", "image/png", nil
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "webp", "image/webp", nil
	}
	return "", "", ErrRevisionFormat
}
