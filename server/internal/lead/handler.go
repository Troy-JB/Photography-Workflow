package lead

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/project"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Register(r chi.Router) {
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/leads", h.create)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/leads", h.list)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/leads/{leadID}", h.get)
	r.With(auth.RequireAnyRole("owner", "staff")).Patch("/leads/{leadID}", h.update)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/leads/{leadID}/transitions", h.transition)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/leads/{leadID}/follow-ups", h.addFollowUp)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/leads/{leadID}/follow-ups", h.listFollowUps)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/leads/{leadID}/convert", h.convert)
}

type createRequest struct {
	Name           string     `json:"name"`
	Email          string     `json:"email"`
	Company        string     `json:"company"`
	Phone          string     `json:"phone"`
	Wechat         string     `json:"wechat"`
	Source         string     `json:"source"`
	Need           string     `json:"need"`
	BudgetMinCents int64      `json:"budget_min_cents"`
	BudgetMaxCents int64      `json:"budget_max_cents"`
	ShootOn        string     `json:"shoot_on"`
	Note           string     `json:"note"`
	NextFollowUpAt *time.Time `json:"next_follow_up_at"`
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
	shootOn, err := httpx.ParseOptionalDate(req.ShootOn)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
		return
	}
	l, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateInput{
		Name: req.Name, Email: req.Email, Company: req.Company, Phone: req.Phone, Wechat: req.Wechat,
		Source: req.Source, Need: req.Need, BudgetMinCents: req.BudgetMinCents,
		BudgetMaxCents: req.BudgetMaxCents, ShootOn: shootOn, Note: req.Note,
		NextFollowUpAt: req.NextFollowUpAt,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, l)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.List(r.Context(), p.TenantID, r.URL.Query().Get("status"), r.URL.Query().Get("q"), limit, offset)
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
	id, err := parseID(w, r, "leadID", "lead")
	if err != nil {
		return
	}
	l, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, l)
}

type updateRequest struct {
	ExpectedVersion int        `json:"expected_version"`
	Name            *string    `json:"name"`
	Email           *string    `json:"email"`
	Company         *string    `json:"company"`
	Phone           *string    `json:"phone"`
	Wechat          *string    `json:"wechat"`
	Source          *string    `json:"source"`
	Need            *string    `json:"need"`
	BudgetMinCents  *int64     `json:"budget_min_cents"`
	BudgetMaxCents  *int64     `json:"budget_max_cents"`
	ShootOn         *string    `json:"shoot_on"`
	Note            *string    `json:"note"`
	NextFollowUpAt  *time.Time `json:"next_follow_up_at"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r, "leadID", "lead")
	if err != nil {
		return
	}
	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	var shootOn *time.Time
	if req.ShootOn != nil {
		shootOn, err = httpx.ParseOptionalDate(*req.ShootOn)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
			return
		}
	}
	l, err := h.svc.Update(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion, UpdateInput{
		Name: req.Name, Email: req.Email, Company: req.Company, Phone: req.Phone, Wechat: req.Wechat,
		Source: req.Source, Need: req.Need, BudgetMinCents: req.BudgetMinCents,
		BudgetMaxCents: req.BudgetMaxCents, ShootOn: shootOn, Note: req.Note,
		NextFollowUpAt: req.NextFollowUpAt,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, l)
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
	id, err := parseID(w, r, "leadID", "lead")
	if err != nil {
		return
	}
	var req transitionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	l, err := h.svc.Transition(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, State(req.To), req.ExpectedVersion, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, l)
}

type followUpRequest struct {
	Channel string     `json:"channel"`
	Summary string     `json:"summary"`
	NextAt  *time.Time `json:"next_at"`
}

func (h *Handler) addFollowUp(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r, "leadID", "lead")
	if err != nil {
		return
	}
	var req followUpRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	f, err := h.svc.AddFollowUp(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.Channel, req.Summary, req.NextAt)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, f)
}

func (h *Handler) listFollowUps(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r, "leadID", "lead")
	if err != nil {
		return
	}
	items, err := h.svc.ListFollowUps(r.Context(), p.TenantID, id, httpx.IntQuery(r, "limit", 50, 1, 200))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type convertRequest struct {
	ExpectedVersion  int        `json:"expected_version"`
	ClientID         *uuid.UUID `json:"client_id"`
	ProjectID        *uuid.UUID `json:"project_id"`
	CreateProject    *bool      `json:"create_project"`
	ProjectTitle     string     `json:"project_title"`
	TotalAmountCents int64      `json:"total_amount_cents"`
	DepositCents     int64      `json:"deposit_cents"`
}

func (h *Handler) convert(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := parseID(w, r, "leadID", "lead")
	if err != nil {
		return
	}
	var req convertRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	createProject := req.ProjectID == nil
	if req.CreateProject != nil {
		createProject = *req.CreateProject
	}
	l, err := h.svc.Convert(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, ConvertInput{
		ExpectedVersion:  req.ExpectedVersion,
		ClientID:         req.ClientID,
		ProjectID:        req.ProjectID,
		CreateProject:    createProject,
		ProjectTitle:     req.ProjectTitle,
		TotalAmountCents: req.TotalAmountCents,
		DepositCents:     req.DepositCents,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, l)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Lead not found", "")
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found", "")
	case errors.Is(err, ErrClientNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "client_not_found", "Client not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "lead was modified, reload and retry")
	case errors.Is(err, ErrIllegalTransition):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_transition", "Illegal lead transition", err.Error())
	case errors.Is(err, project.ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	case errors.Is(err, project.ErrCodeConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "code_conflict", "Project code already exists", "retry")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func parseID(w http.ResponseWriter, r *http.Request, param, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid "+name+" id", "")
		return uuid.Nil, err
	}
	return id, nil
}
