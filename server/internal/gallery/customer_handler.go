package gallery

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/idempotency"
	"photography-workflow/server/internal/payment"
)

// CustomerHandler serves /g/v1. It never runs under staff authentication: the
// only credentials are the share token and the session derived from it.
type CustomerHandler struct {
	svc            *CustomerService
	tokens         *auth.GalleryTokens
	pool           *pgxpool.Pool
	payments       *payment.Service
	log            *slog.Logger
	limiter        *rateLimiter
	sessionLimiter *rateLimiter
}

func (h *CustomerHandler) SetPayments(svc *payment.Service) { h.payments = svc }

func NewCustomerHandler(svc *CustomerService, tokens *auth.GalleryTokens, pool *pgxpool.Pool, log *slog.Logger) *CustomerHandler {
	return &CustomerHandler{
		svc:            svc,
		tokens:         tokens,
		pool:           pool,
		log:            log,
		limiter:        newRateLimiter(120, time.Minute),
		sessionLimiter: newRateLimiter(30, time.Minute),
	}
}

func (h *CustomerHandler) Register(r chi.Router) {
	r.Route("/g/v1", func(r chi.Router) { h.RegisterRoutes(r) })
}

// RegisterRoutes adds the customer routes to an existing /g/v1 router so proof
// routes can be mounted alongside them without a second mount.
func (h *CustomerHandler) RegisterRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(securityHeaders)
		r.Use(h.limiter.middleware(sessionRateKey))
		r.Group(func(r chi.Router) {
			r.Use(h.sessionLimiter.middleware(clientIP))
			r.Post("/session", h.createSession)
			r.Post("/capture-events", h.recordCapture)
		})
		r.Group(func(r chi.Router) {
			r.Use(h.svc.SessionMiddleware(h.tokens, h.log))
			r.Use(idempotency.Middleware(h.pool, h.log))
			r.Get("/session", h.getSession)
			r.Get("/albums/{albumID}/manifest", h.manifest)
			r.Post("/selections", h.selections)
			r.Post("/albums/{albumID}/submit", h.submit)
		})
	})
}

type captureEventRequest struct {
	Token string `json:"token"`
	Event string `json:"event"`
}

func (h *CustomerHandler) recordCapture(w http.ResponseWriter, r *http.Request) {
	var req captureEventRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if err := h.svc.RecordCapture(r.Context(), req.Token, req.Event, clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
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
	view, err := h.svc.CreateSession(r.Context(), req.Token, clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (h *CustomerHandler) getSession(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.GalleryFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Gallery session required", "")
		return
	}
	album, err := h.svc.Album(r.Context(), gp)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.requestSelectionRefunds(r, gp.TenantID, album)
	httpx.WriteJSON(w, http.StatusOK, album)
}

func (h *CustomerHandler) requestSelectionRefunds(r *http.Request, tenantID uuid.UUID, album *AlbumView) {
	if h.payments == nil || album == nil {
		return
	}
	for _, task := range album.RefundTasks {
		if _, err := h.payments.CreateRefund(r.Context(), tenantID, uuid.Nil, clientIP(r), payment.CreateRefundInput{
			PaymentID: task.PaymentID, AmountCents: task.AmountCents, Reason: "套餐选片数量调整退款",
		}); err != nil {
			h.log.Warn("selection adjustment refund will be retried on the next album refresh", "gallery_id", album.ID, "payment_id", task.PaymentID, "err", err)
		}
	}
}

func (h *CustomerHandler) manifest(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.GalleryFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Gallery session required", "")
		return
	}
	if !h.matchAlbum(w, r, gp) {
		return
	}
	cursor := r.URL.Query().Get("cursor")
	limit := httpx.IntQuery(r, "limit", 40, 1, 60)
	page, err := h.svc.Page(r.Context(), gp, cursor, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

type selectionRequest struct {
	ExpectedVersion *int            `json:"expected_version"`
	Items           []SelectionItem `json:"items"`
}

func (h *CustomerHandler) selections(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.GalleryFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Gallery session required", "")
		return
	}
	var req selectionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.ExpectedVersion == nil || *req.ExpectedVersion < 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "expected_version is required")
		return
	}
	res, err := h.svc.ApplySelections(r.Context(), gp, clientIP(r), SelectionInput{
		ExpectedVersion: *req.ExpectedVersion,
		Items:           req.Items,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

type submitRequest struct {
	ExpectedVersion *int `json:"expected_version"`
}

func (h *CustomerHandler) submit(w http.ResponseWriter, r *http.Request) {
	gp, ok := auth.GalleryFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Gallery session required", "")
		return
	}
	if !h.matchAlbum(w, r, gp) {
		return
	}
	var req submitRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.ExpectedVersion == nil || *req.ExpectedVersion < 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "expected_version is required")
		return
	}
	album, err := h.svc.Submit(r.Context(), gp, clientIP(r), *req.ExpectedVersion)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.requestSelectionRefunds(r, gp.TenantID, album)
	if album.ChargeBatchID != nil && h.payments != nil {
		order, _, err := h.payments.CreateOrder(r.Context(), gp.TenantID, uuid.Nil, clientIP(r), payment.CreateOrderInput{
			ProjectID: album.ProjectID, Kind: payment.KindSelectionExtra, SelectionChargeBatchID: album.ChargeBatchID,
		})
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusConflict, "supplemental_payment_failed", "补选费用单创建失败", err.Error())
			return
		}
		album.PaymentURL, album.PaymentOrderID = order.PayURL, &order.ID
	}
	httpx.WriteJSON(w, http.StatusOK, album)
}

