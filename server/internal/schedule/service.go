package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/outbox"
)

var (
	ErrResourceNotFound    = errors.New("resource not found")
	ErrResourceConflict    = errors.New("resource name already exists")
	ErrBookingNotFound     = errors.New("booking not found")
	ErrBlockNotFound       = errors.New("resource block not found")
	ErrProjectNotFound     = errors.New("project not found")
	ErrActiveBooking       = errors.New("project already has an active booking")
	ErrSlotTaken           = errors.New("time slot is already occupied")
	ErrOutsideAvailability = errors.New("time slot is outside resource availability")
	ErrDepositRequired     = errors.New("deposit must be paid before confirming a booking")
	ErrIllegalState        = errors.New("illegal booking state")
	ErrVersionConflict     = errors.New("booking or resource version conflict")
	ErrHoldExpired         = errors.New("booking hold has expired")
	ErrInvalidInput        = errors.New("invalid scheduling input")
)

type Service struct {
	pool    *pgxpool.Pool
	loc     *time.Location
	holdTTL time.Duration
	alerts  *alert.Service
	notices *notice.Service
	log     *slog.Logger
}

func NewService(pool *pgxpool.Pool, loc *time.Location, holdTTL time.Duration, alerts *alert.Service, notices *notice.Service, log *slog.Logger) *Service {
	if loc == nil {
		loc = time.UTC
	}
	if holdTTL <= 0 {
		holdTTL = 30 * time.Minute
	}
	return &Service{pool: pool, loc: loc, holdTTL: holdTTL, alerts: alerts, notices: notices, log: log}
}

// planNotices refreshes reminders from inside the caller's transaction. A
// planning failure is logged but never rolls back the booking or its payment.
func (s *Service) planNotices(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) {
	if s.notices == nil {
		return
	}
	if err := s.notices.PlanBooking(ctx, tx, tenantID, bookingID); err != nil {
		s.log.Warn("plan booking reminders", "booking_id", bookingID, "err", err)
	}
}

