package proof

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
	"photography-workflow/server/internal/storage"
)

var (
	ErrTokenInvalid   = errors.New("proof token is not valid")
	ErrTokenExpired   = errors.New("proof token has expired")
	ErrTokenRevoked   = errors.New("proof token has been revoked")
	ErrTokenExhausted = errors.New("proof token visit limit reached")
	ErrRoundClosed    = errors.New("proof round is closed")
)

type CustomerConfig struct {
	SessionTTL time.Duration
	PresignTTL time.Duration
	PageLimit  int
}

// CustomerService serves the proof link: session exchange, the revision-aware
// manifest, customer comments and the decision. Staff operations go through
// Service; this type only uses the customer principal.
type CustomerService struct {
	svc    *Service
	pool   *pgxpool.Pool
	store  storage.Storage
	tokens *auth.ProofTokens
	cfg    CustomerConfig
	log    *slog.Logger
}

func NewCustomerService(svc *Service, tokens *auth.ProofTokens, cfg CustomerConfig, log *slog.Logger) *CustomerService {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 4 * time.Hour
	}
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = 10 * time.Minute
	}
	if cfg.PageLimit <= 0 {
		cfg.PageLimit = 40
	}
	return &CustomerService{svc: svc, pool: svc.pool, store: svc.store, tokens: tokens, cfg: cfg, log: log}
}

type CustomerRound struct {
	ID           uuid.UUID `json:"id"`
	ProjectID    uuid.UUID `json:"project_id"`
	GalleryID    uuid.UUID `json:"gallery_id"`
	RoundNo      int       `json:"round_no"`
	Status       string    `json:"status"`
	DueAt        time.Time `json:"due_at"`
	Version      int       `json:"version"`
	ProjectTitle string    `json:"project_title"`
	GalleryTitle string    `json:"gallery_title"`
}

type SessionView struct {
	Session   string        `json:"session"`
	ExpiresAt time.Time     `json:"expires_at"`
	Round     CustomerRound `json:"round"`
}

type ManifestItem struct {
	AssetID    uuid.UUID `json:"asset_id"`
	Filename   string    `json:"filename"`
	Position   int       `json:"position"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	IsVideo    bool      `json:"is_video"`
	ThumbURL   string    `json:"thumb_url"`
	DisplayURL string    `json:"display_url"`
	LQIP       string    `json:"lqip,omitempty"`
	Color      string    `json:"color,omitempty"`
	// RevisionNo > 0 means this is the retouched version; the UI shows a badge
	// so the customer knows what they are reviewing.
	RevisionID *uuid.UUID `json:"revision_id,omitempty"`
	RevisionNo int        `json:"revision_no"`
	Comments   int        `json:"comments"`
}

type ManifestPage struct {
	Round      CustomerRound  `json:"round"`
	Items      []ManifestItem `json:"items"`
	Total      int            `json:"total"`
	Limit      int            `json:"limit"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

func hashProofToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func encodeCursor(position int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(position)))
}

