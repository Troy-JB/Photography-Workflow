package proof

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
)

const (
	AuthorClient = "client"
	AuthorStaff  = "staff"

	// MaxCommentBody bounds free text; annotations are bounded separately by
	// Config.AnnotationMaxBytes.
	MaxCommentBody  = 2000
	maxClientRefLen = 120
)

var ErrCommentForbidden = errors.New("comment belongs to the other side")

// Comment is one annotation or reply on a round asset. Coordinates are stored
// normalised, so the same comment renders over any revision size.
type Comment struct {
	ID           uuid.UUID       `json:"id"`
	ProofRoundID uuid.UUID       `json:"proof_round_id"`
	AssetID      uuid.UUID       `json:"asset_id"`
	ParentID     *uuid.UUID      `json:"parent_id,omitempty"`
	AuthorType   string          `json:"author_type"`
	AuthorUserID *uuid.UUID      `json:"author_user_id,omitempty"`
	ClientRef    string          `json:"client_ref,omitempty"`
	Body         string          `json:"body"`
	Annotation   json.RawMessage `json:"annotation,omitempty"`
	ResolvedAt   *time.Time      `json:"resolved_at,omitempty"`
	ResolvedBy   *uuid.UUID      `json:"resolved_by,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

const commentColumns = `id, proof_round_id, asset_id, parent_id, author_type, author_user_id,
	client_ref, body, annotation, resolved_at, resolved_by, created_at, updated_at`

func scanComment(row scanner) (*Comment, error) {
	var c Comment
	if err := row.Scan(&c.ID, &c.ProofRoundID, &c.AssetID, &c.ParentID, &c.AuthorType,
		&c.AuthorUserID, &c.ClientRef, &c.Body, &c.Annotation, &c.ResolvedAt, &c.ResolvedBy,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

type CommentInput struct {
	AssetID    uuid.UUID
	ParentID   *uuid.UUID
	Body       string
	Annotation []byte
	// ClientRef carries the Idempotency-Key for customer submissions.
	ClientRef string
}

// CommentAuthor describes who wrote or is resolving a comment. Customer
// sessions have no user row, so UserID is nil and only the token is audited.
type CommentAuthor struct {
	Type    string
	UserID  *uuid.UUID
	TokenID *uuid.UUID
	IP      string
}

// validateComment normalises the input and enforces the published limits. An
// annotation without text is valid; an empty comment is not.
func validateComment(in CommentInput, maxAnnotation int) (body string, annotation json.RawMessage, clientRef string, err error) {
	body = strings.TrimSpace(in.Body)
	if utf8.RuneCountInString(body) > MaxCommentBody {
		return "", nil, "", fmt.Errorf("%w: comment must be at most %d characters", ErrCommentLimit, MaxCommentBody)
	}
	if len(in.Annotation) > 0 {
		annotation, err = ValidateAnnotation(in.Annotation, maxAnnotation)
		if err != nil {
			return "", nil, "", err
		}
	}
	if body == "" && annotation == nil {
		return "", nil, "", fmt.Errorf("%w: comment needs text or an annotation", ErrInvalidInput)
	}
	clientRef = strings.TrimSpace(in.ClientRef)
	if len(clientRef) > maxClientRefLen {
		return "", nil, "", fmt.Errorf("%w: client_ref must be at most %d characters", ErrInvalidInput, maxClientRefLen)
	}
	return body, annotation, clientRef, nil
}

// CreateComment stores an annotation or reply. A repeated client_ref returns the
// original comment, so a retried mobile request cannot duplicate a note.
func (s *Service) CreateComment(ctx context.Context, tenantID, roundID uuid.UUID, author CommentAuthor,
	in CommentInput) (*Comment, error) {
	body, annotation, clientRef, err := validateComment(in, s.cfg.AnnotationMaxBytes)
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Comment, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, false)
		if err != nil {
			return nil, err
		}
		if round.Status == StatusApproved {
			return nil, fmt.Errorf("%w: the round is approved and read-only", ErrInvalidState)
		}
		var inRound int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM proof_round_items
			WHERE tenant_id = $1 AND proof_round_id = $2 AND asset_id = $3`,
			tenantID, roundID, in.AssetID).Scan(&inRound); err != nil {
			return nil, fmt.Errorf("check round item: %w", err)
		}
		if inRound == 0 {
			return nil, ErrUnknownAsset
		}
		if in.ParentID != nil {
			parent, err := scanComment(tx.QueryRow(ctx, `
				SELECT `+commentColumns+` FROM comments
				WHERE tenant_id = $1 AND proof_round_id = $2 AND id = $3`,
				tenantID, roundID, *in.ParentID))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, ErrParentComment
				}
				return nil, fmt.Errorf("load parent comment: %w", err)
			}
			if parent.AssetID != in.AssetID {
				return nil, fmt.Errorf("%w: reply must stay on the same asset", ErrParentComment)
			}
			if parent.ParentID != nil {
				return nil, fmt.Errorf("%w: replies are one level deep", ErrParentComment)
			}
		}
		if clientRef != "" {
			existing, err := scanComment(tx.QueryRow(ctx, `
				SELECT `+commentColumns+` FROM comments
				WHERE tenant_id = $1 AND proof_round_id = $2 AND client_ref = $3`,
				tenantID, roundID, clientRef))
			if err == nil {
				return existing, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("check comment idempotency: %w", err)
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO comments
				(tenant_id, proof_round_id, asset_id, parent_id, author_type, author_user_id,
				 client_ref, body, annotation)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, proof_round_id, client_ref) WHERE client_ref <> '' DO NOTHING
			RETURNING `+commentColumns,
			tenantID, roundID, in.AssetID, in.ParentID, author.Type, author.UserID,
			clientRef, body, annotation)
		comment, err := scanComment(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// A concurrent retry with the same client_ref won the insert.
				return scanComment(tx.QueryRow(ctx, `
					SELECT `+commentColumns+` FROM comments
					WHERE tenant_id = $1 AND proof_round_id = $2 AND client_ref = $3`,
					tenantID, roundID, clientRef))
			}
			return nil, fmt.Errorf("insert comment: %w", err)
		}
		detail := map[string]any{
			"comment_id": comment.ID, "asset_id": in.AssetID,
			"author_type": author.Type, "has_annotation": annotation != nil,
		}
		if author.TokenID != nil {
			detail["token_id"] = *author.TokenID
		}
		if err := audit.Write(ctx, tx, tenantID, author.UserID, "proof_comment.created", "proof_round", roundID.String(), author.IP, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "proof_round", roundID.String(), "proof.comment.created",
			map[string]any{"comment_id": comment.ID, "asset_id": in.AssetID, "author_type": author.Type}); err != nil {
			return nil, err
		}
		return comment, nil
	})
}

// ResolveComment marks a note as handled. Staff may resolve any note; customers
// may resolve notes written on their side of the round. Repeating the call is a
// no-op, which keeps the mobile UI retry-safe.
func (s *Service) ResolveComment(ctx context.Context, tenantID, roundID, commentID uuid.UUID,
	author CommentAuthor) (*Comment, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Comment, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, false)
		if err != nil {
			return nil, err
		}
		if round.Status == StatusApproved {
			return nil, fmt.Errorf("%w: the round is approved and read-only", ErrInvalidState)
		}
		comment, err := scanComment(tx.QueryRow(ctx, `
			SELECT `+commentColumns+` FROM comments
			WHERE tenant_id = $1 AND proof_round_id = $2 AND id = $3`,
			tenantID, roundID, commentID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrCommentNotFound
			}
			return nil, fmt.Errorf("load comment: %w", err)
		}
		if author.Type == AuthorClient && comment.AuthorType != AuthorClient {
			return nil, ErrCommentForbidden
		}
		if comment.ResolvedAt != nil {
			return comment, nil
		}
		updated, err := scanComment(tx.QueryRow(ctx, `
			UPDATE comments
			SET resolved_at = now(), resolved_by = $4, updated_at = now()
			WHERE tenant_id = $1 AND proof_round_id = $2 AND id = $3
			RETURNING `+commentColumns,
			tenantID, roundID, commentID, author.UserID))
		if err != nil {
			return nil, fmt.Errorf("resolve comment: %w", err)
		}
		// A thread is resolved as a unit: leaving replies open would hide them
		// behind the resolved root and inflate the open count forever.
		if updated.ParentID == nil {
			if _, err := tx.Exec(ctx, `
				UPDATE comments
				SET resolved_at = now(), resolved_by = $4, updated_at = now()
				WHERE tenant_id = $1 AND proof_round_id = $2 AND parent_id = $3 AND resolved_at IS NULL`,
				tenantID, roundID, commentID, author.UserID); err != nil {
				return nil, fmt.Errorf("resolve comment replies: %w", err)
			}
		}
		if err := audit.Write(ctx, tx, tenantID, author.UserID, "proof_comment.resolved", "proof_round", roundID.String(), author.IP,
			map[string]any{"comment_id": commentID, "asset_id": comment.AssetID, "author_type": author.Type}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "proof_round", roundID.String(), "proof.comment.resolved",
			map[string]any{"comment_id": commentID, "asset_id": comment.AssetID}); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

type CommentFilter struct {
	AssetID         *uuid.UUID
	IncludeResolved bool
}

// ListComments returns the thread in creation order. The cap keeps a pathological
// album from exhausting memory; the UI loads comments per asset.
func (s *Service) ListComments(ctx context.Context, tenantID, roundID uuid.UUID, f CommentFilter) ([]Comment, error) {
	out := []Comment{}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := loadRound(ctx, tx, tenantID, roundID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+commentColumns+` FROM comments
			WHERE tenant_id = $1 AND proof_round_id = $2
			  AND ($3::uuid IS NULL OR asset_id = $3::uuid)
			  AND ($4 OR resolved_at IS NULL)
			ORDER BY created_at, id
			LIMIT 2000`, tenantID, roundID, f.AssetID, f.IncludeResolved)
		if err != nil {
			return fmt.Errorf("list comments: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanComment(rows)
			if err != nil {
				return fmt.Errorf("scan comment: %w", err)
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// commentCounts returns annotation counts per asset for the customer manifest.
func commentCounts(ctx context.Context, tx pgx.Tx, tenantID, roundID uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := tx.Query(ctx, `
		SELECT asset_id, count(*) FROM comments
		WHERE tenant_id = $1 AND proof_round_id = $2
		GROUP BY asset_id`, tenantID, roundID)
	if err != nil {
		return nil, fmt.Errorf("count comments: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int)
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan comment count: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}
