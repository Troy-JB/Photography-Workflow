package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrNotFound = errors.New("job not found")

type Job struct {
	ID          uuid.UUID       `json:"id"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	SubjectType string          `json:"subject_type,omitempty"`
	SubjectID   *uuid.UUID      `json:"subject_id,omitempty"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Result      json.RawMessage `json:"result"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
}

const columns = `id, kind, status, subject_type, subject_id, attempts, max_attempts,
	payload, result, last_error, created_at, finished_at`

// Enqueue inserts a job in the caller's transaction. A queued/running job for
// the same (tenant, kind, subject) is returned instead, so retries cannot fork
// duplicate work.
func Enqueue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, kind, subjectType string, subjectID *uuid.UUID, payload any) (*Job, error) {
	raw := json.RawMessage(`{}`)
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal job payload: %w", err)
		}
		raw = b
	}

	existing, err := active(ctx, tx, tenantID, kind, subjectID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO jobs (tenant_id, kind, subject_type, subject_id, payload)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, kind, subject_id)
			WHERE status IN ('queued','running') AND subject_id IS NOT NULL
		DO NOTHING
		RETURNING `+columns,
		tenantID, kind, subjectType, subjectID, raw)
	job, err := scan(row)
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("enqueue job: %w", err)
	}

	// Lost the insert race (or a concurrent worker just finished); use the
	// winning job.
	existing, err = active(ctx, tx, tenantID, kind, subjectID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("enqueue job: conflict without an active job")
	}
	return existing, nil
}

func active(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, kind string, subjectID *uuid.UUID) (*Job, error) {
	if subjectID == nil {
		return nil, nil
	}
	row := tx.QueryRow(ctx, `
		SELECT `+columns+` FROM jobs
		WHERE tenant_id = $1 AND kind = $2 AND subject_id = $3
		  AND status IN ('queued','running')
		LIMIT 1`, tenantID, kind, *subjectID)
	job, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load active job: %w", err)
	}
	return job, nil
}

func Get(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (*Job, error) {
	row := tx.QueryRow(ctx, `SELECT `+columns+` FROM jobs WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	job, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

func List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, status string, limit int) ([]Job, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+columns+` FROM jobs
		WHERE tenant_id = $1 AND ($2::text = '' OR status = $2::text)
		ORDER BY created_at DESC
		LIMIT $3`, tenantID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	out := make([]Job, 0, limit)
	for rows.Next() {
		job, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, *job)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (*Job, error) {
	var j Job
	if err := row.Scan(&j.ID, &j.Kind, &j.Status, &j.SubjectType, &j.SubjectID,
		&j.Attempts, &j.MaxAttempts, &j.Payload, &j.Result, &j.LastError,
		&j.CreatedAt, &j.FinishedAt); err != nil {
		return nil, err
	}
	return &j, nil
}
