package proof

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/media"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
)

const (
	maxRoundDeadline = 180 * 24 * time.Hour
)

// DerivativesIncompleteError lists how many snapshot assets still lack
// thumb/display previews. Derivation jobs were queued before it is returned.
type DerivativesIncompleteError struct {
	Missing int
	Sample  []uuid.UUID
}

func (e *DerivativesIncompleteError) Error() string {
	return fmt.Sprintf("%d assets have no complete thumb/display pair for this proof round", e.Missing)
}

func (e *DerivativesIncompleteError) Unwrap() error { return ErrDerivativesIncomplete }

// RevisionIncompleteError tells the operator which assets still need a
// retouched version before the round can be resubmitted.
type RevisionIncompleteError struct {
	Missing int
	Sample  []uuid.UUID
}

func (e *RevisionIncompleteError) Error() string {
	return fmt.Sprintf("%d assets have no retouched revision for this round", e.Missing)
}

func (e *RevisionIncompleteError) Unwrap() error { return ErrRevisionIncomplete }

type CreateRoundInput struct {
	ProjectID      uuid.UUID
	GalleryID      uuid.UUID
	DueAt          time.Time
	AssetIDs       []uuid.UUID
	ChangeOrderID  *uuid.UUID
	OverrideReason string
}

// CreatedRound carries the only copy of the first proof link.
type CreatedRound struct {
	Round *Round       `json:"round"`
	Token *IssuedToken `json:"token"`
	Items int          `json:"items"`
}

type RoundSummary struct {
	Round
	AssetCount   int `json:"asset_count"`
	CommentCount int `json:"comment_count"`
	OpenComments int `json:"open_comments"`
}

// RoundItem is one snapshot asset with its current previews. RevisionNo > 0
// means the customer is looking at a retouched version.
type RoundItem struct {
	AssetID           uuid.UUID  `json:"asset_id"`
	Filename          string     `json:"filename"`
	Position          int        `json:"position"`
	Width             int        `json:"width"`
	Height            int        `json:"height"`
	IsVideo           bool       `json:"is_video"`
	RevisionID        *uuid.UUID `json:"revision_id,omitempty"`
	RevisionNo        int        `json:"revision_no"`
	RevisionState     string     `json:"revision_state,omitempty"`
	RevisionCreatedAt *time.Time `json:"revision_created_at,omitempty"`
	ThumbURL          string     `json:"thumb_url,omitempty"`
	DisplayURL        string     `json:"display_url,omitempty"`
	Comments          int        `json:"comments"`
	OpenComments      int        `json:"open_comments"`
}

