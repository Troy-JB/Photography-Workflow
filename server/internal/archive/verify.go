package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"photography-workflow/server/internal/project"
	"photography-workflow/server/internal/storage"
)

// PlanVerify snapshots the project's stored objects (once) and queues one
// verification job per object. Retries reuse the snapshot: verified objects stay
// verified and failed objects are re-queued after their status is reset.
func (s *Service) PlanVerify(ctx context.Context, tenantID, archiveID uuid.UUID) (*VerifyResult, error) {
	res := &VerifyResult{ArchiveID: archiveID, Status: "stale"}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var (
			status       string
			projectID    uuid.UUID
			projectState string
			title        string
		)
		err := tx.QueryRow(ctx, `
			SELECT a.status, a.project_id, p.state, p.title
			FROM archives a
			JOIN projects p ON p.tenant_id = a.tenant_id AND p.id = a.project_id
			WHERE a.tenant_id = $1 AND a.id = $2
			FOR UPDATE OF a`, tenantID, archiveID).
			Scan(&status, &projectID, &projectState, &title)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load archive for verification: %w", err)
		}
		switch status {
		case StatusPending:
			tag, err := tx.Exec(ctx, `
				UPDATE archives SET status = 'verifying', updated_at = now()
				WHERE tenant_id = $1 AND id = $2 AND status = 'pending'`, tenantID, archiveID)
			if err != nil {
				return fmt.Errorf("start archive verification: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return nil
			}
		case StatusVerifying:
		default:
			return nil
		}
		res.Status = StatusVerifying
		if projectState != string(project.StateArchived) {
			if err := s.failVerifyTx(ctx, tx, tenantID, archiveID, projectID, title,
				"project is no longer archived", nil); err != nil {
				return err
			}
			res.Status = StatusFailed
			return nil
		}

		if err := s.snapshotTx(ctx, tx, tenantID, archiveID, projectID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE archive_items SET status = 'pending', note = '', checked_at = NULL, updated_at = now()
			WHERE tenant_id = $1 AND archive_id = $2 AND status IN ('missing','mismatch')`,
			tenantID, archiveID); err != nil {
			return fmt.Errorf("reset failed archive items: %w", err)
		}

		rows, err := tx.Query(ctx, `
			SELECT id FROM archive_items
			WHERE tenant_id = $1 AND archive_id = $2 AND status = 'pending'
			ORDER BY kind, object_key`, tenantID, archiveID)
		if err != nil {
			return fmt.Errorf("list archive items to verify: %w", err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan archive item id: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			var total int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM archive_items WHERE tenant_id = $1 AND archive_id = $2`,
				tenantID, archiveID).Scan(&total); err != nil {
				return fmt.Errorf("count archive items: %w", err)
			}
			if total == 0 {
				if err := s.failVerifyTx(ctx, tx, tenantID, archiveID, projectID, title,
					"project has no stored objects to archive", nil); err != nil {
					return err
				}
				res.Status = StatusFailed
				return nil
			}
			// Everything is already terminal; finalize in case a previous run
			// committed the last item and crashed before summarizing.
			return s.finalizeVerifyTx(ctx, tx, tenantID, archiveID)
		}
		for _, id := range ids {
			if _, err := jobs.Enqueue(ctx, tx, tenantID, JobVerifyItem, ItemSubject, &id,
				ItemPayload{ArchiveID: archiveID, ItemID: id}); err != nil {
				return err
			}
		}
		res.Items = len(ids)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// snapshotTx records every object the project owns. The UNIQUE (archive_id,
