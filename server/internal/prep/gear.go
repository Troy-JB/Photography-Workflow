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

const gearColumns = `g.id, g.booking_id, g.resource_id, coalesce(r.name, ''), g.name,
	g.category, g.quantity, g.checked, g.checked_at, g.note, g.version,
	g.created_at, g.updated_at`

type GearItem struct {
	ID           uuid.UUID  `json:"id"`
	BookingID    uuid.UUID  `json:"booking_id"`
	ResourceID   *uuid.UUID `json:"resource_id,omitempty"`
	ResourceName string     `json:"resource_name,omitempty"`
	Name         string     `json:"name"`
	Category     string     `json:"category"`
	Quantity     int        `json:"quantity"`
	Checked      bool       `json:"checked"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	Note         string     `json:"note"`
	Version      int        `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type GearInput struct {
	ResourceID *uuid.UUID
	Name       string
	Category   string
	Quantity   int
	Note       string
}

type GearUpdate struct {
	Name     *string
	Category *string
	Quantity *int
	Checked  *bool
	Note     *string
}

func scanGear(row interface{ Scan(...any) error }) (*GearItem, error) {
	var g GearItem
	if err := row.Scan(&g.ID, &g.BookingID, &g.ResourceID, &g.ResourceName, &g.Name,
		&g.Category, &g.Quantity, &g.Checked, &g.CheckedAt, &g.Note, &g.Version,
		&g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}
	return &g, nil
}

func validateGear(name, category, note string, quantity int) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(name) > 120 {
		return fmt.Errorf("%w: name is too long", ErrInvalidInput)
	}
	if len(category) > 120 {
		return fmt.Errorf("%w: category is too long", ErrInvalidInput)
	}
	if len(note) > 1000 {
		return fmt.Errorf("%w: note is too long", ErrInvalidInput)
	}
	if quantity < 1 || quantity > 100 {
		return fmt.Errorf("%w: quantity must be between 1 and 100", ErrInvalidInput)
	}
	return nil
}

func (s *Service) ListGear(ctx context.Context, tenantID, bookingID uuid.UUID) ([]GearItem, error) {
	items, err := withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) ([]GearItem, error) {
		if err := requireBooking(ctx, tx, tenantID, bookingID); err != nil {
			return nil, err
		}
		return listGear(ctx, tx, tenantID, bookingID)
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []GearItem{}
	}
	return items, nil
}

func (s *Service) AddGear(ctx context.Context, tenantID, actor, bookingID uuid.UUID, ip string, in GearInput) (*GearItem, error) {
	if in.Quantity == 0 {
		in.Quantity = 1
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Category = strings.TrimSpace(in.Category)
	in.Note = strings.TrimSpace(in.Note)
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*GearItem, error) {
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		if in.ResourceID != nil {
			var resourceName string
			err := tx.QueryRow(ctx, `
				SELECT name FROM resources
				WHERE id = $1 AND tenant_id = $2 AND kind = 'equipment'`,
				*in.ResourceID, tenantID).Scan(&resourceName)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: resource_id must reference an equipment resource", ErrInvalidInput)
			}
			if err != nil {
				return nil, fmt.Errorf("load equipment resource: %w", err)
			}
			if in.Name == "" {
				in.Name = resourceName
			}
		}
		if err := validateGear(in.Name, in.Category, in.Note, in.Quantity); err != nil {
			return nil, err
		}
		item, err := scanGear(tx.QueryRow(ctx, `
			INSERT INTO shoot_gear (tenant_id, booking_id, resource_id, name, category, quantity, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING id, booking_id, resource_id, '', name, category, quantity, checked,
				checked_at, note, version, created_at, updated_at`,
			tenantID, bookingID, in.ResourceID, in.Name, in.Category, in.Quantity, in.Note))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrGearConflict
			}
			return nil, fmt.Errorf("insert gear: %w", err)
		}
		if in.ResourceID != nil {
			item.ResourceName = in.Name
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gear.created", "shoot_gear",
			item.ID.String(), ip, map[string]any{
				"booking_id": bookingID, "resource_id": in.ResourceID, "version": item.Version,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "gear.created",
			map[string]any{"gear_id": item.ID}); err != nil {
			return nil, err
		}
		return item, nil
	})
}

