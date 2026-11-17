package brief

import (
	"encoding/json"
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
	r.With(auth.RequireAnyRole("owner", "staff", "retoucher", "assistant")).
		Get("/projects/{projectID}/brief", h.get)
	r.With(auth.RequireAnyRole("owner", "staff")).
		Put("/projects/{projectID}/brief", h.upsert)
}

type upsertRequest struct {
	ExpectedVersion int             `json:"expected_version"`
	Style           string          `json:"style"`
	ShotCount       int             `json:"shot_count"`
	Venue           string          `json:"venue"`
	Crew            string          `json:"crew"`
	DeliverableSpec string          `json:"deliverable_spec"`
	UsageScope      string          `json:"usage_scope"`
	Requirements    string          `json:"requirements"`
	Details         json.RawMessage `json:"details"`
}

func (h *Handler) upsert(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
		return
	}
	var req upsertRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	b, err := h.svc.Upsert(r.Context(), p.TenantID, p.UserID, projectID, r.RemoteAddr, req.ExpectedVersion, Input{
		Style: req.Style, ShotCount: req.ShotCount, Venue: req.Venue, Crew: req.Crew,
		DeliverableSpec: req.DeliverableSpec, UsageScope: req.UsageScope,
		Requirements: req.Requirements, Details: req.Details,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, b)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
		return
	}
	b, err := h.svc.Get(r.Context(), p.TenantID, projectID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, b)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Brief not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "brief was modified, reload and retry")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