type Resource struct {
	ID                  uuid.UUID          `json:"id"`
	Kind                string             `json:"kind"`
	Name                string             `json:"name"`
	Active              bool               `json:"active"`
	BufferBeforeMinutes int                `json:"buffer_before_minutes"`
	BufferAfterMinutes  int                `json:"buffer_after_minutes"`
	Notes               string             `json:"notes"`
	Version             int                `json:"version"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	Rules               []AvailabilityRule `json:"rules,omitempty"`
}

type ResourceInput struct {
	Kind                string
	Name                string
	Active              *bool
	BufferBeforeMinutes int
	BufferAfterMinutes  int
	Notes               string
}

type ResourceUpdate struct {
	Kind                *string
	Name                *string
	Active              *bool
	BufferBeforeMinutes *int
	BufferAfterMinutes  *int
	Notes               *string
}

const resourceColumns = `id, kind, name, active, buffer_before_minutes, buffer_after_minutes,
	notes, version, created_at, updated_at`

func scanResource(row interface{ Scan(...any) error }) (*Resource, error) {
	var r Resource
	if err := row.Scan(&r.ID, &r.Kind, &r.Name, &r.Active, &r.BufferBeforeMinutes,
		&r.BufferAfterMinutes, &r.Notes, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func prepareResourceInput(in ResourceInput) (ResourceInput, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Name = strings.TrimSpace(in.Name)
	in.Notes = strings.TrimSpace(in.Notes)
	if !ValidResourceKind(in.Kind) {
		return in, fmt.Errorf("%w: kind must be photographer, studio, makeup, assistant or equipment", ErrInvalidInput)
	}
	if in.Name == "" {
		return in, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(in.Name) > 120 || len(in.Notes) > 2000 {
		return in, fmt.Errorf("%w: name or notes is too long", ErrInvalidInput)
	}
	if in.BufferBeforeMinutes < 0 || in.BufferBeforeMinutes > 1440 ||
		in.BufferAfterMinutes < 0 || in.BufferAfterMinutes > 1440 {
		return in, fmt.Errorf("%w: buffers must be between 0 and 1440 minutes", ErrInvalidInput)
	}
	return in, nil
}

func (s *Service) CreateResource(ctx context.Context, tenantID, actor uuid.UUID, ip string, in ResourceInput) (*Resource, error) {
	in, err := prepareResourceInput(in)
	if err != nil {
		return nil, err
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Resource, error) {
		row := tx.QueryRow(ctx, `
			INSERT INTO resources (tenant_id, kind, name, active, buffer_before_minutes,
				buffer_after_minutes, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING `+resourceColumns,
			tenantID, in.Kind, in.Name, active, in.BufferBeforeMinutes, in.BufferAfterMinutes, in.Notes)
		r, err := scanResource(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, fmt.Errorf("%w: %q", ErrResourceConflict, in.Name)
			}
			return nil, fmt.Errorf("insert resource: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "resource.created", "resource", r.ID.String(), ip,
			map[string]any{"kind": r.Kind, "name": r.Name}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "resource", r.ID.String(), "resource.created",
			map[string]any{"kind": r.Kind, "name": r.Name}); err != nil {
			return nil, err
		}
		return r, nil
	})
}

func (s *Service) UpdateResource(ctx context.Context, tenantID, actor, resourceID uuid.UUID, ip string, expectedVersion int, in ResourceUpdate) (*Resource, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if in.Kind != nil {
		kind := strings.TrimSpace(*in.Kind)
		if !ValidResourceKind(kind) {
			return nil, fmt.Errorf("%w: unknown resource kind %q", ErrInvalidInput, kind)
		}
		in.Kind = &kind
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 120 {
			return nil, fmt.Errorf("%w: name is required and must be at most 120 characters", ErrInvalidInput)
		}
		in.Name = &name
	}
	if in.Notes != nil {
		notes := strings.TrimSpace(*in.Notes)
		if len(notes) > 2000 {
			return nil, fmt.Errorf("%w: notes is too long", ErrInvalidInput)
		}
		in.Notes = &notes
	}
	if in.BufferBeforeMinutes != nil && (*in.BufferBeforeMinutes < 0 || *in.BufferBeforeMinutes > 1440) {
		return nil, fmt.Errorf("%w: buffer_before_minutes must be 0..1440", ErrInvalidInput)
	}
	if in.BufferAfterMinutes != nil && (*in.BufferAfterMinutes < 0 || *in.BufferAfterMinutes > 1440) {
		return nil, fmt.Errorf("%w: buffer_after_minutes must be 0..1440", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Resource, error) {
		row := tx.QueryRow(ctx, `
			UPDATE resources SET
				kind = COALESCE($3::text, kind),
				name = COALESCE($4::text, name),
				active = COALESCE($5::boolean, active),
				buffer_before_minutes = COALESCE($6::int, buffer_before_minutes),
				buffer_after_minutes = COALESCE($7::int, buffer_after_minutes),
				notes = COALESCE($8::text, notes),
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND version = $9
			RETURNING `+resourceColumns,
			resourceID, tenantID, in.Kind, in.Name, in.Active, in.BufferBeforeMinutes,
			in.BufferAfterMinutes, in.Notes, expectedVersion)
		r, err := scanResource(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, s.resourceWriteMiss(ctx, tx, tenantID, resourceID)
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrResourceConflict
			}
			return nil, fmt.Errorf("update resource: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "resource.updated", "resource", r.ID.String(), ip,
			map[string]any{"version": r.Version}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "resource", r.ID.String(), "resource.updated",
			map[string]any{"version": r.Version}); err != nil {
			return nil, err
		}
		return r, nil
	})
}

func (s *Service) resourceWriteMiss(ctx context.Context, tx pgx.Tx, tenantID, resourceID uuid.UUID) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM resources WHERE id = $1 AND tenant_id = $2`,
		resourceID, tenantID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrResourceNotFound
		}
		return fmt.Errorf("check resource: %w", err)
	}
	return ErrVersionConflict
}

