package quote

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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
)

var (
	ErrNotFound        = errors.New("quote not found")
	ErrProjectNotFound = errors.New("project not found")
	ErrVersionConflict = errors.New("quote version conflict")
	ErrIllegalState    = errors.New("illegal quote state")
	ErrInvalidInput    = errors.New("invalid quote input")
	ErrDraftExists     = errors.New("a draft quote already exists")
	ErrActiveContract  = errors.New("project has an active contract")
	ErrExpired         = errors.New("quote has expired")
	ErrProjectClosed   = errors.New("project is cancelled or archived")
)

type Item struct {
	ID             uuid.UUID `json:"id"`
	Kind           string    `json:"kind"`
	Description    string    `json:"description"`
	Quantity       int       `json:"quantity"`
	UnitPriceCents int64     `json:"unit_price_cents"`
	AmountCents    int64     `json:"amount_cents"`
	Sort           int       `json:"sort"`
}

type Quote struct {
	ID            uuid.UUID       `json:"id"`
	ProjectID     uuid.UUID       `json:"project_id"`
	Version       int             `json:"version"`
	Status        string          `json:"status"`
	Currency      string          `json:"currency"`
	SubtotalCents int64           `json:"subtotal_cents"`
	DiscountCents int64           `json:"discount_cents"`
	TaxCents      int64           `json:"tax_cents"`
	TotalCents    int64           `json:"total_cents"`
	DepositCents  int64           `json:"deposit_cents"`
	ValidUntil    *time.Time      `json:"valid_until,omitempty"`
	Terms         string          `json:"terms"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	RowVersion    int             `json:"row_version"`
	CreatedBy     *uuid.UUID      `json:"created_by,omitempty"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	AcceptedAt    *time.Time      `json:"accepted_at,omitempty"`
	RejectedAt    *time.Time      `json:"rejected_at,omitempty"`
	SupersededAt  *time.Time      `json:"superseded_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Items         []Item          `json:"items"`
}

type Input struct {
	ProjectID     uuid.UUID
	Currency      string
	DiscountCents int64
	TaxCents      int64
	DepositCents  int64
	ValidUntil    *time.Time
	Terms         string
	Items         []ItemInput
}

type UpdateInput struct {
	Currency      string
	DiscountCents int64
	TaxCents      int64
	DepositCents  int64
	ValidUntil    *time.Time
	Terms         string
	Items         []ItemInput
}

type Service struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	notifier Notifier
}

type Notifier interface {
	QuoteSent(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, q *Quote) error
}

const columns = `id, project_id, version, status, currency, subtotal_cents, discount_cents,
	tax_cents, total_cents, deposit_cents, valid_until, terms, snapshot, row_version,
	created_by, sent_at, accepted_at, rejected_at, superseded_at, created_at, updated_at`

func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in Input) (*Quote, error) {
	subtotal, total, err := Calculate(in.Items, in.DiscountCents, in.TaxCents)
	if err != nil {
		return nil, err
	}
	if in.DepositCents < 0 || in.DepositCents > total {
		return nil, fmt.Errorf("%w: deposit must be between 0 and total", ErrInvalidInput)
	}
	terms := strings.TrimSpace(in.Terms)

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Quote, error) {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, in.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, ErrProjectNotFound
			}
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: project is cancelled or archived", ErrIllegalState)
			}
			return nil, fmt.Errorf("check quote project: %w", err)
		}
		var (
			projectCurrency string
			projectState    string
		)
		if err := tx.QueryRow(ctx, `
			SELECT currency, state FROM projects
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, in.ProjectID, tenantID).
			Scan(&projectCurrency, &projectState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrProjectNotFound
			}
			return nil, fmt.Errorf("lock project: %w", err)
		}
		if projectState == "cancelled" || projectState == "archived" {
			return nil, fmt.Errorf("%w: project is %s", ErrIllegalState, projectState)
		}
		var draft bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM quotes WHERE tenant_id = $1 AND project_id = $2 AND status = 'draft')`,
			tenantID, in.ProjectID).Scan(&draft); err != nil {
			return nil, fmt.Errorf("check draft quote: %w", err)
		}
		if draft {
			return nil, ErrDraftExists
		}
		var version int
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(max(version), 0) + 1 FROM quotes
			WHERE tenant_id = $1 AND project_id = $2`, tenantID, in.ProjectID).Scan(&version); err != nil {
			return nil, fmt.Errorf("next quote version: %w", err)
		}
		currency, err := normalizeCurrency(in.Currency, projectCurrency)
		if err != nil {
			return nil, err
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO quotes (tenant_id, project_id, version, currency, subtotal_cents,
				discount_cents, tax_cents, total_cents, deposit_cents, valid_until, terms, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			RETURNING `+columns,
			tenantID, in.ProjectID, version, currency, subtotal,
			in.DiscountCents, in.TaxCents, total, in.DepositCents, in.ValidUntil, terms, actor)
		q, err := scan(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrDraftExists
			}
			return nil, fmt.Errorf("insert quote: %w", err)
		}
		if q.Items, err = insertItems(ctx, tx, tenantID, q.ID, in.Items); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "quote.created", "quote", q.ID.String(), ip,
			map[string]any{"project_id": q.ProjectID, "version": q.Version, "total_cents": q.TotalCents}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "quote", q.ID.String(), "quote.created",
			map[string]any{"project_id": q.ProjectID, "version": q.Version}); err != nil {
			return nil, err
		}
		return q, nil
	})
}

func (s *Service) Update(ctx context.Context, tenantID, actor, quoteID uuid.UUID, ip string, expectedRowVersion int, in UpdateInput) (*Quote, error) {
	subtotal, total, err := Calculate(in.Items, in.DiscountCents, in.TaxCents)
	if err != nil {
		return nil, err
	}
	if in.DepositCents < 0 || in.DepositCents > total {
		return nil, fmt.Errorf("%w: deposit must be between 0 and total", ErrInvalidInput)
	}
	if expectedRowVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	terms := strings.TrimSpace(in.Terms)

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Quote, error) {
		var (
			status     string
			rowVersion int
			currency   string
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, row_version, currency FROM quotes
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, quoteID, tenantID).
			Scan(&status, &rowVersion, &currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock quote: %w", err)
		}
		if rowVersion != expectedRowVersion {
			return nil, ErrVersionConflict
		}
		if !Editable(status) {
			return nil, fmt.Errorf("%w: %s quotes are frozen", ErrIllegalState, status)
		}
		currency, err = normalizeCurrency(in.Currency, currency)
		if err != nil {
			return nil, err
		}
		row := tx.QueryRow(ctx, `
			UPDATE quotes SET currency = $3, subtotal_cents = $4, discount_cents = $5,
				tax_cents = $6, total_cents = $7, deposit_cents = $8, valid_until = $9,
				terms = $10, row_version = row_version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns,
			quoteID, tenantID, currency, subtotal, in.DiscountCents, in.TaxCents,
			total, in.DepositCents, in.ValidUntil, terms)
		q, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("update quote: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM quote_items WHERE tenant_id = $1 AND quote_id = $2`,
			tenantID, quoteID); err != nil {
			return nil, fmt.Errorf("replace quote items: %w", err)
		}
		if q.Items, err = insertItems(ctx, tx, tenantID, q.ID, in.Items); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "quote.updated", "quote", quoteID.String(), ip,
			map[string]any{"version": q.Version, "total_cents": q.TotalCents}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "quote", quoteID.String(), "quote.updated",
			map[string]any{"version": q.Version}); err != nil {
			return nil, err
		}
		return q, nil
	})
}