type RetouchTaskSummary struct {
	ID          uuid.UUID  `json:"id"`
	Status      string     `json:"status"`
	Provider    string     `json:"provider"`
	DueAt       *time.Time `json:"due_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

type RoundDetail struct {
	Round
	AssetCount   int                 `json:"asset_count"`
	CommentCount int                 `json:"comment_count"`
	OpenComments int                 `json:"open_comments"`
	Items        []RoundItem         `json:"items"`
	Tokens       []Token             `json:"tokens"`
	Retouch      *RetouchTaskSummary `json:"retouch,omitempty"`
}

type ExtendInput struct {
	ExpectedVersion int
	DueAt           time.Time
}

// CreateRound snapshots the customer's selected assets and, when the project has
// used up its included rounds, enforces the supplement-amendment gate.
func (s *Service) CreateRound(ctx context.Context, tenantID, actor uuid.UUID, role, ip string,
	in CreateRoundInput) (*CreatedRound, error) {
	now := time.Now()
	if in.ProjectID == uuid.Nil || in.GalleryID == uuid.Nil {
		return nil, fmt.Errorf("%w: project_id and gallery_id are required", ErrInvalidInput)
	}
	if in.DueAt.IsZero() || !in.DueAt.After(now) {
		return nil, fmt.Errorf("%w: due_at must be in the future", ErrInvalidInput)
	}
	if in.DueAt.After(now.Add(maxRoundDeadline)) {
		return nil, fmt.Errorf("%w: due_at must be within %d days", ErrInvalidInput, int(maxRoundDeadline.Hours()/24))
	}
	if in.ChangeOrderID != nil || strings.TrimSpace(in.OverrideReason) != "" {
		return nil, fmt.Errorf("%w: contract and change-order round gates are disabled; extra-image charges use selection payment orders", ErrInvalidInput)
	}

	type outcome struct {
		created *CreatedRound
		missing int
		sample  []uuid.UUID
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*outcome, error) {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, in.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, ErrProjectNotFound
			}
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: project is cancelled or archived", ErrInvalidState)
			}
			return nil, fmt.Errorf("lock project: %w", err)
		}
		var (
			galleryProject uuid.UUID
			galleryStatus  string
		)
		if err := tx.QueryRow(ctx, `
			SELECT project_id, status FROM galleries
			WHERE tenant_id = $1 AND id = $2`, tenantID, in.GalleryID).
			Scan(&galleryProject, &galleryStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrGalleryNotFound
			}
			return nil, fmt.Errorf("load gallery: %w", err)
		}
		if galleryProject != in.ProjectID {
			return nil, fmt.Errorf("%w: gallery belongs to another project", ErrGalleryNotFound)
		}
		if galleryStatus != "published" {
			return nil, fmt.Errorf("%w: proof rounds require a published gallery", ErrInvalidState)
		}
		if err := ensureSelectionChargesSettled(ctx, tx, tenantID, in.ProjectID, in.GalleryID); err != nil {
			return nil, err
		}

		clientEmail, err := projectClientEmail(ctx, tx, tenantID, in.ProjectID)
		if err != nil {
			return nil, err
		}

		assets, err := snapshotSelection(ctx, tx, tenantID, in.GalleryID, in.AssetIDs, s.cfg.MaxAssets)
		if err != nil {
			return nil, err
		}

		watermark, err := galleryWatermark(ctx, tx, tenantID, in.GalleryID, s.cfg.DefaultWatermark)
		if err != nil {
			return nil, err
		}
		missing, err := missingDerivatives(ctx, tx, tenantID, assets)
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			for _, id := range missing {
				if _, err := media.EnqueueDerive(ctx, tx, tenantID, id, watermark, false); err != nil {
					return nil, fmt.Errorf("queue derive: %w", err)
				}
			}
			sample := missing
			if len(sample) > 5 {
				sample = sample[:5]
			}
			return &outcome{missing: len(missing), sample: sample}, nil
		}

		var roundNo int
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(max(round_no), 0) + 1 FROM proof_rounds
			WHERE tenant_id = $1 AND project_id = $2`, tenantID, in.ProjectID).Scan(&roundNo); err != nil {
			return nil, fmt.Errorf("next round number: %w", err)
		}
		var changeOrderID, overrideBy *uuid.UUID
		const overrideReason = ""

		round, err := scanRound(tx.QueryRow(ctx, `
			INSERT INTO proof_rounds
				(tenant_id, project_id, gallery_id, round_no, status, due_at,
				 change_order_id, override_by, override_reason, client_email, created_by)
			VALUES ($1,$2,$3,$4,'open',$5,$6,$7,$8,$9,$10)
			RETURNING `+roundColumns,
			tenantID, in.ProjectID, in.GalleryID, roundNo, in.DueAt,
			changeOrderID, overrideBy, overrideReason, clientEmail, actor))
		if err != nil {
			return nil, fmt.Errorf("insert proof round: %w", err)
		}
		for i, assetID := range assets {
			if _, err := tx.Exec(ctx, `
				INSERT INTO proof_round_items (tenant_id, proof_round_id, asset_id, position)
				VALUES ($1,$2,$3,$4)`,
				tenantID, round.ID, assetID, i+1); err != nil {
				return nil, fmt.Errorf("insert proof round item: %w", err)
			}
		}

		token, err := s.issueToken(ctx, tx, tenantID, round, &actor, ip, TokenInput{Label: "first"})
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "proof_round.created", "proof_round", round.ID.String(), ip,
			map[string]any{
				"gallery_id": in.GalleryID, "round_no": roundNo, "assets": len(assets),
				"change_order_id": changeOrderID,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "proof_round", round.ID.String(), "proof_round.created",
			map[string]any{
				"project_id": in.ProjectID, "gallery_id": in.GalleryID,
				"round_no": roundNo, "assets": len(assets),
			}); err != nil {
			return nil, err
		}
		if s.notifier != nil {
			if err := s.notifier.ProofInvite(ctx, tx, tenantID, round, token); err != nil {
				return nil, err
			}
		}
		return &outcome{created: &CreatedRound{Round: round, Token: token, Items: len(assets)}}, nil
	})
	if err != nil {
		return nil, err
	}
	if res.missing > 0 {
		return nil, &DerivativesIncompleteError{Missing: res.missing, Sample: res.sample}
	}
	return res.created, nil
}

