package prep

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

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
	read := auth.RequireAnyRole("owner", "staff", "assistant")
	write := auth.RequireAnyRole("owner", "staff", "assistant")

	r.With(read).Get("/bookings/{bookingID}/shot-list", h.getShotList)
	r.With(write).Post("/bookings/{bookingID}/shot-list", h.createShotList)
	r.With(write).Patch("/shot-lists/{shotListID}", h.updateShotList)
	r.With(write).Post("/shot-lists/{shotListID}/items", h.addShotItem)
	r.With(write).Patch("/shot-items/{shotItemID}", h.updateShotItem)
	r.With(write).Delete("/shot-items/{shotItemID}", h.deleteShotItem)

	r.With(read).Get("/bookings/{bookingID}/gear", h.listGear)
	r.With(write).Post("/bookings/{bookingID}/gear", h.addGear)
	r.With(write).Patch("/gear/{gearID}", h.updateGear)
	r.With(write).Delete("/gear/{gearID}", h.deleteGear)

	r.With(read).Get("/bookings/{bookingID}/authorizations", h.listAuthorizations)
	r.With(write).Post("/bookings/{bookingID}/authorizations", h.createAuthorization)
	r.With(write).Patch("/authorizations/{authorizationID}", h.updateAuthorization)
	r.With(write).Delete("/authorizations/{authorizationID}", h.deleteAuthorization)

	r.With(write).Post("/bookings/{bookingID}/call-sheets", h.freezeCallSheet)
	r.With(read).Get("/bookings/{bookingID}/call-sheets", h.listCallSheets)
	r.With(read).Get("/call-sheets/{callSheetID}", h.getCallSheet)
	r.With(read).Get("/call-sheets/{callSheetID}/html", h.renderCallSheetHTML)
}

type shotListRequest struct {
	Title string `json:"title"`
	Note  string `json:"note"`
}

func (h *Handler) createShotList(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	var req shotListRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	list, err := h.svc.CreateShotList(r.Context(), p.TenantID, p.UserID, bookingID, r.RemoteAddr,
		ShotListInput{Title: req.Title, Note: req.Note})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, list)
}

