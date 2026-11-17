package notify

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

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/db"
)

// Service owns the worker side of the email queue: it turns pending messages
// into jobs, delivers them over SMTP and records terminal failures.
type Service struct {
	pool   *pgxpool.Pool
	mailer Mailer
	cfg    EmailConfig
	alerts *alert.Service
	log    *slog.Logger
}

func NewService(pool *pgxpool.Pool, cfg EmailConfig, alerts *alert.Service, log *slog.Logger) *Service {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	return &Service{
		pool:   pool,
		mailer: NewSMTPSender(cfg),
		cfg:    cfg,
		alerts: alerts,
		log:    log,
	}
}

type queuedMessage struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	ToEmail  string
	Subject  string
	BodyText string
	Status   string
	Attempts int
	Payload  json.RawMessage
}

// DispatchDue enqueues email.send jobs for pending rows. Failed rows older
// than the retry backoff window are picked up again so a crashed worker cannot
// strand a message. The jobs unique index keeps concurrent workers apart.
func (s *Service) DispatchDue(ctx context.Context, limit int) (int, error) {
	if s == nil || !s.cfg.Enabled {
		return 0, nil
	}
	if limit <= 0 {
		limit = 100
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO jobs (tenant_id, kind, subject_type, subject_id, payload, max_attempts)
		SELECT m.tenant_id, $1, $2, m.id, '{}'::jsonb, $3
		FROM email_messages m
		WHERE (m.status = 'pending'
			OR (m.status = 'failed' AND m.updated_at < now() - interval '10 minutes'))
			AND NOT EXISTS (
				SELECT 1 FROM jobs j
				WHERE j.tenant_id = m.tenant_id AND j.kind = $1
					AND j.subject_id = m.id AND j.status IN ('queued','running'))
		ORDER BY m.created_at
		LIMIT $4
		ON CONFLICT DO NOTHING`, JobKind, SubjectType, maxAttempts+3, limit)
	if err != nil {
		return 0, fmt.Errorf("dispatch email: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Deliver sends one queued message. It re-checks the proof link first so a
// revoked token or closed round never reaches the customer's inbox.
func (s *Service) Deliver(ctx context.Context, tenantID, messageID uuid.UUID) (any, error) {
	m, err := s.load(ctx, tenantID, messageID)
	if err != nil {
		return nil, err
	}
	switch m.Status {
	case StatusSent, StatusSkipped, StatusDead:
		return map[string]any{"status": m.Status}, nil
	}
	if !s.cfg.Enabled {
		if err := s.finish(ctx, m, StatusSkipped, "email channel is not configured"); err != nil {
			return nil, err
		}
		return map[string]any{"status": StatusSkipped}, nil
	}
	ok, reason, err := s.checkLink(ctx, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := s.finish(ctx, m, StatusSkipped, reason); err != nil {
			return nil, err
		}
		s.log.Info("email skipped: link is no longer valid", "message_id", m.ID, "reason", reason)
		return map[string]any{"status": StatusSkipped}, nil
	}

	sendErr := s.mailer.Send(ctx, Message{
		From: s.cfg.From, To: m.ToEmail, Subject: m.Subject, Body: m.BodyText,
	})
	if sendErr == nil {
		if err := s.finish(ctx, m, StatusSent, ""); err != nil {
			return nil, err
		}
		s.log.Info("email sent", "message_id", m.ID, "attempts", m.Attempts+1)
		return map[string]any{"status": StatusSent}, nil
	}

	if errors.Is(sendErr, ErrPermanent) || m.Attempts+1 >= maxAttempts {
		if err := s.finish(ctx, m, StatusDead, sendErr.Error()); err != nil {
			return nil, err
		}
		if aerr := s.raiseDeliveryAlert(ctx, m, sendErr); aerr != nil {
			s.log.Error("raise email delivery alert", "message_id", m.ID, "err", aerr)
		}
		s.log.Warn("email delivery abandoned", "message_id", m.ID, "attempts", m.Attempts+1)
		return map[string]any{"status": StatusDead}, nil
	}
	if err := s.finish(ctx, m, StatusFailed, sendErr.Error()); err != nil {
		return nil, err
	}
	return nil, sendErr
}

// MarkDead records a message whose job exhausted its retries.
func (s *Service) MarkDead(ctx context.Context, tenantID, messageID uuid.UUID, lastErr string) error {
	m, err := s.load(ctx, tenantID, messageID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if m.Status == StatusSent || m.Status == StatusSkipped || m.Status == StatusDead {
		return nil
	}
	if err := s.finish(ctx, m, StatusDead, lastErr); err != nil {
		return err
	}
	return s.raiseDeliveryAlert(ctx, m, errors.New(lastErr))
}

func (s *Service) load(ctx context.Context, tenantID, messageID uuid.UUID) (*queuedMessage, error) {
	var m queuedMessage
	err := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, to_email, subject, body_text, status, attempts, payload
		FROM email_messages WHERE tenant_id = $1 AND id = $2`,
		tenantID, messageID).
		Scan(&m.ID, &m.TenantID, &m.ToEmail, &m.Subject, &m.BodyText, &m.Status, &m.Attempts, &m.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load email message: %w", err)
	}
	return &m, nil
}

