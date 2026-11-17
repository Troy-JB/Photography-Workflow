package archive

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

// Handler serves the staff archive API. Verification/retention are visible to
// the whole studio; destroying stored objects requires the owner.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Register(r chi.Router) {
	read := auth.RequireAnyRole("owner", "staff", "assistant", "retoucher")
	staff := auth.RequireAnyRole("owner", "staff", "assistant")
	owner := auth.RequireAnyRole("owner")

	r.With(read).Get("/archives", h.list)
	r.With(staff).Post("/archives", h.create)
	r.Route("/archives/{archiveID}", func(r chi.Router) {
		r.With(read).Get("/", h.get)
		r.With(read).Get("/items", h.listItems)
		r.With(staff).Post("/verify", h.verify)
		r.With(owner).Post("/deletion", h.requestDeletion)
	})
	r.With(read).Get("/retention-policy", h.getPolicy)
	r.With(owner).Put("/retention-policy", h.setPolicy)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	projectID, ok := optionalUUIDQuery(w, r, "project_id")
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !validStatus(status) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown status")
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.List(r.Context(), p.TenantID, projectID, status, limit, offset)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

type createRequest struct {
	ProjectID uuid.UUID `json:"project_id"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
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
	summary, created, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, req.ProjectID)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, summary)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.archiveID(w, r)
	if !ok {
		return
	}
	detail, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func (h *Handler) listItems(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.archiveID(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !validItemStatus(status) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown status")
		return
	}
	limit := httpx.IntQuery(r, "limit", 100, 1, 500)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListItems(r.Context(), p.TenantID, id, status, limit, offset)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.archiveID(w, r)
	if !ok {
		return
	}
	summary, err := h.svc.Retry(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, id)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, summary)
}

type deletionRequest struct {
	Confirm string `json:"confirm"`
	Reason  string `json:"reason"`
}

func (h *Handler) requestDeletion(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.archiveID(w, r)
	if !ok {
		return
	}
	var req deletionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	summary, err := h.svc.RequestDeletion(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, id, req.Confirm, req.Reason)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, summary)
}

func (h *Handler) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	policy, err := h.svc.GetPolicy(r.Context(), p.TenantID)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policy)
}

type policyRequest struct {
	KeepDays int `json:"keep_days"`
	WarnDays int `json:"warn_days"`
}

func (h *Handler) setPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req policyRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	policy, err := h.svc.SetPolicy(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, req.KeepDays, req.WarnDays)
	if err != nil {
		fail(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policy)
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return nil, false
	}
	return p, true
}

func (h *Handler) archiveID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "archiveID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid archive id", "")
		return uuid.Nil, false
	}
	return id, true
}

func optionalUUIDQuery(w http.ResponseWriter, r *http.Request, name string) (*uuid.UUID, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid "+name, "")
		return nil, false
	}
	return &id, true
}

func validStatus(status string) bool {
	switch status {
	case StatusPending, StatusVerifying, StatusReady, StatusFailed, StatusDeleting, StatusDeleted:
		return true
	}
	return false
}

func validItemStatus(status string) bool {
	switch status {
	case ItemPending, ItemVerified, ItemMissing, ItemMismatch, ItemDeleted:
		return true
	}
	return false
}

func fail(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Archive not found", "")
	case errors.Is(err, ErrProjectNotArchived):
		httpx.WriteProblem(w, r, http.StatusConflict, "project_not_archived", "Project is not archived",
			"archive a project only after it reaches the archived state")
	case errors.Is(err, ErrVerifyNotAllowed):
		httpx.WriteProblem(w, r, http.StatusConflict, "verify_not_allowed", "Archive cannot be verified again",
			"only a failed verification can be retried")
	case errors.Is(err, ErrDeletionNotAllowed):
		httpx.WriteProblem(w, r, http.StatusConflict, "deletion_not_allowed", "Archive cannot be deleted",
			"the archive must be verified before its objects can be deleted")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, log, err)
	}
}
