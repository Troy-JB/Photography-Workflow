package alert

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
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/outbox"
)

const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

const (
	StatusOpen         = "open"
	StatusAcknowledged = "acknowledged"
	StatusResolved     = "resolved"
)

// NotifyJobKind is the jobs.kind the worker executes for webhook delivery.
const NotifyJobKind = "notify.webhook"

// NotifySubjectType groups alert notifications under the jobs queue.
const NotifySubjectType = "payment_alert"

const notifyMaxAttempts = 5

var (
	ErrNotFound     = errors.New("payment alert not found")
	ErrNotOpen      = errors.New("payment alert is not open")
	ErrInvalidInput = errors.New("invalid alert input")
)

// Config controls webhook delivery. The worker performs the HTTP call; the
// service only decides whether an alert needs a notification.
type Config struct {
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
	if cfg.WebhookTimeout <= 0 {
		cfg.WebhookTimeout = 10 * time.Second
	}
	cfg.WebhookURL = strings.TrimSpace(cfg.WebhookURL)
	cfg.WebhookSecret = strings.TrimSpace(cfg.WebhookSecret)
	return &Service{pool: pool, cfg: cfg, log: log}
}

func (s *Service) WebhookEnabled() bool { return s.cfg.WebhookURL != "" }

const alertColumns = `id, tenant_id, project_id, subject_type, subject_id, code, severity, status,
	dedupe_key, title, detail, amount_cents, occurrences, first_seen_at, last_seen_at,
	acknowledged_by, acknowledged_at, resolved_by, resolved_at, resolution_note,
	notify_status, notify_attempts, last_notify_error, created_at, updated_at`