// finish moves the row to a terminal or retry state. Successful delivery
// clears the body, which is the only place a proof link exists in the queue.
func (s *Service) finish(ctx context.Context, m *queuedMessage, status, lastErr string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE email_messages SET
			status = $3,
			attempts = attempts + 1,
			last_error = $4,
			body_text = CASE WHEN $3 = 'sent' THEN '' ELSE body_text END,
			sent_at = CASE WHEN $3 = 'sent' THEN now() ELSE sent_at END,
			updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status NOT IN ('sent','skipped','dead')`,
		m.TenantID, m.ID, status, truncate(lastErr, 500))
	if err != nil {
		return fmt.Errorf("update email message: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	m.Status = status
	return nil
}

type linkPayload struct {
	ProofRoundID uuid.UUID `json:"proof_round_id"`
	DeliveryID   uuid.UUID `json:"delivery_id"`
	QuoteID      uuid.UUID `json:"quote_id"`
	TokenID      uuid.UUID `json:"token_id"`
}

// checkLink rejects mail whose round, gallery or token has moved on. The
// worker reads these tables through its permissive policies, so every query
// still filters by tenant.
func (s *Service) checkLink(ctx context.Context, m *queuedMessage) (bool, string, error) {
	var payload linkPayload
	if len(m.Payload) > 0 {
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			return true, "", nil
		}
	}
	if payload.ProofRoundID == uuid.Nil && payload.DeliveryID == uuid.Nil && payload.QuoteID == uuid.Nil {
		return true, "", nil
	}
	if payload.DeliveryID != uuid.Nil {
		return s.checkDeliveryLink(ctx, m, payload)
	}
	if payload.QuoteID != uuid.Nil {
		var projectClosed bool
		err := s.pool.QueryRow(ctx, `
			SELECT p.state IN ('cancelled','archived') OR (
				EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
				AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
			)
		FROM quotes q JOIN projects p ON p.tenant_id = q.tenant_id AND p.id = q.project_id
		WHERE q.tenant_id = $1 AND q.id = $2 AND q.status IN ('sent','accepted')`,
			m.TenantID, payload.QuoteID).Scan(&projectClosed)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "quote is no longer active", nil
		}
		if err != nil {
			return false, "", fmt.Errorf("check quote email: %w", err)
		}
		if projectClosed {
			return false, "project is cancelled or archived", nil
		}
		return true, "", nil
	}
	var (
		roundStatus   string
		galleryStatus string
		galleryGone   *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT r.status, g.status, g.revoked_at
		FROM proof_rounds r
		JOIN galleries g ON g.tenant_id = r.tenant_id AND g.id = r.gallery_id
		WHERE r.tenant_id = $1 AND r.id = $2`, m.TenantID, payload.ProofRoundID).
		Scan(&roundStatus, &galleryStatus, &galleryGone)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "proof round no longer exists", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check proof link round: %w", err)
	}
	if galleryStatus == "revoked" || galleryGone != nil {
		return false, "gallery was revoked", nil
	}
	if roundStatus == "approved" {
		return false, "round is already approved", nil
	}
	if payload.TokenID == uuid.Nil {
		return true, "", nil
	}
	var (
		revokedAt *time.Time
		expiresAt *time.Time
		maxVisits int
		visits    int
	)
	err = s.pool.QueryRow(ctx, `
		SELECT revoked_at, expires_at, max_visits, visits
		FROM proof_round_tokens
		WHERE tenant_id = $1 AND id = $2 AND proof_round_id = $3`,
		m.TenantID, payload.TokenID, payload.ProofRoundID).
		Scan(&revokedAt, &expiresAt, &maxVisits, &visits)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "proof token no longer exists", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check proof link token: %w", err)
	}
	switch {
	case revokedAt != nil:
		return false, "proof token was revoked", nil
	case expiresAt != nil && !expiresAt.After(time.Now()):
		return false, "proof token expired", nil
	case maxVisits > 0 && visits >= maxVisits:
		return false, "proof token reached its visit limit", nil
	}
	return true, "", nil
}

