package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "golang.org/x/image/webp"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/proof"
)

// Worker-side knobs. A retouch task that has not been delivered after
// OverdueAfter raises an operational alert.
type ServiceConfig struct {
	OverdueAfter time.Duration
	MaxBytes     int64
}

type Importer interface {
	ImportRevision(ctx context.Context, tenantID, roundID uuid.UUID, actor *uuid.UUID, ip string,
		in proof.RevisionInput, content io.Reader) (*proof.ImportedRevision, error)
}

// Service processes the retouch integration queue: it exports queued tasks,
// imports files dropped back by the retoucher, reconciles delivery state and
// alerts on overdue work. It runs on the worker pool (the dedicated worker
// role), never on the API pool.
type Service struct {
	pool     *pgxpool.Pool
	provider Provider
	importer Importer
	alerts   *alert.Service
	cfg      ServiceConfig
	log      *slog.Logger
}

func NewService(pool *pgxpool.Pool, provider Provider, importer Importer, alerts *alert.Service,
	cfg ServiceConfig, log *slog.Logger) *Service {
	if cfg.OverdueAfter <= 0 {
		cfg.OverdueAfter = 72 * time.Hour
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 50 << 20
	}
	return &Service{pool: pool, provider: provider, importer: importer, alerts: alerts, cfg: cfg, log: log}
}

// Dispatcher is the proof service's export hook. It only records queued jobs in
// the caller's transaction; the worker performs the filesystem work, so a slow
// or broken share never blocks starting a retouch job.
type Dispatcher struct {
	provider string
}

func NewDispatcher(provider string) *Dispatcher {
	return &Dispatcher{provider: strings.ToLower(strings.TrimSpace(provider))}
}

func (d *Dispatcher) ExportTask(ctx context.Context, tx pgx.Tx, tenantID, retouchTaskID uuid.UUID) error {
	if d.provider == "" || d.provider == ProviderManual {
		return nil
	}
	for _, direction := range []string{"export", "import"} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO integration_jobs (tenant_id, project_id, retouch_task_id, provider, direction)
			SELECT $1, project_id, $2, $3, $4 FROM retouch_tasks
			WHERE tenant_id = $1 AND id = $2
			ON CONFLICT DO NOTHING`,
			tenantID, retouchTaskID, d.provider, direction); err != nil {
			return fmt.Errorf("queue %s job: %w", direction, err)
		}
	}
	return nil
}

const (
	jobExport = "export"
	jobImport = "import"

	maxExportAttempts = 3
)

// Sweep runs one pass. Steps are independent: a broken export must not stop
// imports or delivery reconciliation.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	exports, exportErr := s.processExports(ctx)
	imports, importErr := s.processImports(ctx, uuid.Nil)
	delivered, deliverErr := s.reconcileDeliveries(ctx)
	_, overdueErr := s.alertOverdue(ctx)
	total := exports + imports + delivered
	for _, err := range []error{exportErr, importErr, deliverErr, overdueErr} {
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// SyncNow runs the import step for one tenant so an operator can force a scan
// from the admin app instead of waiting for the next poll.
func (s *Service) SyncNow(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return s.processImports(ctx, tenantID)
}

type exportJob struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	RetouchTaskID uuid.UUID
	Attempts      int
}

func (s *Service) processExports(ctx context.Context) (int, error) {
	if s.provider.Name() == ProviderManual {
		return 0, nil
	}
	// A worker that died mid-export leaves the row running; retrying is safe
	// because exports overwrite their own files.
	if _, err := s.pool.Exec(ctx, `
		UPDATE integration_jobs SET status = 'queued', updated_at = now()
		WHERE direction = 'export' AND status = 'running' AND updated_at < now() - interval '5 minutes'`); err != nil {
		return 0, fmt.Errorf("requeue stalled exports: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
			SELECT id FROM integration_jobs
			WHERE direction = 'export' AND status = 'queued'
			ORDER BY created_at
			LIMIT 5
			FOR UPDATE SKIP LOCKED
		)
		UPDATE integration_jobs j
		SET status = 'running', attempts = j.attempts + 1, updated_at = now()
		FROM claimed
		WHERE j.id = claimed.id
		RETURNING j.id, j.tenant_id, j.retouch_task_id, j.attempts`)
	if err != nil {
		return 0, fmt.Errorf("claim exports: %w", err)
	}
	var jobs []exportJob
	for rows.Next() {
		var j exportJob
		if err := rows.Scan(&j.ID, &j.TenantID, &j.RetouchTaskID, &j.Attempts); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan export job: %w", err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("claim exports: %w", err)
	}

	for _, job := range jobs {
		if err := s.runExport(ctx, job); err != nil {
			s.log.Error("retouch export failed", "job_id", job.ID, "task_id", job.RetouchTaskID, "err", err)
			s.completeExport(ctx, job, err)
			continue
		}
		s.completeExport(ctx, job, nil)
	}
	return len(jobs), nil
}

