package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/storage"
)

type Item struct {
	ID               uuid.UUID  `json:"id"`
	Code             string     `json:"code"`
	Title            string     `json:"title"`
	ClientID         *uuid.UUID `json:"client_id,omitempty"`
	ClientName       string     `json:"client_name"`
	ProjectState     string     `json:"project_state"`
	Stage            Stage      `json:"workflow_stage"`
	StageLabel       string     `json:"workflow_stage_label"`
	PaymentStatus    string     `json:"payment_status"`
	Currency         string     `json:"currency"`
	AmountCents      int64      `json:"amount_cents"`
	ShootAt          *time.Time `json:"shoot_at,omitempty"`
	OrderAt          time.Time  `json:"order_at"`
	CreatedAt        time.Time  `json:"created_at"`
	SelectionDueAt   *time.Time `json:"selection_due_at,omitempty"`
	ProofDueAt       *time.Time `json:"proof_due_at,omitempty"`
	CoverURL         string     `json:"cover_url,omitempty"`
	GalleryID        *uuid.UUID `json:"gallery_id,omitempty"`
	ProofRoundID     *uuid.UUID `json:"proof_round_id,omitempty"`
	DeliveryID       *uuid.UUID `json:"delivery_id,omitempty"`
	DeliveryStatus   string     `json:"delivery_status,omitempty"`
	GallerySubmitted bool       `json:"gallery_submitted"`
}

type Service struct {
	pool     *pgxpool.Pool
	store    storage.Storage
	log      *slog.Logger
	timezone *time.Location
}

func NewService(pool *pgxpool.Pool, store storage.Storage, log *slog.Logger, timezones ...*time.Location) *Service {
	tz := time.UTC
	if len(timezones) > 0 && timezones[0] != nil {
		tz = timezones[0]
	}
	return &Service{pool: pool, store: store, log: log, timezone: tz}
}

func dateHasPassed(value *time.Time, now time.Time, timezone *time.Location) bool {
	if value == nil {
		return false
	}
	if timezone == nil {
		timezone = time.UTC
	}
	shoot := value.In(timezone)
	today := now.In(timezone)
	year, month, day := shoot.Date()
	todayYear, todayMonth, todayDay := today.Date()
	if year != todayYear {
		return year < todayYear
	}
	if month != todayMonth {
		return month < todayMonth
	}
	return day < todayDay
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]Item, error) {
	items := make([]Item, 0)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.id, p.code, p.title, p.client_id, coalesce(c.name,''),
			       CASE WHEN p.state NOT IN ('cancelled','archived')
			         AND EXISTS (SELECT 1 FROM bookings cb WHERE cb.tenant_id = p.tenant_id AND cb.project_id = p.id AND cb.status = 'cancelled')
			         AND NOT EXISTS (SELECT 1 FROM bookings ab WHERE ab.tenant_id = p.tenant_id AND ab.project_id = p.id AND ab.status IN ('held','confirmed'))
			         THEN 'cancelled' ELSE p.state END,
			       p.payment_status,
			       p.currency, p.total_amount_cents + p.change_amount_cents, p.created_at,
			       coalesce(b.starts_at, p.shoot_start_at), coalesce(b.created_at, p.created_at),
			       coalesce(g.expires_at, p.selection_deadline::timestamptz), g.id, coalesce(g.status,''), coalesce(g.selection_count,0), g.submitted_at,
			       pr.id, coalesce(pr.status,''), pr.due_at, d.id, coalesce(d.status,''),
			       coalesce(der.object_key,'')
			FROM projects p
			LEFT JOIN clients c ON c.tenant_id = p.tenant_id AND c.id = p.client_id
			LEFT JOIN LATERAL (
				SELECT starts_at, created_at FROM bookings b
				WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id
				ORDER BY CASE WHEN b.status IN ('held','confirmed') THEN 0 ELSE 1 END, b.created_at DESC LIMIT 1
			) b ON true
			LEFT JOIN LATERAL (
				SELECT g.id, g.status, g.submitted_at, g.expires_at,
				       (SELECT count(*) FROM gallery_selections gs WHERE gs.tenant_id = g.tenant_id AND gs.gallery_id = g.id) AS selection_count
				FROM galleries g WHERE g.tenant_id = p.tenant_id AND g.project_id = p.id
				ORDER BY CASE WHEN g.status = 'published' THEN 0 ELSE 1 END, g.created_at DESC LIMIT 1
			) g ON true
			LEFT JOIN LATERAL (
				SELECT id, status, due_at FROM proof_rounds pr
				WHERE pr.tenant_id = p.tenant_id AND pr.project_id = p.id
				ORDER BY pr.created_at DESC LIMIT 1
			) pr ON true
			LEFT JOIN LATERAL (
				SELECT id, status FROM deliveries d WHERE d.tenant_id = p.tenant_id AND d.project_id = p.id
				ORDER BY d.created_at DESC LIMIT 1
			) d ON true
			LEFT JOIN LATERAL (
				SELECT ad.object_key FROM gallery_assets ga
				JOIN assets a ON a.tenant_id = ga.tenant_id AND a.id = ga.asset_id
				JOIN asset_derivatives ad ON ad.tenant_id = ga.tenant_id AND ad.asset_id = ga.asset_id
				WHERE ga.tenant_id = p.tenant_id AND ga.gallery_id = g.id
				  AND ad.kind = 'thumb' AND ad.is_watermarked = false
			ORDER BY ga.position LIMIT 1
			) der ON true
			WHERE p.tenant_id = $1
			ORDER BY coalesce(b.starts_at,p.shoot_start_at) DESC NULLS LAST, p.created_at DESC`, tenantID)
		if err != nil {
			return fmt.Errorf("list workflow projects: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var item Item
			var state, paymentStatus, galleryStatus, proofStatus, deliveryStatus string
			var selected int
			var submitted *time.Time
			var coverKey string
			if err := rows.Scan(&item.ID, &item.Code, &item.Title, &item.ClientID, &item.ClientName,
				&state, &paymentStatus, &item.Currency, &item.AmountCents, &item.CreatedAt,
				&item.ShootAt, &item.OrderAt, &item.SelectionDueAt, &item.GalleryID, &galleryStatus,
				&selected, &submitted, &item.ProofRoundID, &proofStatus, &item.ProofDueAt,
				&item.DeliveryID, &deliveryStatus, &coverKey); err != nil {
				return fmt.Errorf("scan workflow project: %w", err)
			}
			item.ProjectState = state
			item.PaymentStatus = paymentStatus
			item.DeliveryStatus = deliveryStatus
			item.GallerySubmitted = submitted != nil
			item.Stage = DeriveStage(Milestones{ProjectState: state, PaymentStatus: paymentStatus,
				ShootDatePassed: dateHasPassed(item.ShootAt, time.Now(), s.timezone),
				GalleryStatus:   galleryStatus, SelectedCount: selected, Submitted: submitted != nil,
				ProofStatus: proofStatus, DeliveryStatus: deliveryStatus})
			item.StageLabel = StageLabel(item.Stage)
			if coverKey != "" && s.store != nil {
				url, err := s.store.PresignGet(ctx, coverKey, 5*time.Minute)
				if err == nil {
					item.CoverURL = url
				}
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}
