package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/payment"
	"photography-workflow/server/internal/project"
)

var _ payment.OrderHook = (*Service)(nil)

const bookingColumns = `id, project_id, status, starts_at, ends_at, note, hold_expires_at,
	confirmed_at, cancelled_at, cancel_reason, version, created_by, created_at, updated_at`

type Booking struct {
	ID            uuid.UUID         `json:"id"`
	ProjectID     uuid.UUID         `json:"project_id"`
	Status        BookingStatus     `json:"status"`
	StartsAt      time.Time         `json:"starts_at"`
	EndsAt        time.Time         `json:"ends_at"`
	Note          string            `json:"note"`
	HoldExpiresAt *time.Time        `json:"hold_expires_at,omitempty"`
	ConfirmedAt   *time.Time        `json:"confirmed_at,omitempty"`
	CancelledAt   *time.Time        `json:"cancelled_at,omitempty"`
	CancelReason  string            `json:"cancel_reason"`
	Version       int               `json:"version"`
	CreatedBy     *uuid.UUID        `json:"created_by,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Resources     []BookingResource `json:"resources"`
}

type BookingResource struct {
	ResourceID uuid.UUID `json:"resource_id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
}

type HoldInput struct {
	ProjectID   uuid.UUID
	ResourceIDs []uuid.UUID
	StartsAt    time.Time
	EndsAt      time.Time
	Note        string
}

type RescheduleInput struct {
	StartsAt    time.Time
	EndsAt      time.Time
	ResourceIDs []uuid.UUID
	Note        *string
}

type projectGate struct {
	State         string
	PaymentStatus string
}

type resolvedResource struct {
	Resource
	Rules []AvailabilityRule
}

func scanBooking(row interface{ Scan(...any) error }) (*Booking, error) {
	var (
		b      Booking
		status string
	)
	if err := row.Scan(&b.ID, &b.ProjectID, &status, &b.StartsAt, &b.EndsAt, &b.Note,
		&b.HoldExpiresAt, &b.ConfirmedAt, &b.CancelledAt, &b.CancelReason,
		&b.Version, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	b.Status = BookingStatus(status)
	b.Resources = []BookingResource{}
	return &b, nil
}

func (s *Service) CreateHold(ctx context.Context, tenantID, actor uuid.UUID, ip string, in HoldInput) (*Booking, error) {
	if in.ProjectID == uuid.Nil {
		return nil, fmt.Errorf("%w: project_id is required", ErrInvalidInput)
	}
	ids := dedupeUUIDs(in.ResourceIDs)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: at least one resource is required", ErrInvalidInput)
	}
	if err := ValidateWindow(in.StartsAt, in.EndsAt, s.loc); err != nil {
		return nil, err
	}
	in.Note = strings.TrimSpace(in.Note)
	if len(in.Note) > 2000 {
		return nil, fmt.Errorf("%w: note is too long", ErrInvalidInput)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Booking, error) {
		gate, err := lockProjectGate(ctx, tx, tenantID, in.ProjectID)
		if err != nil {
			return nil, err
		}
		if gate.State == "cancelled" || gate.State == "archived" {
			return nil, fmt.Errorf("%w: project is %s", ErrIllegalState, gate.State)
		}
		var active bool
		err = tx.QueryRow(ctx, `
			SELECT true FROM bookings
			WHERE tenant_id = $1 AND project_id = $2 AND status IN ('held','confirmed')`,
			tenantID, in.ProjectID).Scan(&active)
		switch {
		case err == nil:
			return nil, ErrActiveBooking
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, fmt.Errorf("check active booking: %w", err)
		}

		resources, err := loadHoldResources(ctx, tx, tenantID, ids)
		if err != nil {
			return nil, err
		}
		if err := s.checkAvailability(resources, in.StartsAt, in.EndsAt); err != nil {
			return nil, err
		}
		if err := slotPrecheck(ctx, tx, tenantID, ids, in.StartsAt, in.EndsAt); err != nil {
			return nil, err
		}

		expires := time.Now().UTC().Add(s.holdTTL)
		row := tx.QueryRow(ctx, `
			INSERT INTO bookings (tenant_id, project_id, status, starts_at, ends_at, note,
				hold_expires_at, created_by)
			VALUES ($1,$2,'held',$3,$4,$5,$6,$7)
			RETURNING `+bookingColumns,
			tenantID, in.ProjectID, in.StartsAt, in.EndsAt, in.Note, expires, actor)
		booking, err := scanBooking(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrActiveBooking
			}
			return nil, fmt.Errorf("insert booking: %w", err)
		}
		if err := insertBookingResources(ctx, tx, tenantID, booking.ID, ids); err != nil {
			return nil, err
		}
		for _, res := range resources {
			blockStart, blockEnd := BlockRange(in.StartsAt, in.EndsAt, res.BufferBeforeMinutes, res.BufferAfterMinutes)
			if _, err := insertBlock(ctx, tx, tenantID, res.ID, res.Name, &booking.ID, BlockHold,
				blockStart, blockEnd, &expires, "", &actor); err != nil {
				return nil, err
			}
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "booking.held", "booking", booking.ID.String(), ip,
			map[string]any{"project_id": in.ProjectID, "resources": len(ids),
				"starts_at": in.StartsAt.UTC(), "ends_at": in.EndsAt.UTC()}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", booking.ID.String(), "booking.held",
			map[string]any{"project_id": in.ProjectID, "resources": len(ids)}); err != nil {
			return nil, err
		}
		s.planNotices(ctx, tx, tenantID, booking.ID)
		booking.Resources = resourcesAsBookingResources(resources)
		return booking, nil
	})
}