func (s *Service) Send(ctx context.Context, tenantID, actor, quoteID uuid.UUID, ip string, expectedRowVersion int) (*Quote, error) {
	if expectedRowVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Quote, error) {
		q, status, rowVersion, err := lockQuote(ctx, tx, tenantID, quoteID)
		if err != nil {
			return nil, err
		}
		if rowVersion != expectedRowVersion {
			return nil, ErrVersionConflict
		}
		if !CanSend(status) {
			return nil, fmt.Errorf("%w: cannot send a %s quote", ErrIllegalState, status)
		}
		var projectClosed bool
		if err := tx.QueryRow(ctx, `
			SELECT p.state IN ('cancelled','archived') OR (
				EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
				AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
			)
			FROM projects p WHERE p.id = $1 AND p.tenant_id = $2 FOR UPDATE`,
			q.ProjectID, tenantID).Scan(&projectClosed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrProjectNotFound
			}
			return nil, fmt.Errorf("lock quote project: %w", err)
		}
		if projectClosed {
			return nil, fmt.Errorf("%w: cannot send a quote for a cancelled project", ErrIllegalState)
		}
		if q.Items, err = loadItems(ctx, tx, tenantID, quoteID); err != nil {
			return nil, err
		}
		if len(q.Items) == 0 {
			return nil, fmt.Errorf("%w: quote has no items", ErrIllegalState)
		}
		if q.ValidUntil == nil {
			fallback := time.Now().UTC().AddDate(0, 0, 7)
			q.ValidUntil = &fallback
		}
		validUntil := q.ValidUntil
		snapshot, err := buildSnapshot(ctx, tx, tenantID, q)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return nil, fmt.Errorf("marshal quote snapshot: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE quotes SET status = 'superseded', superseded_at = now(),
				row_version = row_version + 1, updated_at = now()
			WHERE tenant_id = $1 AND project_id = $2 AND id <> $3
				AND status IN ('sent','accepted')`,
			tenantID, q.ProjectID, quoteID); err != nil {
			return nil, fmt.Errorf("supersede previous quotes: %w", err)
		}

		row := tx.QueryRow(ctx, `
			UPDATE quotes SET status = 'sent', snapshot = $3, valid_until = $4,
				sent_at = now(), row_version = row_version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, quoteID, tenantID, raw, validUntil)
		sent, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("send quote: %w", err)
		}
		sent.Items = q.Items
		// The frozen quote is the payment source of truth even when signing is skipped.
		if _, err := tx.Exec(ctx, `
			UPDATE projects SET currency = $3, total_amount_cents = $4, deposit_cents = $5,
				balance_cents = $6, updated_at = now()
			WHERE id = $1 AND tenant_id = $2`,
			q.ProjectID, tenantID, sent.Currency, sent.TotalCents, sent.DepositCents,
			sent.TotalCents-sent.DepositCents); err != nil {
			return nil, fmt.Errorf("sync project amounts from sent quote: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "quote.sent", "quote", quoteID.String(), ip,
			map[string]any{"version": sent.Version, "total_cents": sent.TotalCents, "valid_until": validUntil}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "quote", quoteID.String(), "quote.sent",
			map[string]any{"project_id": sent.ProjectID, "version": sent.Version, "total_cents": sent.TotalCents}); err != nil {
			return nil, err
		}
		if s.notifier != nil {
			if err := s.notifier.QuoteSent(ctx, tx, tenantID, sent); err != nil {
				return nil, fmt.Errorf("notify customer about sent quote: %w", err)
			}
		}
		return sent, nil
	})
}

