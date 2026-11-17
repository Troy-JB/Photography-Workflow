package client

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
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/clients", h.create)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/clients", h.list)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/clients/{clientID}", h.get)
	r.With(auth.RequireAnyRole("owner", "staff")).Patch("/clients/{clientID}", h.update)
}

type createRequest struct {
	Name    string   `json:"name"`
	Company string   `json:"company"`
	Phone   string   `json:"phone"`
	Wechat  string   `json:"wechat"`
	Email   string   `json:"email"`
	Source  string   `json:"source"`
	Tags    []string `json:"tags"`
	Note    string   `json:"note"`
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
	c, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, Input{
		Name: req.Name, Company: req.Company, Phone: req.Phone, Wechat: req.Wechat,
		Email: req.Email, Source: req.Source, Tags: req.Tags, Note: req.Note,
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
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.List(r.Context(), p.TenantID, r.URL.Query().Get("q"), limit, offset)
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
	id, err := uuid.Parse(chi.URLParam(r, "clientID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid client id", "")
		return
	}
	c, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

type updateRequest struct {
	ExpectedVersion int       `json:"expected_version"`
	Name            *string   `json:"name"`
	Company         *string   `json:"company"`
	Phone           *string   `json:"phone"`
	Wechat          *string   `json:"wechat"`
	Email           *string   `json:"email"`
	Source          *string   `json:"source"`
	Tags            *[]string `json:"tags"`
	Note            *string   `json:"note"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "clientID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid client id", "")
		return
	}
	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	c, err := h.svc.Update(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion, UpdateInput{
		Name: req.Name, Company: req.Company, Phone: req.Phone, Wechat: req.Wechat,
		Email: req.Email, Source: req.Source, Tags: req.Tags, Note: req.Note,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Client not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "client was modified, reload and retry")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
