package brief

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
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
)

var (
	ErrNotFound        = errors.New("brief not found")
	ErrVersionConflict = errors.New("brief version conflict")
	ErrInvalidInput    = errors.New("invalid brief input")
)

type Brief struct {
	ProjectID       uuid.UUID       `json:"project_id"`
	Style           string          `json:"style"`
	ShotCount       int             `json:"shot_count"`
	Venue           string          `json:"venue"`
	Crew            string          `json:"crew"`
	DeliverableSpec string          `json:"deliverable_spec"`
	UsageScope      string          `json:"usage_scope"`
	Requirements    string          `json:"requirements"`
	Details         json.RawMessage `json:"details"`
	Version         int             `json:"version"`
	UpdatedBy       *uuid.UUID      `json:"updated_by,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type Input struct {
	Style           string
	ShotCount       int
	Venue           string
	Crew            string
	DeliverableSpec string
	UsageScope      string
	Requirements    string
	Details         json.RawMessage
}

type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

const columns = `project_id, style, shot_count, venue, crew, deliverable_spec, usage_scope,
	requirements, details, version, updated_by, created_at, updated_at`

func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

func (s *Service) Upsert(ctx context.Context, tenantID, actor, projectID uuid.UUID, ip string, expectedVersion int, in Input) (*Brief, error) {
	clean, err := normalize(in)
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Brief, error) {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, projectID); err != nil {
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, ErrNotFound
			}
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: project is cancelled or archived", ErrInvalidInput)
			}
			return nil, fmt.Errorf("check project: %w", err)
		}

		var current int
		err := tx.QueryRow(ctx, `
			SELECT version FROM project_briefs
			WHERE tenant_id = $1 AND project_id = $2 FOR UPDATE`, tenantID, projectID).Scan(&current)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if expectedVersion != 0 {
				return nil, ErrVersionConflict
			}
			row := tx.QueryRow(ctx, `
				INSERT INTO project_briefs (tenant_id, project_id, style, shot_count, venue, crew,
					deliverable_spec, usage_scope, requirements, details, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
				RETURNING `+columns,
				tenantID, projectID, clean.Style, clean.ShotCount, clean.Venue, clean.Crew,
				clean.DeliverableSpec, clean.UsageScope, clean.Requirements, clean.Details, actor)
			return s.finish(ctx, tx, tenantID, actor, ip, row, 0)
		case err != nil:
			return nil, fmt.Errorf("lock brief: %w", err)
		}
		if expectedVersion != current {
			return nil, ErrVersionConflict
		}
		row := tx.QueryRow(ctx, `
			UPDATE project_briefs SET
				style = $3, shot_count = $4, venue = $5, crew = $6,
				deliverable_spec = $7, usage_scope = $8, requirements = $9,
				details = $10, version = version + 1, updated_by = $11, updated_at = now()
			WHERE tenant_id = $1 AND project_id = $2
			RETURNING `+columns,
			tenantID, projectID, clean.Style, clean.ShotCount, clean.Venue, clean.Crew,
			clean.DeliverableSpec, clean.UsageScope, clean.Requirements, clean.Details, actor)
		return s.finish(ctx, tx, tenantID, actor, ip, row, current)
	})
}

func (s *Service) finish(ctx context.Context, tx pgx.Tx, tenantID, actor uuid.UUID, ip string, row rowScanner, fromVersion int) (*Brief, error) {
	b, err := scan(row)
	if err != nil {
		return nil, fmt.Errorf("write brief: %w", err)
	}
	if err := audit.Write(ctx, tx, tenantID, &actor, "brief.updated", "project", b.ProjectID.String(), ip,
		map[string]any{"version": b.Version, "from_version": fromVersion}); err != nil {
		return nil, err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "project", b.ProjectID.String(), "project.brief.updated",
		map[string]any{"version": b.Version}); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Service) Get(ctx context.Context, tenantID, projectID uuid.UUID) (*Brief, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Brief, error) {
		b, err := scan(tx.QueryRow(ctx, `
			SELECT `+columns+` FROM project_briefs WHERE tenant_id = $1 AND project_id = $2`,
			tenantID, projectID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("get brief: %w", err)
		}
		return b, nil
	})
}

func normalize(in Input) (Input, error) {
	in.Style = strings.TrimSpace(in.Style)
	in.Venue = strings.TrimSpace(in.Venue)
	in.Crew = strings.TrimSpace(in.Crew)
	in.DeliverableSpec = strings.TrimSpace(in.DeliverableSpec)
	in.UsageScope = strings.TrimSpace(in.UsageScope)
	in.Requirements = strings.TrimSpace(in.Requirements)
	if in.ShotCount < 0 {
		return Input{}, fmt.Errorf("%w: shot_count must not be negative", ErrInvalidInput)
	}
	if len(in.Details) == 0 || strings.TrimSpace(string(in.Details)) == "null" {
		in.Details = json.RawMessage(`{}`)
	}
	if !json.Valid(in.Details) {
		return Input{}, fmt.Errorf("%w: details must be valid json", ErrInvalidInput)
	}
	return in, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (*Brief, error) {
	var b Brief
	if err := row.Scan(&b.ProjectID, &b.Style, &b.ShotCount, &b.Venue, &b.Crew,
		&b.DeliverableSpec, &b.UsageScope, &b.Requirements, &b.Details,
		&b.Version, &b.UpdatedBy, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}