func (s *Service) runExport(ctx context.Context, job exportJob) error {
	task, err := s.loadTask(ctx, job.TenantID, job.RetouchTaskID)
	if err != nil {
		return err
	}
	ref, err := s.provider.Export(ctx, *task)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"external_ref": ref, "items": len(task.Items)})
	if _, err := s.pool.Exec(ctx, `
		UPDATE integration_jobs SET status = 'completed', last_error = '', payload = $2, updated_at = now()
		WHERE id = $1`, job.ID, payload); err != nil {
		return fmt.Errorf("complete export job: %w", err)
	}
	s.log.Info("retouch task exported", "task_id", task.ID, "tenant_id", task.TenantID,
		"items", len(task.Items), "ref", ref)
	return nil
}

func (s *Service) completeExport(ctx context.Context, job exportJob, cause error) {
	if cause == nil {
		return
	}
	dead := job.Attempts >= maxExportAttempts
	status := "queued"
	if dead {
		status = "failed"
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE integration_jobs SET status = $2, last_error = $3, updated_at = now()
		WHERE id = $1`, job.ID, status, truncate(cause.Error(), 500)); err != nil {
		s.log.Error("record export failure", "job_id", job.ID, "err", err)
	}
	if !dead {
		return
	}
	var task struct {
		proofRoundID uuid.UUID
		projectID    uuid.UUID
	}
	err := s.pool.QueryRow(ctx, `
		SELECT proof_round_id, project_id FROM retouch_tasks WHERE tenant_id = $1 AND id = $2`,
		job.TenantID, job.RetouchTaskID).Scan(&task.proofRoundID, &task.projectID)
	if err != nil {
		s.log.Warn("load failed export task", "task_id", job.RetouchTaskID, "err", err)
		return
	}
	s.raiseAlert(ctx, job.TenantID, &task.projectID, "retouch_export_failed", alert.SeverityCritical,
		"retouch_export_failed:"+job.RetouchTaskID.String(),
		"修图任务导出失败",
		map[string]any{"retouch_task_id": job.RetouchTaskID, "error": cause.Error()})
}

// loadTask materialises a retouch job for the provider.
func (s *Service) loadTask(ctx context.Context, tenantID, taskID uuid.UUID) (*Task, error) {
	task := &Task{ID: taskID, TenantID: tenantID}
	var started *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT t.proof_round_id, t.started_at, r.round_no
		FROM retouch_tasks t
		JOIN proof_rounds r ON r.tenant_id = t.tenant_id AND r.id = t.proof_round_id
		WHERE t.tenant_id = $1 AND t.id = $2`, tenantID, taskID).
		Scan(&task.RoundID, &started, &task.RoundNo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("retouch task %s no longer exists", taskID)
		}
		return nil, fmt.Errorf("load retouch task: %w", err)
	}
	if started != nil {
		task.StartedAt = *started
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ti.asset_id, a.filename, ti.position, a.original_key
		FROM retouch_task_items ti
		JOIN assets a ON a.tenant_id = ti.tenant_id AND a.id = ti.asset_id
		WHERE ti.tenant_id = $1 AND ti.retouch_task_id = $2
		ORDER BY ti.position`, tenantID, taskID)
	if err != nil {
		return nil, fmt.Errorf("load retouch items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			item TaskItem
			key  string
		)
		if err := rows.Scan(&item.AssetID, &item.Filename, &item.Position, &key); err != nil {
			return nil, fmt.Errorf("scan retouch item: %w", err)
		}
		item.SourceKey = key
		item.Ext = filepath.Ext(key)
		task.Items = append(task.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(task.Items) == 0 {
		return nil, fmt.Errorf("retouch task %s has no items", taskID)
	}
	return task, nil
}

type importTarget struct {
	TenantID   uuid.UUID
	RoundID    uuid.UUID
	TaskState  string
	StartedAt  *time.Time
	RoundNo    int
	RoundState string
}

// processImports reads files handed back by the retoucher. onlyTenant restricts
// the work to one tenant for the manual sync endpoint; uuid.Nil means all.
func (s *Service) processImports(ctx context.Context, onlyTenant uuid.UUID) (int, error) {
	files, err := s.provider.Poll(ctx)
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, imp := range files {
		tenant, ok := s.resolveImport(ctx, imp)
		if !ok {
			continue
		}
		if onlyTenant != uuid.Nil && tenant.TenantID != onlyTenant {
			continue
		}
		s.importFile(ctx, imp, tenant)
		handled++
	}
	return handled, nil
}

// resolveImport finds the task behind an incoming file. Files for unknown or
// finished tasks are archived so the incoming directory stays clean.
func (s *Service) resolveImport(ctx context.Context, imp Import) (importTarget, bool) {
	var t importTarget
	err := s.pool.QueryRow(ctx, `
		SELECT tk.tenant_id, tk.proof_round_id, tk.status, tk.started_at, r.status, r.round_no
		FROM retouch_tasks tk
		JOIN proof_rounds r ON r.tenant_id = tk.tenant_id AND r.id = tk.proof_round_id
		WHERE tk.id = $1`, imp.TaskID).
		Scan(&t.TenantID, &t.RoundID, &t.TaskState, &t.StartedAt, &t.RoundState, &t.RoundNo)
	if err != nil {
		s.log.Warn("incoming file for unknown retouch task", "task_id", imp.TaskID, "file", imp.Filename)
		s.archive(imp, true)
		return t, false
	}
	if t.RoundState != proof.StatusRetouching || (t.TaskState != "in_progress" && t.TaskState != "delivered") {
		s.log.Info("incoming file for a finished retouch task", "task_id", imp.TaskID, "file", imp.Filename)
		s.archive(imp, true)
		return t, false
	}
	return t, true
}

func (s *Service) importFile(ctx context.Context, imp Import, t importTarget) {
	fail := func(cause error, permanent bool) {
		s.log.Warn("retouch import rejected", "task_id", imp.TaskID, "asset_id", imp.AssetID, "err", cause)
		s.archive(imp, false)
		s.recordImportProblem(ctx, t, imp, cause)
		if permanent {
			s.raiseAlert(ctx, t.TenantID, nil, "integration_import_failed", alert.SeverityWarning,
				"integration_import_failed:"+imp.TaskID.String()+":"+imp.AssetID.String(),
				"修图回传文件被拒绝",
				map[string]any{
					"retouch_task_id": imp.TaskID, "asset_id": imp.AssetID,
					"file": imp.Filename, "error": cause.Error(),
				})
		}
	}

	if imp.Size <= 0 || imp.Size > s.cfg.MaxBytes {
		fail(fmt.Errorf("file size %d is outside 1..%d bytes", imp.Size, s.cfg.MaxBytes), true)
		return
	}
	f, err := os.Open(imp.Path)
	if err != nil {
		// The file may have been moved by a concurrent sweep; skip quietly.
		s.log.Info("incoming file vanished", "path", imp.Filename, "err", err)
		return
	}
	defer f.Close()

	sha, width, height, err := inspectImage(f, imp.Size, s.cfg.MaxBytes)
	if err != nil {
		fail(err, true)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		fail(err, true)
		return
	}
	res, err := s.importer.ImportRevision(ctx, t.TenantID, t.RoundID, nil, "integration", proof.RevisionInput{
		AssetID:  imp.AssetID,
		Filename: imp.Filename,
		SHA256:   sha,
		Size:     imp.Size,
		Width:    width,
		Height:   height,
		Note:     "folder_watch import",
	}, f)
	if err != nil {
		switch {
		case errors.Is(err, proof.ErrInvalidState), errors.Is(err, proof.ErrRoundNotFound):
			// The round moved on while the file was in flight; keep it but do
			// not treat it as a checksum failure.
			s.archive(imp, true)
		default:
			fail(err, true)
		}
		return
	}
	s.archive(imp, true)
	s.log.Info("retouch revision imported", "task_id", imp.TaskID, "asset_id", imp.AssetID,
		"revision_no", res.RevisionNo, "duplicate", res.Duplicate)
	s.markImportProgress(ctx, t, imp.TaskID)
}

func (s *Service) recordImportProblem(ctx context.Context, t importTarget, imp Import, cause error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE integration_jobs SET status = 'running', last_error = $3, updated_at = now()
		WHERE tenant_id = $1 AND retouch_task_id = $2 AND direction = 'import'`,
		t.TenantID, imp.TaskID, truncate(cause.Error(), 500)); err != nil {
		s.log.Error("record import problem", "task_id", imp.TaskID, "err", err)
	}
}

