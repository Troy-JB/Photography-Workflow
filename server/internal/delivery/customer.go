package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/storage"
)

type CustomerConfig struct {
	SessionTTL time.Duration
	PresignTTL time.Duration
	PageLimit  int
}

// CustomerService serves the delivery link: session exchange, the file list
// and presigned download grants. It only ever sees the customer principal.
type CustomerService struct {
	pool   *pgxpool.Pool
	store  storage.Storage
	tokens *auth.DeliveryTokens
	cfg    CustomerConfig
	log    *slog.Logger
}

func NewCustomerService(pool *pgxpool.Pool, store storage.Storage, tokens *auth.DeliveryTokens,
	cfg CustomerConfig, log *slog.Logger) *CustomerService {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * time.Minute
	}
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = 5 * time.Minute
	}
	if cfg.PageLimit <= 0 {
		cfg.PageLimit = 40
	}
	return &CustomerService{pool: pool, store: store, tokens: tokens, cfg: cfg, log: log}
}

type CustomerPackage struct {
	ID         uuid.UUID `json:"id"`
	Spec       string    `json:"spec"`
	FileName   string    `json:"file_name,omitempty"`
	SizeBytes  int64     `json:"size_bytes"`
	EntryCount int       `json:"entry_count"`
}

type CustomerDelivery struct {
	ID                 uuid.UUID         `json:"id"`
	ProjectTitle       string            `json:"project_title"`
	RoundNo            int               `json:"round_no"`
	ItemCount          int               `json:"item_count"`
	DownloadsRemaining int               `json:"downloads_remaining"`
	ExpiresAt          time.Time         `json:"expires_at"`
	WebReady           int               `json:"web_ready"`
	WebSkipped         int               `json:"web_skipped"`
	Packages           []CustomerPackage `json:"packages"`
}

type SessionView struct {
	Session   string           `json:"session"`
	ExpiresAt time.Time        `json:"expires_at"`
	Delivery  CustomerDelivery `json:"delivery"`
}

type CustomerItem struct {
	ID           uuid.UUID `json:"id"`
	Position     int       `json:"position"`
	FileName     string    `json:"file_name"`
	OriginalSize int64     `json:"original_size"`
	RevisionNo   int       `json:"revision_no"`
	WebAvailable bool      `json:"web_available"`
	WebWidth     int       `json:"web_width,omitempty"`
	WebHeight    int       `json:"web_height,omitempty"`
	WebSize      int64     `json:"web_size,omitempty"`
	ThumbURL     string    `json:"thumb_url,omitempty"`
}

type CustomerItemsPage struct {
	Items  []CustomerItem `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

type DownloadInput struct {
	PackageID *uuid.UUID
	ItemID    *uuid.UUID
	Spec      string
}

type DownloadGrant struct {
	URL                string    `json:"url"`
	FileName           string    `json:"file_name"`
	ExpiresAt          time.Time `json:"expires_at"`
	DownloadsRemaining int       `json:"downloads_remaining"`
}

func hashDeliveryToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateSession exchanges a link token for a short-lived session. The token is
// looked up by hash before the tenant is known; the returned session is capped
// at the token expiry and never contains the plaintext token.
func (s *CustomerService) CreateSession(ctx context.Context, rawToken, ip string) (*SessionView, error) {
	rawToken = strings.TrimSpace(rawToken)
	if len(rawToken) < 20 || len(rawToken) > 200 {
		return nil, ErrTokenInvalid
	}
	var (
		tokenID    uuid.UUID
		tenantID   uuid.UUID
		deliveryID uuid.UUID
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT token_id, tenant_id, delivery_id FROM delivery_token_lookup($1)`,
		hashDeliveryToken(rawToken)).Scan(&tokenID, &tenantID, &deliveryID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenInvalid
		}
		return nil, fmt.Errorf("lookup delivery token: %w", err)
	}

	type granted struct {
		view      CustomerDelivery
		expiresAt time.Time
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*granted, error) {
		st, err := loadTokenState(ctx, tx, tenantID, tokenID, deliveryID, true)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		if err := checkTokenGate(st, now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE download_tokens SET last_used_at = now(), last_used_ip = $3
			WHERE tenant_id = $1 AND id = $2`, tenantID, tokenID, ip); err != nil {
			return nil, fmt.Errorf("record delivery visit: %w", err)
		}
		view, err := loadCustomerDelivery(ctx, tx, tenantID, st)
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "delivery.session.created", "delivery",
			deliveryID.String(), ip, map[string]any{"token_id": tokenID}); err != nil {
			return nil, err
		}
		deadline := now.Add(s.cfg.SessionTTL)
		if st.ExpiresAt.Before(deadline) {
			deadline = st.ExpiresAt
		}
		return &granted{view: view, expiresAt: deadline}, nil
	})
	if err != nil {
		return nil, err
	}
	ttl := time.Until(res.expiresAt)
	if ttl <= 0 {
		return nil, ErrTokenExpired
	}
	session, expiresAt, err := s.tokens.Issue(tenantID, deliveryID, tokenID, ttl)
	if err != nil {
		return nil, err
	}
	return &SessionView{Session: session, ExpiresAt: expiresAt, Delivery: res.view}, nil
}

// Summary refreshes the delivery header after a reload. The guard already
// re-checked the token, payment and round; this only loads display fields.
func (s *CustomerService) Summary(ctx context.Context, dp *auth.DeliveryPrincipal) (*CustomerDelivery, error) {
	var view *CustomerDelivery
	err := db.WithTenant(ctx, s.pool, dp.TenantID, func(tx pgx.Tx) error {
		st, err := loadTokenState(ctx, tx, dp.TenantID, dp.TokenID, dp.DeliveryID, false)
		if err != nil {
			return err
		}
		if err := checkTokenGate(st, time.Now()); err != nil {
			return err
		}
		loaded, err := loadCustomerDelivery(ctx, tx, dp.TenantID, st)
		if err != nil {
			return err
		}
		view = &loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

func loadCustomerDelivery(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, st *tokenState) (CustomerDelivery, error) {
	view := CustomerDelivery{
		ID:                 st.DeliveryID,
		ProjectTitle:       st.ProjectTitle,
		RoundNo:            st.RoundNo,
		ItemCount:          st.ItemCount,
		DownloadsRemaining: maxInt(st.MaxDownloads-st.Downloads, 0),
		ExpiresAt:          st.ExpiresAt,
		Packages:           []CustomerPackage{},
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE web_status = 'ready'),
		       count(*) FILTER (WHERE web_status = 'skipped')
		FROM delivery_items WHERE tenant_id = $1 AND delivery_id = $2`,
		tenantID, st.DeliveryID).Scan(&view.WebReady, &view.WebSkipped); err != nil {
		return view, fmt.Errorf("count customer web states: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, spec, file_name, size_bytes, entry_count
		FROM delivery_packages
		WHERE tenant_id = $1 AND delivery_id = $2 AND version = $3
		ORDER BY spec`, tenantID, st.DeliveryID, st.BuildVersion)
	if err != nil {
		return view, fmt.Errorf("load customer packages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p CustomerPackage
		if err := rows.Scan(&p.ID, &p.Spec, &p.FileName, &p.SizeBytes, &p.EntryCount); err != nil {
			return view, fmt.Errorf("scan customer package: %w", err)
		}
		view.Packages = append(view.Packages, p)
	}
	return view, rows.Err()
}

// Items pages the delivered files with short-lived thumbnail URLs. Original
// object keys never appear here; downloading requires an explicit grant.
func (s *CustomerService) Items(ctx context.Context, dp *auth.DeliveryPrincipal, limit, offset int) (*CustomerItemsPage, error) {
	if limit <= 0 {
		limit = s.cfg.PageLimit
	}
	if limit > 60 {
		limit = 60
	}
	page := &CustomerItemsPage{Items: []CustomerItem{}, Limit: limit, Offset: offset}
	err := db.WithTenant(ctx, s.pool, dp.TenantID, func(tx pgx.Tx) error {
		st, err := loadTokenState(ctx, tx, dp.TenantID, dp.TokenID, dp.DeliveryID, false)
		if err != nil {
			return err
		}
		if err := checkTokenGate(st, time.Now()); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM delivery_items WHERE tenant_id = $1 AND delivery_id = $2`,
			dp.TenantID, dp.DeliveryID).Scan(&page.Total); err != nil {
			return fmt.Errorf("count customer items: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT i.id, i.position, i.original_name, i.original_size, i.revision_no,
			       i.web_status, i.web_width, i.web_height, i.web_size,
			       coalesce(t.object_key, '')
			FROM delivery_items i
			LEFT JOIN asset_derivatives t
				ON t.tenant_id = i.tenant_id AND t.asset_id = i.asset_id
				AND t.kind = 'thumb' AND t.is_watermarked = false
			WHERE i.tenant_id = $1 AND i.delivery_id = $2
			ORDER BY i.position LIMIT $3 OFFSET $4`,
			dp.TenantID, dp.DeliveryID, limit, offset)
		if err != nil {
			return fmt.Errorf("list customer items: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				it        CustomerItem
				webStatus string
				thumbKey  string
			)
			if err := rows.Scan(&it.ID, &it.Position, &it.FileName, &it.OriginalSize, &it.RevisionNo,
				&webStatus, &it.WebWidth, &it.WebHeight, &it.WebSize, &thumbKey); err != nil {
				return fmt.Errorf("scan customer item: %w", err)
			}
			it.WebAvailable = webStatus == WebReady
			if thumbKey != "" {
				if it.ThumbURL, err = s.store.PresignGet(ctx, thumbKey, s.cfg.PresignTTL); err != nil {
					return fmt.Errorf("sign delivery thumb: %w", err)
				}
			}
			page.Items = append(page.Items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// Grant consumes one download and returns a presigned URL. The token row is
// locked so concurrent tabs cannot exceed max_downloads.
func (s *CustomerService) Grant(ctx context.Context, dp *auth.DeliveryPrincipal, ip string,
	in DownloadInput) (*DownloadGrant, error) {
	spec := strings.ToLower(strings.TrimSpace(in.Spec))
	if spec != "" && spec != SpecWeb && spec != SpecOriginal {
		return nil, fmt.Errorf("%w: spec must be web or original", ErrInvalidInput)
	}
	if in.PackageID == nil && in.ItemID == nil && spec == "" {
		return nil, fmt.Errorf("%w: package_id or item_id is required", ErrInvalidInput)
	}
	if in.ItemID != nil && spec == "" {
		return nil, fmt.Errorf("%w: spec is required for an item download", ErrInvalidInput)
	}

	grant, err := db.WithTenantResult(ctx, s.pool, dp.TenantID, func(tx pgx.Tx) (*DownloadGrant, error) {
		st, err := loadTokenState(ctx, tx, dp.TenantID, dp.TokenID, dp.DeliveryID, true)
		if err != nil {
			return nil, err
		}
		if err := checkTokenGate(st, time.Now()); err != nil {
			return nil, err
		}
		if st.Downloads >= st.MaxDownloads {
			return nil, ErrDownloadsExhausted
		}

		key, fileName, err := s.resolveDownload(ctx, tx, dp.TenantID, st, in, spec)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE download_tokens SET downloads = downloads + 1,
				last_used_at = now(), last_used_ip = $3
			WHERE tenant_id = $1 AND id = $2`, dp.TenantID, dp.TokenID, ip); err != nil {
			return nil, fmt.Errorf("record delivery download: %w", err)
		}
		if err := audit.Write(ctx, tx, dp.TenantID, nil, "delivery.download.granted", "delivery",
			dp.DeliveryID.String(), ip, map[string]any{
				"token_id": dp.TokenID, "package_id": in.PackageID, "item_id": in.ItemID,
				"spec": spec, "file_name": fileName,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, dp.TenantID, "delivery", dp.DeliveryID.String(), "delivery.downloaded",
			map[string]any{"token_id": dp.TokenID, "spec": spec}); err != nil {
			return nil, err
		}
		url, err := s.presign(ctx, key, fileName)
		if err != nil {
			return nil, err
		}
		return &DownloadGrant{
			URL:                url,
			FileName:           fileName,
			ExpiresAt:          time.Now().Add(s.cfg.PresignTTL),
			DownloadsRemaining: st.MaxDownloads - st.Downloads - 1,
		}, nil
	})
	return grant, err
}

// resolveDownload picks the object for a grant. Every lookup is scoped to the
// session's delivery, so a package or item id from another delivery is a 404.
func (s *CustomerService) resolveDownload(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	st *tokenState, in DownloadInput, spec string) (string, string, error) {
	switch {
	case in.PackageID != nil:
		var (
			objectKey string
			fileName  string
			status    string
			rowSpec   string
		)
		err := tx.QueryRow(ctx, `
			SELECT object_key, file_name, status, spec FROM delivery_packages
			WHERE tenant_id = $1 AND delivery_id = $2 AND version = $3 AND id = $4`,
			tenantID, st.DeliveryID, st.BuildVersion, *in.PackageID).
			Scan(&objectKey, &fileName, &status, &rowSpec)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrPackageNotFound
		}
		if err != nil {
			return "", "", fmt.Errorf("load delivery package: %w", err)
		}
		if status != PackageReady || objectKey == "" {
			return "", "", ErrDeliveryNotReady
		}
		return objectKey, fileName, nil

	case in.ItemID != nil:
		var (
			originalKey  string
			originalName string
			webKey       string
			webStatus    string
		)
		err := tx.QueryRow(ctx, `
			SELECT original_object_key, original_name, web_object_key, web_status
			FROM delivery_items
			WHERE tenant_id = $1 AND delivery_id = $2 AND id = $3`,
			tenantID, st.DeliveryID, *in.ItemID).
			Scan(&originalKey, &originalName, &webKey, &webStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrItemNotFound
		}
		if err != nil {
			return "", "", fmt.Errorf("load delivery item: %w", err)
		}
		if spec == SpecWeb {
			if webStatus != WebReady || webKey == "" {
				return "", "", ErrWebSpecUnavailable
			}
			return webKey, WebFileName(originalName), nil
		}
		return originalKey, originalName, nil
	}

	// Spec-only: the customer UI's two package cards download the current ZIP
	// without needing its id first.
	var (
		objectKey string
		fileName  string
		status    string
	)
	err := tx.QueryRow(ctx, `
		SELECT object_key, file_name, status FROM delivery_packages
		WHERE tenant_id = $1 AND delivery_id = $2 AND version = $3 AND spec = $4`,
		tenantID, st.DeliveryID, st.BuildVersion, spec).Scan(&objectKey, &fileName, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrPackageNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("load delivery package by spec: %w", err)
	}
	if status != PackageReady || objectKey == "" {
		return "", "", ErrDeliveryNotReady
	}
	return objectKey, fileName, nil
}

func (s *CustomerService) presign(ctx context.Context, key, fileName string) (string, error) {
	if named, ok := s.store.(storage.NamedPresigner); ok && fileName != "" {
		return named.PresignGetNamed(ctx, key, fileName, s.cfg.PresignTTL)
	}
	return s.store.PresignGet(ctx, key, s.cfg.PresignTTL)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
