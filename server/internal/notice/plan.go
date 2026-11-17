package notice

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type bookingState struct {
	ID            uuid.UUID
	ProjectID     uuid.UUID
	Status        string
	StartsAt      time.Time
	HoldExpiresAt *time.Time
	PendingAuths  int
}

type plannedNotice struct {
	Type      string
	Title     string
	Body      string
	DedupeKey string
	DueAt     time.Time
	Payload   any
}

type bookingPayload struct {
	BookingID             uuid.UUID  `json:"booking_id"`
	ProjectID             uuid.UUID  `json:"project_id"`
	StartsAt              *time.Time `json:"starts_at,omitempty"`
	HoldExpiresAt         *time.Time `json:"hold_expires_at,omitempty"`
	PendingAuthorizations *int       `json:"pending_authorizations,omitempty"`
	LeadSeconds           *int64     `json:"lead_seconds,omitempty"`
}

// PlanBooking replaces the pending reminders for one booking according to its
// current state. It runs inside the caller's transaction behind a savepoint:
// notification planning must never abort a booking or payment write.
func (s *Service) PlanBooking(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) error {
	if s == nil || !s.cfg.Enabled {
		return nil
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT notice_plan_booking"); err != nil {
		return fmt.Errorf("notice savepoint: %w", err)
	}
	err := s.planBooking(ctx, tx, tenantID, bookingID)
	if err != nil {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT notice_plan_booking")
		return err
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT notice_plan_booking"); err != nil {
		return fmt.Errorf("notice savepoint release: %w", err)
	}
	return nil
}

func (s *Service) planBooking(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) error {
	var state bookingState
	err := tx.QueryRow(ctx, `
		SELECT id, project_id, status, starts_at, hold_expires_at
		FROM bookings WHERE id = $1 AND tenant_id = $2`, bookingID, tenantID).
		Scan(&state.ID, &state.ProjectID, &state.Status, &state.StartsAt, &state.HoldExpiresAt)
	if err != nil {
		return fmt.Errorf("load booking for reminder plan: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE notifications SET status = 'cancelled', updated_at = now()
		WHERE tenant_id = $1 AND subject_type = 'booking' AND subject_id = $2
			AND status = 'pending'`, tenantID, bookingID); err != nil {
		return fmt.Errorf("cancel pending reminders: %w", err)
	}
	if state.Status == "confirmed" {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM booking_authorizations
			WHERE tenant_id = $1 AND booking_id = $2 AND status = 'pending'`,
			tenantID, bookingID).Scan(&state.PendingAuths); err != nil {
			return fmt.Errorf("count pending authorizations: %w", err)
		}
	}
	for _, n := range planFor(time.Now(), state, s.cfg) {
		if err := insertPlanned(ctx, tx, tenantID, bookingID, n); err != nil {
			return err
		}
	}
	return nil
}

// planFor is pure so lead math, dedupe keys and applicability are unit tested
// without a database.
func planFor(now time.Time, state bookingState, cfg Config) []plannedNotice {
	if !cfg.Enabled {
		return nil
	}
	loc := cfg.Timezone
	if loc == nil {
		loc = time.UTC
	}
	var out []plannedNotice
	startsLocal := state.StartsAt.In(loc).Format("2006-01-02 15:04 MST")
	startsCopy := state.StartsAt
	base := bookingPayload{BookingID: state.ID, ProjectID: state.ProjectID, StartsAt: &startsCopy}

	switch state.Status {
	case "held":
		if cfg.HoldLead > 0 && state.HoldExpiresAt != nil && state.HoldExpiresAt.After(now) {
			expires := *state.HoldExpiresAt
			due := expires.Add(-cfg.HoldLead)
			if due.Before(now) {
				due = now
			}
			payload := base
			payload.HoldExpiresAt = &expires
			out = append(out, plannedNotice{
				Type:      TypeHoldExpiry,
				Title:     "锁位即将到期",
				Body:      fmt.Sprintf("预约 %s 的锁位将于 %s 到期，请尽快支付定金。", state.ID, expires.In(loc).Format("2006-01-02 15:04 MST")),
				DedupeKey: fmt.Sprintf("hold:%s:%d", state.ID, expires.Unix()),
				DueAt:     due,
				Payload:   payload,
			})
		}
	case "confirmed":
		if !state.StartsAt.After(now) {
			return out
		}
		for _, lead := range cfg.ShootLeads {
			lead := lead
			if lead <= 0 {
				continue
			}
			due := state.StartsAt.Add(-lead)
			if due.Before(now) {
				due = now
			}
			leadSeconds := int64(lead / time.Second)
			payload := base
			payload.LeadSeconds = &leadSeconds
			out = append(out, plannedNotice{
				Type:      TypeShootReminder,
				Title:     "拍摄提醒",
				Body:      fmt.Sprintf("拍摄将于 %s 开始，请确认通告与器材。", startsLocal),
				DedupeKey: fmt.Sprintf("shoot:%s:%d:%d", state.ID, state.StartsAt.Unix(), leadSeconds),
				DueAt:     due,
				Payload:   payload,
			})
		}
		if cfg.AuthLead > 0 && state.PendingAuths > 0 {
			due := state.StartsAt.Add(-cfg.AuthLead)
			if due.Before(now) {
				due = now
			}
			pending := state.PendingAuths
			payload := base
			payload.PendingAuthorizations = &pending
			out = append(out, plannedNotice{
				Type:      TypeAuthIncomplete,
				Title:     "授权未完成",
				Body:      fmt.Sprintf("拍摄 %s 前仍有 %d 项授权未完成，请补齐肖像、场地或使用授权。", startsLocal, pending),
				DedupeKey: fmt.Sprintf("auth:%s:%d", state.ID, state.StartsAt.Unix()),
				DueAt:     due,
				Payload:   payload,
			})
		}
	}
	return out
}

func insertPlanned(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID, n plannedNotice) error {
	raw, err := json.Marshal(n.Payload)
	if err != nil {
		return fmt.Errorf("marshal reminder payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key,
			title, body, payload, due_at)
		SELECT $1, $2, 'booking', $3, $4, $5, $6, $7, $8
		WHERE NOT EXISTS (
			SELECT 1 FROM notifications
			WHERE tenant_id = $1 AND dedupe_key = $4 AND status = 'sent')
		ON CONFLICT DO NOTHING`,
		tenantID, n.Type, bookingID, n.DedupeKey, n.Title, n.Body, raw, n.DueAt); err != nil {
		return fmt.Errorf("insert reminder: %w", err)
	}
	return nil
}

// CancelForBooking drops pending reminders without planning new ones. The
// worker's hold expiry uses it because the pwf_worker role may only UPDATE
// notifications, not insert them.
func (s *Service) CancelForBooking(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) error {
	if s == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT notice_cancel_booking"); err != nil {
		return fmt.Errorf("notice savepoint: %w", err)
	}
	_, err := tx.Exec(ctx, `
		UPDATE notifications SET status = 'cancelled', updated_at = now()
		WHERE tenant_id = $1 AND subject_type = 'booking' AND subject_id = $2
			AND status = 'pending'`, tenantID, bookingID)
	if err != nil {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT notice_cancel_booking")
		return fmt.Errorf("cancel booking reminders: %w", err)
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT notice_cancel_booking"); err != nil {
		return fmt.Errorf("notice savepoint release: %w", err)
	}
	return nil
}
