package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/project"
	"photography-workflow/server/internal/storage"
)

// Archive states. A ready archive is never deleted automatically; deletion is
// a separate owner-approved action that walks the snapshot object by object.
const (
	StatusPending   = "pending"
	StatusVerifying = "verifying"
	StatusReady     = "ready"
	StatusFailed    = "failed"
	StatusDeleting  = "deleting"
	StatusDeleted   = "deleted"
)

// Snapshot item states.
const (
	ItemPending  = "pending"
	ItemVerified = "verified"
	ItemMissing  = "missing"
	ItemMismatch = "mismatch"
	ItemDeleted  = "deleted"
)

// Object kinds recorded in the snapshot. The kind is informational; the object
// key is what gets verified and deleted.
const (
	KindOriginal           = "original"
	KindDerivative         = "derivative"
	KindRevision           = "revision"
	KindRevisionDerivative = "revision_derivative"
	KindDeliveryWeb        = "delivery_web"
	KindDeliveryPackage    = "delivery_package"
	KindContractDocument   = "contract_document"
	KindContractSigned     = "contract_signed"
	KindInvoiceFile        = "invoice_file"
)

// Job kinds executed by the worker.
const (
	JobVerify     = "archive.verify"
	JobVerifyItem = "archive.verify_item"
	JobDeleteItem = "archive.delete_item"

	SubjectType = "archive"
	ItemSubject = "archive_item"
)

// Alert codes raised by the archive pipeline.
const (
	AlertVerifyFailed  = "archive_verify_failed"
	AlertDeleteFailed  = "archive_delete_failed"
	AlertDeleteSkipped = "archive_delete_skipped"
)

var (
	ErrNotFound           = errors.New("archive not found")
	ErrInvalidInput       = errors.New("invalid archive input")
	ErrProjectNotArchived = errors.New("project is not archived")
	ErrNotReady           = errors.New("archive is not ready")
	ErrVerifyNotAllowed   = errors.New("archive cannot be verified again")
	ErrDeletionNotAllowed = errors.New("archive cannot be deleted")
)

// Config carries the defaults used when a tenant has no retention policy row.
// The values are snapshotted onto each archive at creation time, so changing
// the policy later never rewrites an existing retained date.
type Config struct {
	SweepInterval   time.Duration
	DefaultKeepDays int
	DefaultWarnDays int
	Timezone        *time.Location
}

type Service struct {
	pool   *pgxpool.Pool
	store  storage.Storage
	cfg    Config
	log    *slog.Logger
	alerts *alert.Service
}

func NewService(pool *pgxpool.Pool, store storage.Storage, cfg Config, log *slog.Logger) *Service {
	if cfg.DefaultKeepDays <= 0 {
		// An entirely unset config gets the documented defaults; an explicit
		// warn_days of 0 with a configured keep period is preserved.
		cfg.DefaultKeepDays = 365
		if cfg.DefaultWarnDays == 0 {
			cfg.DefaultWarnDays = 30
		}
	}
	if cfg.DefaultWarnDays < 0 || cfg.DefaultWarnDays >= cfg.DefaultKeepDays {
		cfg.DefaultWarnDays = 30
		if cfg.DefaultWarnDays >= cfg.DefaultKeepDays {
			cfg.DefaultWarnDays = cfg.DefaultKeepDays / 2
		}
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 5 * time.Minute
	}
	if cfg.Timezone == nil {
		cfg.Timezone = time.UTC
	}
	return &Service{pool: pool, store: store, cfg: cfg, log: log}
}

func (s *Service) SetAlerts(a *alert.Service) { s.alerts = a }

func (s *Service) SweepInterval() time.Duration { return s.cfg.SweepInterval }