type Alert struct {
	ID              uuid.UUID       `json:"id"`
	TenantID        uuid.UUID       `json:"tenant_id"`
	ProjectID       *uuid.UUID      `json:"project_id,omitempty"`
	SubjectType     string          `json:"subject_type"`
	SubjectID       string          `json:"subject_id"`
	Code            string          `json:"code"`
	Severity        string          `json:"severity"`
	Status          string          `json:"status"`
	DedupeKey       string          `json:"dedupe_key"`
	Title           string          `json:"title"`
	Detail          json.RawMessage `json:"detail"`
	AmountCents     *int64          `json:"amount_cents,omitempty"`
	Occurrences     int             `json:"occurrences"`
	FirstSeenAt     time.Time       `json:"first_seen_at"`
	LastSeenAt      time.Time       `json:"last_seen_at"`
	AcknowledgedBy  *uuid.UUID      `json:"acknowledged_by,omitempty"`
	AcknowledgedAt  *time.Time      `json:"acknowledged_at,omitempty"`
	ResolvedBy      *uuid.UUID      `json:"resolved_by,omitempty"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
	ResolutionNote  string          `json:"resolution_note"`
	NotifyStatus    string          `json:"notify_status"`
	NotifyAttempts  int             `json:"notify_attempts"`
	LastNotifyError string          `json:"last_notify_error,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// Input describes one security finding. Callers keep dedupe keys stable so a
// repeated event folds into the open alert instead of flooding operators.
type Input struct {
	ProjectID   *uuid.UUID
	SubjectType string
	SubjectID   string
	Code        string
	Severity    string
	DedupeKey   string
	Title       string
	Detail      map[string]any
	AmountCents *int64
}

func ValidSeverity(v string) bool {
	switch v {
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return true
	}
	return false
}

func ValidStatus(v string) bool {
	switch v {
	case StatusOpen, StatusAcknowledged, StatusResolved:
		return true
	}
	return false
}

// Raise records an alert inside the caller's transaction. The write is guarded
// by a savepoint: a broken notification path must never abort the payment
// transaction that detected the finding.
func (s *Service) Raise(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, in Input) (*Alert, error) {
	if err := validateInput(in); err != nil {
		return nil, err
	}
	detail := json.RawMessage(`{}`)
	if len(in.Detail) > 0 {
		raw, err := json.Marshal(in.Detail)
		if err != nil {
			return nil, fmt.Errorf("marshal alert detail: %w", err)
		}
		detail = raw
	}

	if _, err := tx.Exec(ctx, "SAVEPOINT payment_alert_raise"); err != nil {
		return nil, fmt.Errorf("alert savepoint: %w", err)
	}
	notifyStatus := "skipped"
	if s.WebhookEnabled() {
		notifyStatus = "pending"
	}
	a, inserted, err := s.upsert(ctx, tx, tenantID, in, detail, notifyStatus)
	if err == nil && inserted && s.WebhookEnabled() {
		err = enqueueNotification(ctx, tx, tenantID, a.ID)
	}
	if err != nil {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT payment_alert_raise")
		return nil, err
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT payment_alert_raise"); err != nil {
		return nil, fmt.Errorf("alert savepoint release: %w", err)
	}

	httpx.Inc("payment_alerts_total", map[string]string{"code": in.Code, "severity": in.Severity})
	return a, nil
}

func (s *Service) upsert(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, in Input, detail json.RawMessage, notifyStatus string) (*Alert, bool, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO payment_alerts (tenant_id, project_id, subject_type, subject_id, code,
			severity, dedupe_key, title, detail, amount_cents, notify_status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (tenant_id, dedupe_key) WHERE status IN ('open','acknowledged')
		DO UPDATE SET occurrences = payment_alerts.occurrences + 1,
			last_seen_at = now(), detail = EXCLUDED.detail, updated_at = now()
		RETURNING `+alertColumns+`, (xmax = 0) AS inserted`,
		tenantID, in.ProjectID, in.SubjectType, in.SubjectID, in.Code,
		in.Severity, in.DedupeKey, in.Title, detail, in.AmountCents, notifyStatus)

	var (
		a        Alert
		inserted bool
	)
	if err := row.Scan(&a.ID, &a.TenantID, &a.ProjectID, &a.SubjectType, &a.SubjectID,
		&a.Code, &a.Severity, &a.Status, &a.DedupeKey, &a.Title, &a.Detail, &a.AmountCents,
		&a.Occurrences, &a.FirstSeenAt, &a.LastSeenAt, &a.AcknowledgedBy, &a.AcknowledgedAt,
		&a.ResolvedBy, &a.ResolvedAt, &a.ResolutionNote, &a.NotifyStatus, &a.NotifyAttempts,
		&a.LastNotifyError, &a.CreatedAt, &a.UpdatedAt, &inserted); err != nil {
		return nil, false, fmt.Errorf("upsert payment alert: %w", err)
	}
	return &a, inserted, nil
}

func enqueueNotification(ctx context.Context, tx pgx.Tx, tenantID, alertID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs (tenant_id, kind, subject_type, subject_id, payload, max_attempts)
		VALUES ($1, $2, $3, $4, '{}'::jsonb, $5)
		ON CONFLICT DO NOTHING`,
		tenantID, NotifyJobKind, NotifySubjectType, alertID, notifyMaxAttempts); err != nil {
		return fmt.Errorf("enqueue alert notification: %w", err)
	}
	return nil
}

func validateInput(in Input) error {
	switch {
	case strings.TrimSpace(in.Code) == "":
		return fmt.Errorf("%w: code is required", ErrInvalidInput)
	case !ValidSeverity(in.Severity):
		return fmt.Errorf("%w: severity must be info, warning or critical", ErrInvalidInput)
	case strings.TrimSpace(in.DedupeKey) == "":
		return fmt.Errorf("%w: dedupe_key is required", ErrInvalidInput)
	case strings.TrimSpace(in.Title) == "":
		return fmt.Errorf("%w: title is required", ErrInvalidInput)
	case len(in.Code) > 64 || len(in.DedupeKey) > 200 || len(in.Title) > 200:
		return fmt.Errorf("%w: code, dedupe_key or title is too long", ErrInvalidInput)
	}
	return nil
}

type Filter struct {
	Status    string
	Severity  string
	Code      string
	ProjectID *uuid.UUID
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, filter Filter, limit, offset int) ([]Alert, int, error) {
	if filter.Status != "" && !ValidStatus(filter.Status) {
		return nil, 0, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, filter.Status)
	}
	if filter.Severity != "" && !ValidSeverity(filter.Severity) {
		return nil, 0, fmt.Errorf("%w: unknown severity %q", ErrInvalidInput, filter.Severity)
	}
	var (
		items []Alert
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const where = `tenant_id = $1 AND ($2::text = '' OR status = $2)
			AND ($3::text = '' OR severity = $3) AND ($4::text = '' OR code = $4)
			AND ($5::uuid IS NULL OR project_id = $5)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_alerts WHERE `+where,
			tenantID, filter.Status, filter.Severity, filter.Code, filter.ProjectID).Scan(&total); err != nil {
			return fmt.Errorf("count payment alerts: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+alertColumns+` FROM payment_alerts WHERE `+where+`
			ORDER BY created_at DESC LIMIT $6 OFFSET $7`,
			tenantID, filter.Status, filter.Severity, filter.Code, filter.ProjectID, limit, offset)
		if err != nil {
			return fmt.Errorf("list payment alerts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAlert(rows)
			if err != nil {
				return err
			}
			items = append(items, *a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Service) Get(ctx context.Context, tenantID, alertID uuid.UUID) (*Alert, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Alert, error) {
		a, err := scanAlert(tx.QueryRow(ctx, `
			SELECT `+alertColumns+` FROM payment_alerts WHERE id = $1 AND tenant_id = $2`,
			alertID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get payment alert: %w", err)
		}
		return a, nil
	})
}

func (s *Service) Acknowledge(ctx context.Context, tenantID, actor, alertID uuid.UUID, ip string) (*Alert, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Alert, error) {
		current, err := scanAlert(tx.QueryRow(ctx, `
			SELECT `+alertColumns+` FROM payment_alerts
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, alertID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lock payment alert: %w", err)
		}
		if current.Status != StatusOpen {
			return nil, fmt.Errorf("%w: alert is %s", ErrNotOpen, current.Status)
		}
		updated, err := scanAlert(tx.QueryRow(ctx, `
			UPDATE payment_alerts SET status = 'acknowledged', acknowledged_by = $3,
				acknowledged_at = now(), updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+alertColumns, alertID, tenantID, actor))
		if err != nil {
			return nil, fmt.Errorf("acknowledge payment alert: %w", err)
		}
		detail := map[string]any{
			"code": updated.Code, "severity": updated.Severity, "status": updated.Status,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "payment_alert.acknowledged",
			"payment_alert", alertID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "payment_alert", alertID.String(),
			"payment_alert.acknowledged", detail); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

func (s *Service) Resolve(ctx context.Context, tenantID, actor, alertID uuid.UUID, note, ip string) (*Alert, error) {
	note = strings.TrimSpace(note)
	if len(note) > 500 {
		return nil, fmt.Errorf("%w: resolution note must be at most 500 chars", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Alert, error) {
		current, err := scanAlert(tx.QueryRow(ctx, `
			SELECT `+alertColumns+` FROM payment_alerts
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, alertID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lock payment alert: %w", err)
		}
		if current.Status != StatusOpen && current.Status != StatusAcknowledged {
			return nil, fmt.Errorf("%w: alert is %s", ErrNotOpen, current.Status)
		}
		updated, err := scanAlert(tx.QueryRow(ctx, `
			UPDATE payment_alerts SET status = 'resolved', resolved_by = $3, resolved_at = now(),
				resolution_note = $4, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+alertColumns, alertID, tenantID, actor, note))
		if err != nil {
			return nil, fmt.Errorf("resolve payment alert: %w", err)
		}
		detail := map[string]any{
			"code": updated.Code, "severity": updated.Severity, "status": updated.Status,
			"note": note,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "payment_alert.resolved",
			"payment_alert", alertID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "payment_alert", alertID.String(),
			"payment_alert.resolved", detail); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// LoadForDelivery reads one alert outside a tenant session. The worker role
// holds a permissive policy on payment_alerts for this purpose.
func (s *Service) LoadForDelivery(ctx context.Context, tenantID, alertID uuid.UUID) (*Alert, error) {
	a, err := scanAlert(s.pool.QueryRow(ctx, `
		SELECT `+alertColumns+` FROM payment_alerts WHERE id = $1 AND tenant_id = $2`,
		alertID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load payment alert: %w", err)
	}
	return a, nil
}

// RecordNotify updates the delivery state after the worker attempted a webhook.
// It runs under the worker role, which holds a permissive policy on this table.
func (s *Service) RecordNotify(ctx context.Context, tenantID, alertID uuid.UUID, status, lastError string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE payment_alerts SET notify_status = $3, notify_attempts = notify_attempts + 1,
			last_notify_error = $4, updated_at = now()
		WHERE id = $1 AND tenant_id = $2`, alertID, tenantID, status, lastError); err != nil {
		return fmt.Errorf("record alert notification: %w", err)
	}
	return nil
}

func scanAlert(row rowScanner) (*Alert, error) {
	var a Alert
	if err := row.Scan(&a.ID, &a.TenantID, &a.ProjectID, &a.SubjectType, &a.SubjectID,
		&a.Code, &a.Severity, &a.Status, &a.DedupeKey, &a.Title, &a.Detail, &a.AmountCents,
		&a.Occurrences, &a.FirstSeenAt, &a.LastSeenAt, &a.AcknowledgedBy, &a.AcknowledgedAt,
		&a.ResolvedBy, &a.ResolvedAt, &a.ResolutionNote, &a.NotifyStatus, &a.NotifyAttempts,
		&a.LastNotifyError, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}
