package delivery

import (
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

// CustomerHandler serves /g/v1/delivery. It shares the customer mount with the
// gallery and proof handlers but never uses staff authentication.
type CustomerHandler struct {
	cust           *CustomerService
	guard          *Guard
	log            *slog.Logger
	limiter        *rateLimiter
	sessionLimiter *rateLimiter
}

func NewCustomerHandler(cust *CustomerService, tokens *auth.DeliveryTokens, log *slog.Logger) *CustomerHandler {
	return &CustomerHandler{
		cust:           cust,
		guard:          NewGuard(cust.pool, tokens, log),
		log:            log,
		limiter:        newRateLimiter(120, time.Minute),
		sessionLimiter: newRateLimiter(30, time.Minute),
	}
}

func (h *CustomerHandler) RegisterRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(securityHeaders)
		r.Use(h.limiter.middleware(sessionRateKey))
		r.Group(func(r chi.Router) {
			r.Use(h.sessionLimiter.middleware(clientIP))
			r.Post("/delivery/session", h.createSession)
		})
		r.Group(func(r chi.Router) {
			r.Use(h.guard.Middleware())
			r.Use(idempotency.Middleware(h.cust.pool, h.log))
			r.Get("/delivery/session", h.getSession)
			r.Get("/delivery/items", h.items)
			r.Post("/delivery/downloads", h.download)
		})
	})
}

type deliverySessionRequest struct {
	Token string `json:"token"`
}

func (h *CustomerHandler) createSession(w http.ResponseWriter, r *http.Request) {
	var req deliverySessionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	view, err := h.cust.CreateSession(r.Context(), req.Token, clientIP(r))
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (h *CustomerHandler) getSession(w http.ResponseWriter, r *http.Request) {
	dp, ok := auth.DeliveryFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Delivery session required", "")
		return
	}
	view, err := h.cust.Summary(r.Context(), dp)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (h *CustomerHandler) items(w http.ResponseWriter, r *http.Request) {
	dp, ok := auth.DeliveryFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Delivery session required", "")
		return
	}
	limit := httpx.IntQuery(r, "limit", 40, 1, 60)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	page, err := h.cust.Items(r.Context(), dp, limit, offset)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

type downloadRequest struct {
	PackageID *uuid.UUID `json:"package_id"`
	ItemID    *uuid.UUID `json:"item_id"`
	Spec      string     `json:"spec"`
}

func (h *CustomerHandler) download(w http.ResponseWriter, r *http.Request) {
	dp, ok := auth.DeliveryFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Delivery session required", "")
		return
	}
	var req downloadRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	grant, err := h.cust.Grant(r.Context(), dp, clientIP(r), DownloadInput{
		PackageID: req.PackageID,
		ItemID:    req.ItemID,
		Spec:      strings.TrimSpace(req.Spec),
	})
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, grant)
}