// Archive is the JSON view of an archive row with its project summary.
type Archive struct {
	ID                  uuid.UUID  `json:"id"`
	TenantID            uuid.UUID  `json:"tenant_id"`
	ProjectID           uuid.UUID  `json:"project_id"`
	Status              string     `json:"status"`
	Version             int        `json:"version"`
	ObjectCount         int        `json:"object_count"`
	VerifiedCount       int        `json:"verified_count"`
	FailedCount         int        `json:"failed_count"`
	TotalBytes          int64      `json:"total_bytes"`
	KeepDays            int        `json:"keep_days"`
	WarnDays            int        `json:"warn_days"`
	RetentionUntil      *time.Time `json:"retention_until,omitempty"`
	WarnedAt            *time.Time `json:"warned_at,omitempty"`
	VerifiedAt          *time.Time `json:"verified_at,omitempty"`
	DeletionRequestedAt *time.Time `json:"deletion_requested_at,omitempty"`
	ConfirmedBy         *uuid.UUID `json:"confirmed_by,omitempty"`
	Reason              string     `json:"reason,omitempty"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// Summary adds the project fields the staff UI needs.
type Summary struct {
	Archive
	ProjectCode  string `json:"project_code"`
	ProjectTitle string `json:"project_title"`
	ProjectState string `json:"project_state"`
}

// Item is one archived object. The object key is exposed to staff so a failed
// verification can be investigated; customers never see this view.
type Item struct {
	ID             uuid.UUID  `json:"id"`
	ArchiveID      uuid.UUID  `json:"archive_id"`
	AssetID        *uuid.UUID `json:"asset_id,omitempty"`
	Kind           string     `json:"kind"`
	ObjectKey      string     `json:"object_key"`
	ExpectedSHA256 string     `json:"expected_sha256,omitempty"`
	ExpectedSize   int64      `json:"expected_size"`
	ActualSHA256   string     `json:"actual_sha256,omitempty"`
	ActualSize     int64      `json:"actual_size"`
	Status         string     `json:"status"`
	Note           string     `json:"note,omitempty"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Detail is the staff detail view; Items is capped and the handler exposes a
// paginated list for the full snapshot.
type Detail struct {
	Summary
	Items []Item `json:"items"`
}

// Policy is the tenant retention policy.
type Policy struct {
	KeepDays  int        `json:"keep_days"`
	WarnDays  int        `json:"warn_days"`
	UpdatedBy *uuid.UUID `json:"updated_by,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// VerifyPayload is the archive.verify job payload.
type VerifyPayload struct {
	ArchiveID uuid.UUID `json:"archive_id"`
}

// ItemPayload is the payload of one per-object verify/delete job.
type ItemPayload struct {
	ArchiveID uuid.UUID `json:"archive_id"`
	ItemID    uuid.UUID `json:"item_id"`
}

// VerifyResult reports a snapshot run.
type VerifyResult struct {
	ArchiveID uuid.UUID `json:"archive_id"`
	Status    string    `json:"status"`
	Items     int       `json:"items"`
}

// ItemResult reports one object verification or deletion.
type ItemResult struct {
	ArchiveID uuid.UUID `json:"archive_id"`
	ItemID    uuid.UUID `json:"item_id"`
	Status    string    `json:"status"`
}

const archiveColumns = `a.id, a.tenant_id, a.project_id, a.status, a.version,
	a.object_count, a.verified_count, a.failed_count, a.total_bytes, a.keep_days,
	a.warn_days, a.retention_until, a.warned_at, a.verified_at, a.deletion_requested_at,
	a.confirmed_by, a.reason, a.deleted_at, a.last_error, a.created_at, a.updated_at,
	p.code, p.title, p.state`

const archiveFrom = `FROM archives a
	JOIN projects p ON p.tenant_id = a.tenant_id AND p.id = a.project_id`

const itemColumns = `id, archive_id, asset_id, kind, object_key, expected_sha256,
	expected_size, actual_sha256, actual_size, status, note, checked_at, created_at`

func scanSummary(row scanner) (*Summary, error) {
	var a Summary
	if err := row.Scan(&a.ID, &a.TenantID, &a.ProjectID, &a.Status, &a.Version,
		&a.ObjectCount, &a.VerifiedCount, &a.FailedCount, &a.TotalBytes, &a.KeepDays,
		&a.WarnDays, &a.RetentionUntil, &a.WarnedAt, &a.VerifiedAt,
		&a.DeletionRequestedAt, &a.ConfirmedBy, &a.Reason, &a.DeletedAt, &a.LastError,
		&a.CreatedAt, &a.UpdatedAt, &a.ProjectCode, &a.ProjectTitle, &a.ProjectState); err != nil {
		return nil, err
	}
	return &a, nil
}

func scanItem(row scanner) (*Item, error) {
	var it Item
	if err := row.Scan(&it.ID, &it.ArchiveID, &it.AssetID, &it.Kind, &it.ObjectKey,
		&it.ExpectedSHA256, &it.ExpectedSize, &it.ActualSHA256, &it.ActualSize,
		&it.Status, &it.Note, &it.CheckedAt, &it.CreatedAt); err != nil {
		return nil, err
	}
	return &it, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func loadSummary(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (*Summary, error) {
	a, err := scanSummary(tx.QueryRow(ctx, `SELECT `+archiveColumns+` `+archiveFrom+`
		WHERE a.tenant_id = $1 AND a.id = $2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load archive: %w", err)
	}
	return a, nil
}