func (s *Service) Accept(ctx context.Context, tenantID, actor, quoteID uuid.UUID, ip string, expectedRowVersion int) (*Quote, error) {
	return s.decide(ctx, tenantID, actor, quoteID, ip, expectedRowVersion, StatusAccepted)
}

func (s *Service) AcceptForCustomer(ctx context.Context, tenantID, customerID, quoteID uuid.UUID, ip string, expectedRowVersion int) (*Quote, error) {
	return s.decideReasonWithCustomer(ctx, tenantID, nil, &customerID, quoteID, ip, expectedRowVersion, StatusAccepted, "")
}

func (s *Service) Reject(ctx context.Context, tenantID, actor, quoteID uuid.UUID, ip string, expectedRowVersion int, reason string) (*Quote, error) {
	return s.decideReason(ctx, tenantID, actor, quoteID, ip, expectedRowVersion, StatusRejected, reason)
}

func (s *Service) decide(ctx context.Context, tenantID, actor, quoteID uuid.UUID, ip string, expectedRowVersion int, to string) (*Quote, error) {
	return s.decideReason(ctx, tenantID, actor, quoteID, ip, expectedRowVersion, to, "")
}

type decisionOutcome struct {
	quote   *Quote
	expired bool
}

func (s *Service) decideReason(ctx context.Context, tenantID, actor, quoteID uuid.UUID, ip string, expectedRowVersion int, to, reason string) (*Quote, error) {
	return s.decideReasonWithCustomer(ctx, tenantID, &actor, nil, quoteID, ip, expectedRowVersion, to, reason)
}

