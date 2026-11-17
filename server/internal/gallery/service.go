package gallery

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"photography-workflow/server/internal/payment"
	projectstate "photography-workflow/server/internal/project"
	"photography-workflow/server/internal/storage"
)

const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusExpired   = "expired"
	StatusRevoked   = "revoked"
)

var (
	ErrNotFound              = errors.New("gallery not found")
	ErrProjectNotFound       = errors.New("project not found")
	ErrVersionConflict       = errors.New("gallery version conflict")
	ErrInvalidState          = errors.New("gallery is not in a state that allows this change")
	ErrInvalidInput          = errors.New("invalid gallery input")
	ErrAssetInvalid          = errors.New("asset cannot be added to this gallery")
	ErrAssetLimit            = errors.New("gallery asset limit reached")
	ErrAssetNotFound         = errors.New("asset is not in this gallery")
	ErrTokenNotFound         = errors.New("gallery token not found")
	ErrDerivativesIncomplete = errors.New("gallery derivatives incomplete")
)

// ValidStatus reports whether a status filter may be used in a list query.
func ValidStatus(v string) bool {
	switch v {
	case StatusDraft, StatusPublished, StatusExpired, StatusRevoked:
		return true
	}
	return false
}

// DerivativesIncompleteError tells the operator how much preview rendering is
// missing. Derivation jobs for the missing assets were already queued.
type DerivativesIncompleteError struct {
	Missing int
	Sample  []uuid.UUID
}

func (e *DerivativesIncompleteError) Error() string {
	return fmt.Sprintf("%d assets have no complete thumb/display pair for this gallery", e.Missing)
}

func (e *DerivativesIncompleteError) Unwrap() error { return ErrDerivativesIncomplete }

const columns = `id, project_id, title, status, round_no, manifest_version,
	selection_limit, download_allowed, expires_at, published_at, watermark_text,
	version, selection_version, submitted_at, revoked_at, manifest_key, created_at, updated_at,
	first_submitted_at, selection_edit_until, selection_locked_at, included_retouch_count, extra_retouch_unit_price_cents`

type Gallery struct {
	ID                         uuid.UUID  `json:"id"`
	ProjectID                  uuid.UUID  `json:"project_id"`
	Title                      string     `json:"title"`
	Status                     string     `json:"status"`
	RoundNo                    int        `json:"round_no"`
	ManifestVersion            int        `json:"manifest_version"`
	SelectionLimit             *int       `json:"selection_limit"`
	DownloadAllowed            bool       `json:"download_allowed"`
	ExpiresAt                  *time.Time `json:"expires_at,omitempty"`
	PublishedAt                *time.Time `json:"published_at,omitempty"`
	WatermarkText              string     `json:"watermark_text"`
	Version                    int        `json:"version"`
	SelectionVersion           int        `json:"selection_version"`
	SubmittedAt                *time.Time `json:"submitted_at,omitempty"`
	RevokedAt                  *time.Time `json:"revoked_at,omitempty"`
	ManifestKey                string     `json:"manifest_key,omitempty"`
	CreatedAt                  time.Time  `json:"created_at"`
	UpdatedAt                  time.Time  `json:"updated_at"`
	FirstSubmittedAt           *time.Time `json:"first_submitted_at,omitempty"`
	SelectionEditUntil         *time.Time `json:"selection_edit_until,omitempty"`
	SelectionLockedAt          *time.Time `json:"selection_locked_at,omitempty"`
	IncludedRetouchCount       int        `json:"included_retouch_count"`
	ExtraRetouchUnitPriceCents int64      `json:"extra_retouch_unit_price_cents"`
}

type Summary struct {
	Gallery
	AssetCount int `json:"asset_count"`
}

type SelectionCounts struct {
	Assets    int `json:"assets"`
	Selected  int `json:"selected"`
	Rejected  int `json:"rejected"`
	Undecided int `json:"undecided"`
}

type Token struct {
	ID         uuid.UUID  `json:"id"`
	Label      string     `json:"label"`
	MaxVisits  int        `json:"max_visits"`
	Visits     int        `json:"visits"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// IssuedToken carries the only copy of the plaintext share token. The database
// stores just the SHA-256, so this value cannot be recovered later.
type IssuedToken struct {
	Token
	Plaintext  string `json:"token"`
	GalleryURL string `json:"gallery_url"`
}

type Selection struct {
	AssetID   uuid.UUID `json:"asset_id"`
	Filename  string    `json:"filename"`
	Position  int       `json:"position"`
	Decision  string    `json:"decision"`
	DecidedAt time.Time `json:"decided_at"`
}

type Detail struct {
	Gallery
	Counts     SelectionCounts `json:"counts"`
	Tokens     []Token         `json:"tokens"`
	Selections []Selection     `json:"selections"`
}

// GalleryAsset is one membership row for the staff UI: it carries the grid
// thumbnail and the customer's decision so the operator can triage results
// without opening the customer album.
type GalleryAsset struct {
	AssetID      uuid.UUID `json:"asset_id"`
	Filename     string    `json:"filename"`
	Position     int       `json:"position"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	IsVideo      bool      `json:"is_video"`
	Decision     string    `json:"decision"`
	PreviewReady bool      `json:"preview_ready"`
	ThumbURL     string    `json:"thumb_url,omitempty"`
	LQIP         string    `json:"lqip,omitempty"`
	Color        string    `json:"color,omitempty"`
	DeriveError  string    `json:"derive_error,omitempty"`
}

// DerivativeSummary counts gallery assets whose previews are not publishable,
// plus the ones whose last derivation attempt failed.
type DerivativeSummary struct {
	Missing int `json:"missing"`
	Failed  int `json:"failed"`
}

type GalleryAssets struct {
	Items       []GalleryAsset    `json:"items"`
	Total       int               `json:"total"`
	Limit       int               `json:"limit"`
	Offset      int               `json:"offset"`
	Derivatives DerivativeSummary `json:"derivatives"`
}

// CandidateAsset is a project asset shown in the staff picker. InGallery tells
// the operator which ones are already attached so "add" is idempotent and
// visibly so.
type CandidateAsset struct {
	AssetID      uuid.UUID `json:"asset_id"`
	Filename     string    `json:"filename"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	IsVideo      bool      `json:"is_video"`
	Status       string    `json:"status"`
	InGallery    bool      `json:"in_gallery"`
	PreviewReady bool      `json:"preview_ready"`
	ThumbURL     string    `json:"thumb_url,omitempty"`
	LQIP         string    `json:"lqip,omitempty"`
	Color        string    `json:"color,omitempty"`
}

type CandidatePage struct {
	Items  []CandidateAsset `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

const DecisionNone = "none"

func ValidAssetFilter(v string) bool {
	switch v {
	// "undecided" is the staff-facing name for assets without a decision.
	case "", "all", DecisionNone, "undecided", "selected", "rejected":
		return true
	}
	return false
}

type CreateInput struct {
	ProjectID       uuid.UUID
	Title           string
	ExpiresAt       *time.Time
	SelectionLimit  *int
	DownloadAllowed bool
	WatermarkText   string
}

type UpdateInput struct {
	ExpectedVersion int
	Title           *string
	ExpiresAt       *time.Time
	ClearExpiresAt  bool
	SelectionLimit  *int
	ClearLimit      bool
	DownloadAllowed *bool
	WatermarkText   *string
}

type AssetChangeResult struct {
	Gallery *Gallery `json:"gallery"`
	Added   int      `json:"added"`
	Skipped int      `json:"skipped"`
}

type TokenInput struct {
	Label     string
	MaxVisits int
	ExpiresAt *time.Time
}

type Config struct {
	// DefaultWatermark mirrors gallery.watermark_text: used when a gallery has
	// no snapshot of its own before falling back to the tenant name.
	DefaultWatermark string
	PublicBaseURL    string
	MaxAssets        int
	// PresignTTL bounds the admin thumbnail links returned by ListAssets.
	PresignTTL time.Duration
}

type Service struct {
	pool     *pgxpool.Pool
	store    storage.Storage
	cfg      Config
	log      *slog.Logger
	payments *payment.Service
}

func (s *Service) SetPayments(payments *payment.Service) { s.payments = payments }

func (s *Service) OrderSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, order *payment.Order) error {
	if order == nil || order.Kind != payment.KindSelectionExtra || order.SelectionChargeBatchID == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE selection_charge_batches SET status='paid', updated_at=now() WHERE tenant_id=$1 AND id=$2 AND status='pending'`, tenantID, *order.SelectionChargeBatchID)
	if err != nil {
		return fmt.Errorf("mark selection charge paid: %w", err)
	}
	var galleryID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT gallery_id FROM selection_charge_batches WHERE tenant_id=$1 AND id=$2`, tenantID, *order.SelectionChargeBatchID).Scan(&galleryID); err != nil {
		return fmt.Errorf("load gallery for paid selection batch: %w", err)
	}
	g, err := loadGallery(ctx, tx, tenantID, galleryID, false)
	if err != nil {
		return err
	}
	if g.SelectionEditUntil != nil && !g.SelectionEditUntil.After(time.Now()) {
		job, err := jobs.Enqueue(ctx, tx, tenantID, SelectionRefundJobKind, "gallery", &g.ID, map[string]any{"gallery_id": g.ID})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET run_after=now(), updated_at=now() WHERE tenant_id=$1 AND id=$2 AND status='queued'`, tenantID, job.ID); err != nil {
			return err
		}
	}
	return nil
}

func NewService(pool *pgxpool.Pool, store storage.Storage, cfg Config, log *slog.Logger) *Service {
	if cfg.MaxAssets <= 0 {
		cfg.MaxAssets = 5000
	}
	return &Service{pool: pool, store: store, cfg: cfg, log: log}
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status string, limit, offset int) ([]Summary, int, error) {
	var (
		items []Summary
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		where := `g.tenant_id = $1`
		args := []any{tenantID}
		if projectID != nil {
			args = append(args, *projectID)
			where += fmt.Sprintf(" AND g.project_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			where += fmt.Sprintf(" AND g.status = $%d", len(args))
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM galleries g WHERE `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count galleries: %w", err)
		}
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT %s,
			       (SELECT count(*) FROM gallery_assets ga
			        WHERE ga.tenant_id = g.tenant_id AND ga.gallery_id = g.id)
			FROM galleries g
			WHERE %s
			ORDER BY g.created_at DESC, g.id
			LIMIT $%d OFFSET $%d`, prefixedColumns("g"), where, len(args)-1, len(args)), args...)
		if err != nil {
			return fmt.Errorf("list galleries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it Summary
			if err := scanGalleryInto(rows, &it.Gallery, &it.AssetCount); err != nil {
				return fmt.Errorf("scan gallery: %w", err)
			}
			items = append(items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Summary{}
	}
	return items, total, nil
}

func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateInput) (*Gallery, error) {
	title, watermark, limit, err := validateCreate(in)
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Gallery, error) {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, in.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, ErrProjectNotFound
			}
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
			}
			return nil, fmt.Errorf("check project: %w", err)
		}
		projectID := in.ProjectID
		var round int
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(max(round_no), 0) + 1 FROM galleries
			WHERE tenant_id = $1 AND project_id = $2`, tenantID, projectID).Scan(&round); err != nil {
			return nil, fmt.Errorf("next round: %w", err)
		}
		g, err := scanGallery(tx.QueryRow(ctx, `
			INSERT INTO galleries (tenant_id, project_id, title, status, round_no,
				selection_limit, download_allowed, expires_at, watermark_text)
			VALUES ($1,$2,$3,'draft',$4,$5,$6,$7,$8)
			RETURNING `+columns,
			tenantID, projectID, title, round, limit, in.DownloadAllowed, in.ExpiresAt, watermark))
		if err != nil {
			return nil, fmt.Errorf("insert gallery: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.created", "gallery", g.ID.String(), ip,
			map[string]any{"project_id": g.ProjectID, "round_no": g.RoundNo}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "gallery", g.ID.String(), "gallery.created",
			map[string]any{"project_id": g.ProjectID, "round_no": g.RoundNo}); err != nil {
			return nil, err
		}
		return g, nil
	})
}

func ensureGalleryProjectOperational(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, g *Gallery) error {
	err := projectstate.EnsureOperational(ctx, tx, tenantID, g.ProjectID)
	if errors.Is(err, projectstate.ErrClosed) {
		return fmt.Errorf("%w: project is cancelled or archived", ErrInvalidState)
	}
	if errors.Is(err, projectstate.ErrNotFound) {
		return ErrProjectNotFound
	}
	return err
}

func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (*Detail, error) {
	var out *Detail
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		g, err := loadGallery(ctx, tx, tenantID, id, false)
		if err != nil {
			return err
		}
		d := &Detail{Gallery: *g, Tokens: []Token{}, Selections: []Selection{}}
		if err := tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM gallery_assets WHERE tenant_id = $1 AND gallery_id = $2),
				(SELECT count(*) FROM gallery_selections WHERE tenant_id = $1 AND gallery_id = $2 AND decision = 'selected'),
				(SELECT count(*) FROM gallery_selections WHERE tenant_id = $1 AND gallery_id = $2 AND decision = 'rejected')`,
			tenantID, id).Scan(&d.Counts.Assets, &d.Counts.Selected, &d.Counts.Rejected); err != nil {
			return fmt.Errorf("gallery counts: %w", err)
		}
		d.Counts.Undecided = d.Counts.Assets - d.Counts.Selected - d.Counts.Rejected
		if d.Counts.Undecided < 0 {
			d.Counts.Undecided = 0
		}
		tokens, err := listTokens(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		d.Tokens = tokens
		selections, err := listSelections(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		d.Selections = selections
		out = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) Update(ctx context.Context, tenantID, id, actor uuid.UUID, ip string, in UpdateInput) (*Gallery, error) {
	if in.ExpectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if err := validateUpdate(in); err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Gallery, error) {
		g, err := loadGallery(ctx, tx, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if err := ensureGalleryProjectOperational(ctx, tx, tenantID, g); err != nil {
			return nil, err
		}
		if g.Version != in.ExpectedVersion {
			return nil, ErrVersionConflict
		}
		if g.Status == StatusRevoked || g.Status == StatusExpired {
			return nil, fmt.Errorf("%w: gallery is %s", ErrInvalidState, g.Status)
		}
		// Display derivatives embed the watermark captured at publish time and
		// are not rebuilt by a title/deadline change; allowing it would make the
		// stored text disagree with every preview the customer sees.
		if in.WatermarkText != nil && g.Status != StatusDraft {
			return nil, fmt.Errorf("%w: watermark can only change before publishing", ErrInvalidState)
		}

		sets := []string{"version = version + 1", "updated_at = now()"}
		args := []any{tenantID, id}
		if in.Title != nil {
			args = append(args, strings.TrimSpace(*in.Title))
			sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
		}
		if in.ClearExpiresAt {
			sets = append(sets, "expires_at = NULL")
		} else if in.ExpiresAt != nil {
			args = append(args, *in.ExpiresAt)
			sets = append(sets, fmt.Sprintf("expires_at = $%d", len(args)))
		}
		if in.ClearLimit {
			sets = append(sets, "selection_limit = NULL")
		} else if in.SelectionLimit != nil {
			args = append(args, *in.SelectionLimit)
			sets = append(sets, fmt.Sprintf("selection_limit = $%d", len(args)))
		}
		if in.DownloadAllowed != nil {
			args = append(args, *in.DownloadAllowed)
			sets = append(sets, fmt.Sprintf("download_allowed = $%d", len(args)))
		}
		if in.WatermarkText != nil {
			args = append(args, strings.TrimSpace(*in.WatermarkText))
			sets = append(sets, fmt.Sprintf("watermark_text = $%d", len(args)))
		}
		updated, err := scanGallery(tx.QueryRow(ctx, fmt.Sprintf(`
			UPDATE galleries SET %s WHERE tenant_id = $1 AND id = $2 RETURNING %s`,
			strings.Join(sets, ", "), columns), args...))
		if err != nil {
			return nil, fmt.Errorf("update gallery: %w", err)
		}
		detail := map[string]any{}
		if in.Title != nil {
			detail["title"] = updated.Title
		}
		if in.ExpiresAt != nil || in.ClearExpiresAt {
			detail["expires_at"] = updated.ExpiresAt
		}
		if in.SelectionLimit != nil || in.ClearLimit {
			detail["selection_limit"] = updated.SelectionLimit
		}
		if in.DownloadAllowed != nil {
			detail["download_allowed"] = updated.DownloadAllowed
		}
		if in.WatermarkText != nil {
			detail["watermark_text"] = updated.WatermarkText
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.updated", "gallery", updated.ID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "gallery", updated.ID.String(), "gallery.updated",
			map[string]any{"version": updated.Version}); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// AddAssets attaches assets to a draft gallery. The whole request fails when
// one asset does not belong to the project or is not ingest-verified, so the
// operator can fix the batch instead of wondering which rows landed.
func (s *Service) AddAssets(ctx context.Context, tenantID, id, actor uuid.UUID, ip string, assetIDs []uuid.UUID) (*AssetChangeResult, error) {
	ids := dedupe(assetIDs)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: asset_ids is required", ErrInvalidInput)
	}
	if len(ids) > s.cfg.MaxAssets {
		return nil, fmt.Errorf("%w: at most %d assets per gallery", ErrAssetLimit, s.cfg.MaxAssets)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*AssetChangeResult, error) {
		g, err := loadGallery(ctx, tx, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if err := ensureGalleryProjectOperational(ctx, tx, tenantID, g); err != nil {
			return nil, err
		}
		if g.Status != StatusDraft {
			return nil, fmt.Errorf("%w: assets can only change while the gallery is a draft", ErrInvalidState)
		}

		rows, err := tx.Query(ctx, `
			SELECT id, status FROM assets
			WHERE tenant_id = $1 AND project_id = $2 AND id = ANY($3)`,
			tenantID, g.ProjectID, ids)
		if err != nil {
			return nil, fmt.Errorf("load assets: %w", err)
		}
		statuses := make(map[uuid.UUID]string, len(ids))
		for rows.Next() {
			var assetID uuid.UUID
			var status string
			if err := rows.Scan(&assetID, &status); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan asset: %w", err)
			}
			statuses[assetID] = status
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var invalid []uuid.UUID
		for _, assetID := range ids {
			switch statuses[assetID] {
			case "verified", "derivative_ready":
			default:
				invalid = append(invalid, assetID)
			}
		}
		if len(invalid) > 0 {
			return nil, fmt.Errorf("%w: %d not found in project or not ingest-verified (e.g. %s)",
				ErrAssetInvalid, len(invalid), sampleIDs(invalid))
		}

		attached := make(map[uuid.UUID]struct{}, len(ids))
		existing := 0
		rows, err = tx.Query(ctx, `
			SELECT asset_id FROM gallery_assets WHERE tenant_id = $1 AND gallery_id = $2`,
			tenantID, id)
		if err != nil {
			return nil, fmt.Errorf("load gallery assets: %w", err)
		}
		for rows.Next() {
			var assetID uuid.UUID
			if err := rows.Scan(&assetID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan gallery asset: %w", err)
			}
			attached[assetID] = struct{}{}
			existing++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		var toAdd []uuid.UUID
		for _, assetID := range ids {
			if _, ok := attached[assetID]; !ok {
				toAdd = append(toAdd, assetID)
			}
		}
		if existing+len(toAdd) > s.cfg.MaxAssets {
			return nil, fmt.Errorf("%w: at most %d assets per gallery", ErrAssetLimit, s.cfg.MaxAssets)
		}
		if len(toAdd) == 0 {
			return &AssetChangeResult{Gallery: g, Skipped: len(ids)}, nil
		}

		var position int
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(max(position), 0) FROM gallery_assets
			WHERE tenant_id = $1 AND gallery_id = $2`, tenantID, id).Scan(&position); err != nil {
			return nil, fmt.Errorf("next position: %w", err)
		}
		for _, assetID := range toAdd {
			position++
			if _, err := tx.Exec(ctx, `
				INSERT INTO gallery_assets (tenant_id, gallery_id, asset_id, position)
				VALUES ($1,$2,$3,$4)
				ON CONFLICT (gallery_id, asset_id) DO NOTHING`,
				tenantID, id, assetID, position); err != nil {
				return nil, fmt.Errorf("attach asset: %w", err)
			}
		}
		updated, err := bumpVersion(ctx, tx, tenantID, id)
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.assets.added", "gallery", id.String(), ip,
			map[string]any{"added": len(toAdd)}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "gallery", id.String(), "gallery.assets.added",
			map[string]any{"added": len(toAdd)}); err != nil {
			return nil, err
		}
		return &AssetChangeResult{Gallery: updated, Added: len(toAdd), Skipped: len(ids) - len(toAdd)}, nil
	})
}

func (s *Service) RemoveAsset(ctx context.Context, tenantID, id, assetID, actor uuid.UUID, ip string) (*Gallery, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Gallery, error) {
		g, err := loadGallery(ctx, tx, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if err := ensureGalleryProjectOperational(ctx, tx, tenantID, g); err != nil {
			return nil, err
		}
		if g.Status != StatusDraft {
			return nil, fmt.Errorf("%w: assets can only change while the gallery is a draft", ErrInvalidState)
		}
		ct, err := tx.Exec(ctx, `
			DELETE FROM gallery_assets WHERE tenant_id = $1 AND gallery_id = $2 AND asset_id = $3`,
			tenantID, id, assetID)
		if err != nil {
			return nil, fmt.Errorf("remove gallery asset: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return nil, ErrAssetNotFound
		}
		updated, err := bumpVersion(ctx, tx, tenantID, id)
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.assets.removed", "gallery", id.String(), ip,
			map[string]any{"asset_id": assetID}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "gallery", id.String(), "gallery.assets.removed",
			map[string]any{"asset_id": assetID}); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

type publishGate struct {
	gallery   *Gallery
	published bool
	missing   int
	sample    []uuid.UUID
}

// Publish freezes the asset set into an immutable manifest. Rendering gaps are
// queued and reported as 409 so the operator can retry once the worker catches
// up; a failed object write rolls the status change back.
func (s *Service) Publish(ctx context.Context, tenantID, id, actor uuid.UUID, ip string) (*Gallery, error) {
	gate, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*publishGate, error) {
		g, err := loadGallery(ctx, tx, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if err := ensureGalleryProjectOperational(ctx, tx, tenantID, g); err != nil {
			return nil, err
		}
		if g.Status == StatusPublished {
			return &publishGate{gallery: g, published: true}, nil
		}
		if g.Status != StatusDraft {
			return nil, fmt.Errorf("%w: gallery is %s", ErrInvalidState, g.Status)
		}
		watermark, err := media.ResolveWatermark(ctx, tx, s.watermarkText(g.WatermarkText), tenantID)
		if err != nil {
			return nil, err
		}
		rows, err := loadManifestRows(ctx, tx, tenantID, id)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("%w: gallery has no assets", ErrInvalidInput)
		}
		var missing []manifestRow
		for _, r := range rows {
			if !r.ready() || r.DisplayWatermark != watermark {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			for _, r := range missing {
				force := r.DisplayWatermark != "" && r.DisplayWatermark != watermark
				if _, err := media.EnqueueDerive(ctx, tx, tenantID, r.AssetID, watermark, force); err != nil {
					return nil, fmt.Errorf("queue derive: %w", err)
				}
			}
			sample := make([]uuid.UUID, 0, 5)
			for _, r := range missing {
				if len(sample) == 5 {
					break
				}
				sample = append(sample, r.AssetID)
			}
			return &publishGate{gallery: g, missing: len(missing), sample: sample}, nil
		}

		manifest := buildManifest(g, watermark, rows, time.Now())
		body, err := json.Marshal(manifest)
		if err != nil {
			return nil, fmt.Errorf("encode manifest: %w", err)
		}
		key := ManifestObjectKey(tenantID, g.ID, g.ManifestVersion)
		if err := s.store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "application/json"); err != nil {
			return nil, fmt.Errorf("store manifest: %w", err)
		}
		published, err := scanGallery(tx.QueryRow(ctx, `
			UPDATE galleries
			SET status = 'published', published_at = now(), manifest_key = $3,
				included_retouch_count = coalesce((SELECT b.included_retouch_count FROM bookings b WHERE b.tenant_id = galleries.tenant_id AND b.project_id = galleries.project_id ORDER BY b.created_at DESC LIMIT 1), 0),
				extra_retouch_unit_price_cents = coalesce((SELECT b.extra_retouch_unit_price_cents FROM bookings b WHERE b.tenant_id = galleries.tenant_id AND b.project_id = galleries.project_id ORDER BY b.created_at DESC LIMIT 1), 0),
				version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+columns, tenantID, id, key))
		if err != nil {
			return nil, fmt.Errorf("publish gallery: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.published", "gallery", id.String(), ip,
			map[string]any{"assets": len(rows), "manifest_key": key}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "gallery", id.String(), "gallery.published",
			map[string]any{"project_id": g.ProjectID, "assets": len(rows), "manifest_key": key}); err != nil {
			return nil, err
		}
		return &publishGate{gallery: published, published: true}, nil
	})
	if err != nil {
		return nil, err
	}
	if gate.missing > 0 {
		return gate.gallery, &DerivativesIncompleteError{Missing: gate.missing, Sample: gate.sample}
	}
	if s.payments != nil {
		var status string
		var deposit int64
		if err := s.pool.QueryRow(ctx, `SELECT payment_status, deposit_cents FROM projects WHERE tenant_id=$1 AND id=$2`, tenantID, gate.gallery.ProjectID).Scan(&status, &deposit); err != nil {
			return gate.gallery, fmt.Errorf("load gallery payment gate: %w", err)
		}
		if deposit > 0 && (status == payment.StatusDepositPaid || status == payment.StatusBalancePending) {
			if _, _, err := s.payments.CreateOrder(ctx, tenantID, actor, ip, payment.CreateOrderInput{ProjectID: gate.gallery.ProjectID, Kind: payment.KindBalance}); err != nil {
				return gate.gallery, fmt.Errorf("create balance order after gallery publication: %w", err)
			}
		}
	}
	return gate.gallery, nil
}

func (s *Service) CreateToken(ctx context.Context, tenantID, id, actor uuid.UUID, ip string, in TokenInput) (*IssuedToken, error) {
	label := strings.TrimSpace(in.Label)
	if len(label) > 80 {
		return nil, fmt.Errorf("%w: label must be at most 80 characters", ErrInvalidInput)
	}
	if in.MaxVisits < 0 || in.MaxVisits > 100000 {
		return nil, fmt.Errorf("%w: max_visits must be between 0 and 100000", ErrInvalidInput)
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInput)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*IssuedToken, error) {
		g, err := loadGallery(ctx, tx, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if err := ensureGalleryProjectOperational(ctx, tx, tenantID, g); err != nil {
			return nil, err
		}
		if g.Status != StatusPublished {
			return nil, fmt.Errorf("%w: share links require a published gallery", ErrInvalidState)
		}
		var tok Token
		if err := tx.QueryRow(ctx, `
			INSERT INTO gallery_tokens (tenant_id, gallery_id, token_hash, label, max_visits, expires_at, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING id, label, max_visits, visits, expires_at, revoked_at, last_used_at, created_at`,
			tenantID, id, hash, label, in.MaxVisits, in.ExpiresAt, actor).Scan(
			&tok.ID, &tok.Label, &tok.MaxVisits, &tok.Visits, &tok.ExpiresAt, &tok.RevokedAt, &tok.LastUsedAt, &tok.CreatedAt); err != nil {
			return nil, fmt.Errorf("insert gallery token: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.token.created", "gallery", id.String(), ip,
			map[string]any{"token_id": tok.ID, "label": label}); err != nil {
			return nil, err
		}
		return &IssuedToken{
			Token:      tok,
			Plaintext:  token,
			GalleryURL: s.galleryURL(token),
		}, nil
	})
}

func (s *Service) RevokeToken(ctx context.Context, tenantID, id, tokenID, actor uuid.UUID, ip string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			UPDATE gallery_tokens SET revoked_at = now()
			WHERE tenant_id = $1 AND gallery_id = $2 AND id = $3 AND revoked_at IS NULL`,
			tenantID, id, tokenID)
		if err != nil {
			return fmt.Errorf("revoke gallery token: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return ErrTokenNotFound
		}
		return audit.Write(ctx, tx, tenantID, &actor, "gallery.token.revoked", "gallery", id.String(), ip,
			map[string]any{"token_id": tokenID})
	})
}

// Revoke shuts a published gallery down: the customer API rejects its sessions
// on the next request and every share link stops working.
func (s *Service) Revoke(ctx context.Context, tenantID, id, actor uuid.UUID, ip string) (*Gallery, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Gallery, error) {
		g, err := loadGallery(ctx, tx, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if g.Status == StatusRevoked {
			return g, nil
		}
		if g.Status != StatusPublished {
			return nil, fmt.Errorf("%w: only published galleries can be revoked", ErrInvalidState)
		}
		updated, err := scanGallery(tx.QueryRow(ctx, `
			UPDATE galleries
			SET status = 'revoked', revoked_at = now(), version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+columns, tenantID, id))
		if err != nil {
			return nil, fmt.Errorf("revoke gallery: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE gallery_tokens SET revoked_at = now()
			WHERE tenant_id = $1 AND gallery_id = $2 AND revoked_at IS NULL`, tenantID, id); err != nil {
			return nil, fmt.Errorf("revoke gallery tokens: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.revoked", "gallery", id.String(), ip, nil); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "gallery", id.String(), "gallery.revoked", nil); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// Reopen clears the customer's submission so they can change decisions again.
// The selection_version keeps advancing, so an old client cannot replay.
func (s *Service) Reopen(ctx context.Context, tenantID, id, actor uuid.UUID, ip string) (*Gallery, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Gallery, error) {
		g, err := loadGallery(ctx, tx, tenantID, id, true)
		if err != nil {
			return nil, err
		}
		if err := ensureGalleryProjectOperational(ctx, tx, tenantID, g); err != nil {
			return nil, err
		}
		if g.Status != StatusPublished {
			return nil, fmt.Errorf("%w: only published galleries can be reopened", ErrInvalidState)
		}
		if g.ExpiresAt != nil && !g.ExpiresAt.After(time.Now()) {
			return nil, fmt.Errorf("%w: gallery has expired; extend expires_at first", ErrInvalidState)
		}
		updated, err := scanGallery(tx.QueryRow(ctx, `
			UPDATE galleries SET submitted_at = NULL, selection_edit_until = NULL,
				selection_locked_at = NULL, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+columns, tenantID, id))
		if err != nil {
			return nil, fmt.Errorf("reopen gallery: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gallery.reopened", "gallery", id.String(), ip, nil); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "gallery", id.String(), "gallery.reopened", nil); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// watermarkText resolves the per-gallery snapshot against the configured
// default. The last fallback, the tenant name, is applied by ResolveWatermark.
func (s *Service) watermarkText(galleryText string) string {
	if text := strings.TrimSpace(galleryText); text != "" {
		return text
	}
	return s.cfg.DefaultWatermark
}

func (s *Service) galleryURL(token string) string {
	base := strings.TrimRight(s.cfg.PublicBaseURL, "/")
	return base + "/#/a/" + token
}

func validateCreate(in CreateInput) (title, watermark string, limit *int, err error) {
	title = strings.TrimSpace(in.Title)
	if title == "" {
		return "", "", nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if len(title) > 120 {
		return "", "", nil, fmt.Errorf("%w: title must be at most 120 characters", ErrInvalidInput)
	}
	watermark = strings.TrimSpace(in.WatermarkText)
	if len(watermark) > 120 {
		return "", "", nil, fmt.Errorf("%w: watermark_text must be at most 120 characters", ErrInvalidInput)
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		return "", "", nil, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInput)
	}
	if in.SelectionLimit != nil {
		v := *in.SelectionLimit
		if v <= 0 || v > 10000 {
			return "", "", nil, fmt.Errorf("%w: selection_limit must be between 1 and 10000", ErrInvalidInput)
		}
		limit = in.SelectionLimit
	}
	return title, watermark, limit, nil
}

func validateUpdate(in UpdateInput) error {
	if in.Title != nil {
		if strings.TrimSpace(*in.Title) == "" {
			return fmt.Errorf("%w: title must not be empty", ErrInvalidInput)
		}
		if len(strings.TrimSpace(*in.Title)) > 120 {
			return fmt.Errorf("%w: title must be at most 120 characters", ErrInvalidInput)
		}
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInput)
	}
	if in.SelectionLimit != nil {
		v := *in.SelectionLimit
		if v <= 0 || v > 10000 {
			return fmt.Errorf("%w: selection_limit must be between 1 and 10000", ErrInvalidInput)
		}
	}
	if in.WatermarkText != nil && len(strings.TrimSpace(*in.WatermarkText)) > 120 {
		return fmt.Errorf("%w: watermark_text must be at most 120 characters", ErrInvalidInput)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGallery(row rowScanner) (*Gallery, error) {
	var g Gallery
	if err := scanGalleryInto(row, &g); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &g, nil
}

func scanGalleryInto(row rowScanner, g *Gallery, extra ...any) error {
	dest := []any{&g.ID, &g.ProjectID, &g.Title, &g.Status, &g.RoundNo, &g.ManifestVersion,
		&g.SelectionLimit, &g.DownloadAllowed, &g.ExpiresAt, &g.PublishedAt, &g.WatermarkText,
		&g.Version, &g.SelectionVersion, &g.SubmittedAt, &g.RevokedAt, &g.ManifestKey,
		&g.CreatedAt, &g.UpdatedAt, &g.FirstSubmittedAt, &g.SelectionEditUntil, &g.SelectionLockedAt,
		&g.IncludedRetouchCount, &g.ExtraRetouchUnitPriceCents}
	dest = append(dest, extra...)
	return row.Scan(dest...)
}

func prefixedColumns(alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func loadGallery(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, lock bool) (*Gallery, error) {
	query := `SELECT ` + columns + ` FROM galleries WHERE tenant_id = $1 AND id = $2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanGallery(tx.QueryRow(ctx, query, tenantID, id))
}

func bumpVersion(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (*Gallery, error) {
	return scanGallery(tx.QueryRow(ctx, `
		UPDATE galleries SET version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+columns, tenantID, id))
}

func listTokens(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID) ([]Token, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, label, max_visits, visits, expires_at, revoked_at, last_used_at, created_at
		FROM gallery_tokens
		WHERE tenant_id = $1 AND gallery_id = $2
		ORDER BY created_at DESC, id`, tenantID, galleryID)
	if err != nil {
		return nil, fmt.Errorf("list gallery tokens: %w", err)
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Label, &t.MaxVisits, &t.Visits, &t.ExpiresAt,
			&t.RevokedAt, &t.LastUsedAt, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan gallery token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func listSelections(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID) ([]Selection, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.asset_id, a.filename, ga.position, s.decision, s.decided_at
		FROM gallery_selections s
		JOIN gallery_assets ga ON ga.gallery_id = s.gallery_id AND ga.asset_id = s.asset_id
		JOIN assets a ON a.id = s.asset_id AND a.tenant_id = s.tenant_id
		WHERE s.tenant_id = $1 AND s.gallery_id = $2
		ORDER BY ga.position, a.filename`, tenantID, galleryID)
	if err != nil {
		return nil, fmt.Errorf("list gallery selections: %w", err)
	}
	defer rows.Close()
	out := []Selection{}
	for rows.Next() {
		var s Selection
		if err := rows.Scan(&s.AssetID, &s.Filename, &s.Position, &s.Decision, &s.DecidedAt); err != nil {
			return nil, fmt.Errorf("scan gallery selection: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListAssets pages through the gallery membership for the staff UI. The
// filter matches the customer's decision dimension; search is a filename
// substring. Thumbnails are short-lived presigned URLs and are simply omitted
// when signing fails, so one broken object cannot blank the whole page.
func (s *Service) ListAssets(ctx context.Context, tenantID, galleryID uuid.UUID,
	filter, search string, limit, offset int) (*GalleryAssets, error) {
	if filter == "" || filter == "all" {
		filter = "all"
	} else if filter == "undecided" {
		filter = DecisionNone
	}
	search = strings.TrimSpace(search)
	if len(search) > 120 {
		return nil, fmt.Errorf("%w: search must be at most 120 characters", ErrInvalidInput)
	}
	out := &GalleryAssets{Items: []GalleryAsset{}, Limit: limit, Offset: offset}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := loadGallery(ctx, tx, tenantID, galleryID, false); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*)
			FROM gallery_assets ga
			JOIN assets a ON a.id = ga.asset_id AND a.tenant_id = ga.tenant_id
			LEFT JOIN gallery_selections s
				ON s.gallery_id = ga.gallery_id AND s.asset_id = ga.asset_id AND s.tenant_id = ga.tenant_id
			WHERE ga.tenant_id = $1 AND ga.gallery_id = $2
			  AND ($3 = 'all' OR ($3 = 'none' AND s.decision IS NULL) OR s.decision = $3)
			  AND ($4 = '' OR a.filename ILIKE '%' || $4 || '%')`,
			tenantID, galleryID, filter, search).Scan(&out.Total); err != nil {
			return fmt.Errorf("count gallery assets: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT ga.asset_id, a.filename, ga.position, a.width, a.height, a.is_video,
			       coalesce(s.decision, 'none'),
			       coalesce(t.object_key, ''), coalesce(t.meta->>'lqip', ''), coalesce(t.meta->>'color', ''),
			       coalesce(t.object_key <> '' AND t.meta->>'complete' = 'true', false)
			         AND coalesce(d.object_key <> '' AND d.meta->>'complete' = 'true', false),
			       coalesce(nullif(t.meta->>'error', ''), nullif(d.meta->>'error', ''), '')
			FROM gallery_assets ga
			JOIN assets a ON a.id = ga.asset_id AND a.tenant_id = ga.tenant_id
			LEFT JOIN gallery_selections s
				ON s.gallery_id = ga.gallery_id AND s.asset_id = ga.asset_id AND s.tenant_id = ga.tenant_id
			LEFT JOIN asset_derivatives t
				ON t.asset_id = ga.asset_id AND t.tenant_id = ga.tenant_id
				AND t.kind = 'thumb' AND t.is_watermarked = false
			LEFT JOIN asset_derivatives d
				ON d.asset_id = ga.asset_id AND d.tenant_id = ga.tenant_id
				AND d.kind = 'display' AND d.is_watermarked = true
			WHERE ga.tenant_id = $1 AND ga.gallery_id = $2
			  AND ($3 = 'all' OR ($3 = 'none' AND s.decision IS NULL) OR s.decision = $3)
			  AND ($4 = '' OR a.filename ILIKE '%' || $4 || '%')
			ORDER BY ga.position, a.filename
			LIMIT $5 OFFSET $6`,
			tenantID, galleryID, filter, search, limit, offset)
		if err != nil {
			return fmt.Errorf("list gallery assets: %w", err)
		}
		defer rows.Close()
		type pending struct {
			index int
			key   string
		}
		var sign []pending
		for rows.Next() {
			var a GalleryAsset
			var key string
			if err := rows.Scan(&a.AssetID, &a.Filename, &a.Position, &a.Width, &a.Height,
				&a.IsVideo, &a.Decision, &key, &a.LQIP, &a.Color, &a.PreviewReady, &a.DeriveError); err != nil {
				return fmt.Errorf("scan gallery asset: %w", err)
			}
			if a.PreviewReady && key != "" {
				sign = append(sign, pending{index: len(out.Items), key: key})
			}
			out.Items = append(out.Items, a)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ttl := s.cfg.PresignTTL
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		for _, p := range sign {
			url, err := s.store.PresignGet(ctx, p.key, ttl)
			if err != nil {
				s.log.Warn("presign gallery thumb", "gallery_id", galleryID, "error", err)
				continue
			}
			out.Items[p.index].ThumbURL = url
		}
		return tx.QueryRow(ctx, `
			SELECT
				coalesce(count(*) FILTER (WHERE NOT (
					coalesce(t.object_key <> '' AND t.meta->>'complete' = 'true', false)
					AND coalesce(d.object_key <> '' AND d.meta->>'complete' = 'true', false))), 0),
				coalesce(count(*) FILTER (
					WHERE coalesce(t.meta->>'error', '') <> '' OR coalesce(d.meta->>'error', '') <> ''), 0)
			FROM gallery_assets ga
			LEFT JOIN asset_derivatives t
				ON t.asset_id = ga.asset_id AND t.tenant_id = ga.tenant_id
				AND t.kind = 'thumb' AND t.is_watermarked = false
			LEFT JOIN asset_derivatives d
				ON d.asset_id = ga.asset_id AND d.tenant_id = ga.tenant_id
				AND d.kind = 'display' AND d.is_watermarked = true
			WHERE ga.tenant_id = $1 AND ga.gallery_id = $2`,
			tenantID, galleryID).Scan(&out.Derivatives.Missing, &out.Derivatives.Failed)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListCandidates pages through the project's addable assets with gallery
// membership flags. Only ingest-verified assets appear: anything else would be
// rejected by AddAssets anyway.
func (s *Service) ListCandidates(ctx context.Context, tenantID, galleryID uuid.UUID,
	search string, limit, offset int) (*CandidatePage, error) {
	search = strings.TrimSpace(search)
	if len(search) > 120 {
		return nil, fmt.Errorf("%w: search must be at most 120 characters", ErrInvalidInput)
	}
	out := &CandidatePage{Items: []CandidateAsset{}, Limit: limit, Offset: offset}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		g, err := loadGallery(ctx, tx, tenantID, galleryID, false)
		if err != nil {
			return err
		}
		const where = `
			FROM assets a
			LEFT JOIN gallery_assets ga
				ON ga.asset_id = a.id AND ga.tenant_id = a.tenant_id AND ga.gallery_id = $2
			LEFT JOIN asset_derivatives t
				ON t.asset_id = a.id AND t.tenant_id = a.tenant_id
				AND t.kind = 'thumb' AND t.is_watermarked = false
			WHERE a.tenant_id = $1 AND a.project_id = $3
			  AND a.status IN ('verified', 'derivative_ready')
			  AND ($4 = '' OR a.filename ILIKE '%' || $4 || '%')`
		if err := tx.QueryRow(ctx, `SELECT count(*) `+where,
			tenantID, galleryID, g.ProjectID, search).Scan(&out.Total); err != nil {
			return fmt.Errorf("count candidates: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT a.id, a.filename, a.width, a.height, a.is_video, a.status,
			       ga.asset_id IS NOT NULL,
			       coalesce(t.object_key, ''), coalesce(t.meta->>'lqip', ''), coalesce(t.meta->>'color', ''),
			       coalesce(t.object_key <> '' AND t.meta->>'complete' = 'true', false)`+where+`
			ORDER BY a.created_at DESC, a.filename, a.id
			LIMIT $5 OFFSET $6`,
			tenantID, galleryID, g.ProjectID, search, limit, offset)
		if err != nil {
			return fmt.Errorf("list candidates: %w", err)
		}
		defer rows.Close()
		type pending struct {
			index int
			key   string
		}
		var sign []pending
		for rows.Next() {
			var a CandidateAsset
			var key string
			if err := rows.Scan(&a.AssetID, &a.Filename, &a.Width, &a.Height, &a.IsVideo,
				&a.Status, &a.InGallery, &key, &a.LQIP, &a.Color, &a.PreviewReady); err != nil {
				return fmt.Errorf("scan candidate: %w", err)
			}
			if a.PreviewReady && key != "" {
				sign = append(sign, pending{index: len(out.Items), key: key})
			}
			out.Items = append(out.Items, a)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ttl := s.cfg.PresignTTL
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		for _, p := range sign {
			url, err := s.store.PresignGet(ctx, p.key, ttl)
			if err != nil {
				s.log.Warn("presign candidate thumb", "gallery_id", galleryID, "error", err)
				continue
			}
			out.Items[p.index].ThumbURL = url
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type manifestRow struct {
	AssetID          uuid.UUID
	Filename         string
	Position         int
	ContentHash      string
	IsVideo          bool
	ThumbKey         string
	ThumbWidth       int
	ThumbHeight      int
	ThumbComplete    bool
	DisplayKey       string
	DisplayWidth     int
	DisplayHeight    int
	DisplayComplete  bool
	DisplayWatermark string
	LQIP             string
	Color            string
}

func (r manifestRow) ready() bool {
	return r.ThumbComplete && r.DisplayComplete && r.ThumbKey != "" && r.DisplayKey != ""
}

func loadManifestRows(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID) ([]manifestRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT ga.asset_id, a.filename, ga.position, a.content_hash, a.is_video,
		       coalesce(t.object_key, ''), coalesce(t.width, 0), coalesce(t.height, 0),
		       coalesce(t.object_key <> '' AND t.meta->>'complete' = 'true', false),
		       coalesce(d.object_key, ''), coalesce(d.width, 0), coalesce(d.height, 0),
		       coalesce(d.object_key <> '' AND d.meta->>'complete' = 'true', false),
		       coalesce(d.meta->>'watermark', ''),
		       coalesce(t.meta->>'lqip', ''), coalesce(t.meta->>'color', '')
		FROM gallery_assets ga
		JOIN assets a ON a.id = ga.asset_id AND a.tenant_id = ga.tenant_id
		LEFT JOIN asset_derivatives t
			ON t.asset_id = ga.asset_id AND t.tenant_id = ga.tenant_id
			AND t.kind = 'thumb' AND t.is_watermarked = false
		LEFT JOIN asset_derivatives d
			ON d.asset_id = ga.asset_id AND d.tenant_id = ga.tenant_id
			AND d.kind = 'display' AND d.is_watermarked = true
		WHERE ga.tenant_id = $1 AND ga.gallery_id = $2
		ORDER BY ga.position, a.filename, ga.asset_id`, tenantID, galleryID)
	if err != nil {
		return nil, fmt.Errorf("load gallery assets: %w", err)
	}
	defer rows.Close()
	var out []manifestRow
	for rows.Next() {
		var r manifestRow
		if err := rows.Scan(&r.AssetID, &r.Filename, &r.Position, &r.ContentHash, &r.IsVideo,
			&r.ThumbKey, &r.ThumbWidth, &r.ThumbHeight, &r.ThumbComplete,
			&r.DisplayKey, &r.DisplayWidth, &r.DisplayHeight, &r.DisplayComplete,
			&r.DisplayWatermark, &r.LQIP, &r.Color); err != nil {
			return nil, fmt.Errorf("scan gallery asset: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func dedupe(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func sampleIDs(ids []uuid.UUID) string {
	parts := make([]string, 0, 5)
	for _, id := range ids {
		if len(parts) == 5 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, id.String())
	}
	return strings.Join(parts, ", ")
}