// object_key) constraint makes repeated runs harmless.
func (s *Service) snapshotTx(ctx context.Context, tx pgx.Tx, tenantID, archiveID, projectID uuid.UUID) error {
	statements := []string{
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, a.id, 'original', a.original_key, a.content_hash, a.size_bytes
		 FROM assets a
		 WHERE a.tenant_id = $1 AND a.project_id = $3 AND a.original_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, d.asset_id, 'derivative', d.object_key, d.content_hash, d.bytes
		 FROM asset_derivatives d
		 JOIN assets a ON a.tenant_id = d.tenant_id AND a.id = d.asset_id
		 WHERE d.tenant_id = $1 AND a.project_id = $3 AND d.object_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, r.asset_id, 'revision', r.source_key, r.sha256, r.size_bytes
		 FROM asset_revisions r
		 WHERE r.tenant_id = $1 AND r.project_id = $3 AND r.source_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, r.asset_id, 'revision_derivative', rd.object_key, '', 0
		 FROM asset_revision_derivatives rd
		 JOIN asset_revisions r ON r.id = rd.revision_id AND r.tenant_id = $1
		 WHERE r.project_id = $3 AND rd.object_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, i.asset_id, 'delivery_web', i.web_object_key, i.web_sha256, i.web_size
		 FROM delivery_items i
		 JOIN deliveries d ON d.tenant_id = i.tenant_id AND d.id = i.delivery_id
		 WHERE i.tenant_id = $1 AND d.project_id = $3 AND i.web_object_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, NULL, 'delivery_package', pk.object_key, pk.sha256, pk.size_bytes
		 FROM delivery_packages pk
		 JOIN deliveries d ON d.tenant_id = pk.tenant_id AND d.id = pk.delivery_id
		 WHERE pk.tenant_id = $1 AND d.project_id = $3 AND pk.object_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, NULL, 'contract_document', c.document_key, c.document_hash, 0
		 FROM contracts c
		 WHERE c.tenant_id = $1 AND c.project_id = $3 AND c.document_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, NULL, 'contract_signed', c.signed_document_key, c.signed_document_hash, 0
		 FROM contracts c
		 WHERE c.tenant_id = $1 AND c.project_id = $3 AND c.signed_document_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
		`INSERT INTO archive_items (tenant_id, archive_id, asset_id, kind, object_key, expected_sha256, expected_size)
		 SELECT $1, $2, NULL, 'invoice_file', i.file_key, i.file_hash, i.file_size
		 FROM invoices i
		 WHERE i.tenant_id = $1 AND i.project_id = $3 AND i.file_key <> ''
		 ON CONFLICT (archive_id, object_key) DO NOTHING`,
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(ctx, stmt, tenantID, archiveID, projectID); err != nil {
			return fmt.Errorf("snapshot archive objects: %w", err)
		}
	}
	return nil
}

// VerifyItem hashes one object and records the result. The row is re-read under
// the transaction so a retried job cannot overwrite a terminal state.
func (s *Service) VerifyItem(ctx context.Context, tenantID, archiveID, itemID uuid.UUID) (*ItemResult, error) {
	res := &ItemResult{ArchiveID: archiveID, ItemID: itemID, Status: "stale"}
	var (
		objectKey     string
		expectedSHA   string
		expectedSize  int64
		archiveStatus string
		itemStatus    string
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT a.status, i.status, i.object_key, i.expected_sha256, i.expected_size
			FROM archives a
			JOIN archive_items i ON i.tenant_id = a.tenant_id AND i.archive_id = a.id
			WHERE a.tenant_id = $1 AND a.id = $2 AND i.id = $3`,
			tenantID, archiveID, itemID).
			Scan(&archiveStatus, &itemStatus, &objectKey, &expectedSHA, &expectedSize)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load archive item: %w", err)
		}
		if archiveStatus != StatusVerifying {
			return nil
		}
		if itemStatus != ItemPending {
			// A retried job after the item was recorded only needs the
			// summarizing step, which is idempotent.
			res.Status = itemStatus
			return s.finalizeVerify(ctx, tenantID, archiveID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res.Status != "stale" || archiveStatus != StatusVerifying || itemStatus != ItemPending {
		return res, nil
	}

	actualSHA, actualSize, err := s.hashObject(ctx, objectKey)
	if err != nil && !errors.Is(err, errObjectMissing) {
		return nil, err
	}
	itemStatus, note := classifyItem(err, expectedSHA, expectedSize, actualSHA, actualSize)

	updated := false
	err = db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE archive_items SET actual_sha256 = $4, actual_size = $5, status = $6,
				note = $7, checked_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND archive_id = $2 AND id = $3 AND status = 'pending'
			  AND EXISTS (
				SELECT 1 FROM archives a
				WHERE a.tenant_id = $1 AND a.id = $2 AND a.status = 'verifying')`,
			tenantID, archiveID, itemID, actualSHA, actualSize, itemStatus, note)
		if err != nil {
			return fmt.Errorf("record archive verification: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		updated = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !updated {
		res.Status = "stale"
		return res, nil
	}
	if err := s.finalizeVerify(ctx, tenantID, archiveID); err != nil {
		return nil, err
	}
	res.Status = itemStatus
	return res, nil
}

var errObjectMissing = errors.New("object is missing")

// classifyItem maps a hash result to the persisted item state. A missing object
// wins over any metadata mismatch; a recorded hash or size mismatch fails the
// item; an object without a recorded hash is verified on existence alone.
func classifyItem(hashErr error, expectedSHA string, expectedSize int64, actualSHA string, actualSize int64) (string, string) {
	switch {
	case errors.Is(hashErr, errObjectMissing):
		return ItemMissing, "对象不存在"
	case expectedSHA != "" && actualSHA != expectedSHA:
		return ItemMismatch, "SHA-256 与快照不一致"
	case expectedSize > 0 && actualSize != expectedSize:
		return ItemMismatch, "对象大小与快照不一致"
	case expectedSHA == "":
		return ItemVerified, "未记录哈希，仅校验对象存在性"
	}
	return ItemVerified, ""
}

func (s *Service) hashObject(ctx context.Context, key string) (string, int64, error) {
	if _, err := s.store.Stat(ctx, key); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", 0, errObjectMissing
		}
		return "", 0, fmt.Errorf("stat archive object: %w", err)
	}
	rc, err := s.store.Open(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", 0, errObjectMissing
		}
		return "", 0, fmt.Errorf("open archive object: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, fmt.Errorf("hash archive object: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (s *Service) finalizeVerify(ctx context.Context, tenantID, archiveID uuid.UUID) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return s.finalizeVerifyTx(ctx, tx, tenantID, archiveID)
	})
}