func (s *Service) decideReasonWithCustomer(ctx context.Context, tenantID uuid.UUID, actor, customerID *uuid.UUID,
	quoteID uuid.UUID, ip string, expectedRowVersion int, to, reason string) (*Quote, error) {
	if expectedRowVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	// The expiry write must commit even though the caller gets an error, so the
	// transaction returns a value instead of aborting on ErrExpired.
	out, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*decisionOutcome, error) {
		q, status, rowVersion, err := lockQuote(ctx, tx, tenantID, quoteID)
		if err != nil {
			return nil, err
		}
		if customerID != nil {
			var ownsProject bool
			err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM projects p
				JOIN customer_studio_links l
				  ON l.tenant_id = p.tenant_id AND l.client_id = p.client_id
					WHERE p.tenant_id = $1 AND p.id = $2 AND l.customer_id = $3
				)`, tenantID, q.ProjectID, *customerID).Scan(&ownsProject)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			if err != nil {
				return nil, fmt.Errorf("verify customer quote access: %w", err)
			}
			if !ownsProject {
				return nil, ErrNotFound
			}
		}
		if to == StatusAccepted {
			var projectClosed bool
			if err := tx.QueryRow(ctx, `
				SELECT state IN ('cancelled','archived') OR (
					EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
					AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
				)
				FROM projects p WHERE p.tenant_id = $1 AND p.id = $2 FOR UPDATE`,
				tenantID, q.ProjectID).Scan(&projectClosed); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, ErrProjectNotFound
				}
				return nil, fmt.Errorf("check quote project state: %w", err)
			}
			if projectClosed {
				return nil, ErrProjectClosed
			}
		}
		if rowVersion != expectedRowVersion {
			return nil, ErrVersionConflict
		}
		if status != StatusSent {
			return nil, fmt.Errorf("%w: cannot %s a %s quote", ErrIllegalState, to, status)
		}
		if q.ValidUntil != nil {
			today := time.Now().UTC().Truncate(24 * time.Hour)
			if q.ValidUntil.Before(today) {
				if _, err := tx.Exec(ctx, `
					UPDATE quotes SET status = 'expired', row_version = row_version + 1, updated_at = now()
					WHERE id = $1 AND tenant_id = $2`, quoteID, tenantID); err != nil {
					return nil, fmt.Errorf("expire quote: %w", err)
				}
				if err := audit.Write(ctx, tx, tenantID, actor, "quote.expired", "quote", quoteID.String(), ip,
					map[string]any{"version": q.Version, "valid_until": q.ValidUntil}); err != nil {
					return nil, err
				}
				if err := outbox.Emit(ctx, tx, tenantID, "quote", quoteID.String(), "quote.expired",
					map[string]any{"project_id": q.ProjectID, "version": q.Version}); err != nil {
					return nil, err
				}
				return &decisionOutcome{expired: true}, nil
			}
		}
		column := "accepted_at"
		if to == StatusRejected {
			column = "rejected_at"
		}
		row := tx.QueryRow(ctx, `
			UPDATE quotes SET status = $3, `+column+` = now(),
				row_version = row_version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, quoteID, tenantID, to)
		q, err = scan(row)
		if err != nil {
			return nil, fmt.Errorf("mark quote %s: %w", to, err)
		}
		if q.Items, err = loadItems(ctx, tx, tenantID, quoteID); err != nil {
			return nil, err
		}
		detail := map[string]any{"version": q.Version, "reason": reason}
		if customerID != nil {
			detail["customer_id"] = *customerID
		}
		if err := audit.Write(ctx, tx, tenantID, actor, "quote."+to, "quote", quoteID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "quote", quoteID.String(), "quote."+to,
			map[string]any{"project_id": q.ProjectID, "version": q.Version}); err != nil {
			return nil, err
		}
		return &decisionOutcome{quote: q}, nil
	})
	if err != nil {
		return nil, err
	}
	if out.expired {
		return nil, ErrExpired
	}
	return out.quote, nil
}