func (s *Service) Confirm(ctx context.Context, tenantID, actor, bookingID uuid.UUID, ip string, expectedVersion int) (*Booking, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Booking, error) {
		return s.confirmLocked(ctx, tx, tenantID, &actor, ip, bookingID, expectedVersion, false)
	})
}

// confirmLocked is shared by the manual endpoint and the payment settlement
// hook. Lock order: project -> booking -> blocks. paymentVerified is true when
// the caller itself just settled a deposit or full order, in which case the
// project projection still shows the pre-payment status and the gate must not
// be re-read from it.
func (s *Service) confirmLocked(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, ip string, bookingID uuid.UUID, expectedVersion int, paymentVerified bool) (*Booking, error) {
	projectID, err := bookingProjectID(ctx, tx, tenantID, bookingID)
	if err != nil {
		return nil, err
	}
	gate, err := lockProjectGate(ctx, tx, tenantID, projectID)
	if err != nil {
		return nil, err
	}
	if gate.State == "cancelled" || gate.State == "archived" {
		return nil, fmt.Errorf("%w: project is %s", ErrIllegalState, gate.State)
	}
	booking, err := lockBooking(ctx, tx, tenantID, bookingID)
	if err != nil {
		return nil, err
	}
	// Confirmation carries no payload, so an already-confirmed booking is a
	// successful no-op no matter which version the caller holds.
	if booking.Status == StatusConfirmed {
		return attachBookingResources(ctx, tx, tenantID, booking)
	}
	if expectedVersion > 0 && booking.Version != expectedVersion {
		return nil, ErrVersionConflict
	}
	expired := booking.HoldExpiresAt != nil && !booking.HoldExpiresAt.After(time.Now())
	if !CanConfirm(booking.Status, expired) {
		if booking.Status == StatusHeld {
			return nil, fmt.Errorf("%w; create a new hold and pay again", ErrHoldExpired)
		}
		return nil, fmt.Errorf("%w: booking is %s", ErrIllegalState, booking.Status)
	}
	if !paymentVerified && !depositPaid(gate.PaymentStatus) {
		return nil, ErrDepositRequired
	}

	row := tx.QueryRow(ctx, `
		UPDATE bookings SET status = 'confirmed', confirmed_at = now(),
			hold_expires_at = NULL, version = version + 1, updated_at = now()
		WHERE id = $1 AND tenant_id = $2
		RETURNING `+bookingColumns, bookingID, tenantID)
	confirmed, err := scanBooking(row)
	if err != nil {
		return nil, fmt.Errorf("confirm booking: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE resource_blocks SET kind = 'booking', expires_at = NULL, updated_at = now()
		WHERE tenant_id = $1 AND booking_id = $2 AND kind = 'hold'`, tenantID, bookingID); err != nil {
		return nil, fmt.Errorf("promote hold blocks: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE projects SET shoot_start_at = $3, shoot_end_at = $4, updated_at = now()
		WHERE id = $1 AND tenant_id = $2`,
		projectID, tenantID, confirmed.StartsAt, confirmed.EndsAt); err != nil {
		return nil, fmt.Errorf("sync project shoot times: %w", err)
	}
	if err := audit.Write(ctx, tx, tenantID, actor, "booking.confirmed", "booking", bookingID.String(), ip,
		map[string]any{"project_id": projectID, "starts_at": confirmed.StartsAt.UTC(),
			"ends_at": confirmed.EndsAt.UTC(), "auto": actor == nil}); err != nil {
		return nil, err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "booking.confirmed",
		map[string]any{"project_id": projectID}); err != nil {
		return nil, err
	}
	s.planNotices(ctx, tx, tenantID, bookingID)
	return attachBookingResources(ctx, tx, tenantID, confirmed)
}

func (s *Service) Reschedule(ctx context.Context, tenantID, actor, bookingID uuid.UUID, ip string, expectedVersion int, in RescheduleInput) (*Booking, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if err := ValidateWindow(in.StartsAt, in.EndsAt, s.loc); err != nil {
		return nil, err
	}
	var note *string
	if in.Note != nil {
		trimmed := strings.TrimSpace(*in.Note)
		if len(trimmed) > 2000 {
			return nil, fmt.Errorf("%w: note is too long", ErrInvalidInput)
		}
		note = &trimmed
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Booking, error) {
		projectID, err := bookingProjectID(ctx, tx, tenantID, bookingID)
		if err != nil {
			return nil, err
		}
		gate, err := lockProjectGate(ctx, tx, tenantID, projectID)
		if err != nil {
			return nil, err
		}
		if gate.State == "cancelled" || gate.State == "archived" {
			return nil, fmt.Errorf("%w: project is %s", ErrIllegalState, gate.State)
		}
		booking, err := lockBooking(ctx, tx, tenantID, bookingID)
		if err != nil {
			return nil, err
		}
		if booking.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanReschedule(booking.Status) {
			return nil, fmt.Errorf("%w: booking is %s", ErrIllegalState, booking.Status)
		}
		if booking.Status == StatusHeld && booking.HoldExpiresAt != nil && !booking.HoldExpiresAt.After(time.Now()) {
			return nil, ErrHoldExpired
		}

		ids := dedupeUUIDs(in.ResourceIDs)
		if len(ids) == 0 {
			existing, err := bookingResourceIDs(ctx, tx, tenantID, bookingID)
			if err != nil {
				return nil, err
			}
			ids = existing
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("%w: booking has no resources", ErrInvalidInput)
		}
		resources, err := loadHoldResources(ctx, tx, tenantID, ids)
		if err != nil {
			return nil, err
		}
		if err := s.checkAvailability(resources, in.StartsAt, in.EndsAt); err != nil {
			return nil, err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM resource_blocks WHERE tenant_id = $1 AND booking_id = $2`,
			tenantID, bookingID); err != nil {
			return nil, fmt.Errorf("release old blocks: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM booking_resources WHERE tenant_id = $1 AND booking_id = $2`,
			tenantID, bookingID); err != nil {
			return nil, fmt.Errorf("replace booking resources: %w", err)
		}
		if err := insertBookingResources(ctx, tx, tenantID, bookingID, ids); err != nil {
			return nil, err
		}
		blockKind := BlockHold
		var blockExpires *time.Time
		if booking.Status == StatusConfirmed {
			blockKind = BlockBooking
		} else {
			blockExpires = booking.HoldExpiresAt
		}
		for _, res := range resources {
			blockStart, blockEnd := BlockRange(in.StartsAt, in.EndsAt, res.BufferBeforeMinutes, res.BufferAfterMinutes)
			if _, err := insertBlock(ctx, tx, tenantID, res.ID, res.Name, &bookingID, blockKind,
				blockStart, blockEnd, blockExpires, "", &actor); err != nil {
				return nil, err
			}
		}

		row := tx.QueryRow(ctx, `
			UPDATE bookings SET starts_at = $3, ends_at = $4,
				note = COALESCE($5::text, note), version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+bookingColumns, bookingID, tenantID, in.StartsAt, in.EndsAt, note)
		updated, err := scanBooking(row)
		if err != nil {
			return nil, fmt.Errorf("reschedule booking: %w", err)
		}
		if updated.Status == StatusConfirmed {
			if _, err := tx.Exec(ctx, `
				UPDATE projects SET shoot_start_at = $3, shoot_end_at = $4, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`,
				projectID, tenantID, updated.StartsAt, updated.EndsAt); err != nil {
				return nil, fmt.Errorf("sync project shoot times: %w", err)
			}
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "booking.rescheduled", "booking", bookingID.String(), ip,
			map[string]any{"project_id": projectID, "from": booking.StartsAt.UTC(), "to": updated.StartsAt.UTC(),
				"resources": len(ids)}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "booking.rescheduled",
			map[string]any{"project_id": projectID, "starts_at": updated.StartsAt.UTC(),
				"ends_at": updated.EndsAt.UTC()}); err != nil {
			return nil, err
		}
		s.planNotices(ctx, tx, tenantID, bookingID)
		return attachBookingResources(ctx, tx, tenantID, updated)
	})
}

func (s *Service) Cancel(ctx context.Context, tenantID, actor, bookingID uuid.UUID, ip string, expectedVersion int, reason string) (*Booking, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalidInput)
	}
	if len(reason) > 500 {
		return nil, fmt.Errorf("%w: reason is too long", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Booking, error) {
		projectID, err := bookingProjectID(ctx, tx, tenantID, bookingID)
		if err != nil {
			return nil, err
		}
		if _, err := lockProjectGate(ctx, tx, tenantID, projectID); err != nil {
			return nil, err
		}
		booking, err := lockBooking(ctx, tx, tenantID, bookingID)
		if err != nil {
			return nil, err
		}
		if booking.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanCancel(booking.Status) {
			return nil, fmt.Errorf("%w: booking is %s", ErrIllegalState, booking.Status)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM resource_blocks WHERE tenant_id = $1 AND booking_id = $2`,
			tenantID, bookingID); err != nil {
			return nil, fmt.Errorf("release blocks: %w", err)
		}
		row := tx.QueryRow(ctx, `
			UPDATE bookings SET status = 'cancelled', cancelled_at = now(), cancel_reason = $3,
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+bookingColumns, bookingID, tenantID, reason)
		cancelled, err := scanBooking(row)
		if err != nil {
			return nil, fmt.Errorf("cancel booking: %w", err)
		}
		if booking.Status == StatusConfirmed {
			if _, err := tx.Exec(ctx, `
				UPDATE projects SET shoot_start_at = NULL, shoot_end_at = NULL, updated_at = now()
				WHERE id = $1 AND tenant_id = $2 AND shoot_start_at = $3 AND shoot_end_at = $4`,
				projectID, tenantID, booking.StartsAt, booking.EndsAt); err != nil {
				return nil, fmt.Errorf("clear project shoot times: %w", err)
			}
		}
		if err := project.CancelTx(ctx, tx, tenantID, projectID, actor, ip, "booking cancelled: "+reason); err != nil {
			return nil, fmt.Errorf("cancel booking project: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "booking.cancelled", "booking", bookingID.String(), ip,
			map[string]any{"project_id": projectID, "reason": reason, "was": booking.Status}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "booking.cancelled",
			map[string]any{"project_id": projectID, "reason": reason}); err != nil {
			return nil, err
		}
		s.planNotices(ctx, tx, tenantID, bookingID)
		return attachBookingResources(ctx, tx, tenantID, cancelled)
	})
}

func (s *Service) GetBooking(ctx context.Context, tenantID, bookingID uuid.UUID) (*Booking, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Booking, error) {
		row := tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE id = $1 AND tenant_id = $2`,
			bookingID, tenantID)
		booking, err := scanBooking(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrBookingNotFound
			}
			return nil, fmt.Errorf("get booking: %w", err)
		}
		return attachBookingResources(ctx, tx, tenantID, booking)
	})
}