// finalizeVerifyTx summarizes the snapshot once every object is terminal. Only
// the caller that wins the status transition publishes the archive.
func (s *Service) finalizeVerifyTx(ctx context.Context, tx pgx.Tx, tenantID, archiveID uuid.UUID) error {
	var (
		status    string
		projectID uuid.UUID
		title     string
		keepDays  int
	)
	err := tx.QueryRow(ctx, `
		SELECT a.status, a.project_id, p.title, a.keep_days
		FROM archives a
		JOIN projects p ON p.tenant_id = a.tenant_id AND p.id = a.project_id
		WHERE a.tenant_id = $1 AND a.id = $2
		FOR UPDATE OF a`, tenantID, archiveID).
		Scan(&status, &projectID, &title, &keepDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock archive for finalize: %w", err)
	}
	if status != StatusVerifying {
		return nil
	}

	var pending, verified, failed int
	var totalBytes int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status = 'verified'),
		       count(*) FILTER (WHERE status IN ('missing','mismatch')),
		       coalesce(sum(actual_size) FILTER (WHERE status = 'verified'), 0)
		FROM archive_items WHERE tenant_id = $1 AND archive_id = $2`,
		tenantID, archiveID).Scan(&pending, &verified, &failed, &totalBytes); err != nil {
		return fmt.Errorf("sum archive verification: %w", err)
	}
	if pending > 0 {
		return nil
	}

	if failed > 0 {
		missing, mismatch := 0, 0
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status = 'missing'),
			       count(*) FILTER (WHERE status = 'mismatch')
			FROM archive_items WHERE tenant_id = $1 AND archive_id = $2`,
			tenantID, archiveID).Scan(&missing, &mismatch); err != nil {
			return fmt.Errorf("count archive failures: %w", err)
		}
		samples, err := loadFailureSamples(ctx, tx, tenantID, archiveID, 20)
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("%d objects failed verification (missing %d, mismatch %d)",
			failed, missing, mismatch)
		return s.failVerifyTx(ctx, tx, tenantID, archiveID, projectID, title, reason,
			map[string]any{
				"missing": missing, "mismatch": mismatch, "failed": failed,
				"verified": verified, "samples": samples,
			})
	}

	retention := time.Now().Add(time.Duration(keepDays) * 24 * time.Hour)
	tag, err := tx.Exec(ctx, `
		UPDATE archives SET status = 'ready', object_count = $3, verified_count = $4,
			failed_count = 0, total_bytes = $5, verified_at = now(), retention_until = $6,
			last_error = '', updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status = 'verifying'`,
		tenantID, archiveID, verified, verified, totalBytes, retention)
	if err != nil {
		return fmt.Errorf("publish archive: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE assets SET status = 'archived', updated_at = now()
		WHERE tenant_id = $1 AND project_id = $2
		  AND status NOT IN ('archived','missing','corrupt','quarantined')`,
		tenantID, projectID); err != nil {
		return fmt.Errorf("mark assets archived: %w", err)
	}
	retentionText := retention.In(s.cfg.Timezone).Format("2006-01-02")
	if err := s.notifyTx(ctx, tx, tenantID, archiveID, notice.TypeArchiveReady,
		"archive_ready:"+archiveID.String(), "归档校验完成",
		fmt.Sprintf("《%s》归档校验完成：%d 个对象，共 %s，保留至 %s。",
			title, verified, formatBytes(totalBytes), retentionText),
		map[string]any{
			"archive_id": archiveID, "project_id": projectID, "object_count": verified,
			"total_bytes": totalBytes, "retention_until": retention.UTC().Format(time.RFC3339),
		}); err != nil {
		return err
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "archive.ready", "archive", archiveID.String(), "",
		map[string]any{"object_count": verified, "total_bytes": totalBytes,
			"retention_until": retention.UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	return outbox.Emit(ctx, tx, tenantID, "archive", archiveID.String(), "archive.ready",
		map[string]any{"object_count": verified, "total_bytes": totalBytes,
			"retention_until": retention.UTC().Format(time.RFC3339)})
}

// failVerifyTx records a permanent verification failure and raises an alert.
func (s *Service) failVerifyTx(ctx context.Context, tx pgx.Tx, tenantID, archiveID, projectID uuid.UUID,
	title, reason string, detail map[string]any) error {
	reason = truncateText(reason, 500)
	tag, err := tx.Exec(ctx, `
		UPDATE archives SET status = 'failed', last_error = $3, verified_at = now(),
			retention_until = NULL, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status IN ('pending','verifying')`,
		tenantID, archiveID, reason)
	if err != nil {
		return fmt.Errorf("mark archive failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "archive.verify_failed", "archive",
		archiveID.String(), "", map[string]any{"reason": reason}); err != nil {
		return err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "archive", archiveID.String(),
		"archive.verify_failed", map[string]any{"reason": reason}); err != nil {
		return err
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["archive_id"] = archiveID
	detail["project_id"] = projectID
	detail["reason"] = reason
	return s.raiseAlertTx(ctx, tx, tenantID, projectID, AlertVerifyFailed, alert.SeverityWarning,
		fmt.Sprintf("%s:%s", AlertVerifyFailed, archiveID), fmt.Sprintf("归档校验失败：%s", title), detail)
}

func loadFailureSamples(ctx context.Context, tx pgx.Tx, tenantID, archiveID uuid.UUID, limit int) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT object_key || ' (' || status || ')'
		FROM archive_items
		WHERE tenant_id = $1 AND archive_id = $2 AND status IN ('missing','mismatch')
		ORDER BY kind, object_key LIMIT $3`, tenantID, archiveID, limit)
	if err != nil {
		return nil, fmt.Errorf("list archive failures: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan archive failure: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FailJob is the worker dead-letter hook. A permanently failed verify marks the
// archive failed; a permanently failed deletion leaves it visible as failed so
// the owner can retry the approved deletion.
func (s *Service) FailJob(ctx context.Context, tenantID, archiveID uuid.UUID, kind, reason string) error {
	reason = truncateText(strings.TrimSpace(reason), 500)
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
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
			return fmt.Errorf("load archive for failure: %w", err)
		}
		switch kind {
		case JobVerify, JobVerifyItem:
			if status != StatusPending && status != StatusVerifying {
				return nil
			}
			if reason == "" {
				reason = "archive verification job stopped permanently"
			}
			return s.failVerifyTx(ctx, tx, tenantID, archiveID, projectID, title, reason,
				map[string]any{"job_kind": kind})
		case JobDeleteItem:
			if status != StatusDeleting {
				return nil
			}
			if reason == "" {
				reason = "archive deletion job stopped permanently"
			}
			if _, err := tx.Exec(ctx, `
				UPDATE archives SET status = 'failed', last_error = $3, updated_at = now()
				WHERE tenant_id = $1 AND id = $2 AND status = 'deleting'`,
				tenantID, archiveID, reason); err != nil {
				return fmt.Errorf("mark archive deletion failed: %w", err)
			}
			if err := audit.Write(ctx, tx, tenantID, nil, "archive.delete_failed", "archive",
				archiveID.String(), "", map[string]any{"reason": reason}); err != nil {
				return err
			}
			if err := outbox.Emit(ctx, tx, tenantID, "archive", archiveID.String(),
				"archive.delete_failed", map[string]any{"reason": reason}); err != nil {
				return err
			}
			return s.raiseAlertTx(ctx, tx, tenantID, projectID, AlertDeleteFailed, alert.SeverityWarning,
				fmt.Sprintf("%s:%s", AlertDeleteFailed, archiveID),
				fmt.Sprintf("归档删除失败：%s", title),
				map[string]any{"archive_id": archiveID, "reason": reason})
		}
		return nil
	})
}