// Create starts (or returns) the archive of a project. The project must be in
// the archived terminal state; repeated calls are idempotent.
func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, projectID uuid.UUID) (*Summary, bool, error) {
	type result struct {
		id      uuid.UUID
		created bool
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*result, error) {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM projects WHERE tenant_id = $1 AND id = $2`,
			tenantID, projectID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("load project for archive: %w", err)
		}
		if state != string(project.StateArchived) {
			return nil, fmt.Errorf("%w: project state is %s", ErrProjectNotArchived, state)
		}
		id, created, err := s.createArchiveTx(ctx, tx, tenantID, projectID, &actor, ip)
		if err != nil {
			return nil, err
		}
		return &result{id: id, created: created}, nil
	})
	if err != nil {
		return nil, false, err
	}
	summary, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Summary, error) {
		return loadSummary(ctx, tx, tenantID, res.id)
	})
	if err != nil {
		return nil, false, err
	}
	return summary, res.created, nil
}

// createArchiveTx inserts the archive row and queues verification. It enforces
// the archived project state inside SQL so the worker sweep has the same gate.
func (s *Service) createArchiveTx(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID,
	actor *uuid.UUID, ip string) (uuid.UUID, bool, error) {
	keep, warn, err := s.policyValues(ctx, tx, tenantID)
	if err != nil {
		return uuid.Nil, false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO archives (tenant_id, project_id, keep_days, warn_days)
		SELECT $1, p.id, $3, $4 FROM projects p
		WHERE p.tenant_id = $1 AND p.id = $2 AND p.state = 'archived'
		ON CONFLICT (tenant_id, project_id) DO NOTHING
		RETURNING id`, tenantID, projectID, keep, warn).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		if err := tx.QueryRow(ctx, `
			SELECT id, status FROM archives WHERE tenant_id = $1 AND project_id = $2`,
			tenantID, projectID).Scan(&id, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return uuid.Nil, false, fmt.Errorf("%w: project state is not archived", ErrProjectNotArchived)
			}
			return uuid.Nil, false, fmt.Errorf("load existing archive: %w", err)
		}
		if status == StatusPending {
			// A previous create committed but its job was lost; re-enqueue.
			if _, err := jobs.Enqueue(ctx, tx, tenantID, JobVerify, SubjectType, &id,
				VerifyPayload{ArchiveID: id}); err != nil {
				return uuid.Nil, false, err
			}
		}
		return id, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("insert archive: %w", err)
	}
	if _, err := jobs.Enqueue(ctx, tx, tenantID, JobVerify, SubjectType, &id,
		VerifyPayload{ArchiveID: id}); err != nil {
		return uuid.Nil, false, err
	}
	if err := audit.Write(ctx, tx, tenantID, actor, "archive.created", "archive", id.String(), ip,
		map[string]any{"project_id": projectID, "keep_days": keep, "warn_days": warn}); err != nil {
		return uuid.Nil, false, err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "archive", id.String(), "archive.created",
		map[string]any{"project_id": projectID, "keep_days": keep, "warn_days": warn}); err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// Retry re-runs verification of a failed archive. Retrying a ready archive is