func ensureSelectionChargesSettled(ctx context.Context, tx pgx.Tx, tenantID, projectID, galleryID uuid.UUID) error {
	var included int
	var unitPrice int64
	if err := tx.QueryRow(ctx, `SELECT included_retouch_count,extra_retouch_unit_price_cents FROM galleries WHERE tenant_id=$1 AND id=$2`, tenantID, galleryID).Scan(&included, &unitPrice); err != nil {
		return err
	}
	var selected int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM gallery_selections WHERE tenant_id=$1 AND gallery_id=$2 AND decision='selected'`, tenantID, galleryID).Scan(&selected); err != nil {
		return err
	}
	required := int64(max(0, selected-included)) * unitPrice
	if required == 0 {
		return nil
	}
	var paid, refunded int64
	var pending int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT coalesce(sum(amount_cents),0) FROM selection_charge_batches WHERE tenant_id=$1 AND project_id=$2 AND gallery_id=$3 AND status='paid'),
		(SELECT coalesce(sum(r.amount_cents),0) FROM refunds r JOIN payments p ON p.tenant_id=r.tenant_id AND p.id=r.payment_id JOIN payment_orders o ON o.tenant_id=p.tenant_id AND o.id=p.order_id JOIN selection_charge_batches b ON b.tenant_id=o.tenant_id AND b.id=o.selection_charge_batch_id WHERE b.tenant_id=$1 AND b.project_id=$2 AND b.gallery_id=$3 AND r.status IN ('pending','succeeded')),
		(SELECT count(*) FROM selection_charge_batches WHERE tenant_id=$1 AND project_id=$2 AND gallery_id=$3 AND status='pending')`, tenantID, projectID, galleryID).Scan(&paid, &refunded, &pending); err != nil {
		return err
	}
	if pending > 0 || paid-refunded < required {
		return ErrSelectionChargeRequired
	}
	return nil
}

// projectClientEmail snapshots the recipient of proof mail. It stays empty when
// the project has no client or the client has no address; only the explicit
// resend endpoint treats that as an error.
func projectClientEmail(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) (string, error) {
	var email string
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(c.email, '') FROM projects p
		LEFT JOIN clients c ON c.tenant_id = p.tenant_id AND c.id = p.client_id
		WHERE p.tenant_id = $1 AND p.id = $2`, tenantID, projectID).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrProjectNotFound
		}
		return "", fmt.Errorf("load project client email: %w", err)
	}
	return strings.TrimSpace(email), nil
}

// snapshotSelection freezes the selected assets in gallery order. An optional
// subset must itself be selected, so a round can never show rejected frames.
func snapshotSelection(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID,
	only []uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT ga.asset_id
		FROM gallery_assets ga
		JOIN gallery_selections s
			ON s.tenant_id = ga.tenant_id AND s.gallery_id = ga.gallery_id AND s.asset_id = ga.asset_id
		WHERE ga.tenant_id = $1 AND ga.gallery_id = $2 AND s.decision = 'selected'
		ORDER BY ga.position, ga.asset_id`, tenantID, galleryID)
	if err != nil {
		return nil, fmt.Errorf("load selected assets: %w", err)
	}
	defer rows.Close()
	wanted := make(map[uuid.UUID]struct{}, len(only))
	for _, id := range only {
		wanted[id] = struct{}{}
	}
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan selected asset: %w", err)
		}
		if len(wanted) > 0 {
			if _, ok := wanted[id]; !ok {
				continue
			}
			delete(wanted, id)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(wanted) > 0 {
		return nil, fmt.Errorf("%w: some assets are not selected in this gallery", ErrAssetInvalid)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the gallery has no submitted selection", ErrAssetInvalid)
	}
	if limit > 0 && len(out) > limit {
		return nil, fmt.Errorf("%w: at most %d assets per round", ErrAssetLimit, limit)
	}
	return out, nil
}

