package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/outbox"
)

const gallerySweepInterval = 60 * time.Second
const gallerySweepBatch = 100

const galleryTypeDeadline = "gallery_deadline"

// sweepGalleries closes albums whose deadline passed and plans the 24h-before
// reminder. It runs on the worker role because it scans every tenant; the API
// role is RLS-scoped to one tenant at a time.
func (w *Worker) sweepGalleries(ctx context.Context) (int, error) {
	expired, err := expireGalleries(ctx, w.pool, gallerySweepBatch)
	reminded, rerr := w.planGalleryDeadlines(ctx, gallerySweepBatch)
	return expired + reminded, errors.Join(err, rerr)
}

func expireGalleries(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	if limit <= 0 {
		limit = gallerySweepBatch
	}
	var (
		expired int
		failed  []error
		skip    []uuid.UUID
	)
	for expired < limit {
		n, stuck, err := expireOneGallery(ctx, pool, skip)
		if err != nil {
			if stuck == nil {
				return expired, err
			}
			failed = append(failed, err)
			skip = append(skip, *stuck)
			continue
		}
		if n == 0 {
			break
		}
		expired += n
	}
	return expired, errors.Join(failed...)
}

// expireOneGallery claims one lapsed album and closes it together with every
// live share link. audit/outbox commit in the same transaction so the customer
// 410 and the record appear atomically.
func expireOneGallery(ctx context.Context, pool *pgxpool.Pool, skip []uuid.UUID) (int, *uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("begin gallery expiry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		galleryID uuid.UUID
		tenantID  uuid.UUID
		projectID uuid.UUID
		expiresAt time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, project_id, expires_at FROM galleries
		WHERE status = 'published' AND expires_at IS NOT NULL AND expires_at <= now()
			AND id <> ALL(COALESCE($1::uuid[], '{}'::uuid[]))
		ORDER BY expires_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, skip).Scan(&galleryID, &tenantID, &projectID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("claim expired gallery: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE galleries SET status = 'expired', version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status = 'published'`,
		tenantID, galleryID); err != nil {
		stuck := galleryID
		return 0, &stuck, fmt.Errorf("expire gallery %s: %w", galleryID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE gallery_tokens SET revoked_at = now()
		WHERE tenant_id = $1 AND gallery_id = $2 AND revoked_at IS NULL`,
		tenantID, galleryID); err != nil {
		stuck := galleryID
		return 0, &stuck, fmt.Errorf("revoke tokens of expired gallery %s: %w", galleryID, err)
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "gallery.expired", "gallery",
		galleryID.String(), "", map[string]any{"expires_at": expiresAt, "project_id": projectID}); err != nil {
		stuck := galleryID
		return 0, &stuck, err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "gallery", galleryID.String(), "gallery.expired",
		map[string]any{"expires_at": expiresAt, "project_id": projectID}); err != nil {
		stuck := galleryID
		return 0, &stuck, err
	}
	if err := tx.Commit(ctx); err != nil {
		stuck := galleryID
		return 0, &stuck, fmt.Errorf("commit expired gallery %s: %w", galleryID, err)
	}
	return 1, nil, nil
}

// planGalleryDeadlines queues one reminder per published, unsubmitted album
// that is inside the lead window. Rows are pending notifications; the existing
// notice.deliver loop owns retries and dead-lettering.
func (w *Worker) planGalleryDeadlines(ctx context.Context, limit int) (int, error) {
	lead := w.opts.GalleryReminderLead
	if lead <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = gallerySweepBatch
	}
	tz := time.UTC
	if w.opts.Reminders.Timezone != nil {
		tz = w.opts.Reminders.Timezone
	}

	rows, err := w.pool.Query(ctx, `
		SELECT g.tenant_id, g.id, g.project_id, g.title, g.expires_at
		FROM galleries g
		WHERE g.status = 'published' AND g.submitted_at IS NULL
			AND g.expires_at IS NOT NULL
			AND g.expires_at > now() AND g.expires_at <= now() + make_interval(secs => $1::double precision)
			AND NOT EXISTS (
				SELECT 1 FROM notifications n
				WHERE n.tenant_id = g.tenant_id AND n.dedupe_key = $3 || g.id)
		ORDER BY g.expires_at
		LIMIT $2`, int64(lead/time.Second), limit, galleryTypeDeadline+":")
	if err != nil {
		return 0, fmt.Errorf("scan galleries for deadline reminders: %w", err)
	}
	type candidate struct {
		tenantID  uuid.UUID
		galleryID uuid.UUID
		projectID uuid.UUID
		title     string
		expiresAt time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.tenantID, &c.galleryID, &c.projectID, &c.title, &c.expiresAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan deadline candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scan deadline candidates: %w", err)
	}

	reminded := 0
	var failed []error
	for _, c := range candidates {
		payload, _ := json.Marshal(map[string]any{
			"gallery_id": c.galleryID, "project_id": c.projectID,
			"expires_at": c.expiresAt.UTC().Format(time.RFC3339),
		})
		tag, err := w.pool.Exec(ctx, `
			INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key, title, body, payload, due_at)
			VALUES ($1, $2, 'gallery', $3, $4, '选片即将截止', $5, $6, now())
			ON CONFLICT DO NOTHING`,
			c.tenantID, galleryTypeDeadline, c.galleryID,
			galleryTypeDeadline+":"+c.galleryID.String(),
			deadlineBody(c.title, c.expiresAt, tz),
			payload)
		if err != nil {
			failed = append(failed, fmt.Errorf("queue deadline reminder for gallery %s: %w", c.galleryID, err))
			continue
		}
		reminded += int(tag.RowsAffected())
	}
	return reminded, errors.Join(failed...)
}

// galleryOpen reports whether a gallery-scoped notification still describes an
// open album. Deadline reminders that lost the race to expiry are skipped
// instead of delivered after the customer link stopped working.
func (w *Worker) galleryOpen(ctx context.Context, tenantID, galleryID uuid.UUID) (bool, error) {
	var (
		status    string
		expiresAt *time.Time
	)
	err := w.pool.QueryRow(ctx, `
		SELECT status, expires_at FROM galleries
		WHERE tenant_id = $1 AND id = $2`, tenantID, galleryID).Scan(&status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load gallery for notification: %w", err)
	}
	return galleryOpenAt(status, expiresAt, time.Now()), nil
}

func galleryOpenAt(status string, expiresAt *time.Time, now time.Time) bool {
	if status != "published" {
		return false
	}
	return expiresAt == nil || expiresAt.After(now)
}

// deadlineBody renders the reminder in the deployment's schedule timezone.
func deadlineBody(title string, expiresAt time.Time, tz *time.Location) string {
	return fmt.Sprintf("《%s》的选片将于 %s 截止，请提醒客户及时提交。",
		title, expiresAt.In(tz).Format("2006-01-02 15:04"))
}
