package delivery

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

// Handler serves the staff-facing delivery API. Every route requires a staff
// session; deletion-style actions (revoke-all, notify) stay with owner/staff so
// assistants cannot silently retarget a customer link.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Register(r chi.Router) {
	read := auth.RequireAnyRole("owner", "staff", "assistant", "retoucher")
	staff := auth.RequireAnyRole("owner", "staff", "assistant")
	r.With(read).Get("/deliveries", h.list)
	r.With(staff).Post("/deliveries", h.create)
	r.Route("/deliveries/{deliveryID}", func(r chi.Router) {
		r.With(read).Get("/", h.get)
		r.With(staff).Post("/build", h.build)
		r.With(read).Get("/items", h.listItems)
		r.With(read).Get("/tokens", h.listTokens)
		r.With(staff).Post("/tokens", h.issueToken)
		r.With(staff).Post("/tokens/{tokenID}/revoke", h.revokeToken)
		r.With(staff).Post("/tokens/revoke-all", h.revokeAll)
		r.With(staff).Post("/notify", h.notify)
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
	status := r.URL.Query().Get("status")
	if status != "" && !validStatus(status) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown status")
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.List(r.Context(), p.TenantID, projectID, status, limit, offset)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

type createRequest struct {
	ProofRoundID uuid.UUID `json:"proof_round_id"`
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
	if req.ProofRoundID == uuid.Nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "proof_round_id is required")
		return
	}
	summary, created, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, req.ProofRoundID)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, summary)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	detail, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func (h *Handler) build(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	summary, err := h.svc.Build(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, summary)
}

func (h *Handler) listItems(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	webStatus := r.URL.Query().Get("web_status")
	if webStatus != "" && webStatus != WebPending && webStatus != WebReady && webStatus != WebSkipped {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown web_status")
		return
	}
	limit := httpx.IntQuery(r, "limit", 100, 1, 500)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListItems(r.Context(), p.TenantID, id, webStatus, limit, offset)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	tokens, err := h.svc.ListTokens(r.Context(), p.TenantID, id)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": tokens})
}

type issueTokenRequest struct {
	Label         string `json:"label"`
	Recipient     string `json:"recipient"`
	ExpiresInDays int    `json:"expires_in_days"`
	MaxDownloads  int    `json:"max_downloads"`
}

func (h *Handler) issueToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	var req issueTokenRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	token, err := h.svc.IssueToken(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr, IssueTokenInput{
		Label:         req.Label,
		Recipient:     req.Recipient,
		ExpiresInDays: req.ExpiresInDays,
		MaxDownloads:  req.MaxDownloads,
	})
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, token)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid token id", "")
		return
	}
	if _, err := h.svc.RevokeToken(r.Context(), p.TenantID, id, tokenID, p.UserID, r.RemoteAddr); err != nil {
		fail(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) revokeAll(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	revoked, err := h.svc.RevokeAllTokens(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"revoked": revoked})
}

type notifyRequest struct {
	Recipient string `json:"recipient"`
}

func (h *Handler) notify(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.deliveryID(w, r)
	if !ok {
		return
	}
	var req notifyRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.NotifyCustomer(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr, req.Recipient)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, res)
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return nil, false
	}
	return p, true
}

func (h *Handler) deliveryID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "deliveryID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid delivery id", "")
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

func validStatus(status string) bool {
	switch status {
	case StatusPending, StatusBuilding, StatusReady, StatusFailed, StatusBlocked:
		return true
	}
	return false
}

// fail maps delivery errors to RFC7807 problems shared by the staff and
// customer handlers.
func fail(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrRoundNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Delivery not found", "")
	case errors.Is(err, ErrItemNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "item_not_found", "Delivery item not found", "")
	case errors.Is(err, ErrPackageNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "package_not_found", "Delivery package not found", "")
	case errors.Is(err, ErrTokenNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "token_not_found", "Download token not found", "")
	case errors.Is(err, ErrRoundNotApproved):
		httpx.WriteProblem(w, r, http.StatusConflict, "round_not_approved", "Proof round is not approved",
			"approve the proof round before creating or building a delivery")
	case errors.Is(err, ErrPaymentRequired):
		httpx.WriteProblem(w, r, http.StatusConflict, "payment_required", "Project balance is not paid",
			"settle the final payment before building or downloading; refunds revoke access immediately")
	case errors.Is(err, ErrProjectArchived):
		httpx.WriteProblem(w, r, http.StatusGone, "delivery_archived", "Project is archived", "")
	case errors.Is(err, ErrProjectCancelled):
		httpx.WriteProblem(w, r, http.StatusGone, "delivery_cancelled", "Project is cancelled", "")
	case errors.Is(err, ErrDeliveryNotReady):
		httpx.WriteProblem(w, r, http.StatusConflict, "delivery_not_ready", "Delivery is not ready",
			"build the delivery and wait for both packages to finish")
	case errors.Is(err, ErrBuildInProgress):
		httpx.WriteProblem(w, r, http.StatusConflict, "build_in_progress", "A build is already in progress", "")
	case errors.Is(err, ErrDownloadsExhausted):
		httpx.WriteProblem(w, r, http.StatusConflict, "downloads_exhausted", "Download limit reached",
			"ask the studio for a new link")
	case errors.Is(err, ErrTokenExpired):
		httpx.WriteProblem(w, r, http.StatusGone, "link_expired", "Download link expired", "")
	case errors.Is(err, ErrTokenRevoked):
		httpx.WriteProblem(w, r, http.StatusGone, "link_revoked", "Download link was revoked", "")
	case errors.Is(err, ErrTokenInvalid):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_link", "Download link is not valid", "")
	case errors.Is(err, ErrDeliveryMismatch):
		httpx.WriteProblem(w, r, http.StatusForbidden, "delivery_mismatch", "Session belongs to another delivery", "")
	case errors.Is(err, ErrWebSpecUnavailable):
		httpx.WriteProblem(w, r, http.StatusConflict, "web_spec_unavailable", "Web version is not available",
			"this asset was delivered as original only; use the original package")
	case errors.Is(err, ErrClientEmailMissing):
		httpx.WriteProblem(w, r, http.StatusConflict, "client_email_missing", "Project client has no email",
			"add an email address to the client before notifying")
	case errors.Is(err, ErrChannelUnavailable):
		httpx.WriteProblem(w, r, http.StatusConflict, "channel_unavailable", "Notification channel is unavailable",
			"configure the email channel in server settings")
	case errors.Is(err, ErrAssetLimit):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "asset_limit", "Delivery asset limit reached", err.Error())
	case errors.Is(err, ErrOwnerRequired):
		httpx.WriteProblem(w, r, http.StatusForbidden, "owner_required", "Only the owner can perform this action", "")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, log, err)
	}
}