// checkDeliveryLink rejects mail whose delivery, project payment or token has
// moved on. A refund after the mail was queued must not produce a live link.
func (s *Service) checkDeliveryLink(ctx context.Context, m *queuedMessage, payload linkPayload) (bool, string, error) {
	var (
		status        string
		paymentStatus string
		projectState  string
		roundStatus   string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT d.status, p.payment_status, p.state, r.status
		FROM deliveries d
		JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
		JOIN proof_rounds r ON r.tenant_id = d.tenant_id AND r.id = d.proof_round_id
		WHERE d.tenant_id = $1 AND d.id = $2`, m.TenantID, payload.DeliveryID).
		Scan(&status, &paymentStatus, &projectState, &roundStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "delivery no longer exists", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check delivery link: %w", err)
	}
	switch {
	case status != "ready":
		return false, "delivery is not ready", nil
	case paymentStatus != "paid":
		return false, "project balance is not paid", nil
	case projectState == "archived" || projectState == "cancelled":
		return false, "project is archived or cancelled", nil
	case roundStatus != "approved":
		return false, "proof round is no longer approved", nil
	}
	if payload.TokenID == uuid.Nil {
		return true, "", nil
	}
	var (
		revokedAt   *time.Time
		expiresAt   time.Time
		maxDownload int
		downloads   int
	)
	err = s.pool.QueryRow(ctx, `
		SELECT revoked_at, expires_at, max_downloads, downloads
		FROM download_tokens
		WHERE tenant_id = $1 AND id = $2 AND delivery_id = $3`,
		m.TenantID, payload.TokenID, payload.DeliveryID).
		Scan(&revokedAt, &expiresAt, &maxDownload, &downloads)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "download token no longer exists", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check delivery token: %w", err)
	}
	switch {
	case revokedAt != nil:
		return false, "download token was revoked", nil
	case !expiresAt.After(time.Now()):
		return false, "download token expired", nil
	case downloads >= maxDownload:
		return false, "download token reached its download limit", nil
	}
	return true, "", nil
}

func (s *Service) raiseDeliveryAlert(ctx context.Context, m *queuedMessage, cause error) error {
	if s.alerts == nil {
		return nil
	}
	return db.WithTenant(ctx, s.pool, m.TenantID, func(tx pgx.Tx) error {
		_, err := s.alerts.Raise(ctx, tx, m.TenantID, alert.Input{
			SubjectType: "email_message",
			SubjectID:   m.ID.String(),
			Code:        "email_delivery_failed",
			Severity:    alert.SeverityWarning,
			DedupeKey:   "email_delivery_failed:" + m.ID.String(),
			Title:       "审片邮件发送失败",
			Detail: map[string]any{
				"to_email": m.ToEmail,
				"subject":  m.Subject,
				"error":    truncate(cause.Error(), 500),
			},
		})
		return err
	})
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