func (s *Service) ListBookings(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status string, limit, offset int) ([]Booking, int, error) {
	if status != "" && !ValidBookingStatus(status) {
		return nil, 0, fmt.Errorf("%w: unknown booking status %q", ErrInvalidInput, status)
	}
	var (
		items []Booking
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const where = `tenant_id = $1 AND ($2::uuid IS NULL OR project_id = $2)
			AND ($3::text = '' OR status = $3)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE `+where,
			tenantID, projectID, status).Scan(&total); err != nil {
			return fmt.Errorf("count bookings: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE `+where+`
			ORDER BY created_at DESC LIMIT $4 OFFSET $5`, tenantID, projectID, status, limit, offset)
		if err != nil {
			return fmt.Errorf("list bookings: %w", err)
		}
		for rows.Next() {
			b, err := scanBooking(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("scan booking: %w", err)
			}
			items = append(items, *b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, len(items))
		for i := range items {
			ids[i] = items[i].ID
		}
		byBooking, err := loadBookingResources(ctx, tx, tenantID, ids)
		if err != nil {
			return err
		}
		for i := range items {
			if res, ok := byBooking[items[i].ID]; ok {
				items[i].Resources = res
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Booking{}
	}
	return items, total, nil
}

// OrderSettled confirms the project's existing hold when a deposit or full
// payment settles. The payment transaction already holds the order and project
// locks, so only the booking and its blocks are touched here.
func (s *Service) OrderSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, order *payment.Order) error {
	if order == nil || (order.Kind != payment.KindDeposit && order.Kind != payment.KindFull) {
		return nil
	}
	var (
		bookingID   uuid.UUID
		holdExpires *time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT id, hold_expires_at FROM bookings
		WHERE tenant_id = $1 AND project_id = $2 AND status = 'held'
		ORDER BY created_at DESC LIMIT 1
		FOR UPDATE`, tenantID, order.ProjectID).Scan(&bookingID, &holdExpires)
	if err == nil {
		if holdExpires != nil && !holdExpires.After(time.Now()) {
			s.raiseHoldExpired(ctx, tx, tenantID, bookingID, order, holdExpires)
			return nil
		}
		if _, err := s.confirmLocked(ctx, tx, tenantID, nil, "", bookingID, 0, true); err != nil {
			if errors.Is(err, ErrHoldExpired) {
				s.raiseHoldExpired(ctx, tx, tenantID, bookingID, order, holdExpires)
				return nil
			}
			if errors.Is(err, ErrIllegalState) || errors.Is(err, ErrDepositRequired) {
				// The channel captured money against a project that can no
				// longer be auto-confirmed (typically cancelled after the
				// order was created). Rolling back would lose the payment and
				// make the channel retry forever, so keep the settlement and
				// hand the mismatch to an operator.
				s.raiseConfirmBlocked(ctx, tx, tenantID, bookingID, order, err)
				return nil
			}
			return err
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load project hold: %w", err)
	}

	// The worker may have expired the hold before the callback arrived. The
	// money stays captured; operators get a critical alert to refund or rebook.
	var expiredAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT id, hold_expires_at FROM bookings
		WHERE tenant_id = $1 AND project_id = $2 AND status = 'expired'
			AND hold_expires_at > now() - interval '24 hours'
		ORDER BY hold_expires_at DESC LIMIT 1
		FOR UPDATE`, tenantID, order.ProjectID).Scan(&bookingID, &expiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load expired project hold: %w", err)
	}
	s.raiseHoldExpired(ctx, tx, tenantID, bookingID, order, &expiredAt)
	return nil
}

func (s *Service) raiseConfirmBlocked(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID, order *payment.Order, cause error) {
	if s.alerts == nil {
		s.log.Warn("paid booking could not be confirmed; no alert sink configured",
			"booking_id", bookingID, "order_id", order.ID, "err", cause)
		return
	}
	detail := map[string]any{
		"booking_id": bookingID.String(),
		"order_id":   order.ID.String(),
		"kind":       order.Kind,
		"reason":     cause.Error(),
	}
	amount := order.AmountCents
	if _, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
		ProjectID:   &order.ProjectID,
		SubjectType: "booking",
		SubjectID:   bookingID.String(),
		Code:        "booking_confirm_blocked",
		Severity:    alert.SeverityCritical,
		DedupeKey:   "booking_confirm_blocked:" + bookingID.String(),
		Title:       "已收款但预约无法自动确认",
		Detail:      detail,
		AmountCents: &amount,
	}); err != nil {
		s.log.Error("raise booking_confirm_blocked alert", "booking_id", bookingID, "err", err)
	}
}

func (s *Service) raiseHoldExpired(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID, order *payment.Order, expires *time.Time) {
	if s.alerts == nil {
		s.log.Warn("booking hold expired after payment; no alert sink configured",
			"booking_id", bookingID, "order_id", order.ID)
		return
	}
	detail := map[string]any{
		"booking_id": bookingID.String(),
		"order_id":   order.ID.String(),
		"kind":       order.Kind,
	}
	if expires != nil {
		detail["hold_expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	amount := order.AmountCents
	if _, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
		ProjectID:   &order.ProjectID,
		SubjectType: "booking",
		SubjectID:   bookingID.String(),
		Code:        "booking_hold_expired",
		Severity:    alert.SeverityCritical,
		DedupeKey:   "booking_hold_expired:" + bookingID.String(),
		Title:       "锁位已过期但定金已支付",
		Detail:      detail,
		AmountCents: &amount,
	}); err != nil {
		// Raise is savepointed; the payment must still settle.
		s.log.Error("raise booking_hold_expired alert", "booking_id", bookingID, "err", err)
	}
}

func (s *Service) checkAvailability(resources []resolvedResource, start, end time.Time) error {
	for _, res := range resources {
		if !WithinAvailability(res.Rules, start, end, s.loc) {
			return fmt.Errorf("%w: resource %s is not available at that time", ErrOutsideAvailability, res.Name)
		}
	}
	return nil
}

func lockProjectGate(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) (projectGate, error) {
	var g projectGate
	err := tx.QueryRow(ctx, `
		SELECT CASE WHEN state NOT IN ('cancelled','archived')
				AND EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status = 'cancelled')
				AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status IN ('held','confirmed'))
				THEN 'cancelled' ELSE state END,
			payment_status FROM projects
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, projectID, tenantID).
		Scan(&g.State, &g.PaymentStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return g, ErrProjectNotFound
		}
		return g, fmt.Errorf("lock project: %w", err)
	}
	return g, nil
}

