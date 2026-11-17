package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/storage"
)

// AlertRetentionWarning is raised with the in-app retention reminder so the
// studio notices a soon-expiring archive even without opening the inbox.
const AlertRetentionWarning = "archive_retention_warning"

// Sweep auto-creates archives for archived projects that have none and raises
// one retention warning per archive. It never deletes anything.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	work := 0
	created, err := s.sweepCreate(ctx)
	if err != nil {
		return work, err
	}
	work += created
	warned, err := s.sweepRetention(ctx)
	if err != nil {
		return work, err
	}
	work += warned
	return work, nil
}

func (s *Service) sweepCreate(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.tenant_id, p.id
		FROM projects p
		LEFT JOIN archives a ON a.tenant_id = p.tenant_id AND a.project_id = p.id
		WHERE p.state = 'archived' AND a.id IS NULL
		ORDER BY p.updated_at
		LIMIT 20`)
	if err != nil {
		return 0, fmt.Errorf("list archived projects without archives: %w", err)
	}
	type candidate struct {
		tenantID  uuid.UUID
		projectID uuid.UUID
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.tenantID, &c.projectID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan archived project: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	created := 0
	for _, c := range candidates {
		ok, err := db.WithTenantResult(ctx, s.pool, c.tenantID, func(tx pgx.Tx) (bool, error) {
			_, created, err := s.createArchiveTx(ctx, tx, c.tenantID, c.projectID, nil, "")
			return created, err
		})
		if err != nil {
			return created, fmt.Errorf("create archive for project %s: %w", c.projectID, err)
		}
		if ok {
			created++
		}
	}
	return created, nil
}

func (s *Service) sweepRetention(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, project_id, retention_until, warn_days
		FROM archives
		WHERE status = 'ready' AND warned_at IS NULL AND retention_until IS NOT NULL
		  AND retention_until - make_interval(days => warn_days) <= now()
		ORDER BY retention_until
		LIMIT 50`)
	if err != nil {
		return 0, fmt.Errorf("list expiring archives: %w", err)
	}
	type candidate struct {
		id        uuid.UUID
		tenantID  uuid.UUID
		projectID uuid.UUID
		until     time.Time
		warnDays  int
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.tenantID, &c.projectID, &c.until, &c.warnDays); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expiring archive: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	warned := 0
	for _, c := range candidates {
		ok, err := db.WithTenantResult(ctx, s.pool, c.tenantID, func(tx pgx.Tx) (bool, error) {
			tag, err := tx.Exec(ctx, `
				UPDATE archives SET warned_at = now(), updated_at = now()
				WHERE tenant_id = $1 AND id = $2 AND status = 'ready' AND warned_at IS NULL`,
				c.tenantID, c.id)
			if err != nil {
				return false, fmt.Errorf("mark archive retention warning: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return false, nil
			}
			var title string
			if err := tx.QueryRow(ctx, `
				SELECT title FROM projects WHERE tenant_id = $1 AND id = $2`,
				c.tenantID, c.projectID).Scan(&title); err != nil {
				return false, fmt.Errorf("load archive project: %w", err)
			}
			untilText := c.until.In(s.cfg.Timezone).Format("2006-01-02")
			if err := s.notifyTx(ctx, tx, c.tenantID, c.id, notice.TypeArchiveWarning,
				"archive_retention_warning:"+c.id.String(), "归档即将到期",
				fmt.Sprintf("《%s》的归档将于 %s 到期（提前 %d 天提醒）。到期后不会自动删除，请确认保留或申请删除。",
					title, untilText, c.warnDays),
				map[string]any{
					"archive_id": c.id, "project_id": c.projectID,
					"retention_until": c.until.UTC().Format(time.RFC3339), "warn_days": c.warnDays,
				}); err != nil {
				return false, err
			}
			if err := audit.Write(ctx, tx, c.tenantID, nil, "archive.retention_warning", "archive",
				c.id.String(), "", map[string]any{
					"retention_until": c.until.UTC().Format(time.RFC3339), "warn_days": c.warnDays,
				}); err != nil {
				return false, err
			}
			if err := outbox.Emit(ctx, tx, c.tenantID, "archive", c.id.String(),
				"archive.retention_warning", map[string]any{
					"retention_until": c.until.UTC().Format(time.RFC3339),
				}); err != nil {
				return false, err
			}
			if err := s.raiseAlertTx(ctx, tx, c.tenantID, c.projectID, AlertRetentionWarning,
				alert.SeverityInfo, fmt.Sprintf("%s:%s", AlertRetentionWarning, c.id),
				fmt.Sprintf("归档即将到期：%s", title), map[string]any{
					"archive_id": c.id, "retention_until": c.until.UTC().Format(time.RFC3339),
					"warn_days": c.warnDays,
				}); err != nil {
				return false, err
			}
			return true, nil
		})
		if err != nil {
			return warned, err
		}
		if ok {
			warned++
		}
	}
	return warned, nil
}

