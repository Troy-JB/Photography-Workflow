package notice

import (
	"context"
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
)

// JobKind is the jobs.kind the worker executes for reminder delivery.
const JobKind = "notice.deliver"

// SubjectType groups reminder deliveries under the jobs queue.
const SubjectType = "notification"

const maxAttempts = 5

const (
	TypeShootReminder    = "shoot_reminder"
	TypeHoldExpiry       = "hold_expiry"
	TypeAuthIncomplete   = "auth_incomplete"
	TypeGalleryDeadline  = "gallery_deadline"
	TypeGallerySubmitted = "gallery_submitted"
	TypeProofReady       = "proof_ready"
	TypeProofChanges     = "proof_changes_requested"
	TypeProofResubmitted = "proof_resubmitted"
	TypeProofApproved    = "proof_approved"
	TypeProofDeadline    = "proof_deadline"
	TypeRetouchDelivered = "retouch_delivered"
	TypeDeliveryReady    = "delivery_ready"
	TypeArchiveReady     = "archive_ready"
	TypeArchiveWarning   = "archive_retention_warning"
	TypeArchiveDeleted   = "archive_deleted"
	TypeWorkflowEvent    = "workflow_event"
)

const (
	StatusPending   = "pending"
	StatusSent      = "sent"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
	StatusDead      = "dead"
	StatusCancelled = "cancelled"
)

var (
	ErrNotFound     = errors.New("notification not found")
	ErrInvalidInput = errors.New("invalid notification input")
)

// Config controls reminder planning and delivery. Planning still runs when the
// webhook is unconfigured; rows are then skipped at delivery time so they stay
// readable in the app.
type Config struct {
	Enabled        bool
	ShootLeads     []time.Duration
	AuthLead       time.Duration
	HoldLead       time.Duration
	Timezone       *time.Location
	WebhookURL     string
	WebhookSecret  string
	WebhookTimeout time.Duration
}

type Service struct {
	pool *pgxpool.Pool
	cfg  Config
	log  *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg Config, log *slog.Logger) *Service {
	if cfg.Timezone == nil {
		cfg.Timezone = time.UTC
	}
	if cfg.WebhookTimeout <= 0 {
		cfg.WebhookTimeout = 10 * time.Second
	}
	cfg.WebhookURL = strings.TrimSpace(cfg.WebhookURL)
	cfg.WebhookSecret = strings.TrimSpace(cfg.WebhookSecret)
	leads := make([]time.Duration, 0, len(cfg.ShootLeads))
	for _, lead := range cfg.ShootLeads {
		if lead > 0 {
			leads = append(leads, lead)
		}
	}
	cfg.ShootLeads = leads
	return &Service{pool: pool, cfg: cfg, log: log}
}

func (s *Service) Config() Config { return s.cfg }

func (s *Service) WebhookEnabled() bool {
	return s.cfg.Enabled && s.cfg.WebhookURL != ""
}

const notificationColumns = `id, tenant_id, type, subject_type, subject_id, dedupe_key,
	title, body, payload, status, due_at, sent_at, read_at, attempts, last_error,
	created_at, updated_at`

type Notification struct {
	ID          uuid.UUID       `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	Type        string          `json:"type"`
	SubjectType string          `json:"subject_type"`
	SubjectID   uuid.UUID       `json:"subject_id"`
	DedupeKey   string          `json:"dedupe_key"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	DueAt       time.Time       `json:"due_at"`
	SentAt      *time.Time      `json:"sent_at,omitempty"`
	ReadAt      *time.Time      `json:"read_at,omitempty"`
	Attempts    int             `json:"attempts"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Deliveries  []Delivery      `json:"deliveries,omitempty"`
}

type Delivery struct {
	ID             uuid.UUID  `json:"id"`
	NotificationID uuid.UUID  `json:"notification_id"`
	Channel        string     `json:"channel"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	LastError      string     `json:"last_error,omitempty"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func scanNotification(row interface{ Scan(...any) error }) (*Notification, error) {
	var n Notification
	if err := row.Scan(&n.ID, &n.TenantID, &n.Type, &n.SubjectType, &n.SubjectID,
		&n.DedupeKey, &n.Title, &n.Body, &n.Payload, &n.Status, &n.DueAt, &n.SentAt,
		&n.ReadAt, &n.Attempts, &n.LastError, &n.CreatedAt, &n.UpdatedAt); err != nil {
		return nil, err
	}
	return &n, nil
}

func ValidType(v string) bool {
	switch v {
	case TypeShootReminder, TypeHoldExpiry, TypeAuthIncomplete, TypeGalleryDeadline, TypeGallerySubmitted,
		TypeProofReady, TypeProofChanges, TypeProofResubmitted, TypeProofApproved, TypeProofDeadline,
		TypeRetouchDelivered, TypeDeliveryReady, TypeArchiveReady, TypeArchiveWarning, TypeArchiveDeleted,
		TypeWorkflowEvent:
		return true
	}
	return false
}

func EmitWorkflowEvent(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID, eventKey, title, body string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal workflow notification: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key, title, body, payload, due_at)
		VALUES ($1,$2,'project',$3,$4,$5,$6,$7,now()) ON CONFLICT DO NOTHING`,
		tenantID, TypeWorkflowEvent, projectID, eventKey, title, body, data)
	if err != nil {
		return fmt.Errorf("insert workflow notification: %w", err)
	}
	return nil
}

