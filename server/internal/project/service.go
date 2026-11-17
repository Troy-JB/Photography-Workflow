package project

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
)

var (
	ErrNotFound          = errors.New("project not found")
	ErrClosed            = errors.New("project is cancelled or archived")
	ErrVersionConflict   = errors.New("project version conflict")
	ErrIllegalTransition = errors.New("illegal state transition")
	ErrGateNotSatisfied  = errors.New("transition gate not satisfied")
	ErrInvalidInput      = errors.New("invalid project input")
	ErrCodeConflict      = errors.New("project code already exists")
)

// EnsureOperational rejects new work on terminal projects. The booking check
// also recognizes projects cancelled by older booking flows before project
// cancellation was made atomic.
func EnsureOperational(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) error {
	var closed bool
	err := tx.QueryRow(ctx, `
		SELECT p.state IN ('cancelled','archived') OR (
			EXISTS (SELECT 1 FROM bookings b
				WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
			AND NOT EXISTS (SELECT 1 FROM bookings b
				WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
		)
		FROM projects p WHERE p.tenant_id = $1 AND p.id = $2 FOR UPDATE OF p`, tenantID, projectID).Scan(&closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check project state: %w", err)
	}
	if closed {
		return ErrClosed
	}
	return nil
}

const columns = `id, code, title, client_id, state, shoot_start_at, shoot_end_at,
	currency, total_amount_cents, deposit_cents, balance_cents, change_amount_cents,
	selection_deadline, contract_status, payment_status, version, created_at, updated_at`

const stateProjection = `CASE WHEN projects.state NOT IN ('cancelled','archived')
	AND EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status = 'cancelled')
	AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status IN ('held','confirmed'))
	THEN 'cancelled' ELSE projects.state END`

const readColumns = `id, code, title, client_id, ` + stateProjection + `, shoot_start_at, shoot_end_at,
	currency, total_amount_cents, deposit_cents, balance_cents, change_amount_cents,
	selection_deadline, contract_status, payment_status, version, created_at, updated_at`

