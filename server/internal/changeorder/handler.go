package changeorder

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Register(r chi.Router) {
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/change-orders", h.create)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/change-orders", h.list)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/change-orders/{changeOrderID}", h.get)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/change-orders/{changeOrderID}/send", h.send)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/change-orders/{changeOrderID}/payment-order", h.paymentOrder)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/change-orders/{changeOrderID}/cancel", h.cancel)
}

type createRequest struct {
	ProjectID     uuid.UUID     `json:"project_id"`
	Reason        string        `json:"reason"`
	SignerName    string        `json:"signer_name"`
	SignerContact string        `json:"signer_contact"`
	Items         []itemRequest `json:"items"`
}

type itemRequest struct {
	Kind           string `json:"kind"`
	Description    string `json:"description"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	items := make([]ItemInput, len(req.Items))
	for i, it := range req.Items {
		items[i] = ItemInput{
			Kind: it.Kind, Description: it.Description,
			Quantity: it.Quantity, UnitPriceCents: it.UnitPriceCents,
		}
	}
	co, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateInput{
		ProjectID: req.ProjectID, Reason: req.Reason, SignerName: req.SignerName,
		SignerContact: req.SignerContact, Items: items,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, co)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var projectID *uuid.UUID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "invalid project_id")
			return
		}
		projectID = &id
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.List(r.Context(), p.TenantID, projectID,
		r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := changeOrderID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid change order id", "")
		return
	}
	co, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, co)
}

type versionRequest struct {
	ExpectedVersion int `json:"expected_version"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := changeOrderID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid change order id", "")
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	co, err := h.svc.Send(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, co)
}

// paymentOrder is the retry path after a signing callback could not open the
// supplement order (for example a transient payment provider outage).
func (h *Handler) paymentOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := changeOrderID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid change order id", "")
		return
	}
	co, err := h.svc.EnsurePaymentOrder(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, co)
}

type cancelRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := changeOrderID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid change order id", "")
		return
	}
	var req cancelRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	co, err := h.svc.Cancel(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, co)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "change_order_not_found", "Change order not found", "")
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "change order was modified, reload and retry")
	case errors.Is(err, ErrIllegalState):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_state", "Illegal change order state", err.Error())
	case errors.Is(err, ErrActiveChangeOrder):
		httpx.WriteProblem(w, r, http.StatusConflict, "active_change_order", "Project already has an active change order", err.Error())
	case errors.Is(err, ErrPaid):
		httpx.WriteProblem(w, r, http.StatusConflict, "change_order_paid", "Change order is already paid", err.Error())
	case errors.Is(err, ErrOrderInProgress):
		httpx.WriteProblem(w, r, http.StatusConflict, "order_in_progress", "Change order payment order is being created", err.Error())
	case errors.Is(err, ErrPayment):
		httpx.WriteProblem(w, r, http.StatusBadGateway, "payment_provider_error", "Payment provider error", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func changeOrderID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "changeOrderID"))
}
