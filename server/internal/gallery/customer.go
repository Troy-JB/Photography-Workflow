package gallery

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
	"photography-workflow/server/internal/storage"
)

var (
	ErrInvalidShareToken = errors.New("share token is not valid")
	ErrShareExpired      = errors.New("share token has expired")
	ErrShareRevoked      = errors.New("share token has been revoked")
	ErrTooManyVisits     = errors.New("share token visit limit reached")
	ErrBalanceUnpaid     = errors.New("project balance must be paid before selection")
	ErrGalleryExpired    = errors.New("gallery has expired")
	ErrGalleryRevoked    = errors.New("gallery has been revoked")
	ErrAlreadySubmitted  = errors.New("gallery selection was already submitted")
	ErrSelectionLimit    = errors.New("selection limit exceeded")
	ErrUnknownAsset      = errors.New("asset is not in this gallery")
	ErrSelectionBatch    = errors.New("too many selection changes in one request")
)

// VersionConflictError carries the current selection version so the customer
// app can retry against the right state instead of guessing.
type VersionConflictError struct {
	Current int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("selection_version is %d", e.Current)
}

func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }

type CustomerConfig struct {
	SessionTTL time.Duration
	PresignTTL time.Duration
	BatchLimit int
}

const (
	defaultSessionTTL = 2 * time.Hour
	defaultPresignTTL = 10 * time.Minute
	defaultBatchLimit = 200
)

type CustomerService struct {
	pool      *pgxpool.Pool
	store     storage.Storage
	tokens    *auth.GalleryTokens
	cfg       CustomerConfig
	manifests *ManifestLoader
}

func ensureCustomerGalleryProject(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, g *Gallery) error {
	err := projectstate.EnsureOperational(ctx, tx, tenantID, g.ProjectID)
	if errors.Is(err, projectstate.ErrClosed) || errors.Is(err, projectstate.ErrNotFound) {
		return ErrGalleryRevoked
	}
	return err
}

func ensureBalancePaid(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) error {
	var total, coreNet int64
	err := tx.QueryRow(ctx, `SELECT p.total_amount_cents,
		coalesce((SELECT sum(pay.amount_cents) FROM payments pay JOIN payment_orders o ON o.id=pay.order_id
		          WHERE pay.tenant_id=p.tenant_id AND pay.project_id=p.id AND o.kind IN ('deposit','balance','full')),0)
		- coalesce((SELECT sum(r.amount_cents) FROM refunds r JOIN payments pay ON pay.id=r.payment_id JOIN payment_orders o ON o.id=pay.order_id
		            WHERE r.tenant_id=p.tenant_id AND r.project_id=p.id AND r.status='succeeded' AND o.kind IN ('deposit','balance','full')),0)
		FROM projects p WHERE p.tenant_id=$1 AND p.id=$2`, tenantID, projectID).Scan(&total, &coreNet)
	if err != nil {
		return fmt.Errorf("load core balance: %w", err)
	}
	if total <= 0 || coreNet < total {
		return ErrBalanceUnpaid
	}
	return nil
}

func NewCustomerService(pool *pgxpool.Pool, store storage.Storage, tokens *auth.GalleryTokens, cfg CustomerConfig) *CustomerService {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = defaultPresignTTL
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = defaultBatchLimit
	}
	return &CustomerService{
		pool:      pool,
		store:     store,
		tokens:    tokens,
		cfg:       cfg,
		manifests: NewManifestLoader(store),
	}
}

type AlbumView struct {
	ID                         uuid.UUID             `json:"id"`
	ProjectID                  uuid.UUID             `json:"project_id"`
	Title                      string                `json:"title"`
	ExpiresAt                  *time.Time            `json:"expires_at,omitempty"`
	SelectionLimit             *int                  `json:"selection_limit"`
	DownloadAllowed            bool                  `json:"download_allowed"`
	SubmittedAt                *time.Time            `json:"submitted_at,omitempty"`
	SelectionVersion           int                   `json:"selection_version"`
	Counts                     SelectionCounts       `json:"counts"`
	FirstSubmittedAt           *time.Time            `json:"first_submitted_at,omitempty"`
	SelectionEditUntil         *time.Time            `json:"selection_edit_until,omitempty"`
	SelectionLockedAt          *time.Time            `json:"selection_locked_at,omitempty"`
	IncludedRetouchCount       int                   `json:"included_retouch_count"`
	ExtraRetouchUnitPriceCents int64                 `json:"extra_retouch_unit_price_cents"`
	ChargeBatchID              *uuid.UUID            `json:"charge_batch_id,omitempty"`
	ChargeAmountCents          int64                 `json:"charge_amount_cents,omitempty"`
	PaymentURL                 string                `json:"payment_url,omitempty"`
	PaymentOrderID             *uuid.UUID            `json:"payment_order_id,omitempty"`
	RefundTasks                []SelectionRefundTask `json:"-"`
}