func decodeCursor(raw string) (int, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

// CreateSession exchanges a proof link for a short-lived session. Visits are
// counted at exchange time, and the session never outlives the token.
func (s *CustomerService) CreateSession(ctx context.Context, rawToken, ip string) (*SessionView, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" || len(rawToken) > 200 {
		return nil, ErrTokenInvalid
	}
	var (
		tokenID  uuid.UUID
		tenantID uuid.UUID
		roundID  uuid.UUID
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT token_id, tenant_id, proof_round_id FROM proof_token_lookup($1)`,
		hashProofToken(rawToken)).Scan(&tokenID, &tenantID, &roundID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenInvalid
		}
		return nil, fmt.Errorf("lookup proof token: %w", err)
	}

	type granted struct {
		round     CustomerRound
		expiresAt time.Time
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*granted, error) {
		now := time.Now()
		var (
			visits     int
			maxVisits  int
			expiresAt  *time.Time
			revokedAt  *time.Time
			gallerySt  string
			galleryRev *time.Time
		)
		round := CustomerRound{ID: roundID}
		if err := tx.QueryRow(ctx, `
			SELECT t.visits, t.max_visits, t.expires_at, t.revoked_at,
			       r.status, r.round_no, r.due_at, r.version, r.project_id, r.gallery_id,
			       g.title, g.status, g.revoked_at, p.title
			FROM proof_round_tokens t
			JOIN proof_rounds r ON r.tenant_id = t.tenant_id AND r.id = t.proof_round_id
			JOIN galleries g ON g.tenant_id = r.tenant_id AND g.id = r.gallery_id
			JOIN projects p ON p.tenant_id = r.tenant_id AND p.id = r.project_id
			WHERE t.tenant_id = $1 AND t.id = $2 AND r.id = $3
			FOR UPDATE OF t`,
			tenantID, tokenID, roundID).Scan(&visits, &maxVisits, &expiresAt, &revokedAt,
			&round.Status, &round.RoundNo, &round.DueAt, &round.Version, &round.ProjectID, &round.GalleryID,
			&round.GalleryTitle, &gallerySt, &galleryRev, &round.ProjectTitle); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrTokenInvalid
			}
			return nil, fmt.Errorf("load proof token: %w", err)
		}
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, round.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, ErrRoundClosed
			}
			return nil, fmt.Errorf("check proof project state: %w", err)
		}
		switch {
		case gallerySt == "revoked" || galleryRev != nil:
			return nil, ErrGalleryRevoked
		case revokedAt != nil:
			return nil, ErrTokenRevoked
		case expiresAt != nil && !expiresAt.After(now):
			return nil, ErrTokenExpired
		case maxVisits > 0 && visits >= maxVisits:
			return nil, ErrTokenExhausted
		}
		if _, err := tx.Exec(ctx, `
			UPDATE proof_round_tokens SET visits = visits + 1, last_used_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, tokenID); err != nil {
			return nil, fmt.Errorf("record proof visit: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "proof.session.created", "proof_round", roundID.String(), ip,
			map[string]any{"token_id": tokenID}); err != nil {
			return nil, err
		}
		deadline := now.Add(s.cfg.SessionTTL)
		if expiresAt != nil && expiresAt.Before(deadline) {
			deadline = *expiresAt
		}
		return &granted{round: round, expiresAt: deadline}, nil
	})
	if err != nil {
		return nil, err
	}
	ttl := time.Until(res.expiresAt)
	if ttl <= 0 {
		return nil, ErrTokenExpired
	}
	session, expiresAt, err := s.tokens.Issue(tenantID, roundID, tokenID, ttl)
	if err != nil {
		return nil, err
	}
	return &SessionView{Session: session, ExpiresAt: expiresAt, Round: res.round}, nil
}