func (s *Service) ListAfter(ctx context.Context, tenantID uuid.UUID, after time.Time, afterID uuid.UUID) ([]Notification, error) {
	items := make([]Notification, 0)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+notificationColumns+` FROM notifications
			WHERE tenant_id = $1 AND (created_at, id) > ($2, $3)
			ORDER BY created_at, id LIMIT 100`, tenantID, after, afterID)
		if err != nil {
			return fmt.Errorf("stream notifications: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			n, err := scanNotification(rows)
			if err != nil {
				return fmt.Errorf("scan streamed notification: %w", err)
			}
			items = append(items, *n)
		}
		return rows.Err()
	})
	return items, err
}

func ValidStatus(v string) bool {
	switch v {
	case StatusPending, StatusSent, StatusFailed, StatusSkipped, StatusDead, StatusCancelled:
		return true
	}
	return false
}

type Filter struct {
	Type      string
	Status    string
	BookingID *uuid.UUID
	Unread    bool
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, filter Filter, limit, offset int) ([]Notification, int, int, error) {
	if filter.Type != "" && !ValidType(filter.Type) {
		return nil, 0, 0, fmt.Errorf("%w: unknown notification type %q", ErrInvalidInput, filter.Type)
	}
	if filter.Status != "" && !ValidStatus(filter.Status) {
		return nil, 0, 0, fmt.Errorf("%w: unknown notification status %q", ErrInvalidInput, filter.Status)
	}
	var (
		items  []Notification
		total  int
		unread int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const where = `tenant_id = $1 AND ($2::text = '' OR type = $2)
			AND ($3::text = '' OR status = $3) AND ($4::uuid IS NULL OR subject_id = $4)
			AND (NOT $5::boolean OR read_at IS NULL)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE `+where,
			tenantID, filter.Type, filter.Status, filter.BookingID, filter.Unread).Scan(&total); err != nil {
			return fmt.Errorf("count notifications: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM notifications
			WHERE tenant_id = $1 AND read_at IS NULL`, tenantID).Scan(&unread); err != nil {
			return fmt.Errorf("count unread notifications: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE `+where+`
			ORDER BY created_at DESC, id LIMIT $6 OFFSET $7`,
			tenantID, filter.Type, filter.Status, filter.BookingID, filter.Unread, limit, offset)
		if err != nil {
			return fmt.Errorf("list notifications: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			n, err := scanNotification(rows)
			if err != nil {
				return fmt.Errorf("scan notification: %w", err)
			}
			items = append(items, *n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, 0, err
	}
	if items == nil {
		items = []Notification{}
	}
	return items, total, unread, nil
}

func (s *Service) Get(ctx context.Context, tenantID, notificationID uuid.UUID) (*Notification, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Notification, error) {
		n, err := scanNotification(tx.QueryRow(ctx, `
			SELECT `+notificationColumns+` FROM notifications WHERE id = $1 AND tenant_id = $2`,
			notificationID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get notification: %w", err)
		}
		deliveries, err := loadDeliveries(ctx, tx, tenantID, notificationID)
		if err != nil {
			return nil, err
		}
		n.Deliveries = deliveries
		return n, nil
	})
}

func (s *Service) MarkRead(ctx context.Context, tenantID, actor, notificationID uuid.UUID, ip string) (*Notification, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Notification, error) {
		n, err := scanNotification(tx.QueryRow(ctx, `
			UPDATE notifications SET read_at = COALESCE(read_at, now()), updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+notificationColumns, notificationID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("mark notification read: %w", err)
		}
		n.Deliveries, err = loadDeliveries(ctx, tx, tenantID, notificationID)
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "notification.read",
			"notification", notificationID.String(), ip, map[string]any{"type": n.Type}); err != nil {
			return nil, err
		}
		return n, nil
	})
}

func loadDeliveries(ctx context.Context, tx pgx.Tx, tenantID, notificationID uuid.UUID) ([]Delivery, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, notification_id, channel, status, attempts, last_error, delivered_at,
			created_at, updated_at
		FROM notification_deliveries
		WHERE tenant_id = $1 AND notification_id = $2
		ORDER BY created_at`, tenantID, notificationID)
	if err != nil {
		return nil, fmt.Errorf("list notification deliveries: %w", err)
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.NotificationID, &d.Channel, &d.Status, &d.Attempts,
			&d.LastError, &d.DeliveredAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan notification delivery: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LoadForDelivery reads one notification outside a tenant session. The worker
// role holds a permissive policy on notifications for this purpose.
func (s *Service) LoadForDelivery(ctx context.Context, tenantID, notificationID uuid.UUID) (*Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `
		SELECT `+notificationColumns+` FROM notifications WHERE id = $1 AND tenant_id = $2`,
		notificationID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load notification: %w", err)
	}
	return n, nil
}

// DispatchDue enqueues one delivery job per due pending reminder. The jobs
// unique index keeps concurrent workers from enqueueing the same notification.
func (s *Service) DispatchDue(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	if s == nil || !s.cfg.Enabled {
		return 0, nil
	}
	if limit <= 0 {
		limit = 100
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO jobs (tenant_id, kind, subject_type, subject_id, payload, max_attempts)
		SELECT n.tenant_id, $1, $2, n.id, '{}'::jsonb, $3
		FROM notifications n
		WHERE n.status = 'pending' AND n.due_at <= now()
			AND NOT EXISTS (
				SELECT 1 FROM jobs j
				WHERE j.tenant_id = n.tenant_id AND j.kind = $1
					AND j.subject_id = n.id AND j.status IN ('queued','running'))
		ORDER BY n.due_at
		LIMIT $4
		ON CONFLICT DO NOTHING`, JobKind, SubjectType, maxAttempts, limit)
	if err != nil {
		return 0, fmt.Errorf("dispatch due reminders: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DeliveryState re-checks the booking immediately before sending so an
// expired, cancelled or rescheduled reminder is skipped instead of delivered.
type DeliveryState struct {
	Found         bool
	BookingStatus string
	StartsAt      time.Time
	HoldExpiresAt *time.Time
	PendingAuths  int
}

func (s *Service) DeliveryState(ctx context.Context, tenantID uuid.UUID, n *Notification) (*DeliveryState, error) {
	if n.SubjectType != "booking" {
		return &DeliveryState{}, nil
	}
	state := &DeliveryState{}
	err := s.pool.QueryRow(ctx, `
		SELECT status, starts_at, hold_expires_at FROM bookings
		WHERE tenant_id = $1 AND id = $2`, tenantID, n.SubjectID).
		Scan(&state.BookingStatus, &state.StartsAt, &state.HoldExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load booking for reminder: %w", err)
	}
	state.Found = true
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM booking_authorizations
		WHERE tenant_id = $1 AND booking_id = $2 AND status = 'pending'`,
		tenantID, n.SubjectID).Scan(&state.PendingAuths); err != nil {
		return nil, fmt.Errorf("count pending authorizations: %w", err)
	}
	return state, nil
}

// RecordDelivery updates the delivery row and the notification in one
// transaction. A notification that is no longer pending (cancelled, dead) is
// left untouched and the delivery is recorded as skipped.
func (s *Service) RecordDelivery(ctx context.Context, tenantID, notificationID uuid.UUID, status, lastError string) error {
	return withWorkerTx(ctx, s.pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			UPDATE notifications SET status = $3, attempts = attempts + 1, last_error = $4,
				sent_at = CASE WHEN $3 = 'sent' THEN now() ELSE sent_at END, updated_at = now()
			WHERE tenant_id = $1 AND id = $2 AND status = 'pending'`,
			tenantID, notificationID, status, lastError)
		if err != nil {
			return fmt.Errorf("record notification delivery: %w", err)
		}
		deliveryStatus := status
		if ct.RowsAffected() == 0 {
			deliveryStatus = StatusSkipped
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO notification_deliveries (tenant_id, notification_id, channel, status,
				attempts, last_error, delivered_at)
			VALUES ($1, $2, 'webhook', $3, 1, $4, CASE WHEN $3 = 'sent' THEN now() ELSE NULL END)
			ON CONFLICT (tenant_id, notification_id, channel) DO UPDATE SET
				status = EXCLUDED.status,
				attempts = notification_deliveries.attempts + 1,
				last_error = EXCLUDED.last_error,
				delivered_at = CASE WHEN EXCLUDED.status = 'sent' THEN now()
					ELSE notification_deliveries.delivered_at END,
				updated_at = now()`,
			tenantID, notificationID, deliveryStatus, lastError); err != nil {
			return fmt.Errorf("record notification delivery row: %w", err)
		}
		return nil
	})
}

// MarkDead terminates a notification whose delivery job exhausted retries.
func (s *Service) MarkDead(ctx context.Context, tenantID, notificationID uuid.UUID, lastError string) error {
	return withWorkerTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE notifications SET status = 'dead', last_error = $3, updated_at = now()
			WHERE tenant_id = $1 AND id = $2 AND status = 'pending'`,
			tenantID, notificationID, lastError); err != nil {
			return fmt.Errorf("mark notification dead: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE notification_deliveries SET status = 'dead', last_error = $3, updated_at = now()
			WHERE tenant_id = $1 AND notification_id = $2`,
			tenantID, notificationID, lastError); err != nil {
			return fmt.Errorf("mark notification delivery dead: %w", err)
		}
		return nil
	})
}

func withWorkerTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
