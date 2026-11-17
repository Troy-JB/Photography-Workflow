package prep

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/outbox"
)

const authorizationColumns = `id, booking_id, kind, subject, status, signed_at, note,
	version, created_at, updated_at`

type Authorization struct {
	ID        uuid.UUID  `json:"id"`
	BookingID uuid.UUID  `json:"booking_id"`
	Kind      string     `json:"kind"`
	Subject   string     `json:"subject"`
	Status    string     `json:"status"`
	SignedAt  *time.Time `json:"signed_at,omitempty"`
	Note      string     `json:"note"`
	Version   int        `json:"version"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type AuthorizationInput struct {
	Kind    string
	Subject string
	Status  string
	Note    string
}

type AuthorizationUpdate struct {
	Status *string
	Note   *string
}

type authorizationState struct {
	Status   string
	Note     string
	SignedAt *time.Time
}

func scanAuthorization(row interface{ Scan(...any) error }) (*Authorization, error) {
	var a Authorization
	if err := row.Scan(&a.ID, &a.BookingID, &a.Kind, &a.Subject, &a.Status, &a.SignedAt,
		&a.Note, &a.Version, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// nextAuthorizationState applies a status/note correction. signed_at is always
// server time and only exists while the row is signed; waived requires a note
// because it is an explicit operator decision.
func nextAuthorizationState(current authorizationState, status, note string, now time.Time) (authorizationState, error) {
	status = strings.TrimSpace(status)
	note = strings.TrimSpace(note)
	if !ValidAuthorizationStatus(status) {
		return current, fmt.Errorf("%w: status must be pending, signed or waived", ErrInvalidInput)
	}
	if len(note) > 2000 {
		return current, fmt.Errorf("%w: note is too long", ErrInvalidInput)
	}
	if status == "waived" && note == "" {
		return current, fmt.Errorf("%w: waived authorizations require a note", ErrInvalidInput)
	}
	next := authorizationState{Status: status, Note: note}
	if status == "signed" {
		at := now.UTC()
		if current.Status == "signed" && current.SignedAt != nil {
			at = *current.SignedAt
		}
		next.SignedAt = &at
	}
	return next, nil
}

func prepareAuthorizationInput(in AuthorizationInput, now time.Time) (AuthorizationInput, authorizationState, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Note = strings.TrimSpace(in.Note)
	in.Status = strings.TrimSpace(in.Status)
	if !ValidAuthorizationKind(in.Kind) {
		return in, authorizationState{}, fmt.Errorf("%w: kind must be model_release, venue_permit, usage_license or other", ErrInvalidInput)
	}
	if len(in.Subject) > 200 {
		return in, authorizationState{}, fmt.Errorf("%w: subject is too long", ErrInvalidInput)
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	state, err := nextAuthorizationState(authorizationState{Status: "pending"}, in.Status, in.Note, now)
	if err != nil {
		return in, authorizationState{}, err
	}
	return in, state, nil
}

func (s *Service) ListAuthorizations(ctx context.Context, tenantID, bookingID uuid.UUID) ([]Authorization, error) {
	items, err := withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) ([]Authorization, error) {
		if err := requireBooking(ctx, tx, tenantID, bookingID); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+authorizationColumns+` FROM booking_authorizations
			WHERE tenant_id = $1 AND booking_id = $2
			ORDER BY kind, subject, id`, tenantID, bookingID)
		if err != nil {
			return nil, fmt.Errorf("list authorizations: %w", err)
		}
		defer rows.Close()
		out := []Authorization{}
		for rows.Next() {
			a, err := scanAuthorization(rows)
			if err != nil {
				return nil, fmt.Errorf("scan authorization: %w", err)
			}
			out = append(out, *a)
		}
		return out, rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []Authorization{}
	}
	return items, nil
}

func (s *Service) CreateAuthorization(ctx context.Context, tenantID, actor, bookingID uuid.UUID, ip string, in AuthorizationInput) (*Authorization, error) {
	now := time.Now().UTC()
	in, state, err := prepareAuthorizationInput(in, now)
	if err != nil {
		return nil, err
	}
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Authorization, error) {
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		a, err := scanAuthorization(tx.QueryRow(ctx, `
			INSERT INTO booking_authorizations (tenant_id, booking_id, kind, subject, status, signed_at, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING `+authorizationColumns,
			tenantID, bookingID, in.Kind, in.Subject, state.Status, state.SignedAt, state.Note))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrAuthorizationConflict
			}
			return nil, fmt.Errorf("insert authorization: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "authorization.created", "booking_authorization",
			a.ID.String(), ip, map[string]any{
				"booking_id": bookingID, "kind": a.Kind, "status": a.Status, "version": a.Version,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "authorization.created",
			map[string]any{"authorization_id": a.ID, "kind": a.Kind, "status": a.Status}); err != nil {
			return nil, err
		}
		s.planNotices(ctx, tx, tenantID, bookingID)
		return a, nil
	})
}

func (s *Service) UpdateAuthorization(ctx context.Context, tenantID, actor, authorizationID uuid.UUID, ip string, expectedVersion int, in AuthorizationUpdate) (*Authorization, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Authorization, error) {
		bookingID, err := authorizationBooking(ctx, tx, tenantID, authorizationID)
		if err != nil {
			return nil, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		current, err := scanAuthorization(tx.QueryRow(ctx, `
			SELECT `+authorizationColumns+` FROM booking_authorizations
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, authorizationID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAuthorizationNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lock authorization: %w", err)
		}
		if current.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		status, note := current.Status, current.Note
		if in.Status != nil {
			status = *in.Status
		}
		if in.Note != nil {
			note = *in.Note
		}
		next, err := nextAuthorizationState(authorizationState{
			Status: current.Status, Note: current.Note, SignedAt: current.SignedAt,
		}, status, note, time.Now())
		if err != nil {
			return nil, err
		}
		updated, err := scanAuthorization(tx.QueryRow(ctx, `
			UPDATE booking_authorizations SET status = $3, note = $4, signed_at = $5,
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+authorizationColumns,
			authorizationID, tenantID, next.Status, next.Note, next.SignedAt))
		if err != nil {
			return nil, fmt.Errorf("update authorization: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "authorization.updated", "booking_authorization",
			authorizationID.String(), ip, map[string]any{
				"booking_id": bookingID, "kind": updated.Kind, "status": updated.Status,
				"was": current.Status, "version": updated.Version,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "authorization.updated",
			map[string]any{"authorization_id": authorizationID, "status": updated.Status}); err != nil {
			return nil, err
		}
		s.planNotices(ctx, tx, tenantID, bookingID)
		return updated, nil
	})
}

func (s *Service) DeleteAuthorization(ctx context.Context, tenantID, actor, authorizationID uuid.UUID, ip string, expectedVersion int) error {
	if expectedVersion <= 0 {
		return fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	_, err := withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (struct{}, error) {
		bookingID, err := authorizationBooking(ctx, tx, tenantID, authorizationID)
		if err != nil {
			return struct{}{}, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return struct{}{}, err
		}
		var (
			status  string
			kind    string
			version int
		)
		err = tx.QueryRow(ctx, `
			SELECT status, kind, version FROM booking_authorizations
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, authorizationID, tenantID).
			Scan(&status, &kind, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, ErrAuthorizationNotFound
		}
		if err != nil {
			return struct{}{}, fmt.Errorf("lock authorization: %w", err)
		}
		if version != expectedVersion {
			return struct{}{}, ErrVersionConflict
		}
		if status != "pending" {
			return struct{}{}, fmt.Errorf("%w: signed or waived authorizations are evidence and cannot be deleted", ErrIllegalState)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM booking_authorizations WHERE id = $1 AND tenant_id = $2`,
			authorizationID, tenantID); err != nil {
			return struct{}{}, fmt.Errorf("delete authorization: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "authorization.deleted", "booking_authorization",
			authorizationID.String(), ip, map[string]any{"booking_id": bookingID, "kind": kind}); err != nil {
			return struct{}{}, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "authorization.deleted",
			map[string]any{"authorization_id": authorizationID, "kind": kind}); err != nil {
			return struct{}{}, err
		}
		s.planNotices(ctx, tx, tenantID, bookingID)
		return struct{}{}, nil
	})
	return err
}

func authorizationBooking(ctx context.Context, tx pgx.Tx, tenantID, authorizationID uuid.UUID) (uuid.UUID, error) {
	var bookingID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT booking_id FROM booking_authorizations WHERE id = $1 AND tenant_id = $2`,
		authorizationID, tenantID).Scan(&bookingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrAuthorizationNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("load authorization booking: %w", err)
	}
	return bookingID, nil
}