// markImportProgress completes the import ledger row once every task item has a
// revision. Derivative readiness is reconciled separately.
func (s *Service) markImportProgress(ctx context.Context, t importTarget, taskID uuid.UUID) {
	since := time.Now()
	if t.StartedAt != nil {
		since = *t.StartedAt
	}
	var missing int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM retouch_task_items ti
		WHERE ti.tenant_id = $1 AND ti.retouch_task_id = $2
		  AND NOT EXISTS (
			SELECT 1 FROM asset_revisions r
			WHERE r.tenant_id = ti.tenant_id AND r.asset_id = ti.asset_id AND r.created_at >= $3
		  )`, t.TenantID, taskID, since).Scan(&missing); err != nil {
		s.log.Error("count imported revisions", "err", err)
		return
	}
	status := "running"
	if missing == 0 {
		status = "completed"
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE integration_jobs SET status = $3, updated_at = now()
		WHERE tenant_id = $1 AND retouch_task_id = $2 AND direction = 'import'`,
		t.TenantID, taskID, status); err != nil {
		s.log.Error("update import progress", "err", err)
	}
}

func (s *Service) archive(imp Import, ok bool) {
	if archiver, isArc := s.provider.(interface{ Archive(Import, bool) error }); isArc {
		if err := archiver.Archive(imp, ok); err != nil {
			s.log.Warn("archive incoming file", "file", imp.Filename, "err", err)
		}
	}
}