type SelectionRefundTask struct {
	PaymentID   uuid.UUID
	AmountCents int64
}

type SessionView struct {
	Session   string    `json:"session"`
	ExpiresAt time.Time `json:"expires_at"`
	Album     AlbumView `json:"album"`
}

type PageItem struct {
	AssetID    uuid.UUID `json:"asset_id"`
	Filename   string    `json:"filename"`
	Position   int       `json:"position"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	IsVideo    bool      `json:"is_video"`
	ThumbURL   string    `json:"thumb_url"`
	DisplayURL string    `json:"display_url"`
	LQIP       string    `json:"lqip,omitempty"`
	Color      string    `json:"color,omitempty"`
	Decision   string    `json:"decision"`
}

type ManifestPage struct {
	Items            []PageItem `json:"items"`
	NextCursor       string     `json:"next_cursor,omitempty"`
	SelectionVersion int        `json:"selection_version"`
}

type SelectionItem struct {
	AssetID  uuid.UUID `json:"asset_id"`
	Decision string    `json:"decision"`
}

type SelectionInput struct {
	ExpectedVersion int
	Items           []SelectionItem
}

type SelectionResult struct {
	ProjectID        uuid.UUID       `json:"project_id"`
	SelectionVersion int             `json:"selection_version"`
	Counts           SelectionCounts `json:"counts"`
}

func hashShareToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession exchanges a share token for a short-lived session. The token
// hash is resolved without tenant context, then everything else runs inside
// the tenant so RLS still applies.
func (s *CustomerService) CreateSession(ctx context.Context, rawToken, ip string) (*SessionView, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" || len(rawToken) > 200 {
		return nil, ErrInvalidShareToken
	}
	var (
		tokenID   uuid.UUID
		tenantID  uuid.UUID
		galleryID uuid.UUID
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT token_id, tenant_id, gallery_id FROM gallery_token_lookup($1)`,
		hashShareToken(rawToken)).Scan(&tokenID, &tenantID, &galleryID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidShareToken
		}
		return nil, fmt.Errorf("lookup share token: %w", err)
	}

	type granted struct {
		album     AlbumView
		expiresAt time.Time
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*granted, error) {
		now := time.Now()
		var (
			visits    int
			maxVisits int
			expiresAt *time.Time
			revokedAt *time.Time
		)
		if err := tx.QueryRow(ctx, `
			SELECT visits, max_visits, expires_at, revoked_at
			FROM gallery_tokens
			WHERE tenant_id = $1 AND id = $2
			FOR UPDATE`, tenantID, tokenID).Scan(&visits, &maxVisits, &expiresAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("load share token: %w", err)
		}
		switch {
		case revokedAt != nil:
			return nil, ErrShareRevoked
		case expiresAt != nil && !expiresAt.After(now):
			return nil, ErrShareExpired
		case maxVisits > 0 && visits >= maxVisits:
			return nil, ErrTooManyVisits
		}
		if _, err := tx.Exec(ctx, `
			UPDATE gallery_tokens SET visits = visits + 1, last_used_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, tokenID); err != nil {
			return nil, fmt.Errorf("record token visit: %w", err)
		}
		g, err := loadGallery(ctx, tx, tenantID, galleryID, false)
		if err != nil {
			return nil, err
		}
		if err := ensureCustomerGalleryProject(ctx, tx, tenantID, g); err != nil {
			return nil, err
		}
		if err := checkAlbumOpen(g, now); err != nil {
			return nil, err
		}
		if err := ensureBalancePaid(ctx, tx, tenantID, g.ProjectID); err != nil {
			return nil, err
		}
		counts, err := loadCounts(ctx, tx, tenantID, galleryID)
		if err != nil {
			return nil, err
		}
		album := albumView(g, counts)
		if err := loadPendingSelectionPayment(ctx, tx, tenantID, galleryID, &album); err != nil {
			return nil, err
		}
		deadline := now.Add(s.cfg.SessionTTL)
		if expiresAt != nil && expiresAt.Before(deadline) {
			deadline = *expiresAt
		}
		if g.ExpiresAt != nil && g.ExpiresAt.Before(deadline) {
			deadline = *g.ExpiresAt
		}
		return &granted{album: album, expiresAt: deadline}, nil
	})
	if err != nil {
		return nil, err
	}
	ttl := time.Until(res.expiresAt)
	if ttl <= 0 {
		return nil, ErrGalleryExpired
	}
	session, expiresAt, err := s.tokens.Issue(tenantID, galleryID, tokenID, ttl)
	if err != nil {
		return nil, err
	}
	return &SessionView{Session: session, ExpiresAt: expiresAt, Album: res.album}, nil
}

func (s *CustomerService) RecordCapture(ctx context.Context, rawToken, event, ip string) error {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" || len(rawToken) > 200 || (event != "screen_capture" && event != "screen_recording_start") {
		return ErrInvalidShareToken
	}
	var tokenID, tenantID, galleryID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT token_id, tenant_id, gallery_id FROM gallery_token_lookup($1)`, hashShareToken(rawToken)).
		Scan(&tokenID, &tenantID, &galleryID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidShareToken
		}
		return fmt.Errorf("lookup capture share token: %w", err)
	}
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM gallery_tokens t JOIN galleries g ON g.tenant_id = t.tenant_id AND g.id = t.gallery_id
			WHERE t.tenant_id = $1 AND t.id = $2 AND t.gallery_id = $3
			  AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now())
			  AND g.status = 'published' AND g.revoked_at IS NULL AND (g.expires_at IS NULL OR g.expires_at > now()))`,
			tenantID, tokenID, galleryID).Scan(&valid); err != nil {
			return fmt.Errorf("validate capture token: %w", err)
		}
		if !valid {
			return ErrShareRevoked
		}
		return audit.Write(ctx, tx, tenantID, nil, "gallery.capture_detected", "gallery", galleryID.String(), ip,
			map[string]any{"token_id": tokenID, "event": event, "source": "wechat_miniprogram"})
	})
}

// Album refreshes the customer's view of the gallery after they reload.
func (s *CustomerService) Album(ctx context.Context, gp *auth.GalleryPrincipal) (*AlbumView, error) {
	var out *AlbumView
	err := db.WithTenant(ctx, s.pool, gp.TenantID, func(tx pgx.Tx) error {
		g, err := loadGallery(ctx, tx, gp.TenantID, gp.GalleryID, true)
		if err != nil {
			return err
		}
		if err := ensureCustomerGalleryProject(ctx, tx, gp.TenantID, g); err != nil {
			return err
		}
		if err := checkAlbumOpen(g, time.Now()); err != nil {
			return err
		}
		if err := ensureBalancePaid(ctx, tx, gp.TenantID, g.ProjectID); err != nil {
			return err
		}
		if g.SubmittedAt != nil && g.SelectionEditUntil != nil && !g.SelectionEditUntil.After(time.Now()) && g.SelectionLockedAt == nil {
			locked, err := scanGallery(tx.QueryRow(ctx, `UPDATE galleries SET selection_locked_at=now(), version=version+1, updated_at=now() WHERE tenant_id=$1 AND id=$2 RETURNING `+columns, gp.TenantID, gp.GalleryID))
			if err != nil {
				return err
			}
			g = locked
		}
		counts, err := loadCounts(ctx, tx, gp.TenantID, gp.GalleryID)
		if err != nil {
			return err
		}
		album := albumView(g, counts)
		if err := loadPendingSelectionPayment(ctx, tx, gp.TenantID, g.ID, &album); err != nil {
			return err
		}
		album.RefundTasks, err = pendingSelectionRefunds(ctx, tx, gp.TenantID, g, counts.Selected, time.Now())
		if err != nil {
			return err
		}
		out = &album
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Page serves one slice of the immutable manifest with freshly signed URLs.
// Decisions come from the live table so a refresh never shows stale state.
func (s *CustomerService) Page(ctx context.Context, gp *auth.GalleryPrincipal, cursor string, limit int) (*ManifestPage, error) {
	if limit <= 0 {
		limit = 40
	}
	if limit > 60 {
		limit = 60
	}
	var (
		page         *ManifestPage
		selectionVer int
	)
	err := db.WithTenant(ctx, s.pool, gp.TenantID, func(tx pgx.Tx) error {
		g, err := loadGallery(ctx, tx, gp.TenantID, gp.GalleryID, false)
		if err != nil {
			return err
		}
		if err := ensureCustomerGalleryProject(ctx, tx, gp.TenantID, g); err != nil {
			return err
		}
		if err := checkAlbumOpen(g, time.Now()); err != nil {
			return err
		}
		if err := ensureBalancePaid(ctx, tx, gp.TenantID, g.ProjectID); err != nil {
			return err
		}
		manifest, err := s.manifests.Load(ctx, g.ManifestKey)
		if err != nil {
			return err
		}
		after := -1
		if cursor != "" {
			pos, err := decodeCursor(cursor)
			if err != nil {
				return fmt.Errorf("%w: malformed cursor", ErrInvalidInput)
			}
			after = pos
		}
		var selected []ManifestAsset
		for _, a := range manifest.Assets {
			if a.Position <= after {
				continue
			}
			selected = append(selected, a)
			if len(selected) == limit+1 {
				break
			}
		}
		next := ""
		if len(selected) > limit {
			selected = selected[:limit]
			next = encodeCursor(selected[len(selected)-1].Position)
		}
		decisions, err := loadDecisions(ctx, tx, gp.TenantID, gp.GalleryID, assetIDs(selected))
		if err != nil {
			return err
		}
		items := make([]PageItem, 0, len(selected))
		for _, a := range selected {
			thumbURL, err := s.store.PresignGet(ctx, a.ThumbKey, s.cfg.PresignTTL)
			if err != nil {
				return fmt.Errorf("sign thumb: %w", err)
			}
			displayURL, err := s.store.PresignGet(ctx, a.DisplayKey, s.cfg.PresignTTL)
			if err != nil {
				return fmt.Errorf("sign display: %w", err)
			}
			items = append(items, PageItem{
				AssetID:    a.AssetID,
				Filename:   a.Filename,
				Position:   a.Position,
				Width:      a.DisplayWidth,
				Height:     a.DisplayHeight,
				IsVideo:    a.IsVideo,
				ThumbURL:   thumbURL,
				DisplayURL: displayURL,
				LQIP:       a.LQIP,
				Color:      a.Color,
				Decision:   decisions[a.AssetID],
			})
		}
		selectionVer = g.SelectionVersion
		page = &ManifestPage{Items: items, NextCursor: next, SelectionVersion: selectionVer}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// ApplySelections upserts the customer's decisions under an optimistic version
// check. Exceeding the selection limit rolls the whole batch back.
func (s *CustomerService) ApplySelections(ctx context.Context, gp *auth.GalleryPrincipal, ip string, in SelectionInput) (*SelectionResult, error) {
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("%w: items is required", ErrInvalidInput)
	}
	if len(in.Items) > s.cfg.BatchLimit {
		return nil, fmt.Errorf("%w: at most %d items per request", ErrSelectionBatch, s.cfg.BatchLimit)
	}
	for _, item := range in.Items {
		if item.AssetID == uuid.Nil {
			return nil, fmt.Errorf("%w: asset_id is required", ErrInvalidInput)
		}
		switch item.Decision {
		case "selected", "rejected", "none":
		default:
			return nil, fmt.Errorf("%w: decision must be selected, rejected or none", ErrInvalidInput)
		}
	}

	return db.WithTenantResult(ctx, s.pool, gp.TenantID, func(tx pgx.Tx) (*SelectionResult, error) {
		g, err := loadGallery(ctx, tx, gp.TenantID, gp.GalleryID, true)
		if err != nil {
			return nil, err
		}
		if err := ensureCustomerGalleryProject(ctx, tx, gp.TenantID, g); err != nil {
			return nil, err
		}
		if err := checkAlbumOpen(g, time.Now()); err != nil {
			return nil, err
		}
		if err := ensureBalancePaid(ctx, tx, gp.TenantID, g.ProjectID); err != nil {
			return nil, err
		}
		if g.SubmittedAt != nil {
			if g.SelectionEditUntil == nil || !g.SelectionEditUntil.After(time.Now()) {
				if g.SelectionLockedAt == nil {
					if _, err := tx.Exec(ctx, `UPDATE galleries SET selection_locked_at=now(), version=version+1 WHERE tenant_id=$1 AND id=$2 AND selection_locked_at IS NULL`, gp.TenantID, gp.GalleryID); err != nil {
						return nil, err
					}
					g.SelectionLockedAt = ptrTime(time.Now())
				}
				for _, item := range in.Items {
					if item.Decision != "selected" {
						return nil, ErrAlreadySubmitted
					}
					var selected bool
					if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM gallery_selections WHERE tenant_id=$1 AND gallery_id=$2 AND asset_id=$3 AND decision='selected')`, gp.TenantID, gp.GalleryID, item.AssetID).Scan(&selected); err != nil {
						return nil, err
					}
					if selected {
						return nil, ErrAlreadySubmitted
					}
				}
			}
		}
		if in.ExpectedVersion != g.SelectionVersion {
			return nil, &VersionConflictError{Current: g.SelectionVersion}
		}

		ids := make([]uuid.UUID, 0, len(in.Items))
		seen := make(map[uuid.UUID]struct{}, len(in.Items))
		for _, item := range in.Items {
			if _, ok := seen[item.AssetID]; ok {
				continue
			}
			seen[item.AssetID] = struct{}{}
			ids = append(ids, item.AssetID)
		}
		var valid int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM gallery_assets
			WHERE tenant_id = $1 AND gallery_id = $2 AND asset_id = ANY($3)`,
			gp.TenantID, gp.GalleryID, ids).Scan(&valid); err != nil {
			return nil, fmt.Errorf("check gallery assets: %w", err)
		}
		if valid != len(ids) {
			return nil, ErrUnknownAsset
		}

		for _, item := range in.Items {
			switch item.Decision {
			case "none":
				if _, err := tx.Exec(ctx, `
					DELETE FROM gallery_selections
					WHERE tenant_id = $1 AND gallery_id = $2 AND asset_id = $3`,
					gp.TenantID, gp.GalleryID, item.AssetID); err != nil {
					return nil, fmt.Errorf("clear selection: %w", err)
				}
			default:
				if _, err := tx.Exec(ctx, `
					INSERT INTO gallery_selections (tenant_id, gallery_id, asset_id, decision)
					VALUES ($1,$2,$3,$4)
					ON CONFLICT (gallery_id, asset_id) DO UPDATE
					SET decision = EXCLUDED.decision, updated_at = now()`,
					gp.TenantID, gp.GalleryID, item.AssetID, item.Decision); err != nil {
					return nil, fmt.Errorf("store selection: %w", err)
				}
			}
		}

		counts, err := loadCounts(ctx, tx, gp.TenantID, gp.GalleryID)
		if err != nil {
			return nil, err
		}
		if g.SelectionLimit != nil && counts.Selected > *g.SelectionLimit {
			return nil, fmt.Errorf("%w: %d of %d", ErrSelectionLimit, counts.Selected, *g.SelectionLimit)
		}

		var selectionVersion int
		if err := tx.QueryRow(ctx, `
			UPDATE galleries
			SET selection_version = selection_version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING selection_version`, gp.TenantID, gp.GalleryID).Scan(&selectionVersion); err != nil {
			return nil, fmt.Errorf("bump selection version: %w", err)
		}
		if err := audit.Write(ctx, tx, gp.TenantID, nil, "gallery.selections.updated", "gallery",
			gp.GalleryID.String(), ip, map[string]any{
				"selection_version": selectionVersion,
				"items":             len(in.Items),
				"token_id":          gp.TokenID,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, gp.TenantID, "gallery", gp.GalleryID.String(), "gallery.selections.updated",
			map[string]any{"selection_version": selectionVersion, "selected": counts.Selected}); err != nil {
			return nil, err
		}
		return &SelectionResult{ProjectID: g.ProjectID, SelectionVersion: selectionVersion, Counts: counts}, nil
	})
}

// Submit freezes the decision set. Repeating a submit after success returns the
// same album, so a flaky network never produces a second submission event.
func (s *CustomerService) Submit(ctx context.Context, gp *auth.GalleryPrincipal, ip string, expectedVersion int) (*AlbumView, error) {
	return db.WithTenantResult(ctx, s.pool, gp.TenantID, func(tx pgx.Tx) (*AlbumView, error) {
		g, err := loadGallery(ctx, tx, gp.TenantID, gp.GalleryID, true)
		if err != nil {
			return nil, err
		}
		if err := ensureCustomerGalleryProject(ctx, tx, gp.TenantID, g); err != nil {
			return nil, err
		}
		if err := checkAlbumOpen(g, time.Now()); err != nil {
			return nil, err
		}
		if err := ensureBalancePaid(ctx, tx, gp.TenantID, g.ProjectID); err != nil {
			return nil, err
		}
		if g.SubmittedAt != nil {
			if g.SelectionEditUntil == nil || !g.SelectionEditUntil.After(time.Now()) {
				if _, err := tx.Exec(ctx, `UPDATE galleries SET selection_locked_at=now(), version=version+1, updated_at=now() WHERE tenant_id=$1 AND id=$2 AND selection_locked_at IS NULL`, gp.TenantID, gp.GalleryID); err != nil {
					return nil, err
				}
				g.SelectionLockedAt = ptrTime(time.Now())
			}
			if expectedVersion != g.SelectionVersion {
				return nil, &VersionConflictError{Current: g.SelectionVersion}
			}
			counts, err := loadCounts(ctx, tx, gp.TenantID, gp.GalleryID)
			if err != nil {
				return nil, err
			}
			batchID, amount, err := createSelectionChargeBatch(ctx, tx, gp.TenantID, g, counts.Selected)
			if err != nil {
				return nil, err
			}
			updated, err := scanGallery(tx.QueryRow(ctx, `UPDATE galleries SET submitted_at=now(), version=version+1, updated_at=now() WHERE tenant_id=$1 AND id=$2 RETURNING `+columns, gp.TenantID, gp.GalleryID))
			if err != nil {
				return nil, err
			}
			if err := enqueueSelectionRefund(ctx, tx, gp.TenantID, updated); err != nil {
				return nil, err
			}
			album := albumView(updated, counts)
			album.ChargeBatchID, album.ChargeAmountCents = batchID, amount
			album.RefundTasks, err = pendingSelectionRefunds(ctx, tx, gp.TenantID, updated, counts.Selected, time.Now())
			if err != nil {
				return nil, err
			}
			return &album, nil
		}
		if expectedVersion != g.SelectionVersion {
			return nil, &VersionConflictError{Current: g.SelectionVersion}
		}
		counts, err := loadCounts(ctx, tx, gp.TenantID, gp.GalleryID)
		if err != nil {
			return nil, err
		}
		if g.SelectionLimit != nil && counts.Selected > *g.SelectionLimit {
			return nil, fmt.Errorf("%w: %d of %d", ErrSelectionLimit, counts.Selected, *g.SelectionLimit)
		}
		batchID, amount, err := createSelectionChargeBatch(ctx, tx, gp.TenantID, g, counts.Selected)
		if err != nil {
			return nil, err
		}
		updated, err := scanGallery(tx.QueryRow(ctx, `
			UPDATE galleries
			SET submitted_at = now(), first_submitted_at = coalesce(first_submitted_at,now()),
				selection_edit_until = coalesce(selection_edit_until,now() + interval '2 hours'),
				selection_locked_at = CASE WHEN selection_edit_until IS NOT NULL AND selection_edit_until <= now() THEN coalesce(selection_locked_at,now()) ELSE selection_locked_at END,
				version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+columns, gp.TenantID, gp.GalleryID))
		if err != nil {
			return nil, fmt.Errorf("submit gallery: %w", err)
		}
		if err := enqueueSelectionRefund(ctx, tx, gp.TenantID, updated); err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(map[string]any{
			"gallery_id": gp.GalleryID, "project_id": updated.ProjectID, "selected": counts.Selected,
			"rejected": counts.Rejected, "workflow_stage": "retouch_delivery",
		})
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key, title, body, payload, due_at)
			VALUES ($1,'gallery_submitted','gallery',$2,$3,$4,$5,$6,now())
			ON CONFLICT DO NOTHING`,
			gp.TenantID, gp.GalleryID, "gallery_submitted:"+gp.GalleryID.String(),
			"客户已提交选片", fmt.Sprintf("《%s》已提交：选中 %d 张，弃选 %d 张。", updated.Title, counts.Selected, counts.Rejected),
			payload); err != nil {
			return nil, fmt.Errorf("queue gallery notification: %w", err)
		}
		if err := audit.Write(ctx, tx, gp.TenantID, nil, "gallery.submitted", "gallery",
			gp.GalleryID.String(), ip, map[string]any{
				"selected": counts.Selected, "rejected": counts.Rejected, "token_id": gp.TokenID,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, gp.TenantID, "gallery", gp.GalleryID.String(), "gallery.submitted",
			map[string]any{"selected": counts.Selected, "rejected": counts.Rejected}); err != nil {
			return nil, err
		}
		album := albumView(updated, counts)
		album.ChargeBatchID, album.ChargeAmountCents = batchID, amount
		return &album, nil
	})
}

func createSelectionChargeBatch(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, g *Gallery, selected int) (*uuid.UUID, int64, error) {
	if g.ExtraRetouchUnitPriceCents <= 0 || selected <= g.IncludedRetouchCount {
		return nil, 0, nil
	}
	var paidQuantity, pendingQuantity int
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(quantity) FILTER (WHERE status='paid'),0), coalesce(sum(quantity) FILTER (WHERE status='pending'),0)
		FROM selection_charge_batches WHERE tenant_id=$1 AND gallery_id=$2`, tenantID, g.ID).Scan(&paidQuantity, &pendingQuantity); err != nil {
		return nil, 0, err
	}
	desiredPending := max(0, selected-g.IncludedRetouchCount-paidQuantity)
	if pendingQuantity > 0 {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM selection_charge_batches WHERE tenant_id=$1 AND gallery_id=$2 AND status='pending' ORDER BY created_at LIMIT 1 FOR UPDATE`, tenantID, g.ID).Scan(&id); err != nil {
			return nil, 0, err
		}
		quantity := max(pendingQuantity, desiredPending)
		amount := int64(quantity) * g.ExtraRetouchUnitPriceCents
		if _, err := tx.Exec(ctx, `UPDATE selection_charge_batches SET quantity=$3, unit_price_cents=$4, amount_cents=$5, updated_at=now() WHERE tenant_id=$1 AND id=$2`, tenantID, id, quantity, g.ExtraRetouchUnitPriceCents, amount); err != nil {
			return nil, 0, err
		}
		return &id, amount, nil
	}
	if desiredPending <= 0 {
		return nil, 0, nil
	}
	amount := int64(desiredPending) * g.ExtraRetouchUnitPriceCents
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO selection_charge_batches (tenant_id,project_id,gallery_id,quantity,unit_price_cents,amount_cents) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, tenantID, g.ProjectID, g.ID, desiredPending, g.ExtraRetouchUnitPriceCents, amount).Scan(&id); err != nil {
		return nil, 0, err
	}
	return &id, amount, nil
}

const SelectionRefundJobKind = "gallery.selection_refund"

func enqueueSelectionRefund(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, g *Gallery) error {
	if g.SelectionEditUntil == nil || g.ExtraRetouchUnitPriceCents <= 0 {
		return nil
	}
	job, err := jobs.Enqueue(ctx, tx, tenantID, SelectionRefundJobKind, "gallery", &g.ID, map[string]any{"gallery_id": g.ID})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET run_after=$3, updated_at=now() WHERE tenant_id=$1 AND id=$2 AND status='queued'`, tenantID, job.ID, *g.SelectionEditUntil); err != nil {
		return fmt.Errorf("schedule selection refund: %w", err)
	}
	return nil
}