func (h *Handler) getShotList(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	list, err := h.svc.GetShotList(r.Context(), p.TenantID, bookingID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

type shotListUpdateRequest struct {
	ExpectedVersion int     `json:"expected_version"`
	Title           *string `json:"title"`
	Note            *string `json:"note"`
}

func (h *Handler) updateShotList(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "shotListID", "shot list")
	if !ok {
		return
	}
	var req shotListUpdateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	list, err := h.svc.UpdateShotList(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr,
		req.ExpectedVersion, ShotListUpdate{Title: req.Title, Note: req.Note})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

type shotItemRequest struct {
	Category     string `json:"category"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	ReferenceURL string `json:"reference_url"`
	Priority     string `json:"priority"`
}

func (h *Handler) addShotItem(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	listID, ok := pathID(w, r, "shotListID", "shot list")
	if !ok {
		return
	}
	var req shotItemRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	item, err := h.svc.AddShotItem(r.Context(), p.TenantID, p.UserID, listID, r.RemoteAddr,
		ShotItemInput{
			Category: req.Category, Title: req.Title, Description: req.Description,
			ReferenceURL: req.ReferenceURL, Priority: req.Priority,
		})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, item)
}

type shotItemUpdateRequest struct {
	ExpectedVersion int     `json:"expected_version"`
	Category        *string `json:"category"`
	Title           *string `json:"title"`
	Description     *string `json:"description"`
	ReferenceURL    *string `json:"reference_url"`
	Priority        *string `json:"priority"`
	Done            *bool   `json:"done"`
}

func (h *Handler) updateShotItem(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "shotItemID", "shot item")
	if !ok {
		return
	}
	var req shotItemUpdateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	item, err := h.svc.UpdateShotItem(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr,
		req.ExpectedVersion, ShotItemUpdate{
			Category: req.Category, Title: req.Title, Description: req.Description,
			ReferenceURL: req.ReferenceURL, Priority: req.Priority, Done: req.Done,
		})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, item)
}

func (h *Handler) deleteShotItem(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "shotItemID", "shot item")
	if !ok {
		return
	}
	version, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteShotItem(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, version); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type gearRequest struct {
	ResourceID *uuid.UUID `json:"resource_id"`
	Name       string     `json:"name"`
	Category   string     `json:"category"`
	Quantity   int        `json:"quantity"`
	Note       string     `json:"note"`
}

func (h *Handler) listGear(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	items, err := h.svc.ListGear(r.Context(), p.TenantID, bookingID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) addGear(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	var req gearRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	item, err := h.svc.AddGear(r.Context(), p.TenantID, p.UserID, bookingID, r.RemoteAddr, GearInput{
		ResourceID: req.ResourceID, Name: req.Name, Category: req.Category,
		Quantity: req.Quantity, Note: req.Note,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, item)
}

type gearUpdateRequest struct {
	ExpectedVersion int     `json:"expected_version"`
	Name            *string `json:"name"`
	Category        *string `json:"category"`
	Quantity        *int    `json:"quantity"`
	Checked         *bool   `json:"checked"`
	Note            *string `json:"note"`
}

func (h *Handler) updateGear(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "gearID", "gear")
	if !ok {
		return
	}
	var req gearUpdateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	item, err := h.svc.UpdateGear(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr,
		req.ExpectedVersion, GearUpdate{
			Name: req.Name, Category: req.Category, Quantity: req.Quantity,
			Checked: req.Checked, Note: req.Note,
		})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, item)
}

func (h *Handler) deleteGear(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "gearID", "gear")
	if !ok {
		return
	}
	version, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteGear(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, version); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type authorizationRequest struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Status  string `json:"status"`
	Note    string `json:"note"`
}

func (h *Handler) listAuthorizations(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	items, err := h.svc.ListAuthorizations(r.Context(), p.TenantID, bookingID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) createAuthorization(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	var req authorizationRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	a, err := h.svc.CreateAuthorization(r.Context(), p.TenantID, p.UserID, bookingID, r.RemoteAddr,
		AuthorizationInput{Kind: req.Kind, Subject: req.Subject, Status: req.Status, Note: req.Note})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, a)
}

type authorizationUpdateRequest struct {
	ExpectedVersion int     `json:"expected_version"`
	Status          *string `json:"status"`
	Note            *string `json:"note"`
}

func (h *Handler) updateAuthorization(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "authorizationID", "authorization")
	if !ok {
		return
	}
	var req authorizationUpdateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	a, err := h.svc.UpdateAuthorization(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr,
		req.ExpectedVersion, AuthorizationUpdate{Status: req.Status, Note: req.Note})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

func (h *Handler) deleteAuthorization(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "authorizationID", "authorization")
	if !ok {
		return
	}
	version, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteAuthorization(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, version); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) freezeCallSheet(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	sheet, err := h.svc.FreezeCallSheet(r.Context(), p.TenantID, p.UserID, bookingID, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, sheet)
}

func (h *Handler) listCallSheets(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	bookingID, ok := pathID(w, r, "bookingID", "booking")
	if !ok {
		return
	}
	items, err := h.svc.ListCallSheets(r.Context(), p.TenantID, bookingID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) getCallSheet(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "callSheetID", "call sheet")
	if !ok {
		return
	}
	sheet, err := h.svc.GetCallSheet(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sheet)
}

func (h *Handler) renderCallSheetHTML(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "callSheetID", "call sheet")
	if !ok {
		return
	}
	page, err := h.svc.RenderCallSheetHTML(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrBookingNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "booking_not_found", "Booking not found", err.Error())
	case errors.Is(err, ErrShotListNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "shot_list_not_found", "Shot list not found", err.Error())
	case errors.Is(err, ErrShotItemNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "shot_item_not_found", "Shot item not found", err.Error())
	case errors.Is(err, ErrGearNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "gear_not_found", "Gear not found", err.Error())
	case errors.Is(err, ErrAuthorizationNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "authorization_not_found", "Authorization not found", err.Error())
	case errors.Is(err, ErrCallSheetNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "call_sheet_not_found", "Call sheet not found", err.Error())
	case errors.Is(err, ErrShotListExists):
		httpx.WriteProblem(w, r, http.StatusConflict, "shot_list_exists", "Shot list already exists", err.Error())
	case errors.Is(err, ErrGearConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "gear_conflict", "Gear name already exists", err.Error())
	case errors.Is(err, ErrAuthorizationConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "authorization_conflict",
			"Authorization already exists", err.Error())
	case errors.Is(err, ErrIllegalState):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_state", "Illegal booking state", err.Error())
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict",
			"the booking or record was modified; reload and retry")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return nil, false
	}
	return p, true
}

func pathID(w http.ResponseWriter, r *http.Request, param, label string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid "+label+" id", "")
		return uuid.Nil, false
	}
	return id, true
}

func expectedVersion(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("expected_version")
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed",
			"expected_version query parameter is required")
		return 0, false
	}
	return v, true
}