// reconcileDeliveries flips a retouch task to delivered once every item has a
// derivative-ready revision created after the task started, then tells staff.
func (s *Service) reconcileDeliveries(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.tenant_id, t.proof_round_id, t.started_at, r.round_no
		FROM retouch_tasks t
		JOIN proof_rounds r ON r.tenant_id = t.tenant_id AND r.id = t.proof_round_id
		WHERE t.status = 'in_progress' AND t.started_at IS NOT NULL
		ORDER BY t.started_at
		LIMIT 25`)
	if err != nil {
		return 0, fmt.Errorf("load active retouch tasks: %w", err)
	}
	type candidate struct {
		ID        uuid.UUID
		TenantID  uuid.UUID
		RoundID   uuid.UUID
		StartedAt time.Time
		RoundNo   int
	}
	var tasks []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ID, &c.TenantID, &c.RoundID, &c.StartedAt, &c.RoundNo); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan retouch task: %w", err)
		}
		tasks = append(tasks, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	delivered := 0
	for _, task := range tasks {
		count, err := s.readyRevisionCount(ctx, task.TenantID, task.ID, task.StartedAt)
		if err != nil {
			s.log.Error("check retouch delivery", "task_id", task.ID, "err", err)
			continue
		}
		if count > 0 {
			continue
		}
		if err := s.markDelivered(ctx, task.TenantID, task.ID, task.RoundID, task.RoundNo); err != nil {
			s.log.Error("mark retouch delivered", "task_id", task.ID, "err", err)
			continue
		}
		delivered++
	}
	return delivered, nil
}

// readyRevisionCount returns how many task items still lack a ready revision.
func (s *Service) readyRevisionCount(ctx context.Context, tenantID, taskID uuid.UUID, since time.Time) (int, error) {
	var missing int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM retouch_task_items ti
		WHERE ti.tenant_id = $1 AND ti.retouch_task_id = $2
		  AND NOT EXISTS (
			SELECT 1 FROM asset_revisions r
			WHERE r.tenant_id = ti.tenant_id AND r.asset_id = ti.asset_id
			  AND r.status = 'derivative_ready' AND r.created_at >= $3
		  )`, tenantID, taskID, since).Scan(&missing)
	if err != nil {
		return 0, fmt.Errorf("count pending revisions: %w", err)
	}
	return missing, nil
}

