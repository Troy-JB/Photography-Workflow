package lead

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/project"
)

var (
	ErrNotFound          = errors.New("lead not found")
	ErrVersionConflict   = errors.New("lead version conflict")
	ErrIllegalTransition = errors.New("illegal lead transition")
	ErrInvalidInput      = errors.New("invalid lead input")
	ErrProjectNotFound   = errors.New("project not found")
	ErrClientNotFound    = errors.New("client not found")
)

type Lead struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	Email          string     `json:"email"`
	Company        string     `json:"company"`
	Phone          string     `json:"phone"`
	Wechat         string     `json:"wechat"`
	Source         string     `json:"source"`
	Need           string     `json:"need"`
	BudgetMinCents int64      `json:"budget_min_cents"`
	BudgetMaxCents int64      `json:"budget_max_cents"`
	ShootOn        *time.Time `json:"shoot_on,omitempty"`
	Status         State      `json:"status"`
	OwnerUserID    *uuid.UUID `json:"owner_user_id,omitempty"`
	ClientID       *uuid.UUID `json:"client_id,omitempty"`
	ProjectID      *uuid.UUID `json:"project_id,omitempty"`
	LostReason     string     `json:"lost_reason"`
	Note           string     `json:"note"`
	NextFollowUpAt *time.Time `json:"next_follow_up_at,omitempty"`
	Version        int        `json:"version"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type FollowUp struct {
	ID          int64      `json:"id"`
	LeadID      uuid.UUID  `json:"lead_id"`
	Channel     string     `json:"channel"`
	Summary     string     `json:"summary"`
	NextAt      *time.Time `json:"next_at,omitempty"`
	ActorUserID *uuid.UUID `json:"actor_user_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type CreateInput struct {
	Name           string
	Email          string
	Company        string
	Phone          string
	Wechat         string
	Source         string
	Need           string
	BudgetMinCents int64
	BudgetMaxCents int64
	ShootOn        *time.Time
	Note           string
	NextFollowUpAt *time.Time
}

type UpdateInput struct {
	Name           *string
	Email          *string
	Company        *string
	Phone          *string
	Wechat         *string
	Source         *string
	Need           *string
	BudgetMinCents *int64
	BudgetMaxCents *int64
	ShootOn        *time.Time
	Note           *string
	NextFollowUpAt *time.Time
}

type ConvertInput struct {
	ExpectedVersion  int
	ClientID         *uuid.UUID
	ProjectID        *uuid.UUID
	CreateProject    bool
	ProjectTitle     string
	TotalAmountCents int64
	DepositCents     int64
}

type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

const columns = `id, name, email, company, phone, wechat, source, need, budget_min_cents, budget_max_cents,
	shoot_on, status, owner_user_id, client_id, project_id, lost_reason, note,
	next_follow_up_at, version, created_at, updated_at`

func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateInput) (*Lead, error) {
	clean, err := normalizeCreate(in)
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Lead, error) {
		row := tx.QueryRow(ctx, `
			INSERT INTO leads (tenant_id, name, email, company, phone, wechat, source, need,
				budget_min_cents, budget_max_cents, shoot_on, note, next_follow_up_at, owner_user_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			RETURNING `+columns,
			tenantID, clean.Name, clean.Email, clean.Company, clean.Phone, clean.Wechat, clean.Source, clean.Need,
			clean.BudgetMinCents, clean.BudgetMaxCents, clean.ShootOn, clean.Note, clean.NextFollowUpAt, actor)
		l, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("insert lead: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "lead.created", "lead", l.ID.String(), ip,
			map[string]any{"name": l.Name, "status": l.Status}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "lead", l.ID.String(), "lead.created",
			map[string]any{"name": l.Name, "status": l.Status}); err != nil {
			return nil, err
		}
		return l, nil
	})
}

