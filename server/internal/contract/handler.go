package contract

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
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/contracts", h.disabled)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/contracts", h.disabled)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/contracts/{contractID}", h.disabled)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/contracts/{contractID}/send", h.disabled)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/contracts/{contractID}/void", h.disabled)
}

func (h *Handler) disabled(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, http.StatusGone, "feature_disabled", "Contract feature disabled", "合同功能暂时停用，现有合同数据已保留。")
}

type createRequest struct {
	ProjectID       uuid.UUID `json:"project_id"`
	QuoteID         uuid.UUID `json:"quote_id"`
	TemplateVersion string    `json:"template_version"`
	SignerName      string    `json:"signer_name"`
	SignerContact   string    `json:"signer_contact"`
	IncludedRounds  *int      `json:"included_rounds"`
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
	c, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateInput{
		ProjectID: req.ProjectID, QuoteID: req.QuoteID, TemplateVersion: req.TemplateVersion,
		SignerName: req.SignerName, SignerContact: req.SignerContact,
		IncludedRounds: req.IncludedRounds,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
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
	c, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

type versionRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r)
	if err != nil {
		return
	}
	var req versionRequest
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
			return
		}
	}
	c, err := h.svc.Send(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (h *Handler) void(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r)
	if err != nil {
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	c, err := h.svc.Void(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Contract not found", "")
	case errors.Is(err, ErrQuoteNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "quote_not_found", "Quote not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "contract was modified, reload and retry")
	case errors.Is(err, ErrActiveContract):
		httpx.WriteProblem(w, r, http.StatusConflict, "active_contract", "Project already has an active contract", "void the existing contract first")
	case errors.Is(err, ErrIllegalState):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_state", "Illegal contract state", err.Error())
	case errors.Is(err, ErrSendInProgress):
		httpx.WriteProblem(w, r, http.StatusConflict, "send_in_progress", "Contract send is in progress",
			"the signing task is being created, retry in a few minutes")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	case errors.Is(err, ErrProvider):
		httpx.WriteProblem(w, r, http.StatusBadGateway, "esign_provider_error", "E-sign provider error", "the signing task could not be created, retry later")
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "contractID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid contract id", "")
		return uuid.Nil, err
	}
	return id, nil
}