// rejected: its retention clock and verified snapshot are already published.
func (s *Service) Retry(ctx context.Context, tenantID, actor uuid.UUID, ip string, archiveID uuid.UUID) (*Summary, error) {
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var (
			status       string
			projectState string
		)
		err := tx.QueryRow(ctx, `
			SELECT a.status, p.state
			FROM archives a
			JOIN projects p ON p.tenant_id = a.tenant_id AND p.id = a.project_id
			WHERE a.tenant_id = $1 AND a.id = $2
			FOR UPDATE OF a`, tenantID, archiveID).Scan(&status, &projectState)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load archive for retry: %w", err)
		}
		if projectState != string(project.StateArchived) {
			return ErrProjectNotArchived
		}
		if status != StatusFailed {
			return fmt.Errorf("%w: archive is %s", ErrVerifyNotAllowed, status)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE archives SET status = 'verifying', last_error = '', updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, archiveID); err != nil {
			return fmt.Errorf("restart archive verification: %w", err)
		}
		if _, err := jobs.Enqueue(ctx, tx, tenantID, JobVerify, SubjectType, &archiveID,
			VerifyPayload{ArchiveID: archiveID}); err != nil {
			return err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "archive.verify_requested", "archive",
			archiveID.String(), ip, nil); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, tenantID, "archive", archiveID.String(), "archive.verify_requested", nil)
	})
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Summary, error) {
		return loadSummary(ctx, tx, tenantID, archiveID)
	})
}