func (h *CustomerHandler) matchAlbum(w http.ResponseWriter, r *http.Request, gp *auth.GalleryPrincipal) bool {
	id, err := uuid.Parse(chi.URLParam(r, "albumID"))
	if err != nil || id != gp.GalleryID {
		// A session is bound to exactly one album; anything else is not found,
		// which also avoids confirming other galleries exist.
		httpx.WriteProblem(w, r, http.StatusNotFound, "album_not_found", "Album not found", "")
		return false
	}
	return true
}

func (h *CustomerHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *VersionConflictError
	switch {
	case errors.As(err, &conflict):
		writeVersionConflict(w, r, conflict.Current)
	case errors.Is(err, ErrInvalidShareToken):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_token", "Invalid share link", "")
	case errors.Is(err, ErrShareExpired):
		httpx.WriteProblem(w, r, http.StatusGone, "expired", "Share link has expired", "")
	case errors.Is(err, ErrShareRevoked):
		httpx.WriteProblem(w, r, http.StatusGone, "revoked", "Share link was revoked", "")
	case errors.Is(err, ErrTooManyVisits):
		httpx.WriteProblem(w, r, http.StatusTooManyRequests, "too_many_visits", "Share link visit limit reached", "")
	case errors.Is(err, ErrBalanceUnpaid):
		httpx.WriteProblem(w, r, http.StatusPaymentRequired, "balance_unpaid", "尾款未支付", "完成尾款支付后即可选片")
	case errors.Is(err, ErrGalleryExpired):
		httpx.WriteProblem(w, r, http.StatusGone, "gallery_expired", "Gallery has expired", "")
	case errors.Is(err, ErrGalleryRevoked):
		httpx.WriteProblem(w, r, http.StatusGone, "gallery_revoked", "Gallery was revoked", "")
	case errors.Is(err, ErrAlreadySubmitted):
		httpx.WriteProblem(w, r, http.StatusConflict, "already_submitted", "Selection was already submitted", "ask the studio to reopen the gallery")
	case errors.Is(err, ErrSelectionLimit):
		httpx.WriteProblem(w, r, http.StatusConflict, "selection_limit_exceeded", "Selection limit exceeded", err.Error())
	case errors.Is(err, ErrUnknownAsset):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "unknown_asset", "Unknown album asset", "one or more asset ids are not in this album")
	case errors.Is(err, ErrSelectionBatch):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "album_not_found", "Album not found", "")
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

// writeVersionConflict includes current_version so a client can retry against
// the newest state without an extra round trip.
func writeVersionConflict(w http.ResponseWriter, r *http.Request, current int) {
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
			Detail:    "selection_version changed; reload the album and retry",
			Code:      "version_conflict",
			RequestID: httpx.RequestIDFrom(r.Context()),
		},
		CurrentVersion: current,
	})
}