// RequestDeletion starts the owner-approved deletion of an archive. The caller
// must be the owner (enforced at the route boundary) and confirm with the exact
// word DELETE; the reason is kept in the archive row and the audit trail.
func (s *Service) RequestDeletion(ctx context.Context, tenantID, actor uuid.UUID, ip string,
	archiveID uuid.UUID, confirm, reason string) (*Summary, error) {
	if strings.TrimSpace(confirm) != "DELETE" {
		return nil, fmt.Errorf("%w: confirmation must be DELETE", ErrInvalidInput)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalidInput)
	}
	if len([]rune(reason)) > 500 {
		return nil, fmt.Errorf("%w: reason must be at most 500 characters", ErrInvalidInput)
	}

	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var (
			status       string
			projectID    uuid.UUID
			projectName  string
			projectState string
			requestedAt  *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT a.status, a.project_id, p.title, p.state, a.deletion_requested_at
			FROM archives a
			JOIN projects p ON p.tenant_id = a.tenant_id AND p.id = a.project_id
			WHERE a.tenant_id = $1 AND a.id = $2
			FOR UPDATE OF a`, tenantID, archiveID).
			Scan(&status, &projectID, &projectName, &projectState, &requestedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load archive for deletion: %w", err)
		}
		if projectState != "archived" {
			return fmt.Errorf("%w: project state is %s", ErrProjectNotArchived, projectState)
		}
		switch status {
		case StatusDeleting:
			return nil
		case StatusReady:
		case StatusFailed:
			// Allow retrying a deletion that failed mid-way.
			if requestedAt == nil {
				return fmt.Errorf("%w: a failed archive must be verified before deletion", ErrDeletionNotAllowed)
			}
		default:
			return fmt.Errorf("%w: archive is %s", ErrDeletionNotAllowed, status)
		}

		rows, err := tx.Query(ctx, `
			SELECT id FROM archive_items
			WHERE tenant_id = $1 AND archive_id = $2 AND status <> 'deleted'
			ORDER BY kind, object_key`, tenantID, archiveID)
		if err != nil {
			return fmt.Errorf("list archive items to delete: %w", err)
		}
		var itemIDs []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan archive item to delete: %w", err)
			}
			itemIDs = append(itemIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE archives SET status = 'deleting',
				deletion_requested_at = coalesce(deletion_requested_at, now()),
				confirmed_by = $3, reason = $4, last_error = '', updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, archiveID, actor, reason); err != nil {
			return fmt.Errorf("mark archive deleting: %w", err)
		}
		for _, id := range itemIDs {
			if _, err := jobs.Enqueue(ctx, tx, tenantID, JobDeleteItem, ItemSubject, &id,
				ItemPayload{ArchiveID: archiveID, ItemID: id}); err != nil {
				return err
			}
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "archive.deletion_requested", "archive",
			archiveID.String(), ip, map[string]any{
				"reason": reason, "objects": len(itemIDs), "project_id": projectID,
			}); err != nil {
			return err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "archive", archiveID.String(),
			"archive.deletion_requested", map[string]any{
				"reason": reason, "objects": len(itemIDs),
			}); err != nil {
			return err
		}
		if len(itemIDs) == 0 {
			return s.finalizeDeletionTx(ctx, tx, tenantID, archiveID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Summary, error) {
		return loadSummary(ctx, tx, tenantID, archiveID)
	})
}

// DeleteItem removes one object and marks its snapshot row. Deleting a missing
// object is success: the goal is that nothing remains at the key.
func (s *Service) DeleteItem(ctx context.Context, tenantID, archiveID, itemID uuid.UUID) (*ItemResult, error) {
	res := &ItemResult{ArchiveID: archiveID, ItemID: itemID, Status: "stale"}
	var (
		archiveStatus string
		itemStatus    string
		objectKey     string
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT a.status, i.status, i.object_key
			FROM archives a
			JOIN archive_items i ON i.tenant_id = a.tenant_id AND i.archive_id = a.id
			WHERE a.tenant_id = $1 AND a.id = $2 AND i.id = $3`,
			tenantID, archiveID, itemID).Scan(&archiveStatus, &itemStatus, &objectKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load archive item for deletion: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if archiveStatus != StatusDeleting {
		return res, nil
	}
	if itemStatus == ItemDeleted {
		res.Status = ItemDeleted
		return res, s.finalizeDeletion(ctx, tenantID, archiveID)
	}

	shared := false
	err = db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM archive_items
				WHERE tenant_id = $1 AND object_key = $2 AND id <> $3
				  AND archive_id <> $4 AND status <> 'deleted')`,
			tenantID, objectKey, itemID, archiveID).Scan(&shared)
	})
	if err != nil {
		return nil, fmt.Errorf("check archive object references: %w", err)
	}

	note := ""
	switch {
	case shared:
		note = "对象仍被其他归档引用，已跳过物理删除"
		if err := s.raiseDeletedSkip(ctx, tenantID, archiveID, itemID, objectKey, note); err != nil {
			return nil, err
		}
	case !s.ownedObjectKey(tenantID, objectKey):
		note = "对象键不属于当前租户前缀，已跳过物理删除"
		s.log.Warn("archive object key outside tenant prefix; skipping delete",
			"archive_id", archiveID, "object_key", objectKey)
	default:
		if err := s.store.Delete(ctx, objectKey); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("delete archive object: %w", err)
		}
	}

	updated := false
	err = db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE archive_items SET status = 'deleted', note = $4, checked_at = now(),
				updated_at = now()
			WHERE tenant_id = $1 AND archive_id = $2 AND id = $3 AND status <> 'deleted'`,
			tenantID, archiveID, itemID, note)
		if err != nil {
			return fmt.Errorf("mark archive item deleted: %w", err)
		}
		updated = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !updated {
		res.Status = "stale"
		return res, nil
	}
	res.Status = ItemDeleted
	return res, s.finalizeDeletion(ctx, tenantID, archiveID)
}

