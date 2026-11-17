package signing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/esign"
	"photography-workflow/server/internal/httpx"
)

const maxCallbackBody = 1 << 20

// TaskHandler applies a verified provider event to the aggregate that owns the
// signing task. Implementations stay transactional and idempotent.
type TaskHandler interface {
	HandleTaskEvent(ctx context.Context, tenantID, subjectID uuid.UUID, providerName string, ev *esign.CallbackEvent) (*esign.TaskEventResult, error)
}

// Handler authenticates provider callbacks once and routes them to the owning
// aggregate (contract or change order) by the esign_tasks mapping.
type Handler struct {
	pool         *pgxpool.Pool
	provider     esign.Provider
	contracts    TaskHandler
	changeOrders TaskHandler
	log          *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, provider esign.Provider, contracts, changeOrders TaskHandler, log *slog.Logger) *Handler {
	return &Handler{pool: pool, provider: provider, contracts: contracts, changeOrders: changeOrders, log: log}
}

func (h *Handler) Register(r chi.Router) {
	r.Post("/webhooks/esign/{provider}", h.callback)
}

type callbackResponse struct {
	Status        string     `json:"status"`
	SubjectType   string     `json:"subject_type,omitempty"`
	SubjectID     *uuid.UUID `json:"subject_id,omitempty"`
	ContractID    *uuid.UUID `json:"contract_id,omitempty"`
	ChangeOrderID *uuid.UUID `json:"change_order_id,omitempty"`
}

type taskRoute struct {
	TenantID      uuid.UUID
	ContractID    *uuid.UUID
	ChangeOrderID *uuid.UUID
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "provider")
	if providerName != h.provider.Name() {
		httpx.WriteProblem(w, r, http.StatusNotFound, "unknown_provider", "Unknown e-sign provider", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBody))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large", "")
		return
	}
	ev, err := h.provider.VerifyCallback(r, body)
	switch {
	case errors.Is(err, esign.ErrInvalidSignature):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_signature", "Invalid callback signature", "")
		return
	case errors.Is(err, esign.ErrMalformedCallback):
		httpx.WriteProblem(w, r, http.StatusBadRequest, "malformed_callback", "Malformed callback", err.Error())
		return
	case err != nil:
		h.log.Error("verify esign callback", "provider", providerName, "err", err)
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Internal error", "")
		return
	}

	route, err := h.lookup(r.Context(), providerName, ev.TaskID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if route == nil {
		httpx.WriteJSON(w, http.StatusOK, callbackResponse{Status: "ignored"})
		return
	}

	var (
		handler TaskHandler
		subject uuid.UUID
	)
	switch {
	case route.ContractID != nil:
		handler, subject = h.contracts, *route.ContractID
	case route.ChangeOrderID != nil:
		handler, subject = h.changeOrders, *route.ChangeOrderID
	}
	res, err := handler.HandleTaskEvent(r.Context(), route.TenantID, subject, providerName, ev)
	if err != nil {
		// Unknown failures return 500 so the provider retries; the event insert
		// is idempotent, so a retry can never double-apply a signature.
		httpx.Internal(w, r, h.log, err)
		return
	}
	out := callbackResponse{
		Status:        res.Status,
		SubjectType:   res.SubjectType,
		SubjectID:     &res.SubjectID,
		ContractID:    route.ContractID,
		ChangeOrderID: route.ChangeOrderID,
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) lookup(ctx context.Context, providerName, taskID string) (*taskRoute, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var route taskRoute
	err := h.pool.QueryRow(ctx, `
		SELECT tenant_id, contract_id, change_order_id FROM esign_tasks
		WHERE provider = $1 AND sign_task_id = $2`, providerName, taskID).
		Scan(&route.TenantID, &route.ContractID, &route.ChangeOrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}
