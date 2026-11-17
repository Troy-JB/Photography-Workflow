package proof

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	projectstate "photography-workflow/server/internal/project"
)

// A proof round remains active through any number of review and retouch cycles.
const (
	StatusOpen             = "open"
	StatusInReview         = "in_review"
	StatusChangesRequested = "changes_requested"
	StatusRetouching       = "retouching"
	StatusResubmitted      = "resubmitted"
	StatusApproved         = "approved"
)

func ValidStatus(v string) bool {
	switch v {
	case StatusOpen, StatusInReview, StatusChangesRequested, StatusRetouching, StatusResubmitted, StatusApproved:
		return true
	}
	return false
}

var (
	ErrRoundNotFound           = errors.New("proof round not found")
	ErrProjectNotFound         = errors.New("project not found")
	ErrGalleryNotFound         = errors.New("gallery not found")
	ErrInvalidState            = errors.New("proof round is not in a state that allows this change")
	ErrInvalidInput            = errors.New("invalid proof input")
	ErrVersionConflict         = errors.New("proof round version conflict")
	ErrAssetInvalid            = errors.New("asset cannot be added to this proof round")
	ErrAssetLimit              = errors.New("proof round asset limit reached")
	ErrDerivativesIncomplete   = errors.New("proof assets have no preview derivatives")
	ErrUnknownAsset            = errors.New("asset is not in this proof round")
	ErrTokenNotFound           = errors.New("proof token not found")
	ErrInvalidAnnotation       = errors.New("invalid annotation")
	ErrCommentNotFound         = errors.New("comment not found")
	ErrParentComment           = errors.New("parent comment is not in this asset")
	ErrCommentLimit            = errors.New("comment is too long")
	ErrChecksumMismatch        = errors.New("revision checksum does not match the upload")
	ErrRevisionIncomplete      = errors.New("retouched revisions are incomplete")
	ErrSelectionChargeRequired = errors.New("selection extra-photo payment is required")
)

// Round is one customer review cycle. Items are frozen when it is created so a
// later change to the gallery selection cannot alter an issued link.
type Round struct {
	ID             uuid.UUID  `json:"id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	GalleryID      uuid.UUID  `json:"gallery_id"`
	RoundNo        int        `json:"round_no"`
	Status         string     `json:"status"`
	DueAt          time.Time  `json:"due_at"`
	ViewedAt       *time.Time `json:"viewed_at,omitempty"`
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	ApprovedAt     *time.Time `json:"approved_at,omitempty"`
	ChangeOrderID  *uuid.UUID `json:"change_order_id,omitempty"`
	OverrideBy     *uuid.UUID `json:"override_by,omitempty"`
	OverrideReason string     `json:"override_reason,omitempty"`
	ClientEmail    string     `json:"client_email,omitempty"`
	Version        int        `json:"version"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const roundColumns = `id, project_id, gallery_id, round_no, status, due_at, viewed_at,
	submitted_at, approved_at, change_order_id, override_by, override_reason,
	client_email, version, created_by, created_at, updated_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanRound(row scanner) (*Round, error) {
	var r Round
	if err := row.Scan(&r.ID, &r.ProjectID, &r.GalleryID, &r.RoundNo, &r.Status, &r.DueAt,
		&r.ViewedAt, &r.SubmittedAt, &r.ApprovedAt, &r.ChangeOrderID, &r.OverrideBy,
		&r.OverrideReason, &r.ClientEmail, &r.Version, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func loadRound(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, forUpdate bool) (*Round, error) {
	q := `SELECT ` + roundColumns + ` FROM proof_rounds WHERE tenant_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanRound(tx.QueryRow(ctx, q, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoundNotFound
		}
		return nil, fmt.Errorf("load proof round: %w", err)
	}
	if forUpdate {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, r.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: project is cancelled or archived", ErrInvalidState)
			}
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, ErrProjectNotFound
			}
			return nil, fmt.Errorf("check proof project state: %w", err)
		}
	}
	return r, nil
}

// VersionConflictError carries the current version so the customer app can
// retry against the right state instead of guessing.
type VersionConflictError struct {
	Current int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("version is %d", e.Current)
}

func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }
