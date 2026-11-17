package schedule

import (
	"errors"
	"log/slog"
	"net/http"
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
	write := auth.RequireAnyRole("owner", "staff")
	read := auth.RequireAnyRole("owner", "staff", "retoucher", "assistant")

	r.With(write).Post("/resources", h.createResource)
	r.With(read).Get("/resources", h.listResources)
	r.With(read).Get("/resources/{resourceID}", h.getResource)
	r.With(write).Patch("/resources/{resourceID}", h.updateResource)
	r.With(write).Put("/resources/{resourceID}/availability", h.setAvailability)
	r.With(read).Get("/resources/{resourceID}/availability", h.getAvailability)
	r.With(read).Get("/schedule/availability", h.availability)
	r.With(write).Post("/resources/{resourceID}/blocks", h.createBlock)
	r.With(read).Get("/resources/{resourceID}/blocks", h.listBlocks)
	r.With(write).Delete("/resources/{resourceID}/blocks/{blockID}", h.deleteBlock)

	r.With(write).Post("/bookings", h.createHold)
	r.With(read).Get("/bookings", h.listBookings)
	r.With(read).Get("/bookings/{bookingID}", h.getBooking)
	r.With(write).Post("/bookings/{bookingID}/confirm", h.confirm)
	r.With(write).Post("/bookings/{bookingID}/reschedule", h.reschedule)
	r.With(write).Post("/bookings/{bookingID}/cancel", h.cancel)
}

type resourceRequest struct {
	Kind                string `json:"kind"`
	Name                string `json:"name"`
	Active              *bool  `json:"active"`
	BufferBeforeMinutes int    `json:"buffer_before_minutes"`
	BufferAfterMinutes  int    `json:"buffer_after_minutes"`
	Notes               string `json:"notes"`
}

func (h *Handler) createResource(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req resourceRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.CreateResource(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, ResourceInput{
		Kind: req.Kind, Name: req.Name, Active: req.Active,
		BufferBeforeMinutes: req.BufferBeforeMinutes, BufferAfterMinutes: req.BufferAfterMinutes,
		Notes: req.Notes,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, res)
}

type resourceUpdateRequest struct {
	ExpectedVersion     int     `json:"expected_version"`
	Kind                *string `json:"kind"`
	Name                *string `json:"name"`
	Active              *bool   `json:"active"`
	BufferBeforeMinutes *int    `json:"buffer_before_minutes"`
	BufferAfterMinutes  *int    `json:"buffer_after_minutes"`
	Notes               *string `json:"notes"`
}

func (h *Handler) updateResource(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := resourceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid resource id", "")
		return
	}
	var req resourceUpdateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.UpdateResource(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion,
		ResourceUpdate{
			Kind: req.Kind, Name: req.Name, Active: req.Active,
			BufferBeforeMinutes: req.BufferBeforeMinutes, BufferAfterMinutes: req.BufferAfterMinutes,
			Notes: req.Notes,
		})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) getResource(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := resourceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid resource id", "")
		return
	}
	res, err := h.svc.GetResource(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) listResources(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var active *bool
	switch r.URL.Query().Get("active") {
	case "true":
		v := true
		active = &v
	case "false":
		v := false
		active = &v
	case "":
	default:
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed",
			"active must be true or false")
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListResources(r.Context(), p.TenantID,
		r.URL.Query().Get("kind"), active, limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

type availabilityRequest struct {
	ExpectedVersion int                `json:"expected_version"`
	Rules           []AvailabilityRule `json:"rules"`
}

func (h *Handler) setAvailability(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := resourceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid resource id", "")
		return
	}
	var req availabilityRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.SetAvailabilityRules(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr,
		req.ExpectedVersion, req.Rules)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"resource_id": res.ID, "version": res.Version, "rules": res.Rules,
	})
}

func (h *Handler) getAvailability(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := resourceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid resource id", "")
		return
	}
	res, err := h.svc.GetResource(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rules := res.Rules
	if rules == nil {
		rules = []AvailabilityRule{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"resource_id": res.ID, "version": res.Version, "rules": rules,
	})
}

func (h *Handler) availability(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	from, err := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed",
			"from must be RFC3339")
		return
	}
	to, err := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed",
			"to must be RFC3339")
		return
	}
	out, err := h.svc.CheckAvailability(r.Context(), p.TenantID, r.URL.Query().Get("kind"), from, to)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type blockRequest struct {
	Kind     string    `json:"kind"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Note     string    `json:"note"`
}

func (h *Handler) createBlock(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := resourceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid resource id", "")
		return
	}
	var req blockRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	block, err := h.svc.CreateBlock(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, BlockInput{
		Kind: req.Kind, StartsAt: req.StartsAt, EndsAt: req.EndsAt, Note: req.Note,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, block)
}

func (h *Handler) listBlocks(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := resourceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid resource id", "")
		return
	}
	from, err := httpx.ParseOptionalDate(r.URL.Query().Get("from"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
		return
	}
	to, err := httpx.ParseOptionalDate(r.URL.Query().Get("to"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
		return
	}
	var fromAt, toAt time.Time
	if from != nil {
		fromAt = *from
	}
	if to != nil {
		toAt = *to
	}
	items, err := h.svc.ListBlocks(r.Context(), p.TenantID, id, fromAt, toAt)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) deleteBlock(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := resourceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid resource id", "")
		return
	}
	blockID, err := uuid.Parse(chi.URLParam(r, "blockID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid block id", "")
		return
	}
	if err := h.svc.DeleteBlock(r.Context(), p.TenantID, p.UserID, id, blockID, r.RemoteAddr); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type holdRequest struct {
	ProjectID   uuid.UUID   `json:"project_id"`
	ResourceIDs []uuid.UUID `json:"resource_ids"`
	StartsAt    time.Time   `json:"starts_at"`
	EndsAt      time.Time   `json:"ends_at"`
	Note        string      `json:"note"`
}

func (h *Handler) createHold(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req holdRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	booking, err := h.svc.CreateHold(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, HoldInput{
		ProjectID: req.ProjectID, ResourceIDs: req.ResourceIDs,
		StartsAt: req.StartsAt, EndsAt: req.EndsAt, Note: req.Note,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, booking)
}

func (h *Handler) listBookings(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var projectID *uuid.UUID
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed",
				"invalid project_id")
			return
		}
		projectID = &id
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListBookings(r.Context(), p.TenantID, projectID,
		r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) getBooking(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := bookingID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid booking id", "")
		return
	}
	booking, err := h.svc.GetBooking(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, booking)
}

type versionRequest struct {
	ExpectedVersion int `json:"expected_version"`
}

func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := bookingID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid booking id", "")
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	booking, err := h.svc.Confirm(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, booking)
}

type rescheduleRequest struct {
	ExpectedVersion int         `json:"expected_version"`
	StartsAt        time.Time   `json:"starts_at"`
	EndsAt          time.Time   `json:"ends_at"`
	ResourceIDs     []uuid.UUID `json:"resource_ids"`
	Note            *string     `json:"note"`
}

func (h *Handler) reschedule(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := bookingID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid booking id", "")
		return
	}
	var req rescheduleRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	booking, err := h.svc.Reschedule(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr,
		req.ExpectedVersion, RescheduleInput{
			StartsAt: req.StartsAt, EndsAt: req.EndsAt, ResourceIDs: req.ResourceIDs, Note: req.Note,
		})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, booking)
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
	id, err := bookingID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid booking id", "")
		return
	}
	var req cancelRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	booking, err := h.svc.Cancel(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr,
		req.ExpectedVersion, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, booking)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrResourceNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "resource_not_found", "Resource not found", err.Error())
	case errors.Is(err, ErrBookingNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "booking_not_found", "Booking not found", err.Error())
	case errors.Is(err, ErrBlockNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "block_not_found", "Block not found", err.Error())
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found", err.Error())
	case errors.Is(err, ErrResourceConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "resource_conflict", "Resource name already exists", err.Error())
	case errors.Is(err, ErrActiveBooking):
		httpx.WriteProblem(w, r, http.StatusConflict, "active_booking", "Project already has an active booking", err.Error())
	case errors.Is(err, ErrSlotTaken):
		httpx.WriteProblem(w, r, http.StatusConflict, "slot_taken", "Time slot is occupied", err.Error())
	case errors.Is(err, ErrDepositRequired):
		httpx.WriteProblem(w, r, http.StatusConflict, "deposit_required", "Deposit not paid", err.Error())
	case errors.Is(err, ErrHoldExpired):
		httpx.WriteProblem(w, r, http.StatusConflict, "hold_expired", "Booking hold expired", err.Error())
	case errors.Is(err, ErrIllegalState):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_state", "Illegal booking state", err.Error())
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict",
			"the booking or resource was modified; reload and retry")
	case errors.Is(err, ErrOutsideAvailability):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "outside_availability",
			"Outside resource availability", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func resourceID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "resourceID"))
}

func bookingID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "bookingID"))
}
