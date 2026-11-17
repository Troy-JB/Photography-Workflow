package proof

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

// maxRevisionFormBytes bounds the whole multipart request: the 50 MB file plus
// field overhead.
const maxRevisionFormBytes = (50 << 20) + (1 << 20)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Register(r chi.Router) {
	staff := auth.RequireAnyRole("owner", "staff", "assistant")
	read := auth.RequireAnyRole("owner", "staff", "assistant", "retoucher")
	r.With(read).Get("/proof-rounds", h.list)
	r.With(staff).Post("/proof-rounds", h.create)
	r.Route("/proof-rounds/{roundID}", func(r chi.Router) {
		r.With(read).Get("/", h.get)
		r.With(staff).Patch("/", h.extend)
		r.With(staff).Post("/tokens", h.issueToken)
		r.With(staff).Delete("/tokens/{tokenID}", h.revokeToken)
		r.With(staff).Post("/notify", h.notify)
		r.With(read).Get("/comments", h.listComments)
		r.With(staff).Post("/comments/{commentID}/resolve", h.resolveComment)
		r.With(staff).Post("/retouch", h.startRetouch)
		r.With(staff).Post("/revisions", h.uploadRevision)
		r.With(staff).Post("/resubmit", h.resubmit)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	projectID, ok := optionalUUIDQuery(w, r, "project_id")
	if !ok {
		return
	}
	galleryID, ok := optionalUUIDQuery(w, r, "gallery_id")
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !ValidStatus(status) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown status")
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListRounds(r.Context(), p.TenantID, projectID, galleryID, status, limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

type createRequest struct {
	ProjectID      uuid.UUID   `json:"project_id"`
	GalleryID      uuid.UUID   `json:"gallery_id"`
	DueAt          time.Time   `json:"due_at"`
	AssetIDs       []uuid.UUID `json:"asset_ids"`
	ChangeOrderID  *uuid.UUID  `json:"change_order_id"`
	OverrideReason string      `json:"override_reason"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	created, err := h.svc.CreateRound(r.Context(), p.TenantID, p.UserID, p.Role, r.RemoteAddr, CreateRoundInput{
		ProjectID:      req.ProjectID,
		GalleryID:      req.GalleryID,
		DueAt:          req.DueAt,
		AssetIDs:       req.AssetIDs,
		ChangeOrderID:  req.ChangeOrderID,
		OverrideReason: req.OverrideReason,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, created)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	detail, err := h.svc.GetRound(r.Context(), p.TenantID, roundID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

type extendRequest struct {
	ExpectedVersion int       `json:"expected_version"`
	DueAt           time.Time `json:"due_at"`
}

func (h *Handler) extend(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	var req extendRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	round, err := h.svc.ExtendDue(r.Context(), p.TenantID, roundID, p.UserID, r.RemoteAddr, ExtendInput{
		ExpectedVersion: req.ExpectedVersion, DueAt: req.DueAt,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, round)
}

type tokenRequest struct {
	Label     string     `json:"label"`
	MaxVisits int        `json:"max_visits"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (h *Handler) issueToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	var req tokenRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	token, err := h.svc.IssueToken(r.Context(), p.TenantID, roundID, p.UserID, r.RemoteAddr, TokenInput{
		Label: req.Label, MaxVisits: req.MaxVisits, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, token)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid token id", "")
		return
	}
	if err := h.svc.RevokeToken(r.Context(), p.TenantID, roundID, tokenID, p.UserID, r.RemoteAddr); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type notifyRequest struct {
	Channel string `json:"channel"`
}

func (h *Handler) notify(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	var req notifyRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	token, err := h.svc.NotifyCustomer(r.Context(), p.TenantID, roundID, p.UserID, r.RemoteAddr, req.Channel)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"token": token.Plaintext, "proof_url": token.ProofURL, "expires_at": token.ExpiresAt,
	})
}

func (h *Handler) listComments(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	assetID, ok := optionalUUIDQuery(w, r, "asset_id")
	if !ok {
		return
	}
	includeResolved := r.URL.Query().Get("include_resolved") == "true"
	items, err := h.svc.ListComments(r.Context(), p.TenantID, roundID, CommentFilter{
		AssetID: assetID, IncludeResolved: includeResolved,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) resolveComment(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	commentID, err := uuid.Parse(chi.URLParam(r, "commentID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid comment id", "")
		return
	}
	comment, err := h.svc.ResolveComment(r.Context(), p.TenantID, roundID, commentID, CommentAuthor{
		Type: AuthorStaff, UserID: &p.UserID, IP: r.RemoteAddr,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, comment)
}

func (h *Handler) startRetouch(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	task, err := h.svc.StartRetouch(r.Context(), p.TenantID, roundID, p.UserID, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, task)
}

func (h *Handler) uploadRevision(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRevisionFormBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid multipart upload", err.Error())
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	assetID, err := uuid.Parse(strings.TrimSpace(r.FormValue("asset_id")))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "asset_id is required")
		return
	}
	size, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("size")), 10, 64)
	if err != nil && r.FormValue("size") != "" {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "size must be a number")
		return
	}
	width, _ := strconv.Atoi(r.FormValue("width"))
	height, _ := strconv.Atoi(r.FormValue("height"))

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "file is required")
		return
	}
	defer file.Close()
	if size == 0 {
		size = header.Size
	}
	actorID := p.UserID
	res, err := h.svc.ImportRevision(r.Context(), p.TenantID, roundID, &actorID, r.RemoteAddr, RevisionInput{
		AssetID:  assetID,
		Filename: header.Filename,
		SHA256:   strings.TrimSpace(r.FormValue("sha256")),
		Size:     size,
		Width:    width,
		Height:   height,
		Note:     strings.TrimSpace(r.FormValue("note")),
	}, file)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, res)
}