func (s *Service) SetAvailabilityRules(ctx context.Context, tenantID, actor, resourceID uuid.UUID, ip string, expectedVersion int, rules []AvailabilityRule) (*Resource, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if err := ValidateRules(rules); err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Resource, error) {
		var current int
		if err := tx.QueryRow(ctx, `SELECT version FROM resources WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			resourceID, tenantID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrResourceNotFound
			}
			return nil, fmt.Errorf("lock resource: %w", err)
		}
		if current != expectedVersion {
			return nil, ErrVersionConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM availability_rules WHERE tenant_id = $1 AND resource_id = $2`,
			tenantID, resourceID); err != nil {
			return nil, fmt.Errorf("clear availability rules: %w", err)
		}
		for _, rule := range rules {
			if _, err := tx.Exec(ctx, `
				INSERT INTO availability_rules (tenant_id, resource_id, weekday, start_minute, end_minute)
				VALUES ($1,$2,$3,$4,$5)`,
				tenantID, resourceID, rule.Weekday, rule.StartMinute, rule.EndMinute); err != nil {
				return nil, fmt.Errorf("insert availability rule: %w", err)
			}
		}
		row := tx.QueryRow(ctx, `
			UPDATE resources SET version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+resourceColumns, resourceID, tenantID)
		r, err := scanResource(row)
		if err != nil {
			return nil, fmt.Errorf("bump resource version: %w", err)
		}
		loaded, err := loadRules(ctx, tx, tenantID, resourceID)
		if err != nil {
			return nil, err
		}
		r.Rules = loaded
		if err := audit.Write(ctx, tx, tenantID, &actor, "resource.availability_updated", "resource",
			resourceID.String(), ip, map[string]any{"rules": len(rules), "version": r.Version}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "resource", resourceID.String(), "resource.availability_updated",
			map[string]any{"rules": len(rules), "version": r.Version}); err != nil {
			return nil, err
		}
		return r, nil
	})
}

func (s *Service) GetResource(ctx context.Context, tenantID, resourceID uuid.UUID) (*Resource, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Resource, error) {
		row := tx.QueryRow(ctx, `SELECT `+resourceColumns+` FROM resources WHERE id = $1 AND tenant_id = $2`,
			resourceID, tenantID)
		r, err := scanResource(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrResourceNotFound
			}
			return nil, fmt.Errorf("get resource: %w", err)
		}
		rules, err := loadRules(ctx, tx, tenantID, resourceID)
		if err != nil {
			return nil, err
		}
		r.Rules = rules
		return r, nil
	})
}

func (s *Service) ListResources(ctx context.Context, tenantID uuid.UUID, kind string, active *bool, limit, offset int) ([]Resource, int, error) {
	if kind != "" && !ValidResourceKind(kind) {
		return nil, 0, fmt.Errorf("%w: unknown resource kind %q", ErrInvalidInput, kind)
	}
	var (
		items []Resource
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const where = `tenant_id = $1 AND ($2::text = '' OR kind = $2)
			AND ($3::boolean IS NULL OR active = $3)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM resources WHERE `+where,
			tenantID, kind, active).Scan(&total); err != nil {
			return fmt.Errorf("count resources: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+resourceColumns+` FROM resources WHERE `+where+`
			ORDER BY kind, name LIMIT $4 OFFSET $5`, tenantID, kind, active, limit, offset)
		if err != nil {
			return fmt.Errorf("list resources: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanResource(rows)
			if err != nil {
				return fmt.Errorf("scan resource: %w", err)
			}
			items = append(items, *r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Resource{}
	}
	return items, total, nil
}

func loadRules(ctx context.Context, tx pgx.Tx, tenantID, resourceID uuid.UUID) ([]AvailabilityRule, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, weekday, start_minute, end_minute FROM availability_rules
		WHERE tenant_id = $1 AND resource_id = $2 ORDER BY weekday, start_minute`,
		tenantID, resourceID)
	if err != nil {
		return nil, fmt.Errorf("load availability rules: %w", err)
	}
	defer rows.Close()
	var out []AvailabilityRule
	for rows.Next() {
		var rule AvailabilityRule
		if err := rows.Scan(&rule.ID, &rule.Weekday, &rule.StartMinute, &rule.EndMinute); err != nil {
			return nil, fmt.Errorf("scan availability rule: %w", err)
		}
		out = append(out, rule)
	}
	if out == nil {
		out = []AvailabilityRule{}
	}
	return out, rows.Err()
}

type BusyInterval struct {
	StartsAt  time.Time  `json:"starts_at"`
	EndsAt    time.Time  `json:"ends_at"`
	Kind      string     `json:"kind"`
	BookingID *uuid.UUID `json:"booking_id,omitempty"`
}

type ResourceAvailability struct {
	ResourceID uuid.UUID      `json:"resource_id"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Busy       []BusyInterval `json:"busy"`
}

type AvailabilityResponse struct {
	From      time.Time              `json:"from"`
	To        time.Time              `json:"to"`
	Resources []ResourceAvailability `json:"resources"`
}

// CheckAvailability lists busy intervals per active resource in a bounded
// window. It is a read-side view; the exclusion constraint remains the arbiter
// when a hold is actually created.
func (s *Service) CheckAvailability(ctx context.Context, tenantID uuid.UUID, kind string, from, to time.Time) (*AvailabilityResponse, error) {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return nil, fmt.Errorf("%w: from and to must form a positive window", ErrInvalidInput)
	}
	if to.Sub(from) > 31*24*time.Hour {
		return nil, fmt.Errorf("%w: availability window may not exceed 31 days", ErrInvalidInput)
	}
	if kind != "" && !ValidResourceKind(kind) {
		return nil, fmt.Errorf("%w: unknown resource kind %q", ErrInvalidInput, kind)
	}
	out := &AvailabilityResponse{From: from.UTC(), To: to.UTC()}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, kind, name FROM resources
			WHERE tenant_id = $1 AND active AND ($2::text = '' OR kind = $2)
			ORDER BY kind, name`, tenantID, kind)
		if err != nil {
			return fmt.Errorf("list availability resources: %w", err)
		}
		index := map[uuid.UUID]int{}
		for rows.Next() {
			var ra ResourceAvailability
			if err := rows.Scan(&ra.ResourceID, &ra.Kind, &ra.Name); err != nil {
				rows.Close()
				return fmt.Errorf("scan availability resource: %w", err)
			}
			ra.Busy = []BusyInterval{}
			index[ra.ResourceID] = len(out.Resources)
			out.Resources = append(out.Resources, ra)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.Resources) == 0 {
			return nil
		}
		blockRows, err := tx.Query(ctx, `
			SELECT resource_id, lower(during), upper(during), kind, booking_id
			FROM resource_blocks
			WHERE tenant_id = $1 AND during && tstzrange($2, $3, '[)')
			ORDER BY lower(during)`, tenantID, from, to)
		if err != nil {
			return fmt.Errorf("list busy intervals: %w", err)
		}
		defer blockRows.Close()
		for blockRows.Next() {
			var (
				resourceID uuid.UUID
				start, end time.Time
				blockKind  string
				bookingID  *uuid.UUID
			)
			if err := blockRows.Scan(&resourceID, &start, &end, &blockKind, &bookingID); err != nil {
				return fmt.Errorf("scan busy interval: %w", err)
			}
			if i, ok := index[resourceID]; ok {
				out.Resources[i].Busy = append(out.Resources[i].Busy, BusyInterval{
					StartsAt: start.UTC(), EndsAt: end.UTC(), Kind: blockKind, BookingID: bookingID,
				})
			}
		}
		return blockRows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out.Resources == nil {
		out.Resources = []ResourceAvailability{}
	}
	return out, nil
}