func bookingProjectID(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT project_id FROM bookings WHERE id = $1 AND tenant_id = $2`,
		bookingID, tenantID).Scan(&projectID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrBookingNotFound
		}
		return uuid.Nil, fmt.Errorf("load booking project: %w", err)
	}
	return projectID, nil
}

func lockBooking(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) (*Booking, error) {
	row := tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, bookingID, tenantID)
	booking, err := scanBooking(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBookingNotFound
		}
		return nil, fmt.Errorf("lock booking: %w", err)
	}
	return booking, nil
}

func loadHoldResources(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID) ([]resolvedResource, error) {
	rows, err := tx.Query(ctx, `SELECT `+resourceColumns+` FROM resources
		WHERE tenant_id = $1 AND id = ANY($2) ORDER BY kind, name`, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("load resources: %w", err)
	}
	var out []resolvedResource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan resource: %w", err)
		}
		if !r.Active {
			rows.Close()
			return nil, fmt.Errorf("%w: resource %q is inactive", ErrResourceNotFound, r.Name)
		}
		out = append(out, resolvedResource{Resource: *r})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("%w: one or more resources do not exist in this tenant", ErrResourceNotFound)
	}
	ruleRows, err := tx.Query(ctx, `
		SELECT resource_id, weekday, start_minute, end_minute FROM availability_rules
		WHERE tenant_id = $1 AND resource_id = ANY($2) ORDER BY resource_id, weekday, start_minute`,
		tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("load availability: %w", err)
	}
	defer ruleRows.Close()
	index := make(map[uuid.UUID]int, len(out))
	for i := range out {
		index[out[i].ID] = i
	}
	for ruleRows.Next() {
		var (
			resourceID uuid.UUID
			rule       AvailabilityRule
		)
		if err := ruleRows.Scan(&resourceID, &rule.Weekday, &rule.StartMinute, &rule.EndMinute); err != nil {
			return nil, fmt.Errorf("scan availability rule: %w", err)
		}
		if i, ok := index[resourceID]; ok {
			out[i].Rules = append(out[i].Rules, rule)
		}
	}
	return out, ruleRows.Err()
}

func slotPrecheck(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID, start, end time.Time) error {
	var name string
	err := tx.QueryRow(ctx, `
		SELECT r.name FROM resource_blocks b
		JOIN resources r ON r.id = b.resource_id AND r.tenant_id = b.tenant_id
		WHERE b.tenant_id = $1 AND b.resource_id = ANY($2) AND b.during && tstzrange($3, $4, '[)')
		ORDER BY r.name LIMIT 1`, tenantID, ids, start, end).Scan(&name)
	if err == nil {
		return fmt.Errorf("%w: resource %s is already occupied", ErrSlotTaken, name)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("check slot occupancy: %w", err)
}

func insertBlock(ctx context.Context, tx pgx.Tx, tenantID, resourceID uuid.UUID, resourceName string, bookingID *uuid.UUID, kind string, start, end time.Time, expires *time.Time, note string, actor *uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO resource_blocks (tenant_id, resource_id, booking_id, kind, during, expires_at, note, created_by)
		VALUES ($1,$2,$3,$4,tstzrange($5,$6,'[)'),$7,$8,$9)
		RETURNING id`,
		tenantID, resourceID, bookingID, kind, start, end, expires, note, actor).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
			return uuid.Nil, fmt.Errorf("%w: resource %s is already occupied", ErrSlotTaken, resourceName)
		}
		return uuid.Nil, fmt.Errorf("insert resource block: %w", err)
	}
	return id, nil
}

