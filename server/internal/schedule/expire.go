package schedule

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/outbox"
)

// ReminderCanceller drops pending reminders that no longer apply. The worker
// uses the notice service here because pwf_worker may only UPDATE notifications
// and cannot plan new ones.
type ReminderCanceller interface {
	CancelForBooking(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) error
}

// ExpireHolds releases lapsed holds. Each booking is updated and its hold
// blocks deleted in one transaction so a crash can never leave an expired
// booking still occupying a resource. It runs on the least-privilege worker
// role, which has grants on bookings, resource_blocks and notifications.
//
// Reminder cancellation is part of the same transaction on purpose: an expired
// booking with a live reminder would still page staff for a slot that is gone,
// so a cancellation failure aborts that booking's expiry. The booking is then
// skipped for the rest of this sweep and retried on the next one, so one broken
// reminder cannot block the queue behind it; the failure is returned to the
// caller for logging.
func ExpireHolds(ctx context.Context, pool *pgxpool.Pool, limit int, reminders ReminderCanceller) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	var (
		expired int
		failed  []error
		skip    []uuid.UUID
	)
	for expired < limit {
		n, stuck, err := expireOne(ctx, pool, reminders, skip)
		if err != nil {
			if stuck == nil {
				return expired, err
			}
			failed = append(failed, err)
			skip = append(skip, *stuck)
			continue
		}
		if n == 0 {
			break
		}
		expired += n
	}
	return expired, errors.Join(failed...)
}

func expireOne(ctx context.Context, pool *pgxpool.Pool, reminders ReminderCanceller, skip []uuid.UUID) (int, *uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("begin expire hold: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		bookingID uuid.UUID
		tenantID  uuid.UUID
		projectID uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, project_id FROM bookings
		WHERE status = 'held' AND hold_expires_at < now()
			AND id <> ALL(COALESCE($1::uuid[], '{}'::uuid[]))
		ORDER BY hold_expires_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, skip).Scan(&bookingID, &tenantID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("claim expired hold: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE bookings SET status = 'expired', version = version + 1, updated_at = now()
		WHERE id = $1 AND tenant_id = $2`, bookingID, tenantID); err != nil {
		return 0, nil, fmt.Errorf("expire booking: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM resource_blocks WHERE tenant_id = $1 AND booking_id = $2 AND kind = 'hold'`,
		tenantID, bookingID); err != nil {
		return 0, nil, fmt.Errorf("release expired blocks: %w", err)
	}
	if reminders != nil {
		if err := reminders.CancelForBooking(ctx, tx, tenantID, bookingID); err != nil {
			stuck := bookingID
			return 0, &stuck, fmt.Errorf("cancel reminders for expired booking %s: %w", bookingID, err)
		}
	}
	if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "booking.expired",
		map[string]any{"project_id": projectID}); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("commit expired hold: %w", err)
	}
	return 1, nil, nil
}
