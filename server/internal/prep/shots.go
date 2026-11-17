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

const shotListColumns = `id, booking_id, title, note, version, created_at, updated_at`

const shotItemColumns = `id, shot_list_id, sort_order, category, title, description,
	reference_url, priority, done, done_at, version, created_at, updated_at`

type ShotList struct {
	ID        uuid.UUID  `json:"id"`
	BookingID uuid.UUID  `json:"booking_id"`
	Title     string     `json:"title"`
	Note      string     `json:"note"`
	Version   int        `json:"version"`
	Items     []ShotItem `json:"items"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type ShotItem struct {
	ID           uuid.UUID  `json:"id"`
	ShotListID   uuid.UUID  `json:"shot_list_id"`
	SortOrder    int        `json:"sort_order"`
	Category     string     `json:"category"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	ReferenceURL string     `json:"reference_url"`
	Priority     string     `json:"priority"`
	Done         bool       `json:"done"`
	DoneAt       *time.Time `json:"done_at,omitempty"`
	Version      int        `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type ShotListInput struct {
	Title string
	Note  string
}

type ShotListUpdate struct {
	Title *string
	Note  *string
}

type ShotItemInput struct {
	Category     string
	Title        string
	Description  string
	ReferenceURL string
	Priority     string
}

type ShotItemUpdate struct {
	Category     *string
	Title        *string
	Description  *string
	ReferenceURL *string
	Priority     *string
	Done         *bool
}

func scanShotList(row interface{ Scan(...any) error }) (*ShotList, error) {
	var l ShotList
	if err := row.Scan(&l.ID, &l.BookingID, &l.Title, &l.Note, &l.Version,
		&l.CreatedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	l.Items = []ShotItem{}
	return &l, nil
}

func scanShotItem(row interface{ Scan(...any) error }) (*ShotItem, error) {
	var i ShotItem
	if err := row.Scan(&i.ID, &i.ShotListID, &i.SortOrder, &i.Category, &i.Title,
		&i.Description, &i.ReferenceURL, &i.Priority, &i.Done, &i.DoneAt,
		&i.Version, &i.CreatedAt, &i.UpdatedAt); err != nil {
		return nil, err
	}
	return &i, nil
}

func prepareShotListInput(in ShotListInput) (ShotListInput, error) {
	title, err := trimMax(in.Title, 200, "title")
	if err != nil {
		return in, err
	}
	note, err := trimMax(in.Note, 2000, "note")
	if err != nil {
		return in, err
	}
	in.Title, in.Note = title, note
	return in, nil
}

func validateShotItem(category, title, description, referenceURL, priority string) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if len(title) > 200 {
		return fmt.Errorf("%w: title is too long", ErrInvalidInput)
	}
	if len(category) > 120 {
		return fmt.Errorf("%w: category is too long", ErrInvalidInput)
	}
	if len(description) > 4000 {
		return fmt.Errorf("%w: description is too long", ErrInvalidInput)
	}
	if err := validateReferenceURL(referenceURL); err != nil {
		return err
	}
	if !ValidPriority(priority) {
		return fmt.Errorf("%w: priority must be low, normal or high", ErrInvalidInput)
	}
	return nil
}

func prepareShotItemInput(in ShotItemInput) (ShotItemInput, error) {
	in.Category = strings.TrimSpace(in.Category)
	in.Title = strings.TrimSpace(in.Title)
	in.Description = strings.TrimSpace(in.Description)
	in.ReferenceURL = strings.TrimSpace(in.ReferenceURL)
	in.Priority = strings.TrimSpace(in.Priority)
	if in.Priority == "" {
		in.Priority = "normal"
	}
	if err := validateShotItem(in.Category, in.Title, in.Description, in.ReferenceURL, in.Priority); err != nil {
		return in, err
	}
	return in, nil
}

func (s *Service) GetShotList(ctx context.Context, tenantID, bookingID uuid.UUID) (*ShotList, error) {
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ShotList, error) {
		if err := requireBooking(ctx, tx, tenantID, bookingID); err != nil {
			return nil, err
		}
		list, err := scanShotList(tx.QueryRow(ctx, `
			SELECT `+shotListColumns+` FROM shot_lists WHERE tenant_id = $1 AND booking_id = $2`,
			tenantID, bookingID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrShotListNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get shot list: %w", err)
		}
		items, err := listShotItems(ctx, tx, tenantID, list.ID)
		if err != nil {
			return nil, err
		}
		list.Items = items
		return list, nil
	})
}

func (s *Service) CreateShotList(ctx context.Context, tenantID, actor, bookingID uuid.UUID, ip string, in ShotListInput) (*ShotList, error) {
	in, err := prepareShotListInput(in)
	if err != nil {
		return nil, err
	}
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ShotList, error) {
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		list, err := scanShotList(tx.QueryRow(ctx, `
			INSERT INTO shot_lists (tenant_id, booking_id, title, note)
			VALUES ($1,$2,$3,$4)
			RETURNING `+shotListColumns, tenantID, bookingID, in.Title, in.Note))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrShotListExists
			}
			return nil, fmt.Errorf("insert shot list: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "shot_list.created", "shot_list",
			list.ID.String(), ip, map[string]any{"booking_id": bookingID, "version": list.Version}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "shot_list.created",
			map[string]any{"shot_list_id": list.ID, "version": list.Version}); err != nil {
			return nil, err
		}
		return list, nil
	})
}

func (s *Service) UpdateShotList(ctx context.Context, tenantID, actor, shotListID uuid.UUID, ip string, expectedVersion int, in ShotListUpdate) (*ShotList, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ShotList, error) {
		bookingID, err := shotListBooking(ctx, tx, tenantID, shotListID)
		if err != nil {
			return nil, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		current, err := scanShotList(tx.QueryRow(ctx, `
			SELECT `+shotListColumns+` FROM shot_lists
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, shotListID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrShotListNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lock shot list: %w", err)
		}
		if current.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		title, note := current.Title, current.Note
		if in.Title != nil {
			title, err = trimMax(*in.Title, 200, "title")
			if err != nil {
				return nil, err
			}
		}
		if in.Note != nil {
			note, err = trimMax(*in.Note, 2000, "note")
			if err != nil {
				return nil, err
			}
		}
		updated, err := scanShotList(tx.QueryRow(ctx, `
			UPDATE shot_lists SET title = $3, note = $4, version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+shotListColumns, shotListID, tenantID, title, note))
		if err != nil {
			return nil, fmt.Errorf("update shot list: %w", err)
		}
		items, err := listShotItems(ctx, tx, tenantID, shotListID)
		if err != nil {
			return nil, err
		}
		updated.Items = items
		if err := audit.Write(ctx, tx, tenantID, &actor, "shot_list.updated", "shot_list",
			shotListID.String(), ip, map[string]any{"booking_id": bookingID, "version": updated.Version}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "shot_list.updated",
			map[string]any{"shot_list_id": shotListID, "version": updated.Version}); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

func (s *Service) AddShotItem(ctx context.Context, tenantID, actor, shotListID uuid.UUID, ip string, in ShotItemInput) (*ShotItem, error) {
	in, err := prepareShotItemInput(in)
	if err != nil {
		return nil, err
	}
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ShotItem, error) {
		bookingID, err := shotListBooking(ctx, tx, tenantID, shotListID)
		if err != nil {
			return nil, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM shot_lists WHERE id = $1 AND tenant_id = $2`,
			shotListID, tenantID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrShotListNotFound
			}
			return nil, fmt.Errorf("check shot list: %w", err)
		}
		var next int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(sort_order), -1) + 1 FROM shot_items
			WHERE tenant_id = $1 AND shot_list_id = $2`, tenantID, shotListID).Scan(&next); err != nil {
			return nil, fmt.Errorf("next shot order: %w", err)
		}
		item, err := scanShotItem(tx.QueryRow(ctx, `
			INSERT INTO shot_items (tenant_id, shot_list_id, sort_order, category, title,
				description, reference_url, priority)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING `+shotItemColumns,
			tenantID, shotListID, next, in.Category, in.Title, in.Description,
			in.ReferenceURL, in.Priority))
		if err != nil {
			return nil, fmt.Errorf("insert shot item: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "shot_item.created", "shot_item",
			item.ID.String(), ip, map[string]any{
				"booking_id": bookingID, "shot_list_id": shotListID, "version": item.Version,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "shot_item.created",
			map[string]any{"shot_item_id": item.ID, "shot_list_id": shotListID}); err != nil {
			return nil, err
		}
		return item, nil
	})
}

func (s *Service) UpdateShotItem(ctx context.Context, tenantID, actor, shotItemID uuid.UUID, ip string, expectedVersion int, in ShotItemUpdate) (*ShotItem, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ShotItem, error) {
		bookingID, err := shotItemBooking(ctx, tx, tenantID, shotItemID)
		if err != nil {
			return nil, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return nil, err
		}
		current, err := scanShotItem(tx.QueryRow(ctx, `
			SELECT `+shotItemColumns+` FROM shot_items
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, shotItemID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrShotItemNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lock shot item: %w", err)
		}
		if current.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		category, title, description := current.Category, current.Title, current.Description
		referenceURL, priority := current.ReferenceURL, current.Priority
		if in.Category != nil {
			category = strings.TrimSpace(*in.Category)
		}
		if in.Title != nil {
			title = strings.TrimSpace(*in.Title)
		}
		if in.Description != nil {
			description = strings.TrimSpace(*in.Description)
		}
		if in.ReferenceURL != nil {
			referenceURL = strings.TrimSpace(*in.ReferenceURL)
		}
		if in.Priority != nil {
			priority = strings.TrimSpace(*in.Priority)
		}
		if err := validateShotItem(category, title, description, referenceURL, priority); err != nil {
			return nil, err
		}
		done, doneAt := current.Done, current.DoneAt
		if in.Done != nil {
			done = *in.Done
		}
		switch {
		case done && doneAt == nil:
			now := time.Now().UTC()
			doneAt = &now
		case !done:
			doneAt = nil
		}
		item, err := scanShotItem(tx.QueryRow(ctx, `
			UPDATE shot_items SET category = $3, title = $4, description = $5,
				reference_url = $6, priority = $7, done = $8, done_at = $9,
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+shotItemColumns,
			shotItemID, tenantID, category, title, description, referenceURL, priority, done, doneAt))
		if err != nil {
			return nil, fmt.Errorf("update shot item: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "shot_item.updated", "shot_item",
			shotItemID.String(), ip, map[string]any{
				"booking_id": bookingID, "shot_list_id": current.ShotListID, "version": item.Version,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "shot_item.updated",
			map[string]any{"shot_item_id": shotItemID, "shot_list_id": current.ShotListID}); err != nil {
			return nil, err
		}
		return item, nil
	})
}

func (s *Service) DeleteShotItem(ctx context.Context, tenantID, actor, shotItemID uuid.UUID, ip string, expectedVersion int) error {
	if expectedVersion <= 0 {
		return fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	_, err := withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (struct{}, error) {
		bookingID, err := shotItemBooking(ctx, tx, tenantID, shotItemID)
		if err != nil {
			return struct{}{}, err
		}
		if _, err := s.lockBooking(ctx, tx, tenantID, bookingID, true); err != nil {
			return struct{}{}, err
		}
		var (
			listID  uuid.UUID
			version int
		)
		err = tx.QueryRow(ctx, `
			SELECT shot_list_id, version FROM shot_items
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, shotItemID, tenantID).Scan(&listID, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, ErrShotItemNotFound
		}
		if err != nil {
			return struct{}{}, fmt.Errorf("lock shot item: %w", err)
		}
		if version != expectedVersion {
			return struct{}{}, ErrVersionConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM shot_items WHERE id = $1 AND tenant_id = $2`,
			shotItemID, tenantID); err != nil {
			return struct{}{}, fmt.Errorf("delete shot item: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "shot_item.deleted", "shot_item",
			shotItemID.String(), ip, map[string]any{"booking_id": bookingID, "shot_list_id": listID}); err != nil {
			return struct{}{}, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "shot_item.deleted",
			map[string]any{"shot_item_id": shotItemID, "shot_list_id": listID}); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}

func listShotItems(ctx context.Context, tx pgx.Tx, tenantID, shotListID uuid.UUID) ([]ShotItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+shotItemColumns+` FROM shot_items
		WHERE tenant_id = $1 AND shot_list_id = $2
		ORDER BY sort_order, id`, tenantID, shotListID)
	if err != nil {
		return nil, fmt.Errorf("list shot items: %w", err)
	}
	defer rows.Close()
	items := []ShotItem{}
	for rows.Next() {
		item, err := scanShotItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan shot item: %w", err)
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func requireBooking(ctx context.Context, tx pgx.Tx, tenantID, bookingID uuid.UUID) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM bookings WHERE id = $1 AND tenant_id = $2`,
		bookingID, tenantID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBookingNotFound
	}
	if err != nil {
		return fmt.Errorf("check booking: %w", err)
	}
	return nil
}

func shotListBooking(ctx context.Context, tx pgx.Tx, tenantID, shotListID uuid.UUID) (uuid.UUID, error) {
	var bookingID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT booking_id FROM shot_lists WHERE id = $1 AND tenant_id = $2`,
		shotListID, tenantID).Scan(&bookingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrShotListNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("load shot list booking: %w", err)
	}
	return bookingID, nil
}

func shotItemBooking(ctx context.Context, tx pgx.Tx, tenantID, shotItemID uuid.UUID) (uuid.UUID, error) {
	var bookingID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT sl.booking_id FROM shot_items si
		JOIN shot_lists sl ON sl.id = si.shot_list_id AND sl.tenant_id = si.tenant_id
		WHERE si.id = $1 AND si.tenant_id = $2`, shotItemID, tenantID).Scan(&bookingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrShotItemNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("load shot item booking: %w", err)
	}
	return bookingID, nil
}