// List returns archives for the staff app, newest first.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status string, limit, offset int) ([]Summary, int, error) {
	out := []Summary{}
	var total int
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		where := `a.tenant_id = $1`
		args := []any{tenantID}
		if projectID != nil {
			args = append(args, *projectID)
			where += fmt.Sprintf(" AND a.project_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			where += fmt.Sprintf(" AND a.status = $%d", len(args))
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM archives a WHERE `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count archives: %w", err)
		}
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s %s WHERE %s
			ORDER BY a.created_at DESC LIMIT $%d OFFSET $%d`,
			archiveColumns, archiveFrom, where, len(args)-1, len(args)), args...)
		if err != nil {
			return fmt.Errorf("list archives: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanSummary(rows)
			if err != nil {
				return fmt.Errorf("scan archive: %w", err)
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	return out, total, err
}

// Get returns the archive with its first item page.
func (s *Service) Get(ctx context.Context, tenantID, archiveID uuid.UUID) (*Detail, error) {
	var detail *Detail
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		summary, err := loadSummary(ctx, tx, tenantID, archiveID)
		if err != nil {
			return err
		}
		items, err := listItemsTx(ctx, tx, tenantID, archiveID, "", 200, 0)
		if err != nil {
			return err
		}
		detail = &Detail{Summary: *summary, Items: items}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// ListItems pages the snapshot for the staff app.
func (s *Service) ListItems(ctx context.Context, tenantID, archiveID uuid.UUID, status string, limit, offset int) ([]Item, int, error) {
	var (
		items []Item
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := loadSummary(ctx, tx, tenantID, archiveID); err != nil {
			return err
		}
		where := `tenant_id = $1 AND archive_id = $2`
		args := []any{tenantID, archiveID}
		if status != "" {
			args = append(args, status)
			where += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM archive_items WHERE `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count archive items: %w", err)
		}
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s FROM archive_items WHERE %s
			ORDER BY kind, object_key LIMIT $%d OFFSET $%d`,
			itemColumns, where, len(args)-1, len(args)), args...)
		if err != nil {
			return fmt.Errorf("list archive items: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return fmt.Errorf("scan archive item: %w", err)
			}
			items = append(items, *it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Item{}
	}
	return items, total, nil
}

func listItemsTx(ctx context.Context, tx pgx.Tx, tenantID, archiveID uuid.UUID,
	status string, limit, offset int) ([]Item, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+itemColumns+` FROM archive_items
		WHERE tenant_id = $1 AND archive_id = $2 AND ($3::text = '' OR status = $3)
		ORDER BY kind, object_key LIMIT $4 OFFSET $5`,
		tenantID, archiveID, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list archive items: %w", err)
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan archive item: %w", err)
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// ---- retention policy ----

func (s *Service) policyValues(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int, int, error) {
	var keep, warn int
	err := tx.QueryRow(ctx, `
		SELECT keep_days, warn_days FROM retention_policies WHERE tenant_id = $1`,
		tenantID).Scan(&keep, &warn)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.cfg.DefaultKeepDays, s.cfg.DefaultWarnDays, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("load retention policy: %w", err)
	}
	return keep, warn, nil
}

func (s *Service) GetPolicy(ctx context.Context, tenantID uuid.UUID) (*Policy, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Policy, error) {
		var p Policy
		err := tx.QueryRow(ctx, `
			SELECT keep_days, warn_days, updated_by, updated_at
			FROM retention_policies WHERE tenant_id = $1`, tenantID).
			Scan(&p.KeepDays, &p.WarnDays, &p.UpdatedBy, &p.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Policy{KeepDays: s.cfg.DefaultKeepDays, WarnDays: s.cfg.DefaultWarnDays}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load retention policy: %w", err)
		}
		return &p, nil
	})
}

// SetPolicy upserts the tenant retention policy. Existing archives keep the
// values they snapshotted at creation time.
func (s *Service) SetPolicy(ctx context.Context, tenantID, actor uuid.UUID, ip string,
	keepDays, warnDays int) (*Policy, error) {
	if keepDays < 1 || keepDays > 3650 {
		return nil, fmt.Errorf("%w: keep_days must be between 1 and 3650", ErrInvalidInput)
	}
	if warnDays < 0 || warnDays >= keepDays {
		return nil, fmt.Errorf("%w: warn_days must be greater than or equal to 0 and less than keep_days", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Policy, error) {
		var p Policy
		if err := tx.QueryRow(ctx, `
			INSERT INTO retention_policies (tenant_id, keep_days, warn_days, updated_by)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (tenant_id) DO UPDATE SET
				keep_days = EXCLUDED.keep_days,
				warn_days = EXCLUDED.warn_days,
				updated_by = EXCLUDED.updated_by,
				updated_at = now()
			RETURNING keep_days, warn_days, updated_by, updated_at`,
			tenantID, keepDays, warnDays, actor).
			Scan(&p.KeepDays, &p.WarnDays, &p.UpdatedBy, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("save retention policy: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "retention_policy.updated",
			"retention_policy", tenantID.String(), ip, map[string]any{
				"keep_days": keepDays, "warn_days": warnDays,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "retention_policy", tenantID.String(),
			"retention_policy.updated", map[string]any{
				"keep_days": keepDays, "warn_days": warnDays,
			}); err != nil {
			return nil, err
		}
		return &p, nil
	})
}

// ---- shared transaction helpers ----

func (s *Service) notifyTx(ctx context.Context, tx pgx.Tx, tenantID, archiveID uuid.UUID,
	kind, dedupe, title, body string, payload map[string]any) error {
	raw := json.RawMessage(`{}`)
	if len(payload) > 0 {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal archive notification payload: %w", err)
		}
		raw = b
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key,
			title, body, payload, due_at)
		VALUES ($1,$2,'archive',$3,$4,$5,$6,$7,now())
		ON CONFLICT DO NOTHING`,
		tenantID, kind, archiveID, dedupe, title, body, raw); err != nil {
		return fmt.Errorf("queue archive notification: %w", err)
	}
	return nil
}

func (s *Service) raiseAlertTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	projectID uuid.UUID, code, severity, dedupe, title string, detail map[string]any) error {
	if s.alerts == nil {
		return nil
	}
	pid := projectID
	if _, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
		ProjectID:   &pid,
		SubjectType: "archive",
		SubjectID:   projectID.String(),
		Code:        code,
		Severity:    severity,
		DedupeKey:   dedupe,
		Title:       title,
		Detail:      detail,
	}); err != nil {
		return fmt.Errorf("raise archive alert: %w", err)
	}
	return nil
}

func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(n)
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == "TB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func truncateText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
