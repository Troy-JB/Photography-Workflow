package payment

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

const maxCallbackBody = 1 << 20

type Handler struct {
	svc      *Service
	provider Provider
	log      *slog.Logger
}

func NewHandler(svc *Service, provider Provider, log *slog.Logger) *Handler {
	return &Handler{svc: svc, provider: provider, log: log}
}

func (h *Handler) Register(r chi.Router) {
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/payments/orders", h.createOrder)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/payments/orders", h.listOrders)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/payments/orders/{orderID}", h.getOrder)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/payments/orders/{orderID}/close", h.closeOrder)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/payments/orders/{orderID}/reconcile", h.reconcileOrder)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/payments", h.listPayments)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/payments/{paymentID}", h.getPayment)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/payments/{paymentID}/refunds", h.createRefund)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/payments/{paymentID}/refunds", h.listRefunds)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/refunds", h.listRefunds)
}

// RegisterWebhook mounts the provider callback outside the authenticated API
// group: the payment provider cannot present a tenant token. The signature
// verified by the adapter is the authentication.
func (h *Handler) RegisterWebhook(r chi.Router) {
	r.Post("/webhooks/payment/{provider}", h.callback)
}

type createOrderRequest struct {
	ProjectID              uuid.UUID  `json:"project_id"`
	Kind                   string     `json:"kind"`
	SelectionChargeBatchID *uuid.UUID `json:"selection_charge_batch_id"`
}

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req createOrderRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.Kind == KindChange {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed",
			"change orders are collected through /api/v1/change-orders")
		return
	}
	order, created, err := h.svc.CreateOrder(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateOrderInput{
		ProjectID: req.ProjectID, Kind: req.Kind, SelectionChargeBatchID: req.SelectionChargeBatchID,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, order)
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var projectID *uuid.UUID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
			return
		}
		projectID = &id
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListOrders(r.Context(), p.TenantID, projectID,
		r.URL.Query().Get("status"), r.URL.Query().Get("kind"), limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid payment order id", "")
		return
	}
	order, err := h.svc.GetOrder(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) closeOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid payment order id", "")
		return
	}
	order, err := h.svc.CloseOrder(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) reconcileOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid payment order id", "")
		return
	}
	order, err := h.svc.ReconcileOrder(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) listPayments(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var projectID *uuid.UUID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
			return
		}
		projectID = &id
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListPayments(r.Context(), p.TenantID, projectID, limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) getPayment(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "paymentID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid payment id", "")
		return
	}
	pay, err := h.svc.GetPayment(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pay)
}

type createRefundRequest struct {
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason"`
}

func (h *Handler) createRefund(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "paymentID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid payment id", "")
		return
	}
	var req createRefundRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	refund, err := h.svc.CreateRefund(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateRefundInput{
		PaymentID: id, AmountCents: req.AmountCents, Reason: req.Reason,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, refund)
}

func (h *Handler) listRefunds(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var paymentID *uuid.UUID
	if raw := chi.URLParam(r, "paymentID"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid payment id", "")
			return
		}
		paymentID = &id
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListRefunds(r.Context(), p.TenantID, paymentID, limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "provider")
	if providerName != h.provider.Name() {
		httpx.WriteProblem(w, r, http.StatusNotFound, "unknown_provider", "Unknown payment provider", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBody))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large", "")
		return
	}
	ev, err := h.provider.VerifyCallback(r, body)
	switch {
	case errors.Is(err, ErrMerchantMismatch):
		h.svc.HandleMerchantMismatch(r.Context(), providerName, body)
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "merchant_mismatch",
			"Callback was issued for another merchant", "")
		return
	case errors.Is(err, ErrInvalidSignature):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_signature", "Invalid callback signature", "")
		return
	case errors.Is(err, ErrMalformedCallback):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "malformed_callback", "Malformed callback", err.Error())
		return
	case err != nil:
		httpx.Internal(w, r, h.log, err)
		return
	}
	result, err := h.svc.HandleCallback(r.Context(), providerName, ev)
	if err != nil {
		h.log.Error("payment callback failed", "provider", providerName, "event_id", ev.EventID, "err", err)
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "callback_failed", "Callback processing failed", "")
		return
	}
	if result.Status == "rejected" {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, result.Reason,
			"Payment callback rejected", "callback amount or currency does not match the order")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Payment not found", "")
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found", "")
	case errors.Is(err, ErrProjectClosed):
		httpx.WriteProblem(w, r, http.StatusConflict, "project_cancelled", "Project cancelled", err.Error())
	case errors.Is(err, ErrDepositUnpaid):
		httpx.WriteProblem(w, r, http.StatusConflict, "deposit_unpaid", "Deposit not paid", err.Error())
	case errors.Is(err, ErrAlreadyPaid):
		httpx.WriteProblem(w, r, http.StatusConflict, "already_paid", "Already paid", err.Error())
	case errors.Is(err, ErrOrderNotPending):
		httpx.WriteProblem(w, r, http.StatusConflict, "order_not_pending", "Payment order not pending", err.Error())
	case errors.Is(err, ErrOrderInProgress):
		httpx.WriteProblem(w, r, http.StatusConflict, "order_in_progress", "Payment order is being created", err.Error())
	case errors.Is(err, ErrOrderPaidAtProvider):
		httpx.WriteProblem(w, r, http.StatusConflict, "order_already_paid", "Order already paid at the payment channel", err.Error())
	case errors.Is(err, ErrReconcileMismatch):
		httpx.WriteProblem(w, r, http.StatusConflict, "reconcile_mismatch", "Provider state does not match the local order", err.Error())
	case errors.Is(err, ErrRefundExceedsRemaining):
		httpx.WriteProblem(w, r, http.StatusConflict, "refund_exceeds_remaining", "Refund exceeds remaining amount", err.Error())
	case errors.Is(err, ErrProvider):
		httpx.WriteProblem(w, r, http.StatusBadGateway, "payment_provider_error", "Payment provider error", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