// Round refreshes the header after a reload. The guard already re-validated the
// token and gallery, so this only loads display fields.
func (s *CustomerService) Round(ctx context.Context, gp *auth.ProofPrincipal) (*CustomerRound, error) {
	var out *CustomerRound
	err := db.WithTenant(ctx, s.pool, gp.TenantID, func(tx pgx.Tx) error {
		r := CustomerRound{ID: gp.RoundID}
		if err := tx.QueryRow(ctx, `
			SELECT r.status, r.round_no, r.due_at, r.version, r.project_id, r.gallery_id,
			       g.title, p.title
			FROM proof_rounds r
			JOIN galleries g ON g.tenant_id = r.tenant_id AND g.id = r.gallery_id
			JOIN projects p ON p.tenant_id = r.tenant_id AND p.id = r.project_id
			WHERE r.tenant_id = $1 AND r.id = $2`, gp.TenantID, gp.RoundID).
			Scan(&r.Status, &r.RoundNo, &r.DueAt, &r.Version, &r.ProjectID, &r.GalleryID,
				&r.GalleryTitle, &r.ProjectTitle); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRoundNotFound
			}
			return fmt.Errorf("load proof round: %w", err)
		}
		out = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Page serves one slice of the round snapshot. The first read moves an open
// round to in_review; items always use the newest ready revision, falling back
// to the M5 previews.
func (s *CustomerService) Page(ctx context.Context, gp *auth.ProofPrincipal, ip, cursor string, limit int) (*ManifestPage, error) {
	if limit <= 0 {
		limit = s.cfg.PageLimit
	}
	if limit > 60 {
		limit = 60
	}
	after := 0
	if cursor != "" {
		pos, err := decodeCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed cursor", ErrInvalidInput)
		}
		after = pos
	}
	page := &ManifestPage{Items: []ManifestItem{}, Limit: limit}
	err := db.WithTenant(ctx, s.pool, gp.TenantID, func(tx pgx.Tx) error {
		round, err := loadRound(ctx, tx, gp.TenantID, gp.RoundID, true)
		if err != nil {
			return err
		}
		if round.Status == StatusOpen {
			if _, err := tx.Exec(ctx, `
				UPDATE proof_rounds
				SET status = 'in_review', viewed_at = coalesce(viewed_at, now()),
				    version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND id = $2`, gp.TenantID, gp.RoundID); err != nil {
				return fmt.Errorf("mark proof viewed: %w", err)
			}
			round, err = loadRound(ctx, tx, gp.TenantID, gp.RoundID, false)
			if err != nil {
				return err
			}
			if err := audit.Write(ctx, tx, gp.TenantID, nil, "proof_round.viewed", "proof_round", gp.RoundID.String(),
				ip, nil); err != nil {
				return err
			}
		}
		cr, err := s.roundView(ctx, tx, gp.TenantID, round)
		if err != nil {
			return err
		}
		page.Round = cr

		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM proof_round_items
			WHERE tenant_id = $1 AND proof_round_id = $2`, gp.TenantID, gp.RoundID).Scan(&page.Total); err != nil {
			return fmt.Errorf("count proof items: %w", err)
		}
		rows, err := tx.Query(ctx, `
			WITH latest AS (
				SELECT DISTINCT ON (asset_id) id, asset_id, revision_no
				FROM asset_revisions
				WHERE tenant_id = $1 AND status = 'derivative_ready'
				ORDER BY asset_id, revision_no DESC
			)
			SELECT i.asset_id, a.filename, i.position, a.is_video,
			       coalesce(rd.width, ad.width, a.width), coalesce(rd.height, ad.height, a.height),
			       latest.id, coalesce(latest.revision_no, 0),
			       coalesce(rt.object_key, ''), coalesce(rd.object_key, ''),
			       coalesce(at.object_key, ''), coalesce(ad.object_key, ''),
			       coalesce(rt.meta->>'lqip', at.meta->>'lqip', ''),
			       coalesce(rt.meta->>'color', at.meta->>'color', ''),
			       coalesce(cc.total, 0)
			FROM proof_round_items i
			JOIN assets a ON a.tenant_id = i.tenant_id AND a.id = i.asset_id
			LEFT JOIN latest ON latest.asset_id = i.asset_id
			LEFT JOIN asset_revision_derivatives rt ON rt.revision_id = latest.id AND rt.kind = 'thumb'
			LEFT JOIN asset_revision_derivatives rd ON rd.revision_id = latest.id AND rd.kind = 'display'
			LEFT JOIN asset_derivatives at
				ON at.tenant_id = i.tenant_id AND at.asset_id = i.asset_id
				AND at.kind = 'thumb' AND at.is_watermarked = false
			LEFT JOIN asset_derivatives ad
				ON ad.tenant_id = i.tenant_id AND ad.asset_id = i.asset_id
				AND ad.kind = 'display' AND ad.is_watermarked = true
			LEFT JOIN (
				SELECT asset_id, count(*) AS total FROM comments
				WHERE tenant_id = $1 AND proof_round_id = $2
				GROUP BY asset_id
			) cc ON cc.asset_id = i.asset_id
			WHERE i.tenant_id = $1 AND i.proof_round_id = $2 AND i.position > $3
			ORDER BY i.position
			LIMIT $4`, gp.TenantID, gp.RoundID, after, limit+1)
		if err != nil {
			return fmt.Errorf("load proof manifest: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				it        ManifestItem
				revID     *uuid.UUID
				revThumb  string
				revDisp   string
				assetTh   string
				assetDisp string
			)
			if err := rows.Scan(&it.AssetID, &it.Filename, &it.Position, &it.IsVideo,
				&it.Width, &it.Height, &revID, &it.RevisionNo, &revThumb, &revDisp, &assetTh, &assetDisp,
				&it.LQIP, &it.Color, &it.Comments); err != nil {
				return fmt.Errorf("scan proof manifest item: %w", err)
			}
			it.RevisionID = revID
			thumb, display := revThumb, revDisp
			if thumb == "" {
				thumb = assetTh
			}
			if display == "" {
				display = assetDisp
			}
			if thumb == "" || display == "" {
				return fmt.Errorf("%w: %s has no previews", ErrDerivativesIncomplete, it.Filename)
			}
			if it.ThumbURL, err = s.store.PresignGet(ctx, thumb, s.cfg.PresignTTL); err != nil {
				return fmt.Errorf("sign proof thumb: %w", err)
			}
			if it.DisplayURL, err = s.store.PresignGet(ctx, display, s.cfg.PresignTTL); err != nil {
				return fmt.Errorf("sign proof display: %w", err)
			}
			page.Items = append(page.Items, it)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			page.NextCursor = encodeCursor(page.Items[len(page.Items)-1].Position)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func (s *CustomerService) roundView(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *Round) (CustomerRound, error) {
	out := CustomerRound{
		ID: r.ID, ProjectID: r.ProjectID, GalleryID: r.GalleryID,
		RoundNo: r.RoundNo, Status: r.Status, DueAt: r.DueAt, Version: r.Version,
	}
	if err := tx.QueryRow(ctx, `
		SELECT g.title, p.title FROM galleries g, projects p
		WHERE g.tenant_id = $1 AND g.id = $2 AND p.tenant_id = $1 AND p.id = $3`,
		tenantID, r.GalleryID, r.ProjectID).Scan(&out.GalleryTitle, &out.ProjectTitle); err != nil {
		return out, fmt.Errorf("load proof titles: %w", err)
	}
	return out, nil
}

type SubmitInput struct {
	Decision        string `json:"decision"`
	ExpectedVersion int    `json:"expected_version"`
}

// Submit records the customer's decision. Approving is final; requesting
// changes after a resubmit is refused because the round already used its single
// retouch cycle. Repeating the same decision is idempotent.
func (s *CustomerService) Submit(ctx context.Context, gp *auth.ProofPrincipal, ip string, in SubmitInput) (*Round, error) {
	decision := strings.ToLower(strings.TrimSpace(in.Decision))
	if decision != StatusApproved && decision != StatusChangesRequested {
		return nil, fmt.Errorf("%w: decision must be approved or changes_requested", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, gp.TenantID, func(tx pgx.Tx) (*Round, error) {
		round, err := loadRound(ctx, tx, gp.TenantID, gp.RoundID, true)
		if err != nil {
			return nil, err
		}
		if round.Version != in.ExpectedVersion {
			return nil, &VersionConflictError{Current: round.Version}
		}
		if round.Status == decision {
			return round, nil
		}
		if round.Status == StatusApproved {
			return nil, fmt.Errorf("%w: the round is already approved", ErrInvalidState)
		}
		switch decision {
		case StatusApproved:
			if round.Status == StatusRetouching {
				return nil, fmt.Errorf("%w: retouching is still in progress", ErrInvalidState)
			}
		case StatusChangesRequested:
			switch round.Status {
			case StatusOpen, StatusInReview:
				// The first feedback opens the retouch cycle.
			case StatusChangesRequested:
				return round, nil
			case StatusResubmitted:
				// Each delivered revision may be reviewed again until approved.
				// The same round keeps its snapshot and accumulates immutable revisions.
			case StatusRetouching:
				return nil, fmt.Errorf("%w: retouching is still in progress", ErrInvalidState)
			}
		}

		var updated *Round
		if decision == StatusApproved {
			updated, err = scanRound(tx.QueryRow(ctx, `
				UPDATE proof_rounds
				SET status = 'approved', approved_at = now(), submitted_at = coalesce(submitted_at, now()),
				    version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND id = $2
				RETURNING `+roundColumns, gp.TenantID, gp.RoundID))
		} else {
			updated, err = scanRound(tx.QueryRow(ctx, `
				UPDATE proof_rounds
				SET status = 'changes_requested', submitted_at = now(),
				    version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND id = $2
				RETURNING `+roundColumns, gp.TenantID, gp.RoundID))
		}
		if err != nil {
			return nil, fmt.Errorf("submit proof round: %w", err)
		}
		event := "proof_round.approved"
		if decision == StatusChangesRequested {
			event = "proof_round.changes_requested"
		}
		if err := audit.Write(ctx, tx, gp.TenantID, nil, event, "proof_round", gp.RoundID.String(), ip,
			map[string]any{"token_id": gp.TokenID, "decision": decision}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, gp.TenantID, "proof_round", gp.RoundID.String(), event,
			map[string]any{"project_id": round.ProjectID, "round_no": round.RoundNo, "decision": decision}); err != nil {
			return nil, err
		}
		if s.svc.notifier != nil {
			if err := s.svc.notifier.ProofDecision(ctx, tx, gp.TenantID, updated, decision); err != nil {
				return nil, err
			}
		}
		return updated, nil
	})
}