func (h *Handler) resubmit(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	roundID, ok := h.roundID(w, r)
	if !ok {
		return
	}
	round, err := h.svc.Resubmit(r.Context(), p.TenantID, roundID, p.UserID, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, round)
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return nil, false
	}
	return p, true
}

func (h *Handler) roundID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "roundID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid proof round id", "")
		return uuid.Nil, false
	}
	return id, true
}

func optionalUUIDQuery(w http.ResponseWriter, r *http.Request, name string) (*uuid.UUID, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid "+name, "")
		return nil, false
	}
	return &id, true
}

func sampleIDs(ids []uuid.UUID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id.String())
	}
	return strings.Join(parts, ", ")
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		derivatives *DerivativesIncompleteError
		revisions   *RevisionIncompleteError
		conflict    *VersionConflictError
	)
	switch {
	case errors.As(err, &derivatives):
		httpx.WriteProblem(w, r, http.StatusConflict, "derivatives_incomplete", "Proof previews are not ready",
			fmt.Sprintf("%d assets are missing thumb/display previews (e.g. %s); derivation jobs were queued, retry after the worker finishes",
				derivatives.Missing, sampleIDs(derivatives.Sample)))
	case errors.As(err, &revisions):
		httpx.WriteProblem(w, r, http.StatusConflict, "revisions_incomplete", "Retouched versions are incomplete",
			fmt.Sprintf("%d assets have no ready revision since retouching started (e.g. %s)",
				revisions.Missing, sampleIDs(revisions.Sample)))
	case errors.As(err, &conflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict",
			fmt.Sprintf("proof round was modified (current version %d); reload and retry", conflict.Current))
	case errors.Is(err, ErrRoundNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Proof round not found", "")
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "project not found in this tenant")
	case errors.Is(err, ErrGalleryNotFound):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "gallery not found in this project")
	case errors.Is(err, ErrSelectionChargeRequired):
		httpx.WriteProblem(w, r, http.StatusPaymentRequired, "selection_payment_required", "Selection payments are not complete", "pay extra-photo charges before starting retouching")
	case errors.Is(err, ErrInvalidState):
		httpx.WriteProblem(w, r, http.StatusConflict, "invalid_state", "Proof round state does not allow this change", err.Error())
	case errors.Is(err, ErrDerivativesIncomplete):
		httpx.WriteProblem(w, r, http.StatusConflict, "derivatives_incomplete", "Proof previews are not ready", err.Error())
	case errors.Is(err, ErrRevisionIncomplete):
		httpx.WriteProblem(w, r, http.StatusConflict, "revisions_incomplete", "Retouched versions are incomplete", err.Error())
	case errors.Is(err, ErrChecksumMismatch):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "checksum_mismatch", "Checksum mismatch", err.Error())
	case errors.Is(err, ErrRevisionTooLarge):
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "Revision is too large", err.Error())
	case errors.Is(err, ErrRevisionFormat):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "invalid_revision", "Unsupported revision format", "upload a JPEG, PNG or WebP image")
	case errors.Is(err, ErrCommentNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "comment_not_found", "Comment not found", "")
	case errors.Is(err, ErrCommentForbidden):
		httpx.WriteProblem(w, r, http.StatusForbidden, "comment_forbidden", "Comment cannot be resolved by the customer", "")
	case errors.Is(err, ErrCommentLimit):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	case errors.Is(err, ErrInvalidAnnotation):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "invalid_annotation", "Invalid annotation", err.Error())
	case errors.Is(err, ErrParentComment):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "invalid_parent", "Invalid reply target", err.Error())
	case errors.Is(err, ErrUnknownAsset):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "asset_invalid", "Asset is not in this proof round", "")
	case errors.Is(err, ErrAssetInvalid):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "asset_invalid", "Invalid proof assets", err.Error())
	case errors.Is(err, ErrAssetLimit):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "asset_limit", "Proof asset limit reached", err.Error())
	case errors.Is(err, ErrTokenNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "token_not_found", "Token not found or already revoked", "")
	case errors.Is(err, ErrClientEmailMissing):
		httpx.WriteProblem(w, r, http.StatusConflict, "client_email_missing", "Project client has no email", "add an email address to the client before sending a proof invitation")
	case errors.Is(err, ErrClientPhoneMissing):
		httpx.WriteProblem(w, r, http.StatusConflict, "client_phone_missing", "Project client has no phone", "add a phone number to the client before sending a proof SMS")
	case errors.Is(err, ErrChannelUnavailable):
		httpx.WriteProblem(w, r, http.StatusConflict, "channel_unavailable", "Notification channel is unavailable", "configure the channel in server settings")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
