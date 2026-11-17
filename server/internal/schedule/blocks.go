package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
)

type Block struct {
	ID         uuid.UUID  `json:"id"`
	ResourceID uuid.UUID  `json:"resource_id"`
	BookingID  *uuid.UUID `json:"booking_id,omitempty"`
	Kind       string     `json:"kind"`
	StartsAt   time.Time  `json:"starts_at"`
	EndsAt     time.Time  `json:"ends_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Note       string     `json:"note"`
	CreatedAt  time.Time  `json:"created_at"`
}

type BlockInput struct {
	Kind     string
	StartsAt time.Time
	EndsAt   time.Time
	Note     string
}

// CreateBlock records an operator-owned manual block or closure. Hold and
// booking blocks can only be produced by the booking aggregate.
func (s *Service) CreateBlock(ctx context.Context, tenantID, actor, resourceID uuid.UUID, ip string, in BlockInput) (*Block, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	if !ValidManualBlockKind(in.Kind) {
		return nil, fmt.Errorf("%w: kind must be manual or closure", ErrInvalidInput)
	}
	if err := ValidateWindow(in.StartsAt, in.EndsAt, s.loc); err != nil {
		return nil, err
	}
	in.Note = strings.TrimSpace(in.Note)
	if len(in.Note) > 2000 {
		return nil, fmt.Errorf("%w: note is too long", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Block, error) {
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM resources WHERE id = $1 AND tenant_id = $2`,
			resourceID, tenantID).Scan(&name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrResourceNotFound
			}
			return nil, fmt.Errorf("load resource: %w", err)
		}
		blockID, err := insertBlock(ctx, tx, tenantID, resourceID, name, nil, in.Kind,
			in.StartsAt, in.EndsAt, nil, in.Note, &actor)
		if err != nil {
			return nil, err
		}
		var block Block
		if err := tx.QueryRow(ctx, `
			SELECT id, resource_id, booking_id, kind, lower(during), upper(during), expires_at, note, created_at
			FROM resource_blocks WHERE id = $1 AND tenant_id = $2`,
			blockID, tenantID).
			Scan(&block.ID, &block.ResourceID, &block.BookingID, &block.Kind,
				&block.StartsAt, &block.EndsAt, &block.ExpiresAt, &block.Note, &block.CreatedAt); err != nil {
			return nil, fmt.Errorf("load created block: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "resource.block_created", "resource_block",
			block.ID.String(), ip, map[string]any{"resource_id": resourceID, "kind": in.Kind,
				"starts_at": in.StartsAt.UTC(), "ends_at": in.EndsAt.UTC()}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "resource", resourceID.String(), "resource.block_created",
			map[string]any{"block_id": block.ID, "kind": in.Kind}); err != nil {
			return nil, err
		}
		return &block, nil
	})
}

func (s *Service) ListBlocks(ctx context.Context, tenantID, resourceID uuid.UUID, from, to time.Time) ([]Block, error) {
	if from.IsZero() {
		from = time.Now().UTC()
	}
	if to.IsZero() {
		to = from.Add(30 * 24 * time.Hour)
	}
	if !to.After(from) {
		return nil, fmt.Errorf("%w: from must be before to", ErrInvalidInput)
	}
	if to.Sub(from) > 366*24*time.Hour {
		return nil, fmt.Errorf("%w: block window may not exceed 366 days", ErrInvalidInput)
	}
	var items []Block
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM resources WHERE id = $1 AND tenant_id = $2`,
			resourceID, tenantID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrResourceNotFound
			}
			return fmt.Errorf("load resource: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT id, resource_id, booking_id, kind, lower(during), upper(during), expires_at, note, created_at
			FROM resource_blocks
			WHERE tenant_id = $1 AND resource_id = $2 AND during && tstzrange($3, $4, '[)')
			ORDER BY lower(during)`, tenantID, resourceID, from, to)
		if err != nil {
			return fmt.Errorf("list blocks: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b Block
			if err := rows.Scan(&b.ID, &b.ResourceID, &b.BookingID, &b.Kind,
				&b.StartsAt, &b.EndsAt, &b.ExpiresAt, &b.Note, &b.CreatedAt); err != nil {
				return fmt.Errorf("scan block: %w", err)
			}
			items = append(items, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []Block{}
	}
	return items, nil
}

func (s *Service) DeleteBlock(ctx context.Context, tenantID, actor, resourceID, blockID uuid.UUID, ip string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var kind string
		var start, end time.Time
		err := tx.QueryRow(ctx, `
			DELETE FROM resource_blocks
			WHERE id = $1 AND tenant_id = $2 AND resource_id = $3 AND kind IN ('manual','closure')
			RETURNING kind, lower(during), upper(during)`, blockID, tenantID, resourceID).Scan(&kind, &start, &end)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBlockNotFound
			}
			return fmt.Errorf("delete block: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "resource.block_deleted", "resource_block",
			blockID.String(), ip, map[string]any{"resource_id": resourceID, "kind": kind,
				"starts_at": start.UTC(), "ends_at": end.UTC()}); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, tenantID, "resource", resourceID.String(), "resource.block_deleted",
			map[string]any{"block_id": blockID, "kind": kind})
	})
}
