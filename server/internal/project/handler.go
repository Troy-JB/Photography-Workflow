package project

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/projects", h.create)
	r.With(auth.RequireAnyRole("owner", "staff", "retoucher", "assistant")).Get("/projects", h.list)
	r.With(auth.RequireAnyRole("owner", "staff", "retoucher", "assistant")).Get("/projects/{projectID}", h.get)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/projects/{projectID}/transitions", h.transition)
}

type createRequest struct {
	Code              string     `json:"code"`
	Title             string     `json:"title"`
	ClientID          *uuid.UUID `json:"client_id"`
	ShootStartAt      *time.Time `json:"shoot_start_at"`
	ShootEndAt        *time.Time `json:"shoot_end_at"`
	Currency          string     `json:"currency"`
	TotalAmountCents  int64      `json:"total_amount_cents"`
	DepositCents      int64      `json:"deposit_cents"`
	BalanceCents      int64      `json:"balance_cents"`
	SelectionDeadline *time.Time `json:"selection_deadline"`
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
	if req.Title == "" {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "title is required")
		return
	}
	if req.Currency != "" && len(req.Currency) != 3 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "currency must be a 3-letter code")
		return
	}
	if req.TotalAmountCents < 0 || req.DepositCents < 0 || req.BalanceCents < 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "amounts must not be negative")
		return
	}

	created, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateInput{
		Code:              req.Code,
		Title:             req.Title,
		ClientID:          req.ClientID,
		ShootStartAt:      req.ShootStartAt,
		ShootEndAt:        req.ShootEndAt,
		Currency:          req.Currency,
		TotalAmountCents:  req.TotalAmountCents,
		DepositCents:      req.DepositCents,
		BalanceCents:      req.BalanceCents,
		SelectionDeadline: req.SelectionDeadline,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, created)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && !ValidState(state) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown state")
		return
	}
	excludeClosed := false
	if raw := r.URL.Query().Get("exclude_closed"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "exclude_closed must be true or false")
			return
		}
		excludeClosed = parsed
	}
	limit := intQuery(r, "limit", 50, 1, 200)
	offset := intQuery(r, "offset", 0, 0, 1<<30)

	items, total, err := h.svc.List(r.Context(), p.TenantID, state, excludeClosed, limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
		return
	}
	item, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, item)
}

type transitionRequest struct {
	To              string `json:"to"`
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
		return
	}
	var req transitionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.To == "" || !ValidState(req.To) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown target state")
		return
	}
	if req.ExpectedVersion <= 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "expected_version is required")
		return
	}

	updated, err := h.svc.Transition(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr, State(req.To), req.ExpectedVersion, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Project not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "project was modified, reload and retry")
	case errors.Is(err, ErrCodeConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "code_conflict", "Project code already exists", "choose another code")
	case errors.Is(err, ErrIllegalTransition):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_transition", "Illegal state transition", err.Error())
	case errors.Is(err, ErrGateNotSatisfied):
		httpx.WriteProblem(w, r, http.StatusConflict, "gate_not_satisfied", "Transition gate not satisfied", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func intQuery(r *http.Request, key string, def, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min {
		return def
	}
	if v > max {
		return max
	}
	return v
}