func insertBookingResources(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID, ids []uuid.UUID) error {
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			INSERT INTO booking_resources (tenant_id, booking_id, resource_id)
			VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, tenantID, bookingID, id); err != nil {
			return fmt.Errorf("link booking resource: %w", err)
		}
	}
	return nil
}

func bookingResourceIDs(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT resource_id FROM booking_resources
		WHERE tenant_id = $1 AND booking_id = $2 ORDER BY resource_id`, tenantID, bookingID)
	if err != nil {
		return nil, fmt.Errorf("load booking resources: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan booking resource: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func loadBookingResources(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID][]BookingResource, error) {
	out := make(map[uuid.UUID][]BookingResource, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT br.booking_id, r.id, r.kind, r.name
		FROM booking_resources br
		JOIN resources r ON r.id = br.resource_id AND r.tenant_id = br.tenant_id
		WHERE br.tenant_id = $1 AND br.booking_id = ANY($2)
		ORDER BY r.kind, r.name`, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("load booking resources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			bookingID uuid.UUID
			res       BookingResource
		)
		if err := rows.Scan(&bookingID, &res.ResourceID, &res.Kind, &res.Name); err != nil {
			return nil, fmt.Errorf("scan booking resource: %w", err)
		}
		out[bookingID] = append(out[bookingID], res)
	}
	return out, rows.Err()
}

func attachBookingResources(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, booking *Booking) (*Booking, error) {
	byBooking, err := loadBookingResources(ctx, tx, tenantID, []uuid.UUID{booking.ID})
	if err != nil {
		return nil, err
	}
	if res, ok := byBooking[booking.ID]; ok {
		booking.Resources = res
	}
	return booking, nil
}

func resourcesAsBookingResources(resources []resolvedResource) []BookingResource {
	out := make([]BookingResource, len(resources))
	for i, res := range resources {
		out[i] = BookingResource{ResourceID: res.ID, Kind: res.Kind, Name: res.Name}
	}
	return out
}

func depositPaid(status string) bool {
	switch status {
	case "deposit_paid", "balance_pending", "change_pending", "paid":
		return true
	}
	return false
}

func dedupeUUIDs(ids []uuid.UUID) []uuid.UUID {
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