func (s *Service) Get(ctx context.Context, tenantID, quoteID uuid.UUID) (*Quote, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Quote, error) {
		q, _, _, err := lockQuoteNoUpdate(ctx, tx, tenantID, quoteID)
		if err != nil {
			return nil, err
		}
		if q.Items, err = loadItems(ctx, tx, tenantID, quoteID); err != nil {
			return nil, err
		}
		return q, nil
	})
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status string, limit, offset int) ([]Quote, int, error) {
	if status != "" && !ValidStatus(status) {
		return nil, 0, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
	}
	var (
		items []Quote
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const filter = `tenant_id = $1 AND ($2::uuid IS NULL OR project_id = $2)
			AND ($3::text = '' OR status = $3::text)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM quotes WHERE `+filter,
			tenantID, projectID, status).Scan(&total); err != nil {
			return fmt.Errorf("count quotes: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM quotes WHERE `+filter+`
			ORDER BY created_at DESC LIMIT $4 OFFSET $5`, tenantID, projectID, status, limit, offset)
		if err != nil {
			return fmt.Errorf("list quotes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			q, err := scan(rows)
			if err != nil {
				return fmt.Errorf("scan quote: %w", err)
			}
			q.Items = []Item{}
			items = append(items, *q)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Quote{}
	}
	return items, total, nil
}

func lockQuote(ctx context.Context, tx pgx.Tx, tenantID, quoteID uuid.UUID) (*Quote, string, int, error) {
	row := tx.QueryRow(ctx, `SELECT `+columns+` FROM quotes
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, quoteID, tenantID)
	q, err := scan(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", 0, ErrNotFound
		}
		return nil, "", 0, fmt.Errorf("lock quote: %w", err)
	}
	return q, q.Status, q.RowVersion, nil
}

func lockQuoteNoUpdate(ctx context.Context, tx pgx.Tx, tenantID, quoteID uuid.UUID) (*Quote, string, int, error) {
	row := tx.QueryRow(ctx, `SELECT `+columns+` FROM quotes
		WHERE id = $1 AND tenant_id = $2`, quoteID, tenantID)
	q, err := scan(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", 0, ErrNotFound
		}
		return nil, "", 0, fmt.Errorf("get quote: %w", err)
	}
	return q, q.Status, q.RowVersion, nil
}

func insertItems(ctx context.Context, tx pgx.Tx, tenantID, quoteID uuid.UUID, inputs []ItemInput) ([]Item, error) {
	out := make([]Item, 0, len(inputs))
	for i, in := range inputs {
		if err := validateItem(&in); err != nil {
			return nil, err
		}
		amount := in.UnitPriceCents * int64(in.Quantity)
		row := tx.QueryRow(ctx, `
			INSERT INTO quote_items (tenant_id, quote_id, kind, description, quantity,
				unit_price_cents, amount_cents, sort)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING id, kind, description, quantity, unit_price_cents, amount_cents, sort`,
			tenantID, quoteID, in.Kind, in.Description, in.Quantity, in.UnitPriceCents, amount, i)
		var it Item
		if err := row.Scan(&it.ID, &it.Kind, &it.Description, &it.Quantity,
			&it.UnitPriceCents, &it.AmountCents, &it.Sort); err != nil {
			return nil, fmt.Errorf("insert quote item: %w", err)
		}
		out = append(out, it)
	}
	return out, nil
}