func (s *Service) CreatePublic(ctx context.Context, tenantID uuid.UUID, ip, email string) (*Lead, error) {
	clean, err := normalizeCreate(CreateInput{
		Name:   "Website inquiry",
		Email:  email,
		Source: "website",
		Need:   "Portrait photography inquiry",
		Note:   "Submitted from the Lumeo website contact form.",
	})
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Lead, error) {
		row := tx.QueryRow(ctx, `
			INSERT INTO leads (tenant_id, name, email, source, need, note)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING `+columns,
			tenantID, clean.Name, clean.Email, clean.Source, clean.Need, clean.Note)
		l, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("insert public lead: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "lead.created", "lead", l.ID.String(), ip,
			map[string]any{"source": "website"}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "lead", l.ID.String(), "lead.created",
			map[string]any{"source": "website"}); err != nil {
			return nil, err
		}
		return l, nil
	})
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, status, q string, limit, offset int) ([]Lead, int, error) {
	if status != "" && !ValidState(status) {
		return nil, 0, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
	}
	q = strings.TrimSpace(q)
	var (
		items []Lead
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const filter = `tenant_id = $1 AND ($2::text = '' OR status = $2::text)
			AND ($3::text = '' OR name ILIKE '%' || $3 || '%' OR company ILIKE '%' || $3 || '%'
				OR email ILIKE '%' || $3 || '%' OR phone LIKE '%' || $3 || '%' OR need ILIKE '%' || $3 || '%')`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM leads WHERE `+filter,
			tenantID, status, q).Scan(&total); err != nil {
			return fmt.Errorf("count leads: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM leads WHERE `+filter+`
			ORDER BY created_at DESC LIMIT $4 OFFSET $5`, tenantID, status, q, limit, offset)
		if err != nil {
			return fmt.Errorf("list leads: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scan(rows)
			if err != nil {
				return fmt.Errorf("scan lead: %w", err)
			}
			items = append(items, *l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Lead{}
	}
	return items, total, nil
}

func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (*Lead, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Lead, error) {
		l, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM leads WHERE id = $1 AND tenant_id = $2`, id, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("get lead: %w", err)
		}
		return l, nil
	})
}

func (s *Service) Update(ctx context.Context, tenantID, actor, id uuid.UUID, ip string, expectedVersion int, in UpdateInput) (*Lead, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: name must not be empty", ErrInvalidInput)
	}
	if in.Email != nil && strings.TrimSpace(*in.Email) != "" && !validEmail(strings.TrimSpace(*in.Email)) {
		return nil, fmt.Errorf("%w: email must be valid", ErrInvalidInput)
	}
	if in.BudgetMinCents != nil && *in.BudgetMinCents < 0 ||
		in.BudgetMaxCents != nil && *in.BudgetMaxCents < 0 {
		return nil, fmt.Errorf("%w: budgets must not be negative", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Lead, error) {
		var (
			version int
			curMin  int64
			curMax  int64
		)
		if err := tx.QueryRow(ctx, `
			SELECT version, budget_min_cents, budget_max_cents FROM leads
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			id, tenantID).Scan(&version, &curMin, &curMax); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock lead: %w", err)
		}
		if version != expectedVersion {
			return nil, ErrVersionConflict
		}
		minCents, maxCents := curMin, curMax
		if in.BudgetMinCents != nil {
			minCents = *in.BudgetMinCents
		}
		if in.BudgetMaxCents != nil {
			maxCents = *in.BudgetMaxCents
		}
		if maxCents != 0 && minCents > maxCents {
			return nil, fmt.Errorf("%w: budget_min_cents must not exceed budget_max_cents", ErrInvalidInput)
		}

		row := tx.QueryRow(ctx, `
			UPDATE leads SET
				name = COALESCE($4, name), email = COALESCE($5, email), company = COALESCE($6, company),
				phone = COALESCE($7, phone), wechat = COALESCE($8, wechat),
				source = COALESCE($9, source), need = COALESCE($10, need),
				budget_min_cents = COALESCE($11, budget_min_cents),
				budget_max_cents = COALESCE($12, budget_max_cents),
				shoot_on = COALESCE($13, shoot_on), note = COALESCE($14, note),
				next_follow_up_at = COALESCE($15, next_follow_up_at),
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND version = $3
			RETURNING `+columns,
			id, tenantID, expectedVersion,
			trimmed(in.Name), trimmed(in.Email), trimmed(in.Company), trimmed(in.Phone), trimmed(in.Wechat),
			trimmed(in.Source), trimmed(in.Need), in.BudgetMinCents, in.BudgetMaxCents,
			in.ShootOn, trimmed(in.Note), in.NextFollowUpAt)
		l, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("update lead: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "lead.updated", "lead", l.ID.String(), ip,
			map[string]any{"version": l.Version}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "lead", l.ID.String(), "lead.updated",
			map[string]any{"version": l.Version}); err != nil {
			return nil, err
		}
		return l, nil
	})
}

