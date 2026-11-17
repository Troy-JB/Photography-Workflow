package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/db"
)

const proofDeadlineLead = 24 * time.Hour

// raiseRevisionAlert records a failed revision derivation. The job keeps
// retrying, and the alert folds into one open row per revision.
func (w *Worker) raiseRevisionAlert(ctx context.Context, tenantID, revisionID uuid.UUID, cause error) {
	err := db.WithTenant(ctx, w.pool, tenantID, func(tx pgx.Tx) error {
		_, err := w.alerts.Raise(ctx, tx, tenantID, alert.Input{
			SubjectType: "asset_revision",
			SubjectID:   revisionID.String(),
			Code:        "revision_derive_failed",
			Severity:    alert.SeverityWarning,
			DedupeKey:   "revision_derive_failed:" + revisionID.String(),
			Title:       "修图版本预览生成失败",
			Detail:      map[string]any{"revision_id": revisionID, "error": cause.Error()},
		})
		return err
	})
	if err != nil {
		w.log.Error("raise revision derive alert", "revision_id", revisionID, "err", err)
	}
}

// sweepProofDeadlines reminds staff and the customer once, 24h before a round
// closes. The notification insert doubles as the idempotency guard: only the
// worker that inserts it mints a token and queues mail, so repeated sweeps and
// concurrent workers cannot spam the client with fresh links.
func (w *Worker) sweepProofDeadlines(ctx context.Context) (int, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT r.id, r.tenant_id
		FROM proof_rounds r
		WHERE r.status <> 'approved'
			AND r.due_at > now()
			AND r.due_at <= now() + make_interval(secs => $1)
			AND NOT EXISTS (
				SELECT 1 FROM notifications n
				WHERE n.tenant_id = r.tenant_id
					AND n.dedupe_key = 'proof_deadline:' || r.id::text)
		ORDER BY r.due_at
		LIMIT 50`, int(proofDeadlineLead.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("scan due proof rounds: %w", err)
	}
	type dueRound struct {
		ID       uuid.UUID
		TenantID uuid.UUID
	}
	var rounds []dueRound
	for rows.Next() {
		var r dueRound
		if err := rows.Scan(&r.ID, &r.TenantID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan due proof round: %w", err)
		}
		rounds = append(rounds, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scan due proof rounds: %w", err)
	}

	sent := 0
	for _, r := range rounds {
		done, err := w.remindProofRound(ctx, r.TenantID, r.ID)
		if err != nil {
			w.log.Warn("proof deadline reminder failed", "round_id", r.ID, "err", err)
			continue
		}
		if done {
			sent++
		}
	}
	return sent, nil
}

func (w *Worker) remindProofRound(ctx context.Context, tenantID, roundID uuid.UUID) (bool, error) {
	sent := false
	err := db.WithTenant(ctx, w.pool, tenantID, func(tx pgx.Tx) error {
		var (
			roundNo   int
			dueAt     time.Time
			galleryID uuid.UUID
			projectID uuid.UUID
			status    string
		)
		if err := tx.QueryRow(ctx, `
			SELECT round_no, due_at, gallery_id, project_id, status
			FROM proof_rounds WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, roundID).
			Scan(&roundNo, &dueAt, &galleryID, &projectID, &status); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return fmt.Errorf("load due proof round: %w", err)
		}
		if status == "approved" || !dueAt.After(time.Now()) {
			return nil
		}
		loc := w.opts.Timezone
		if loc == nil {
			loc = time.UTC
		}
		due := dueAt.In(loc).Format("2006-01-02 15:04")
		payload, err := json.Marshal(map[string]any{
			"project_id": projectID, "gallery_id": galleryID,
			"round_no": roundNo, "due_at": dueAt,
		})
		if err != nil {
			return fmt.Errorf("marshal proof deadline payload: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key,
				title, body, payload, due_at)
			VALUES ($1,'proof_deadline','proof_round',$2,$3,$4,$5,$6,now())
			ON CONFLICT DO NOTHING`,
			tenantID, roundID, "proof_deadline:"+roundID.String(),
			"审片即将截止", fmt.Sprintf("第 %d 轮审片将于 %s 截止，客户尚未通过。", roundNo, due),
			payload)
		if err != nil {
			return fmt.Errorf("queue proof deadline notification: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		round, token, err := w.proof.IssueNotifyToken(ctx, tx, tenantID, roundID, "deadline")
		if err != nil {
			return err
		}
		if err := w.notifier.ProofDeadline(ctx, tx, tenantID, round, token); err != nil {
			return err
		}
		sent = true
		return nil
	})
	return sent, err
}
