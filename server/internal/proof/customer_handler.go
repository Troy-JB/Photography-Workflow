package proof

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/idempotency"
)

// CustomerHandler serves /g/v1/proof. It shares the customer mount with the
// gallery handler but never uses staff authentication.
type CustomerHandler struct {
	cust           *CustomerService
	guard          *Guard
	log            *slog.Logger
	limiter        *rateLimiter
	sessionLimiter *rateLimiter
}

func NewCustomerHandler(cust *CustomerService, tokens *auth.ProofTokens, log *slog.Logger) *CustomerHandler {
	return &CustomerHandler{
		cust:           cust,
		guard:          NewGuard(cust.pool, tokens, log),
		log:            log,
		limiter:        newRateLimiter(120, time.Minute),
		sessionLimiter: newRateLimiter(30, time.Minute),
	}
}

// RegisterRoutes adds the proof routes to the shared /g/v1 router. Register
// expects a router for that prefix (main mounts it once for gallery + proof).
func (h *CustomerHandler) RegisterRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(securityHeaders)
		r.Use(h.limiter.middleware(sessionRateKey))
		r.Group(func(r chi.Router) {
			r.Use(h.sessionLimiter.middleware(clientIP))
			r.Post("/proof/session", h.createSession)
		})
		r.Group(func(r chi.Router) {
			r.Use(h.guard.Middleware())
			r.Use(idempotency.Middleware(h.cust.pool, h.log))
			r.Get("/proof/session", h.getSession)
			r.Get("/proof/rounds/{roundID}/manifest", h.manifest)
			r.Get("/proof/rounds/{roundID}/comments", h.listComments)
			r.Post("/proof/rounds/{roundID}/comments", h.comment)
			r.Post("/proof/rounds/{roundID}/comments/{commentID}/resolve", h.resolveComment)
			r.Post("/proof/rounds/{roundID}/submit", h.submit)
		})
	})
}

type createSessionRequest struct {
	Token string `json:"token"`
}

func (h *CustomerHandler) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	view, err := h.cust.CreateSession(r.Context(), req.Token, clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (h *CustomerHandler) getSession(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.ProofFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof session required", "")
		return
	}
	round, err := h.cust.Round(r.Context(), gp)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"round": round})
}

func (h *CustomerHandler) manifest(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.ProofFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof session required", "")
		return
	}
	limit := httpx.IntQuery(r, "limit", 40, 1, 60)
	page, err := h.cust.Page(r.Context(), gp, clientIP(r), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

type commentRequest struct {
	AssetID    uuid.UUID       `json:"asset_id"`
	ParentID   *uuid.UUID      `json:"parent_id"`
	Body       string          `json:"body"`
	Annotation json.RawMessage `json:"annotation"`
}

// listComments returns the round thread. Customers see both sides of the
// conversation (studio replies included) but only inside their own round; the
// guard has already bound the session to {roundID}.
func (h *CustomerHandler) listComments(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.ProofFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof session required", "")
		return
	}
	assetID, ok := optionalUUIDQuery(w, r, "asset_id")
	if !ok {
		return
	}
	includeResolved := r.URL.Query().Get("include_resolved") == "true"
	items, err := h.cust.svc.ListComments(r.Context(), gp.TenantID, gp.RoundID, CommentFilter{
		AssetID: assetID, IncludeResolved: includeResolved,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *CustomerHandler) comment(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.ProofFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof session required", "")
		return
	}
	var req commentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.AssetID == uuid.Nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "asset_id is required")
		return
	}
	tokenID := gp.TokenID
	comment, err := h.cust.svc.CreateComment(r.Context(), gp.TenantID, gp.RoundID, CommentAuthor{
		Type: AuthorClient, TokenID: &tokenID, IP: clientIP(r),
	}, CommentInput{
		AssetID:    req.AssetID,
		ParentID:   req.ParentID,
		Body:       req.Body,
		Annotation: req.Annotation,
		ClientRef:  strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, comment)
}

func (h *CustomerHandler) resolveComment(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.ProofFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof session required", "")
		return
	}
	commentID, err := uuid.Parse(chi.URLParam(r, "commentID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid comment id", "")
		return
	}
	tokenID := gp.TokenID
	comment, err := h.cust.svc.ResolveComment(r.Context(), gp.TenantID, gp.RoundID, commentID, CommentAuthor{
		Type: AuthorClient, TokenID: &tokenID, IP: clientIP(r),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, comment)
}

func (h *CustomerHandler) submit(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.ProofFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof session required", "")
		return
	}
	var req SubmitInput
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	round, err := h.cust.Submit(r.Context(), gp, clientIP(r), req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, round)
}

func (h *CustomerHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *VersionConflictError
	switch {
	case errors.As(err, &conflict):
		writeProofVersionConflict(w, r, conflict.Current)
	case errors.Is(err, ErrTokenInvalid):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_token", "Invalid proof link", "")
	case errors.Is(err, ErrTokenExpired):
		httpx.WriteProblem(w, r, http.StatusGone, "expired", "Proof link has expired", "")
	case errors.Is(err, ErrTokenRevoked):
		httpx.WriteProblem(w, r, http.StatusGone, "revoked", "Proof link was revoked", "")
	case errors.Is(err, ErrTokenExhausted):
		httpx.WriteProblem(w, r, http.StatusTooManyRequests, "too_many_visits", "Proof link visit limit reached", "")
	case errors.Is(err, ErrGalleryRevoked):
		httpx.WriteProblem(w, r, http.StatusGone, "gallery_revoked", "Gallery was revoked", "")
	case errors.Is(err, ErrRoundClosed):
		httpx.WriteProblem(w, r, http.StatusGone, "round_closed", "Proof round is closed", "")
	case errors.Is(err, ErrRoundNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "round_not_found", "Proof round not found", "")
	case errors.Is(err, ErrSelectionChargeRequired):
		httpx.WriteProblem(w, r, http.StatusPaymentRequired, "selection_payment_required", "Selection payments are not complete", "ask the studio to resend the extra-photo payment link")
	case errors.Is(err, ErrInvalidState):
		httpx.WriteProblem(w, r, http.StatusConflict, "invalid_state", "Proof round state does not allow this change", err.Error())
	case errors.Is(err, ErrInvalidAnnotation):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "invalid_annotation", "Invalid annotation", err.Error())
	case errors.Is(err, ErrCommentLimit):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	case errors.Is(err, ErrParentComment):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "invalid_parent", "Invalid reply target", err.Error())
	case errors.Is(err, ErrCommentNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "comment_not_found", "Comment not found", "")
	case errors.Is(err, ErrCommentForbidden):
		httpx.WriteProblem(w, r, http.StatusForbidden, "comment_forbidden", "Comment cannot be resolved by the customer", "")
	case errors.Is(err, ErrUnknownAsset):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "unknown_asset", "Unknown proof asset", "asset is not part of this round")
	case errors.Is(err, ErrDerivativesIncomplete):
		httpx.WriteProblem(w, r, http.StatusConflict, "previews_unavailable", "Previews are not ready", "")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func writeProofVersionConflict(w http.ResponseWriter, r *http.Request, current int) {
	type conflict struct {
		httpx.Problem
		CurrentVersion int `json:"current_version"`
	}
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(conflict{
		Problem: httpx.Problem{
			Type:      "https://pwf.local/errors/version_conflict",
			Title:     "Version conflict",
			Status:    http.StatusConflict,
			Detail:    fmt.Sprintf("proof round is at version %d; reload and retry", current),
			Code:      "version_conflict",
			RequestID: httpx.RequestIDFrom(r.Context()),
		},
		CurrentVersion: current,
	})
}