func (s *Service) Transition(ctx context.Context, tenantID, actor, id uuid.UUID, ip string, to State, expectedVersion int, reason string) (*Lead, error) {
	if !ValidState(string(to)) || to == StateConverted {
		return nil, fmt.Errorf("%w: unknown target state %q", ErrInvalidInput, to)
	}
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if to == StateLost && strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: reason is required for lost", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Lead, error) {
		var (
			current State
			version int
		)
		if err := tx.QueryRow(ctx, `SELECT status, version FROM leads WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			id, tenantID).Scan(&current, &version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock lead: %w", err)
		}
		if version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanTransition(current, to) {
			return nil, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, current, to)
		}
		row := tx.QueryRow(ctx, `
			UPDATE leads SET status = $3, lost_reason = CASE WHEN $3 = 'lost' THEN $4 ELSE lost_reason END,
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, id, tenantID, string(to), strings.TrimSpace(reason))
		l, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("transition lead: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "lead.transitioned", "lead", id.String(), ip,
			map[string]any{"from": current, "to": to, "reason": reason}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "lead", id.String(), "lead.transitioned",
			map[string]any{"from": current, "to": to, "reason": reason}); err != nil {
			return nil, err
		}
		return l, nil
	})
}

func (s *Service) AddFollowUp(ctx context.Context, tenantID, actor, leadID uuid.UUID, ip string, channel, summary string, nextAt *time.Time) (*FollowUp, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil, fmt.Errorf("%w: summary is required", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*FollowUp, error) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM leads WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			leadID, tenantID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock lead: %w", err)
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO lead_follow_ups (tenant_id, lead_id, actor_user_id, channel, summary, next_at)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id, lead_id, channel, summary, next_at, actor_user_id, created_at`,
			tenantID, leadID, actor, strings.TrimSpace(channel), summary, nextAt)
		var f FollowUp
		if err := row.Scan(&f.ID, &f.LeadID, &f.Channel, &f.Summary, &f.NextAt, &f.ActorUserID, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("insert follow-up: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE leads SET next_follow_up_at = COALESCE($3, next_follow_up_at),
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2`, leadID, tenantID, nextAt); err != nil {
			return nil, fmt.Errorf("touch lead: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "lead.followup.added", "lead", leadID.String(), ip,
			map[string]any{"channel": f.Channel, "summary": f.Summary, "next_at": f.NextAt}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "lead", leadID.String(), "lead.followup.added",
			map[string]any{"channel": f.Channel, "next_at": f.NextAt}); err != nil {
			return nil, err
		}
		return &f, nil
	})
}