func (s *Service) UpdateGear(ctx context.Context, tenantID, actor, gearID uuid.UUID, ip string, expectedVersion int, in GearUpdate) (*GearItem, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*GearItem, error) {
		bookingID, err := gearBooking(ctx, tx, tenantID, gearID)
		if err != nil {
			return nil, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		current, err := scanGear(tx.QueryRow(ctx, `
			SELECT `+gearColumns+` FROM shoot_gear g
			LEFT JOIN resources r ON r.id = g.resource_id AND r.tenant_id = g.tenant_id
			WHERE g.id = $1 AND g.tenant_id = $2 FOR UPDATE OF g`, gearID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrGearNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lock gear: %w", err)
		}
		if current.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		name, category, note, quantity := current.Name, current.Category, current.Note, current.Quantity
		if in.Name != nil {
			name = strings.TrimSpace(*in.Name)
		}
		if in.Category != nil {
			category = strings.TrimSpace(*in.Category)
		}
		if in.Note != nil {
			note = strings.TrimSpace(*in.Note)
		}
		if in.Quantity != nil {
			quantity = *in.Quantity
		}
		if err := validateGear(name, category, note, quantity); err != nil {
			return nil, err
		}
		checked, checkedAt := current.Checked, current.CheckedAt
		if in.Checked != nil {
			checked = *in.Checked
		}
		switch {
		case checked && checkedAt == nil:
			now := time.Now().UTC()
			checkedAt = &now
		case !checked:
			checkedAt = nil
		}
		item, err := scanGear(tx.QueryRow(ctx, `
			UPDATE shoot_gear g SET name = $3, category = $4, quantity = $5,
				checked = $6, checked_at = $7, note = $8, version = version + 1, updated_at = now()
			WHERE g.id = $1 AND g.tenant_id = $2
			RETURNING g.id, g.booking_id, g.resource_id,
				(SELECT coalesce(name, '') FROM resources WHERE id = g.resource_id AND tenant_id = g.tenant_id),
				g.name, g.category, g.quantity, g.checked, g.checked_at, g.note, g.version,
				g.created_at, g.updated_at`,
			gearID, tenantID, name, category, quantity, checked, checkedAt, note))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrGearConflict
			}
			return nil, fmt.Errorf("update gear: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gear.updated", "shoot_gear",
			gearID.String(), ip, map[string]any{
				"booking_id": bookingID, "version": item.Version,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "gear.updated",
			map[string]any{"gear_id": gearID, "version": item.Version}); err != nil {
			return nil, err
		}
		return item, nil
	})
}

func (s *Service) DeleteGear(ctx context.Context, tenantID, actor, gearID uuid.UUID, ip string, expectedVersion int) error {
	if expectedVersion <= 0 {
		return fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	_, err := withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (struct{}, error) {
		bookingID, err := gearBooking(ctx, tx, tenantID, gearID)
		if err != nil {
			return struct{}{}, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return struct{}{}, err
		}
		var version int
		err = tx.QueryRow(ctx, `SELECT version FROM shoot_gear WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			gearID, tenantID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, ErrGearNotFound
		}
		if err != nil {
			return struct{}{}, fmt.Errorf("lock gear: %w", err)
		}
		if version != expectedVersion {
			return struct{}{}, ErrVersionConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM shoot_gear WHERE id = $1 AND tenant_id = $2`,
			gearID, tenantID); err != nil {
			return struct{}{}, fmt.Errorf("delete gear: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "gear.deleted", "shoot_gear",
			gearID.String(), ip, map[string]any{"booking_id": bookingID}); err != nil {
			return struct{}{}, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "gear.deleted",
			map[string]any{"gear_id": gearID}); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}

func listGear(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) ([]GearItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+gearColumns+` FROM shoot_gear g
		LEFT JOIN resources r ON r.id = g.resource_id AND r.tenant_id = g.tenant_id
		WHERE g.tenant_id = $1 AND g.booking_id = $2
		ORDER BY g.name, g.id`, tenantID, bookingID)
	if err != nil {
		return nil, fmt.Errorf("list gear: %w", err)
	}
	defer rows.Close()
	items := []GearItem{}
	for rows.Next() {
		item, err := scanGear(rows)
		if err != nil {
			return nil, fmt.Errorf("scan gear: %w", err)
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func gearBooking(ctx context.Context, tx pgx.Tx, tenantID, gearID uuid.UUID) (uuid.UUID, error) {
	var bookingID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT booking_id FROM shoot_gear WHERE id = $1 AND tenant_id = $2`,
		gearID, tenantID).Scan(&bookingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrGearNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("load gear booking: %w", err)
	}
	return bookingID, nil
}