func missingDerivatives(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, assets []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id
		FROM assets a
		WHERE a.tenant_id = $1 AND a.id = ANY($2)
		  AND NOT EXISTS (
			SELECT 1 FROM asset_derivatives t
			WHERE t.tenant_id = a.tenant_id AND t.asset_id = a.id
			  AND t.kind = 'thumb' AND t.is_watermarked = false
			  AND t.object_key <> '' AND t.meta->>'complete' = 'true'
		  )
		ORDER BY a.id`, tenantID, assets)
	if err != nil {
		return nil, fmt.Errorf("check proof derivatives: %w", err)
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan missing derivative: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Service) ListRounds(ctx context.Context, tenantID uuid.UUID, projectID, galleryID *uuid.UUID,
	status string, limit, offset int) ([]RoundSummary, int, error) {
	out := []RoundSummary{}
	var total int
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		where := `r.tenant_id = $1`
		args := []any{tenantID}
		if projectID != nil {
			args = append(args, *projectID)
			where += fmt.Sprintf(" AND r.project_id = $%d", len(args))
		}
		if galleryID != nil {
			args = append(args, *galleryID)
			where += fmt.Sprintf(" AND r.gallery_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			where += fmt.Sprintf(" AND r.status = $%d", len(args))
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM proof_rounds r WHERE `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count proof rounds: %w", err)
		}
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT r.id, r.project_id, r.gallery_id, r.round_no, r.status, r.due_at, r.viewed_at,
			       r.submitted_at, r.approved_at, r.change_order_id, r.override_by, r.override_reason,
			       r.version, r.created_by, r.created_at, r.updated_at,
			       (SELECT count(*) FROM proof_round_items i
			        WHERE i.tenant_id = r.tenant_id AND i.proof_round_id = r.id),
			       (SELECT count(*) FROM comments c
			        WHERE c.tenant_id = r.tenant_id AND c.proof_round_id = r.id),
			       (SELECT count(*) FROM comments c
			        WHERE c.tenant_id = r.tenant_id AND c.proof_round_id = r.id AND c.resolved_at IS NULL)
			FROM proof_rounds r
			WHERE %s
			ORDER BY r.created_at DESC, r.id
			LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args)), args...)
		if err != nil {
			return fmt.Errorf("list proof rounds: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it RoundSummary
			if err := rows.Scan(&it.ID, &it.ProjectID, &it.GalleryID, &it.RoundNo, &it.Status, &it.DueAt,
				&it.ViewedAt, &it.SubmittedAt, &it.ApprovedAt, &it.ChangeOrderID, &it.OverrideBy,
				&it.OverrideReason, &it.Version, &it.CreatedBy, &it.CreatedAt, &it.UpdatedAt,
				&it.AssetCount, &it.CommentCount, &it.OpenComments); err != nil {
				return fmt.Errorf("scan proof round: %w", err)
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetRound loads the staff view: snapshot items with the newest revision (or the
// original previews), comment counters, tokens and the retouch task.
func (s *Service) GetRound(ctx context.Context, tenantID, roundID uuid.UUID) (*RoundDetail, error) {
	detail, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*RoundDetail, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, false)
		if err != nil {
			return nil, err
		}
		d := &RoundDetail{Round: *round, Items: []RoundItem{}, Tokens: []Token{}}
		if d.AssetCount, d.CommentCount, d.OpenComments, err = roundCounts(ctx, tx, tenantID, roundID); err != nil {
			return nil, err
		}
		if d.Items, err = loadRoundItems(ctx, tx, s, tenantID, roundID); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+tokenColumns+` FROM proof_round_tokens
			WHERE tenant_id = $1 AND proof_round_id = $2
			ORDER BY created_at DESC`, tenantID, roundID)
		if err != nil {
			return nil, fmt.Errorf("list proof tokens: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanToken(rows)
			if err != nil {
				return nil, fmt.Errorf("scan proof token: %w", err)
			}
			d.Tokens = append(d.Tokens, *t)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var task RetouchTaskSummary
		err = tx.QueryRow(ctx, `
			SELECT id, status, provider, due_at, started_at, delivered_at
			FROM retouch_tasks WHERE tenant_id = $1 AND proof_round_id = $2
			ORDER BY created_at DESC LIMIT 1`, tenantID, roundID).
			Scan(&task.ID, &task.Status, &task.Provider, &task.DueAt, &task.StartedAt, &task.DeliveredAt)
		if err == nil {
			d.Retouch = &task
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("load retouch task: %w", err)
		}
		return d, nil
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

func roundCounts(ctx context.Context, tx pgx.Tx, tenantID, roundID uuid.UUID) (assets, comments, open int, err error) {
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM proof_round_items WHERE tenant_id = $1 AND proof_round_id = $2),
		       (SELECT count(*) FROM comments WHERE tenant_id = $1 AND proof_round_id = $2),
		       (SELECT count(*) FROM comments WHERE tenant_id = $1 AND proof_round_id = $2 AND resolved_at IS NULL)`,
		tenantID, roundID).Scan(&assets, &comments, &open); err != nil {
		return 0, 0, 0, fmt.Errorf("count proof round: %w", err)
	}
	return assets, comments, open, nil
}

func loadRoundItems(ctx context.Context, tx pgx.Tx, s *Service, tenantID, roundID uuid.UUID) ([]RoundItem, error) {
	rows, err := tx.Query(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (asset_id) id, asset_id, revision_no, status, created_at
			FROM asset_revisions
			WHERE tenant_id = $1
			ORDER BY asset_id, revision_no DESC
		)
		SELECT i.asset_id, a.filename, i.position, a.width, a.height, a.is_video,
		       latest.id, coalesce(latest.revision_no, 0), coalesce(latest.status, ''), latest.created_at,
		       CASE WHEN latest.status = 'derivative_ready' THEN coalesce(rdt.object_key, '') ELSE '' END,
		       CASE WHEN latest.status = 'derivative_ready' THEN coalesce(rdd.object_key, '') ELSE '' END,
		       coalesce(adt.object_key, ''), coalesce(adp.object_key, ''),
		       coalesce(cc.total, 0), coalesce(cc.open, 0)
		FROM proof_round_items i
		JOIN assets a ON a.tenant_id = i.tenant_id AND a.id = i.asset_id
		LEFT JOIN latest ON latest.asset_id = i.asset_id
		LEFT JOIN asset_revision_derivatives rdt ON rdt.revision_id = latest.id AND rdt.kind = 'thumb'
		LEFT JOIN asset_revision_derivatives rdd ON rdd.revision_id = latest.id AND rdd.kind = 'display'
		LEFT JOIN asset_derivatives adt
			ON adt.tenant_id = i.tenant_id AND adt.asset_id = i.asset_id
			AND adt.kind = 'thumb' AND adt.is_watermarked = false
		LEFT JOIN asset_derivatives adp
			ON adp.tenant_id = i.tenant_id AND adp.asset_id = i.asset_id
			AND adp.kind = 'display' AND adp.is_watermarked = true
		LEFT JOIN (
			SELECT asset_id, count(*) AS total,
			       count(*) FILTER (WHERE resolved_at IS NULL) AS open
			FROM comments
			WHERE tenant_id = $1 AND proof_round_id = $2
			GROUP BY asset_id
		) cc ON cc.asset_id = i.asset_id
		WHERE i.tenant_id = $1 AND i.proof_round_id = $2
		ORDER BY i.position`, tenantID, roundID)
	if err != nil {
		return nil, fmt.Errorf("load proof round items: %w", err)
	}
	defer rows.Close()
	out := []RoundItem{}
	for rows.Next() {
		var (
			it        RoundItem
			revID     *uuid.UUID
			revThumb  string
			revDisp   string
			assetTh   string
			assetDisp string
		)
		if err := rows.Scan(&it.AssetID, &it.Filename, &it.Position, &it.Width, &it.Height, &it.IsVideo,
			&revID, &it.RevisionNo, &it.RevisionState, &it.RevisionCreatedAt, &revThumb, &revDisp,
			&assetTh, &assetDisp, &it.Comments, &it.OpenComments); err != nil {
			return nil, fmt.Errorf("scan proof round item: %w", err)
		}
		it.RevisionID = revID
		thumb, display := revThumb, revDisp
		if thumb == "" {
			thumb = assetTh
		}
		if display == "" {
			display = assetDisp
		}
		if thumb != "" {
			url, err := s.store.PresignGet(ctx, thumb, s.cfg.PresignTTL)
			if err != nil {
				return nil, fmt.Errorf("sign proof thumb: %w", err)
			}
			it.ThumbURL = url
		}
		if display != "" {
			url, err := s.store.PresignGet(ctx, display, s.cfg.PresignTTL)
			if err != nil {
				return nil, fmt.Errorf("sign proof display: %w", err)
			}
			it.DisplayURL = url
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ExtendDue moves the deadline. The expected_version guard stops two staff
// members from silently overwriting each other's change.
func (s *Service) ExtendDue(ctx context.Context, tenantID, roundID, actor uuid.UUID, ip string,
	in ExtendInput) (*Round, error) {
	now := time.Now()
	if !in.DueAt.After(now) {
		return nil, fmt.Errorf("%w: due_at must be in the future", ErrInvalidInput)
	}
	if in.DueAt.After(now.Add(maxRoundDeadline)) {
		return nil, fmt.Errorf("%w: due_at must be within %d days", ErrInvalidInput, int(maxRoundDeadline.Hours()/24))
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Round, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, true)
		if err != nil {
			return nil, err
		}
		if round.Version != in.ExpectedVersion {
			return nil, &VersionConflictError{Current: round.Version}
		}
		if round.Status == StatusApproved {
			return nil, fmt.Errorf("%w: the round is already approved", ErrInvalidState)
		}
		updated, err := scanRound(tx.QueryRow(ctx, `
			UPDATE proof_rounds SET due_at = $3, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+roundColumns, tenantID, roundID, in.DueAt))
		if err != nil {
			return nil, fmt.Errorf("extend proof round: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "proof_round.due_extended", "proof_round", roundID.String(), ip,
			map[string]any{"previous": round.DueAt, "due_at": in.DueAt}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "proof_round", roundID.String(), "proof_round.due_extended",
			map[string]any{"due_at": in.DueAt}); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

func (s *Service) IssueToken(ctx context.Context, tenantID, roundID, actor uuid.UUID, ip string,
	in TokenInput) (*IssuedToken, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*IssuedToken, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, true)
		if err != nil {
			return nil, err
		}
		if round.Status == StatusApproved {
			return nil, fmt.Errorf("%w: the round is already approved", ErrInvalidState)
		}
		return s.issueToken(ctx, tx, tenantID, round, &actor, ip, in)
	})
}

func (s *Service) RevokeToken(ctx context.Context, tenantID, roundID, tokenID, actor uuid.UUID, ip string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := loadRound(ctx, tx, tenantID, roundID, false); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
			UPDATE proof_round_tokens SET revoked_at = now()
			WHERE tenant_id = $1 AND proof_round_id = $2 AND id = $3 AND revoked_at IS NULL`,
			tenantID, roundID, tokenID)
		if err != nil {
			return fmt.Errorf("revoke proof token: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return ErrTokenNotFound
		}
		return audit.Write(ctx, tx, tenantID, &actor, "proof.token.revoked", "proof_round", roundID.String(), ip,
			map[string]any{"token_id": tokenID})
	})
}

// StartRetouch moves changes_requested to retouching and creates the work order.
// Provider integration (folder export) is attached through the exporter hook.
func (s *Service) StartRetouch(ctx context.Context, tenantID, roundID, actor uuid.UUID, ip string) (*RetouchTaskSummary, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*RetouchTaskSummary, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, true)
		if err != nil {
			return nil, err
		}
		if round.Status != StatusChangesRequested && round.Status != StatusResubmitted {
			return nil, fmt.Errorf("%w: retouch can only start after the customer requests changes", ErrInvalidState)
		}
		provider := s.cfg.RetouchProvider
		if provider == "" {
			provider = "manual"
		}
		task := &RetouchTaskSummary{Provider: provider}
		if err := tx.QueryRow(ctx, `
			INSERT INTO retouch_tasks (tenant_id, project_id, proof_round_id, status, provider, started_at, created_by)
			VALUES ($1,$2,$3,'in_progress',$4,now(),$5)
			RETURNING id, status, provider, due_at, started_at, delivered_at`,
			tenantID, round.ProjectID, roundID, provider, actor).
			Scan(&task.ID, &task.Status, &task.Provider, &task.DueAt, &task.StartedAt, &task.DeliveredAt); err != nil {
			return nil, fmt.Errorf("create retouch task: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO retouch_task_items (tenant_id, retouch_task_id, asset_id, position)
			SELECT tenant_id, $2, asset_id, position
			FROM proof_round_items WHERE tenant_id = $1 AND proof_round_id = $3
			ORDER BY position`, tenantID, task.ID, roundID); err != nil {
			return nil, fmt.Errorf("create retouch task items: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE proof_rounds SET status = 'retouching', version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, roundID); err != nil {
			return nil, fmt.Errorf("start retouch: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "proof_round.retouching", "proof_round", roundID.String(), ip,
			map[string]any{"retouch_task_id": task.ID, "provider": provider}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "proof_round", roundID.String(), "proof_round.retouching",
			map[string]any{"retouch_task_id": task.ID, "provider": provider}); err != nil {
			return nil, err
		}
		if s.exporter != nil {
			if err := s.exporter.ExportTask(ctx, tx, tenantID, task.ID); err != nil {
				return nil, err
			}
		}
		return task, nil
	})
}

// Resubmit hands the retouched versions back to the customer. Every snapshot
// asset must have a derivative-ready revision created after the task started;
// otherwise the operator is told exactly what is missing.
func (s *Service) Resubmit(ctx context.Context, tenantID, roundID, actor uuid.UUID, ip string) (*Round, error) {
	type outcome struct {
		round   *Round
		missing []uuid.UUID
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*outcome, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, true)
		if err != nil {
			return nil, err
		}
		if round.Status != StatusRetouching {
			return nil, fmt.Errorf("%w: only a retouching round can be resubmitted", ErrInvalidState)
		}
		var startedAt *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT started_at FROM retouch_tasks
			WHERE tenant_id = $1 AND proof_round_id = $2 AND status IN ('in_progress','delivered')
			ORDER BY created_at DESC LIMIT 1`, tenantID, roundID).Scan(&startedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: no active retouch task", ErrInvalidState)
			}
			return nil, fmt.Errorf("load retouch task: %w", err)
		}
		since := round.UpdatedAt
		if startedAt != nil {
			since = *startedAt
		}
		rows, err := tx.Query(ctx, `
			SELECT i.asset_id
			FROM proof_round_items i
			WHERE i.tenant_id = $1 AND i.proof_round_id = $2
			  AND NOT EXISTS (
				SELECT 1 FROM asset_revisions r
				WHERE r.tenant_id = i.tenant_id AND r.asset_id = i.asset_id
				  AND r.status = 'derivative_ready' AND r.created_at >= $3
			  )
			ORDER BY i.position`, tenantID, roundID, since)
		if err != nil {
			return nil, fmt.Errorf("check resubmit revisions: %w", err)
		}
		defer rows.Close()
		missing := []uuid.UUID{}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan missing revision: %w", err)
			}
			missing = append(missing, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			return &outcome{round: round, missing: missing}, nil
		}
		updated, err := scanRound(tx.QueryRow(ctx, `
			UPDATE proof_rounds
			SET status = 'resubmitted', version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+roundColumns, tenantID, roundID))
		if err != nil {
			return nil, fmt.Errorf("resubmit proof round: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE retouch_tasks SET status = 'delivered', delivered_at = coalesce(delivered_at, now()), updated_at = now()
			WHERE tenant_id = $1 AND proof_round_id = $2 AND status IN ('in_progress','delivered')`, tenantID, roundID); err != nil {
			return nil, fmt.Errorf("deliver retouch task: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "proof_round.resubmitted", "proof_round", roundID.String(), ip, nil); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "proof_round", roundID.String(), "proof_round.resubmitted",
			map[string]any{"project_id": round.ProjectID}); err != nil {
			return nil, err
		}
		if s.notifier != nil {
			// A fresh link goes out with every resubmission: customers often
			// reopen months-old mail and the old token may already be spent.
			token, err := s.issueToken(ctx, tx, tenantID, updated, &actor, ip, TokenInput{Label: "resubmitted"})
			if err != nil {
				return nil, err
			}
			if err := s.notifier.ProofResubmitted(ctx, tx, tenantID, updated, token); err != nil {
				return nil, err
			}
		}
		return &outcome{round: updated}, nil
	})
	if err != nil {
		return nil, err
	}
	if len(res.missing) > 0 {
		sample := res.missing
		if len(sample) > 5 {
			sample = sample[:5]
		}
		return nil, &RevisionIncompleteError{Missing: len(res.missing), Sample: sample}
	}
	return res.round, nil
}