type Project struct {
	ID                uuid.UUID  `json:"id"`
	Code              string     `json:"code"`
	Title             string     `json:"title"`
	ClientID          *uuid.UUID `json:"client_id,omitempty"`
	State             State      `json:"state"`
	ShootStartAt      *time.Time `json:"shoot_start_at,omitempty"`
	ShootEndAt        *time.Time `json:"shoot_end_at,omitempty"`
	Currency          string     `json:"currency"`
	TotalAmountCents  int64      `json:"total_amount_cents"`
	DepositCents      int64      `json:"deposit_cents"`
	BalanceCents      int64      `json:"balance_cents"`
	ChangeAmountCents int64      `json:"change_amount_cents"`
	SelectionDeadline *time.Time `json:"selection_deadline,omitempty"`
	ContractStatus    string     `json:"contract_status"`
	PaymentStatus     string     `json:"payment_status"`
	Version           int        `json:"version"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type CreateInput struct {
	Code              string
	Title             string
	ClientID          *uuid.UUID
	ShootStartAt      *time.Time
	ShootEndAt        *time.Time
	Currency          string
	TotalAmountCents  int64
	DepositCents      int64
	BalanceCents      int64
	SelectionDeadline *time.Time
}

type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateInput) (*Project, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Project, error) {
		return CreateTx(ctx, tx, tenantID, actor, ip, in)
	})
}

// CreateTx creates a project inside an existing tenant-scoped transaction so
// onboarding flows (for example lead conversion) commit atomically.
func CreateTx(ctx context.Context, tx pgx.Tx, tenantID, actor uuid.UUID, ip string, in CreateInput) (*Project, error) {
	prepared, err := prepareCreate(in)
	if err != nil {
		return nil, err
	}
	in = prepared

	if in.ClientID != nil {
		var exists bool
		err := tx.QueryRow(ctx,
			`SELECT true FROM clients WHERE id = $1 AND tenant_id = $2`,
			*in.ClientID, tenantID).Scan(&exists)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: client not found in this tenant", ErrInvalidInput)
			}
			return nil, fmt.Errorf("check client: %w", err)
		}
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO projects (tenant_id, code, title, client_id, state,
			shoot_start_at, shoot_end_at, owner_user_id, currency,
			total_amount_cents, deposit_cents, balance_cents, selection_deadline)
		VALUES ($1,$2,$3,$4,'draft',$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING `+columns,
		tenantID, in.Code, in.Title, in.ClientID, in.ShootStartAt, in.ShootEndAt, actor,
		in.Currency, in.TotalAmountCents, in.DepositCents, in.BalanceCents, in.SelectionDeadline)
	p, err := scanProject(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("%w: %q", ErrCodeConflict, in.Code)
		}
		return nil, fmt.Errorf("insert project: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO project_state_logs (tenant_id, project_id, from_state, to_state, actor_user_id)
		VALUES ($1,$2,'', 'draft', $3)`, tenantID, p.ID, actor); err != nil {
		return nil, fmt.Errorf("insert state log: %w", err)
	}
	if err := audit.Write(ctx, tx, tenantID, &actor, "project.created", "project", p.ID.String(), ip,
		map[string]any{"code": p.Code, "state": p.State}); err != nil {
		return nil, err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "project", p.ID.String(), "project.created",
		map[string]any{"code": p.Code, "state": p.State}); err != nil {
		return nil, err
	}
	return p, nil
}

func prepareCreate(in CreateInput) (CreateInput, error) {
	if strings.TrimSpace(in.Title) == "" {
		return CreateInput{}, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if in.TotalAmountCents < 0 || in.DepositCents < 0 || in.BalanceCents < 0 {
		return CreateInput{}, fmt.Errorf("%w: amounts must not be negative", ErrInvalidInput)
	}
	if in.ShootStartAt != nil && in.ShootEndAt != nil && in.ShootEndAt.Before(*in.ShootStartAt) {
		return CreateInput{}, fmt.Errorf("%w: shoot_end_at must not be before shoot_start_at", ErrInvalidInput)
	}
	in.Code = strings.TrimSpace(in.Code)
	if in.Code == "" {
		in.Code = newCode()
	}
	if in.Currency == "" {
		in.Currency = "CNY"
	}
	if len(in.Currency) != 3 {
		return CreateInput{}, fmt.Errorf("%w: currency must be a 3-letter code", ErrInvalidInput)
	}
	in.Title = strings.TrimSpace(in.Title)
	return in, nil
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, state string, excludeClosed bool, limit, offset int) ([]Project, int, error) {
	var (
		items []Project
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM projects
			WHERE tenant_id = $1 AND ($2::text = '' OR state = $2::text)
			  AND (NOT $3::boolean OR (state NOT IN ('cancelled','archived') AND NOT (
				EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status = 'cancelled')
				AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status IN ('held','confirmed'))
			  )))`,
			tenantID, state, excludeClosed).Scan(&total); err != nil {
			return fmt.Errorf("count projects: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT `+readColumns+` FROM projects
			WHERE tenant_id = $1 AND ($2::text = '' OR state = $2::text)
			  AND (NOT $3::boolean OR (state NOT IN ('cancelled','archived') AND NOT (
				EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status = 'cancelled')
				AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status IN ('held','confirmed'))
			  )))
			ORDER BY created_at DESC
			LIMIT $4 OFFSET $5`, tenantID, state, excludeClosed, limit, offset)
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProject(rows)
			if err != nil {
				return fmt.Errorf("scan project: %w", err)
			}
			items = append(items, *p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Project{}
	}
	return items, total, nil
}

func (s *Service) Get(ctx context.Context, tenantID, projectID uuid.UUID) (*Project, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Project, error) {
		row := tx.QueryRow(ctx, `SELECT `+readColumns+` FROM projects WHERE id = $1 AND tenant_id = $2`, projectID, tenantID)
		p, err := scanProject(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("get project: %w", err)
		}
		return p, nil
	})
}

func (s *Service) Transition(ctx context.Context, tenantID, projectID, actor uuid.UUID, ip string, to State, expectedVersion int, reason string) (*Project, error) {
	if !ValidState(string(to)) {
		return nil, fmt.Errorf("%w: unknown state %q", ErrInvalidInput, to)
	}
	// Optimistic locking is mandatory: a stale caller must reload before acting.
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if (to == StateCancelled || to == StateOnHold) && strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: reason is required for %s", ErrInvalidInput, to)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Project, error) {
		var (
			current       State
			version       int
			paymentStatus string
		)
		err := tx.QueryRow(ctx,
			`SELECT state, version, payment_status
			 FROM projects WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			projectID, tenantID).Scan(&current, &version, &paymentStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock project: %w", err)
		}
		if version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if to == StateCancelled {
			var activeBooking bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM bookings WHERE tenant_id = $1 AND project_id = $2 AND status IN ('held','confirmed'))`,
				tenantID, projectID).Scan(&activeBooking); err != nil {
				return nil, fmt.Errorf("check active booking before project cancellation: %w", err)
			}
			if activeBooking {
				return nil, fmt.Errorf("%w: cancel the active booking through schedule first", ErrGateNotSatisfied)
			}
		}
		if to != StateCancelled {
			var cancelledBooking bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM bookings b
					WHERE b.tenant_id = $1 AND b.project_id = $2 AND b.status = 'cancelled')
				AND NOT EXISTS (SELECT 1 FROM bookings b
					WHERE b.tenant_id = $1 AND b.project_id = $2 AND b.status IN ('held','confirmed'))`,
				tenantID, projectID).Scan(&cancelledBooking); err != nil {
				return nil, fmt.Errorf("check cancelled booking: %w", err)
			}
			if cancelledBooking {
				return nil, fmt.Errorf("%w: project has a cancelled booking", ErrIllegalTransition)
			}
		}
		if current == StateOnHold {
			var interrupted State
			err := tx.QueryRow(ctx, `
				SELECT from_state FROM project_state_logs
				WHERE tenant_id = $1 AND project_id = $2 AND to_state = 'on_hold'
				ORDER BY id DESC LIMIT 1`, tenantID, projectID).Scan(&interrupted)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("load hold origin: %w", err)
			}
			if !CanResume(interrupted, to) {
				return nil, fmt.Errorf("%w: on_hold can only resume to %s or cancelled", ErrIllegalTransition, interrupted)
			}
		} else if !CanTransition(current, to) {
			return nil, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, current, to)
		}
		if err := checkGate(to, paymentStatus); err != nil {
			return nil, err
		}

		row := tx.QueryRow(ctx, `
			UPDATE projects SET state = $3, version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, projectID, tenantID, string(to))
		p, err := scanProject(row)
		if err != nil {
			return nil, fmt.Errorf("update project: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_state_logs (tenant_id, project_id, from_state, to_state, reason, actor_user_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, tenantID, projectID, string(current), string(to), reason, actor); err != nil {
			return nil, fmt.Errorf("insert state log: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "project.transition", "project", projectID.String(), ip,
			map[string]any{"from": current, "to": to, "reason": reason}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "project", projectID.String(), "project.transitioned",
			map[string]any{"from": current, "to": to, "reason": reason}); err != nil {
			return nil, err
		}
		return p, nil
	})
}

// CancelTx moves a project to the terminal cancelled state inside a caller's
// transaction. Booking cancellation uses it so the project and its customer
// visible quote gate change atomically with the booking.
func CancelTx(ctx context.Context, tx pgx.Tx, tenantID, projectID, actor uuid.UUID, ip, reason string) error {
	var current State
	if err := tx.QueryRow(ctx, `
		SELECT state FROM projects WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		projectID, tenantID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock project for cancellation: %w", err)
	}
	if current == StateCancelled || current == StateArchived {
		return nil
	}
	if !CanTransition(current, StateCancelled) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, current, StateCancelled)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE projects SET state = $3, version = version + 1, updated_at = now()
		WHERE id = $1 AND tenant_id = $2`, projectID, tenantID, string(StateCancelled)); err != nil {
		return fmt.Errorf("cancel project: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO project_state_logs (tenant_id, project_id, from_state, to_state, reason, actor_user_id)
		VALUES ($1,$2,$3,$4,$5,$6)`, tenantID, projectID, string(current), string(StateCancelled), reason, actor); err != nil {
		return fmt.Errorf("insert cancellation state log: %w", err)
	}
	detail := map[string]any{"from": current, "to": StateCancelled, "reason": reason, "source": "booking"}
	if err := audit.Write(ctx, tx, tenantID, &actor, "project.transition", "project", projectID.String(), ip, detail); err != nil {
		return err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "project", projectID.String(), "project.transitioned", detail); err != nil {
		return err
	}
	return nil
}

// checkGate keeps payment preconditions on project transitions. Contract
// signing remains available but is optional in the current workflow.
func checkGate(to State, paymentStatus string) error {
	switch to {
	case StateBooked:
		switch paymentStatus {
		case "deposit_paid", "balance_pending", "change_pending", "paid":
		default:
			return fmt.Errorf("%w: deposit must be paid before booked", ErrGateNotSatisfied)
		}
	case StateCompleted:
		if paymentStatus != "paid" {
			return fmt.Errorf("%w: final payment must be paid before completed", ErrGateNotSatisfied)
		}
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (*Project, error) {
	var (
		p                 Project
		state             string
		clientID          *uuid.UUID
		shootStart        *time.Time
		shootEnd          *time.Time
		selectionDeadline *time.Time
	)
	err := row.Scan(&p.ID, &p.Code, &p.Title, &clientID, &state,
		&shootStart, &shootEnd, &p.Currency,
		&p.TotalAmountCents, &p.DepositCents, &p.BalanceCents, &p.ChangeAmountCents, &selectionDeadline,
		&p.ContractStatus, &p.PaymentStatus,
		&p.Version, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.State = State(state)
	p.ClientID = clientID
	p.ShootStartAt = shootStart
	p.ShootEndAt = shootEnd
	p.SelectionDeadline = selectionDeadline
	return &p, nil
}

func newCode() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "P" + time.Now().Format("060102") + "-" + strings.ToUpper(hex.EncodeToString(b))
}