func DueSelectionRefunds(ctx context.Context, pool *pgxpool.Pool, tenantID, galleryID uuid.UUID) ([]SelectionRefundTask, error) {
	return db.WithTenantResult(ctx, pool, tenantID, func(tx pgx.Tx) ([]SelectionRefundTask, error) {
		g, err := loadGallery(ctx, tx, tenantID, galleryID, true)
		if err != nil {
			return nil, err
		}
		if g.SelectionEditUntil == nil || g.SelectionEditUntil.After(time.Now()) {
			return nil, nil
		}
		if g.SelectionLockedAt == nil {
			g, err = scanGallery(tx.QueryRow(ctx, `UPDATE galleries SET selection_locked_at=now(), version=version+1, updated_at=now() WHERE tenant_id=$1 AND id=$2 RETURNING `+columns, tenantID, galleryID))
			if err != nil {
				return nil, err
			}
		}
		counts, err := loadCounts(ctx, tx, tenantID, galleryID)
		if err != nil {
			return nil, err
		}
		return pendingSelectionRefunds(ctx, tx, tenantID, g, counts.Selected, time.Now())
	})
}

func pendingSelectionRefunds(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, g *Gallery, selected int, now time.Time) ([]SelectionRefundTask, error) {
	if g.SelectionEditUntil == nil || g.SelectionEditUntil.After(now) || g.ExtraRetouchUnitPriceCents <= 0 {
		return nil, nil
	}
	target := int64(max(0, selected-g.IncludedRetouchCount)) * g.ExtraRetouchUnitPriceCents
	type paidPayment struct {
		id               uuid.UUID
		amount, refunded int64
	}
	rows, err := tx.Query(ctx, `SELECT p.id,p.amount_cents,coalesce(sum(r.amount_cents) FILTER (WHERE r.status IN ('pending','succeeded')),0)
		FROM payments p JOIN payment_orders o ON o.id=p.order_id JOIN selection_charge_batches b ON b.id=o.selection_charge_batch_id
		LEFT JOIN refunds r ON r.tenant_id=p.tenant_id AND r.payment_id=p.id
		WHERE p.tenant_id=$1 AND b.gallery_id=$2 AND o.kind='selection_extra'
		GROUP BY p.id,p.amount_cents,p.paid_at ORDER BY p.paid_at DESC`, tenantID, g.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paid := []paidPayment{}
	var net int64
	for rows.Next() {
		var p paidPayment
		if err := rows.Scan(&p.id, &p.amount, &p.refunded); err != nil {
			return nil, err
		}
		paid = append(paid, p)
		net += p.amount - p.refunded
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	remaining := net - target
	if remaining <= 0 {
		return nil, nil
	}
	tasks := []SelectionRefundTask{}
	for _, p := range paid {
		available := p.amount - p.refunded
		if available <= 0 {
			continue
		}
		amount := min(available, remaining)
		tasks = append(tasks, SelectionRefundTask{PaymentID: p.id, AmountCents: amount})
		remaining -= amount
		if remaining == 0 {
			break
		}
	}
	return tasks, nil
}

func checkAlbumOpen(g *Gallery, now time.Time) error {
	switch g.Status {
	case StatusPublished:
	case StatusRevoked:
		return ErrGalleryRevoked
	case StatusExpired:
		return ErrGalleryExpired
	default:
		return ErrInvalidShareToken
	}
	if g.RevokedAt != nil {
		return ErrGalleryRevoked
	}
	if g.ExpiresAt != nil && !g.ExpiresAt.After(now) {
		return ErrGalleryExpired
	}
	return nil
}

func albumView(g *Gallery, counts SelectionCounts) AlbumView {
	return AlbumView{
		ID:                         g.ID,
		ProjectID:                  g.ProjectID,
		Title:                      g.Title,
		ExpiresAt:                  g.ExpiresAt,
		SelectionLimit:             g.SelectionLimit,
		DownloadAllowed:            g.DownloadAllowed,
		SubmittedAt:                g.SubmittedAt,
		SelectionVersion:           g.SelectionVersion,
		Counts:                     counts,
		FirstSubmittedAt:           g.FirstSubmittedAt,
		SelectionEditUntil:         g.SelectionEditUntil,
		SelectionLockedAt:          g.SelectionLockedAt,
		IncludedRetouchCount:       g.IncludedRetouchCount,
		ExtraRetouchUnitPriceCents: g.ExtraRetouchUnitPriceCents,
	}
}

func loadPendingSelectionPayment(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID, album *AlbumView) error {
	if album == nil {
		return nil
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT o.id,o.pay_url FROM payment_orders o
		JOIN selection_charge_batches b ON b.tenant_id=o.tenant_id AND b.id=o.selection_charge_batch_id
		WHERE o.tenant_id=$1 AND b.gallery_id=$2 AND o.kind='selection_extra' AND o.status='pending'
		ORDER BY o.created_at DESC LIMIT 1`, tenantID, galleryID).Scan(&id, &album.PaymentURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err == nil {
		album.PaymentOrderID = &id
	}
	return err
}

func ptrTime(value time.Time) *time.Time { return &value }

func loadCounts(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID) (SelectionCounts, error) {
	var c SelectionCounts
	if err := tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM gallery_assets WHERE tenant_id = $1 AND gallery_id = $2),
			(SELECT count(*) FROM gallery_selections WHERE tenant_id = $1 AND gallery_id = $2 AND decision = 'selected'),
			(SELECT count(*) FROM gallery_selections WHERE tenant_id = $1 AND gallery_id = $2 AND decision = 'rejected')`,
		tenantID, galleryID).Scan(&c.Assets, &c.Selected, &c.Rejected); err != nil {
		return c, fmt.Errorf("gallery counts: %w", err)
	}
	c.Undecided = c.Assets - c.Selected - c.Rejected
	if c.Undecided < 0 {
		c.Undecided = 0
	}
	return c, nil
}

func loadDecisions(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT asset_id, decision FROM gallery_selections
		WHERE tenant_id = $1 AND gallery_id = $2 AND asset_id = ANY($3)`,
		tenantID, galleryID, ids)
	if err != nil {
		return nil, fmt.Errorf("load decisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var decision string
		if err := rows.Scan(&id, &decision); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		out[id] = decision
	}
	return out, rows.Err()
}

func assetIDs(assets []ManifestAsset) []uuid.UUID {
	ids := make([]uuid.UUID, len(assets))
	for i, a := range assets {
		ids[i] = a.AssetID
	}
	return ids
}

func encodeCursor(position int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(position)))
}

func decodeCursor(cursor string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(raw))
}
