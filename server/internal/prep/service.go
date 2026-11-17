package prep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/notice"
)

var (
	ErrBookingNotFound       = errors.New("booking not found")
	ErrIllegalState          = errors.New("booking is not writable")
	ErrShotListNotFound      = errors.New("shot list not found")
	ErrShotListExists        = errors.New("booking already has a shot list")
	ErrShotItemNotFound      = errors.New("shot item not found")
	ErrGearNotFound          = errors.New("gear item not found")
	ErrGearConflict          = errors.New("gear name already exists on this booking")
	ErrAuthorizationNotFound = errors.New("authorization not found")
	ErrAuthorizationConflict = errors.New("authorization already exists on this booking")
	ErrCallSheetNotFound     = errors.New("call sheet not found")
	ErrSnapshotCorrupt       = errors.New("call sheet snapshot does not match its hash")
	ErrVersionConflict       = errors.New("version conflict")
	ErrInvalidInput          = errors.New("invalid shoot preparation input")
)

const (
	StatusHeld      = "held"
	StatusConfirmed = "confirmed"
)

// writableBookingStatus is the preparation window: a lapsed, cancelled or
// expired booking keeps its history but accepts no further writes.
func writableBookingStatus(status string) bool {
	return status == StatusHeld || status == StatusConfirmed
}

type Service struct {
	pool    *pgxpool.Pool
	loc     *time.Location
	notices *notice.Service
	log     *slog.Logger
}

func NewService(pool *pgxpool.Pool, loc *time.Location, notices *notice.Service, log *slog.Logger) *Service {
	if loc == nil {
		loc = time.UTC
	}
	return &Service{pool: pool, loc: loc, notices: notices, log: log}
}

type bookingMeta struct {
	ProjectID uuid.UUID
	Status    string
}

// lockBooking serializes preparation writes against cancellation/expiry. The
// booking is the only lock prep takes, so it cannot form a cycle with the
// scheduling project -> booking -> blocks order.
func (s *Service) lockBooking(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID, writable bool) (bookingMeta, error) {
	var meta bookingMeta
	err := tx.QueryRow(ctx, `
		SELECT project_id, status FROM bookings
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, bookingID, tenantID).
		Scan(&meta.ProjectID, &meta.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return meta, ErrBookingNotFound
	}
	if err != nil {
		return meta, fmt.Errorf("lock booking: %w", err)
	}
	if writable && !writableBookingStatus(meta.Status) {
		return meta, fmt.Errorf("%w: booking is %s", ErrIllegalState, meta.Status)
	}
	return meta, nil
}

func (s *Service) planNotices(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) {
	if s.notices == nil {
		return
	}
	if err := s.notices.PlanBooking(ctx, tx, tenantID, bookingID); err != nil && s.log != nil {
		s.log.Warn("plan booking reminders", "booking_id", bookingID, "err", err)
	}
}

func trimMax(raw string, max int, field string) (string, error) {
	v := strings.TrimSpace(raw)
	if len(v) > max {
		return "", fmt.Errorf("%w: %s is too long", ErrInvalidInput, field)
	}
	return v, nil
}

func validateReferenceURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if len(raw) > 1000 {
		return fmt.Errorf("%w: reference_url is too long", ErrInvalidInput)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w: reference_url must be an http(s) URL", ErrInvalidInput)
	}
	return nil
}

func ValidPriority(v string) bool {
	switch v {
	case "low", "normal", "high":
		return true
	}
	return false
}

func ValidAuthorizationKind(v string) bool {
	switch v {
	case "model_release", "venue_permit", "usage_license", "other":
		return true
	}
	return false
}

func ValidAuthorizationStatus(v string) bool {
	switch v {
	case "pending", "signed", "waived":
		return true
	}
	return false
}

// withTenant is a small alias so every prep write commits its audit and outbox
// rows in the same transaction as the business change.
func withTenantResult[T any](ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, fn func(pgx.Tx) (T, error)) (T, error) {
	return db.WithTenantResult(ctx, pool, tenantID, fn)
}