func loadItems(ctx context.Context, tx pgx.Tx, tenantID, quoteID uuid.UUID) ([]Item, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, kind, description, quantity, unit_price_cents, amount_cents, sort
		FROM quote_items WHERE tenant_id = $1 AND quote_id = $2
		ORDER BY sort, id`, tenantID, quoteID)
	if err != nil {
		return nil, fmt.Errorf("load quote items: %w", err)
	}
	defer rows.Close()
	out := make([]Item, 0, 8)
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Kind, &it.Description, &it.Quantity,
			&it.UnitPriceCents, &it.AmountCents, &it.Sort); err != nil {
			return nil, fmt.Errorf("scan quote item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

type snapshotDoc struct {
	Project snapshotProject `json:"project"`
	Client  *snapshotClient `json:"client"`
	Quote   snapshotQuote   `json:"quote"`
	Items   []snapshotItem  `json:"items"`
}

type snapshotProject struct {
	ID    uuid.UUID `json:"id"`
	Code  string    `json:"code"`
	Title string    `json:"title"`
}

type snapshotClient struct {
	Name    string `json:"name"`
	Company string `json:"company"`
}

type snapshotQuote struct {
	Version       int        `json:"version"`
	Currency      string     `json:"currency"`
	SubtotalCents int64      `json:"subtotal_cents"`
	DiscountCents int64      `json:"discount_cents"`
	TaxCents      int64      `json:"tax_cents"`
	TotalCents    int64      `json:"total_cents"`
	DepositCents  int64      `json:"deposit_cents"`
	ValidUntil    *time.Time `json:"valid_until,omitempty"`
	Terms         string     `json:"terms"`
}

type snapshotItem struct {
	Kind           string `json:"kind"`
	Description    string `json:"description"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	AmountCents    int64  `json:"amount_cents"`
}

func buildSnapshot(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, q *Quote) (*snapshotDoc, error) {
	var (
		code, title string
		clientName  *string
		clientCo    *string
	)
	if err := tx.QueryRow(ctx, `
		SELECT p.code, p.title, c.name, c.company
		FROM projects p
		LEFT JOIN clients c ON c.id = p.client_id AND c.tenant_id = p.tenant_id
		WHERE p.id = $1 AND p.tenant_id = $2`, q.ProjectID, tenantID).
		Scan(&code, &title, &clientName, &clientCo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProjectNotFound
		}
		return nil, fmt.Errorf("load project snapshot: %w", err)
	}
	var client *snapshotClient
	if clientName != nil {
		client = &snapshotClient{Name: *clientName}
		if clientCo != nil {
			client.Company = *clientCo
		}
	}
	items := make([]snapshotItem, 0, len(q.Items))
	for _, it := range q.Items {
		items = append(items, snapshotItem{
			Kind: it.Kind, Description: it.Description, Quantity: it.Quantity,
			UnitPriceCents: it.UnitPriceCents, AmountCents: it.AmountCents,
		})
	}
	return &snapshotDoc{
		Project: snapshotProject{ID: q.ProjectID, Code: code, Title: title},
		Client:  client,
		Quote: snapshotQuote{
			Version: q.Version, Currency: q.Currency, SubtotalCents: q.SubtotalCents,
			DiscountCents: q.DiscountCents, TaxCents: q.TaxCents, TotalCents: q.TotalCents,
			DepositCents: q.DepositCents, ValidUntil: q.ValidUntil, Terms: q.Terms,
		},
		Items: items,
	}, nil
}

func normalizeCurrency(input, fallback string) (string, error) {
	currency := strings.ToUpper(strings.TrimSpace(input))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(fallback))
	}
	if len(currency) != 3 {
		return "", fmt.Errorf("%w: currency must be a 3-letter code", ErrInvalidInput)
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return "", fmt.Errorf("%w: currency must be a 3-letter code", ErrInvalidInput)
		}
	}
	return currency, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (*Quote, error) {
	var q Quote
	if err := row.Scan(&q.ID, &q.ProjectID, &q.Version, &q.Status, &q.Currency,
		&q.SubtotalCents, &q.DiscountCents, &q.TaxCents, &q.TotalCents, &q.DepositCents,
		&q.ValidUntil, &q.Terms, &q.Snapshot, &q.RowVersion, &q.CreatedBy,
		&q.SentAt, &q.AcceptedAt, &q.RejectedAt, &q.SupersededAt,
		&q.CreatedAt, &q.UpdatedAt); err != nil {
		return nil, err
	}
	if len(q.Snapshot) == 0 {
		q.Snapshot = json.RawMessage(`{}`)
	}
	return &q, nil
}