func (s *Service) ListFollowUps(ctx context.Context, tenantID, leadID uuid.UUID, limit int) ([]FollowUp, error) {
	items := make([]FollowUp, 0, limit)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, lead_id, channel, summary, next_at, actor_user_id, created_at
			FROM lead_follow_ups WHERE tenant_id = $1 AND lead_id = $2
			ORDER BY created_at DESC, id DESC LIMIT $3`, tenantID, leadID, limit)
		if err != nil {
			return fmt.Errorf("list follow-ups: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var f FollowUp
			if err := rows.Scan(&f.ID, &f.LeadID, &f.Channel, &f.Summary, &f.NextAt, &f.ActorUserID, &f.CreatedAt); err != nil {
				return fmt.Errorf("scan follow-up: %w", err)
			}
			items = append(items, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// Convert atomically links (or creates) the client and project, then marks the
// lead converted. Either every step commits or nothing does.
func (s *Service) Convert(ctx context.Context, tenantID, actor, leadID uuid.UUID, ip string, in ConvertInput) (*Lead, error) {
	if in.ExpectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if in.ProjectID == nil && !in.CreateProject {
		return nil, fmt.Errorf("%w: project_id or create_project is required", ErrInvalidInput)
	}
	if in.TotalAmountCents < 0 || in.DepositCents < 0 {
		return nil, fmt.Errorf("%w: amounts must not be negative", ErrInvalidInput)
	}
	if in.DepositCents > in.TotalAmountCents {
		return nil, fmt.Errorf("%w: deposit_cents must not exceed total_amount_cents", ErrInvalidInput)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Lead, error) {
		var (
			current    State
			version    int
			existingCl *uuid.UUID
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, version, client_id FROM leads
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, leadID, tenantID).
			Scan(&current, &version, &existingCl); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock lead: %w", err)
		}
		if version != in.ExpectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanConvert(current) {
			return nil, fmt.Errorf("%w: lead in status %s cannot be converted", ErrIllegalTransition, current)
		}

		clientID := in.ClientID
		if clientID == nil {
			clientID = existingCl
		}
		if clientID == nil {
			var name, email, company, phone, wechat, source, note string
			if err := tx.QueryRow(ctx, `
				SELECT name, email, company, phone, wechat, source, note FROM leads
				WHERE id = $1 AND tenant_id = $2`, leadID, tenantID).
				Scan(&name, &email, &company, &phone, &wechat, &source, &note); err != nil {
				return nil, fmt.Errorf("load lead contact: %w", err)
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO clients (tenant_id, name, email, company, phone, wechat, source, note)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
				tenantID, name, email, company, phone, wechat, source, note).Scan(&clientID); err != nil {
				return nil, fmt.Errorf("create client from lead: %w", err)
			}
			if err := audit.Write(ctx, tx, tenantID, &actor, "client.created", "client", clientID.String(), ip,
				map[string]any{"from_lead": leadID, "name": name}); err != nil {
				return nil, err
			}
			if err := outbox.Emit(ctx, tx, tenantID, "client", clientID.String(), "client.created",
				map[string]any{"from_lead": leadID, "name": name}); err != nil {
				return nil, err
			}
		} else {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT true FROM clients WHERE id = $1 AND tenant_id = $2`,
				*clientID, tenantID).Scan(&exists); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, ErrClientNotFound
				}
				return nil, fmt.Errorf("check client: %w", err)
			}
		}

		var projectID uuid.UUID
		if in.ProjectID != nil {
			if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id = $1 AND tenant_id = $2`,
				*in.ProjectID, tenantID).Scan(&projectID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, ErrProjectNotFound
				}
				return nil, fmt.Errorf("check project: %w", err)
			}
		} else {
			var title string
			if err := tx.QueryRow(ctx, `SELECT name FROM leads WHERE id = $1 AND tenant_id = $2`,
				leadID, tenantID).Scan(&title); err != nil {
				return nil, fmt.Errorf("load lead name: %w", err)
			}
			if strings.TrimSpace(in.ProjectTitle) != "" {
				title = strings.TrimSpace(in.ProjectTitle)
			} else {
				title += " 拍摄"
			}
			p, err := project.CreateTx(ctx, tx, tenantID, actor, ip, project.CreateInput{
				Title:            title,
				ClientID:         clientID,
				Currency:         "CNY",
				TotalAmountCents: in.TotalAmountCents,
				DepositCents:     in.DepositCents,
				BalanceCents:     in.TotalAmountCents - in.DepositCents,
			})
			if err != nil {
				return nil, err
			}
			projectID = p.ID
		}

		row := tx.QueryRow(ctx, `
			UPDATE leads SET status = 'converted', client_id = $3, project_id = $4,
				next_follow_up_at = NULL, version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, leadID, tenantID, *clientID, projectID)
		l, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("convert lead: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "lead.converted", "lead", leadID.String(), ip,
			map[string]any{"client_id": clientID, "project_id": projectID}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "lead", leadID.String(), "lead.converted",
			map[string]any{"client_id": clientID, "project_id": projectID}); err != nil {
			return nil, err
		}
		return l, nil
	})
}

func normalizeCreate(in CreateInput) (CreateInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.TrimSpace(in.Email)
	in.Company = strings.TrimSpace(in.Company)
	in.Phone = strings.TrimSpace(in.Phone)
	in.Wechat = strings.TrimSpace(in.Wechat)
	in.Source = strings.TrimSpace(in.Source)
	in.Need = strings.TrimSpace(in.Need)
	in.Note = strings.TrimSpace(in.Note)
	if in.Name == "" {
		return CreateInput{}, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if in.Email != "" && !validEmail(in.Email) {
		return CreateInput{}, fmt.Errorf("%w: email must be valid", ErrInvalidInput)
	}
	if in.BudgetMinCents < 0 || in.BudgetMaxCents < 0 {
		return CreateInput{}, fmt.Errorf("%w: budgets must not be negative", ErrInvalidInput)
	}
	if in.BudgetMaxCents != 0 && in.BudgetMinCents > in.BudgetMaxCents {
		return CreateInput{}, fmt.Errorf("%w: budget_min_cents must not exceed budget_max_cents", ErrInvalidInput)
	}
	return in, nil
}

func validEmail(value string) bool {
	if len(value) == 0 || len(value) > 254 {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (*Lead, error) {
	var (
		l      Lead
		status string
	)
	if err := row.Scan(&l.ID, &l.Name, &l.Email, &l.Company, &l.Phone, &l.Wechat, &l.Source, &l.Need,
		&l.BudgetMinCents, &l.BudgetMaxCents, &l.ShootOn, &status, &l.OwnerUserID,
		&l.ClientID, &l.ProjectID, &l.LostReason, &l.Note, &l.NextFollowUpAt,
		&l.Version, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	l.Status = State(status)
	return &l, nil
}