func (s *Service) markDelivered(ctx context.Context, tenantID, taskID, roundID uuid.UUID, roundNo int) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE retouch_tasks SET status = 'delivered', delivered_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND id = $2 AND status = 'in_progress'`, tenantID, taskID)
		if err != nil {
			return fmt.Errorf("mark task delivered: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		payload, _ := json.Marshal(map[string]any{
			"proof_round_id": roundID, "retouch_task_id": taskID, "round_no": roundNo,
		})
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key, title, body, payload, due_at)
			VALUES ($1,'retouch_delivered','proof_round',$2,$3,$4,$5,$6,now())
			ON CONFLICT DO NOTHING`,
			tenantID, roundID, "retouch_delivered:"+taskID.String(),
			"修图文件已回传",
			fmt.Sprintf("第 %d 轮审片的修图文件已全部导入，请复核后回传客户。", roundNo),
			payload); err != nil {
			return fmt.Errorf("queue delivery notification: %w", err)
		}
		if err := outbox.Emit(ctx, tx, tenantID, "retouch_task", taskID.String(), "retouch.delivered",
			map[string]any{"proof_round_id": roundID, "retouch_task_id": taskID}); err != nil {
			return err
		}
		return nil
	})
}

// alertOverdue raises one alert per overdue task; the alert upsert folds
// repeated sweeps into the open alert.
func (s *Service) alertOverdue(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, project_id, proof_round_id, coalesce(started_at, created_at)
		FROM retouch_tasks
		WHERE status IN ('queued','in_progress')
		  AND coalesce(started_at, created_at) < now() - make_interval(secs => $1)
		ORDER BY coalesce(started_at, created_at)
		LIMIT 25`, s.cfg.OverdueAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("load overdue retouch tasks: %w", err)
	}
	type overdue struct {
		ID        uuid.UUID
		TenantID  uuid.UUID
		ProjectID uuid.UUID
		RoundID   uuid.UUID
		Since     time.Time
	}
	var tasks []overdue
	for rows.Next() {
		var t overdue
		if err := rows.Scan(&t.ID, &t.TenantID, &t.ProjectID, &t.RoundID, &t.Since); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan overdue task: %w", err)
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, task := range tasks {
		s.raiseAlert(ctx, task.TenantID, &task.ProjectID, "retouch_overdue", alert.SeverityWarning,
			"retouch_overdue:"+task.ID.String(),
			"修图任务超过约定时间仍未交付",
			map[string]any{
				"retouch_task_id": task.ID, "proof_round_id": task.RoundID,
				"started_at":    task.Since.UTC().Format(time.RFC3339),
				"overdue_after": s.cfg.OverdueAfter.String(),
			})
	}
	return len(tasks), nil
}

func (s *Service) raiseAlert(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID,
	code, severity, dedupe, title string, detail map[string]any) {
	if s.alerts == nil {
		return
	}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
			ProjectID:   projectID,
			SubjectType: "retouch_task",
			SubjectID:   dedupe,
			Code:        code,
			Severity:    severity,
			DedupeKey:   dedupe,
			Title:       title,
			Detail:      detail,
		})
		return err
	})
	if err != nil {
		s.log.Error("raise retouch alert", "code", code, "tenant_id", tenantID, "err", err)
	}
}

// inspectImage hashes a local file and reads its dimensions, rejecting anything
// that is not a decodable JPEG, PNG or WebP.
func inspectImage(f *os.File, size, max int64) (sha string, width, height int, err error) {
	if size <= 0 || size > max {
		return "", 0, 0, fmt.Errorf("file size %d is outside 1..%d bytes", size, max)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, max+1))
	if err != nil {
		return "", 0, 0, fmt.Errorf("read revision file: %w", err)
	}
	if n > max {
		return "", 0, 0, fmt.Errorf("file exceeds %d bytes", max)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, 0, err
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "", 0, 0, fmt.Errorf("file is not a JPEG, PNG or WebP image: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), cfg.Width, cfg.Height, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// Status is the admin integration view: what was exported, what is waiting for
// files and what recently failed.
type Status struct {
	Provider     string         `json:"provider"`
	Root         string         `json:"root,omitempty"`
	Exports      map[string]int `json:"exports"`
	Imports      map[string]int `json:"imports"`
	ActiveTasks  int            `json:"active_tasks"`
	OverdueTasks int            `json:"overdue_tasks"`
	RecentErrors []JobError     `json:"recent_errors"`
}

type JobError struct {
	RetouchTaskID uuid.UUID `json:"retouch_task_id"`
	Direction     string    `json:"direction"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *Service) Status(ctx context.Context, tenantID uuid.UUID) (*Status, error) {
	out := &Status{
		Provider:     s.provider.Name(),
		Exports:      map[string]int{},
		Imports:      map[string]int{},
		RecentErrors: []JobError{},
	}
	if fw, ok := s.provider.(*FolderWatch); ok {
		out.Root = fw.Root()
	}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT direction, status, count(*) FROM integration_jobs
			WHERE tenant_id = $1 AND created_at > now() - interval '30 days'
			GROUP BY direction, status`, tenantID)
		if err != nil {
			return fmt.Errorf("count integration jobs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var direction, status string
			var n int
			if err := rows.Scan(&direction, &status, &n); err != nil {
				return err
			}
			if direction == jobExport {
				out.Exports[status] = n
			} else {
				out.Imports[status] = n
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT
				count(*) FILTER (WHERE status IN ('queued','in_progress')),
				count(*) FILTER (WHERE status IN ('queued','in_progress')
					AND coalesce(started_at, created_at) < now() - make_interval(secs => $2))
			FROM retouch_tasks WHERE tenant_id = $1`,
			tenantID, s.cfg.OverdueAfter.Seconds()).Scan(&out.ActiveTasks, &out.OverdueTasks); err != nil {
			return fmt.Errorf("count retouch tasks: %w", err)
		}
		errRows, err := tx.Query(ctx, `
			SELECT retouch_task_id, direction, attempts, last_error, updated_at
			FROM integration_jobs
			WHERE tenant_id = $1 AND last_error <> ''
			ORDER BY updated_at DESC
			LIMIT 10`, tenantID)
		if err != nil {
			return fmt.Errorf("load integration errors: %w", err)
		}
		defer errRows.Close()
		for errRows.Next() {
			var e JobError
			if err := errRows.Scan(&e.RetouchTaskID, &e.Direction, &e.Attempts, &e.LastError, &e.UpdatedAt); err != nil {
				return err
			}
			out.RecentErrors = append(out.RecentErrors, e)
		}
		return errRows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
