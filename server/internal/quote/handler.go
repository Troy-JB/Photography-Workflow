package quote

import (
	"context"
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
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/quotes", h.create)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/quotes", h.list)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/quotes/{quoteID}", h.get)
	r.With(auth.RequireAnyRole("owner", "staff")).Put("/quotes/{quoteID}", h.update)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/quotes/{quoteID}/send", h.send)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/quotes/{quoteID}/accept", h.accept)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/quotes/{quoteID}/reject", h.reject)
}

type itemRequest struct {
	Kind           string `json:"kind"`
	Description    string `json:"description"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

type createRequest struct {
	ProjectID     uuid.UUID     `json:"project_id"`
	Currency      string        `json:"currency"`
	DiscountCents int64         `json:"discount_cents"`
	TaxCents      int64         `json:"tax_cents"`
	DepositCents  int64         `json:"deposit_cents"`
	ValidUntil    string        `json:"valid_until"`
	Terms         string        `json:"terms"`
	Items         []itemRequest `json:"items"`
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
	if req.ProjectID == uuid.Nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "project_id is required")
		return
	}
	validUntil, err := httpx.ParseOptionalDate(req.ValidUntil)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
		return
	}
	q, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, Input{
		ProjectID: req.ProjectID, Currency: req.Currency, DiscountCents: req.DiscountCents,
		TaxCents: req.TaxCents, DepositCents: req.DepositCents, ValidUntil: validUntil,
		Terms: req.Terms, Items: itemInputs(req.Items),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, q)
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
	items, total, err := h.svc.List(r.Context(), p.TenantID, projectID, r.URL.Query().Get("status"), limit, offset)
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
	id, err := parseID(w, r)
	if err != nil {
		return
	}
	q, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, q)
}

type updateRequest struct {
	ExpectedVersion int           `json:"expected_version"`
	Currency        string        `json:"currency"`
	DiscountCents   int64         `json:"discount_cents"`
	TaxCents        int64         `json:"tax_cents"`
	DepositCents    int64         `json:"deposit_cents"`
	ValidUntil      string        `json:"valid_until"`
	Terms           string        `json:"terms"`
	Items           []itemRequest `json:"items"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r)
	if err != nil {
		return
	}
	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	validUntil, err := httpx.ParseOptionalDate(req.ValidUntil)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
		return
	}
	q, err := h.svc.Update(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion, UpdateInput{
		Currency: req.Currency, DiscountCents: req.DiscountCents, TaxCents: req.TaxCents,
		DepositCents: req.DepositCents, ValidUntil: validUntil, Terms: req.Terms,
		Items: itemInputs(req.Items),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, q)
}

type versionRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(ctx context.Context, tenantID, actor uuid.UUID, ip string, id uuid.UUID, req versionRequest) (*Quote, error) {
		return h.svc.Send(ctx, tenantID, actor, id, ip, req.ExpectedVersion)
	})
}

func (h *Handler) accept(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(ctx context.Context, tenantID, actor uuid.UUID, ip string, id uuid.UUID, req versionRequest) (*Quote, error) {
		return h.svc.Accept(ctx, tenantID, actor, id, ip, req.ExpectedVersion)
	})
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(ctx context.Context, tenantID, actor uuid.UUID, ip string, id uuid.UUID, req versionRequest) (*Quote, error) {
		return h.svc.Reject(ctx, tenantID, actor, id, ip, req.ExpectedVersion, req.Reason)
	})
}

type mutateFunc func(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID, versionRequest) (*Quote, error)

func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, fn mutateFunc) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r)
	if err != nil {
		return
	}
	req := versionRequest{ExpectedVersion: 0}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
			return
		}
	}
	q, err := fn(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, id, req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, q)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Quote not found", "")
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "quote was modified, reload and retry")
	case errors.Is(err, ErrDraftExists):
		httpx.WriteProblem(w, r, http.StatusConflict, "draft_exists", "Draft quote already exists", "update the draft or send it first")
	case errors.Is(err, ErrActiveContract):
		httpx.WriteProblem(w, r, http.StatusConflict, "active_contract", "Project already has an active contract", "void the contract before issuing a new quote")
	case errors.Is(err, ErrExpired):
		httpx.WriteProblem(w, r, http.StatusConflict, "quote_expired", "Quote expired", "create a new quote version")
	case errors.Is(err, ErrIllegalState):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_state", "Illegal quote state", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "quoteID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid quote id", "")
		return uuid.Nil, err
	}
	return id, nil
}

func itemInputs(items []itemRequest) []ItemInput {
	out := make([]ItemInput, 0, len(items))
	for _, it := range items {
		out = append(out, ItemInput{
			Kind: it.Kind, Description: it.Description,
			Quantity: it.Quantity, UnitPriceCents: it.UnitPriceCents,
		})
	}
	return out
}