func (s *Service) ownedObjectKey(tenantID uuid.UUID, key string) bool {
	return strings.HasPrefix(key, "t/"+tenantID.String()+"/") ||
		strings.HasPrefix(key, "tenants/"+tenantID.String()+"/")
}

func (s *Service) raiseDeletedSkip(ctx context.Context, tenantID, archiveID, itemID uuid.UUID,
	objectKey, note string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var projectID uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT project_id FROM archives WHERE tenant_id = $1 AND id = $2`,
			tenantID, archiveID).Scan(&projectID); err != nil {
			return fmt.Errorf("load archive project: %w", err)
		}
		return s.raiseAlertTx(ctx, tx, tenantID, projectID, AlertDeleteSkipped,
			alert.SeverityWarning, fmt.Sprintf("%s:%s", AlertDeleteSkipped, itemID),
			"归档对象被其他归档引用，已跳过删除", map[string]any{
				"archive_id": archiveID, "item_id": itemID, "object_key": objectKey, "note": note,
			})
	})
}

func (s *Service) finalizeDeletion(ctx context.Context, tenantID, archiveID uuid.UUID) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return s.finalizeDeletionTx(ctx, tx, tenantID, archiveID)
	})
}

// finalizeDeletionTx publishes the tombstone once every object is deleted. The
// archive row and audit trail stay; only the objects are gone.
func (s *Service) finalizeDeletionTx(ctx context.Context, tx pgx.Tx, tenantID, archiveID uuid.UUID) error {
	var (
		status    string
		projectID uuid.UUID
		title     string
	)
	err := tx.QueryRow(ctx, `
		SELECT a.status, a.project_id, p.title
		FROM archives a
		JOIN projects p ON p.tenant_id = a.tenant_id AND p.id = a.project_id
		WHERE a.tenant_id = $1 AND a.id = $2
		FOR UPDATE OF a`, tenantID, archiveID).
		Scan(&status, &projectID, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock archive for deletion finalize: %w", err)
	}
	if status != StatusDeleting {
		return nil
	}
	var remaining int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM archive_items
		WHERE tenant_id = $1 AND archive_id = $2 AND status <> 'deleted'`,
		tenantID, archiveID).Scan(&remaining); err != nil {
		return fmt.Errorf("count remaining archive objects: %w", err)
	}
	if remaining > 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE archives SET status = 'deleted', deleted_at = now(), last_error = '',
			updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status = 'deleting'`, tenantID, archiveID)
	if err != nil {
		return fmt.Errorf("mark archive deleted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	var deleted int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM archive_items WHERE tenant_id = $1 AND archive_id = $2`,
		tenantID, archiveID).Scan(&deleted); err != nil {
		return fmt.Errorf("count deleted archive objects: %w", err)
	}
	if err := s.notifyTx(ctx, tx, tenantID, archiveID, notice.TypeArchiveDeleted,
		"archive_deleted:"+archiveID.String(), "归档已删除",
		fmt.Sprintf("《%s》的 %d 个归档对象已删除；业务记录与审计日志仍然保留。", title, deleted),
		map[string]any{
			"archive_id": archiveID, "project_id": projectID, "objects": deleted,
		}); err != nil {
		return err
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "archive.deleted", "archive",
		archiveID.String(), "", map[string]any{"objects": deleted, "project_id": projectID}); err != nil {
		return err
	}
	return outbox.Emit(ctx, tx, tenantID, "archive", archiveID.String(), "archive.deleted",
		map[string]any{"objects": deleted, "project_id": projectID})
}
