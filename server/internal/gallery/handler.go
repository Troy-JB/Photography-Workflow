package gallery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	staff := auth.RequireAnyRole("owner", "staff", "assistant")
	r.With(staff).Get("/galleries", h.list)
	r.With(staff).Post("/galleries", h.create)
	r.Route("/galleries/{galleryID}", func(r chi.Router) {
		r.With(staff).Get("/", h.get)
		r.With(staff).Patch("/", h.update)
		r.With(staff).Get("/assets", h.listAssets)
		r.With(staff).Get("/candidates", h.listCandidates)
		r.With(staff).Post("/assets", h.addAssets)
		r.With(staff).Delete("/assets/{assetID}", h.removeAsset)
		r.With(staff).Post("/publish", h.publish)
		r.With(staff).Post("/tokens", h.createToken)
		r.With(staff).Delete("/tokens/{tokenID}", h.revokeToken)
		r.With(staff).Post("/revoke", h.revoke)
		r.With(staff).Post("/reopen", h.reopen)
	})
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
			httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
			return
		}
		projectID = &id
	}
	status := r.URL.Query().Get("status")
	if status != "" && !ValidStatus(status) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown status")
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.List(r.Context(), p.TenantID, projectID, status, limit, offset)
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

type createRequest struct {
	ProjectID       uuid.UUID  `json:"project_id"`
	Title           string     `json:"title"`
	ExpiresAt       *time.Time `json:"expires_at"`
	SelectionLimit  *int       `json:"selection_limit"`
	DownloadAllowed bool       `json:"download_allowed"`
	WatermarkText   string     `json:"watermark_text"`
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
	g, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateInput{
		ProjectID:       req.ProjectID,
		Title:           req.Title,
		ExpiresAt:       req.ExpiresAt,
		SelectionLimit:  req.SelectionLimit,
		DownloadAllowed: req.DownloadAllowed,
		WatermarkText:   req.WatermarkText,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, g)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	item, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, item)
}

func (h *Handler) listAssets(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	filter := r.URL.Query().Get("filter")
	if !ValidAssetFilter(filter) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown filter")
		return
	}
	limit := httpx.IntQuery(r, "limit", 60, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	res, err := h.svc.ListAssets(r.Context(), p.TenantID, id, filter,
		r.URL.Query().Get("q"), limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) listCandidates(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	limit := httpx.IntQuery(r, "limit", 60, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	res, err := h.svc.ListCandidates(r.Context(), p.TenantID, id, r.URL.Query().Get("q"), limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// optionalTime distinguishes an absent field from an explicit null (clear the
// deadline). It is only used by PATCH.
type optionalTime struct {
	Set   bool
	Value *time.Time
}

func (o *optionalTime) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	o.Value = &t
	return nil
}

type optionalInt struct {
	Set   bool
	Value *int
}

func (o *optionalInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

type updateRequest struct {
	ExpectedVersion int          `json:"expected_version"`
	Title           *string      `json:"title"`
	ExpiresAt       optionalTime `json:"expires_at"`
	SelectionLimit  optionalInt  `json:"selection_limit"`
	DownloadAllowed *bool        `json:"download_allowed"`
	WatermarkText   *string      `json:"watermark_text"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	in := UpdateInput{
		ExpectedVersion: req.ExpectedVersion,
		Title:           req.Title,
		ExpiresAt:       req.ExpiresAt.Value,
		ClearExpiresAt:  req.ExpiresAt.Set && req.ExpiresAt.Value == nil,
		SelectionLimit:  req.SelectionLimit.Value,
		ClearLimit:      req.SelectionLimit.Set && req.SelectionLimit.Value == nil,
		DownloadAllowed: req.DownloadAllowed,
		WatermarkText:   req.WatermarkText,
	}
	updated, err := h.svc.Update(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
}

type addAssetsRequest struct {
	AssetIDs []uuid.UUID `json:"asset_ids"`
}

func (h *Handler) addAssets(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	var req addAssetsRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.AddAssets(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr, req.AssetIDs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) removeAsset(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid asset id", "")
		return
	}
	g, err := h.svc.RemoveAsset(r.Context(), p.TenantID, id, assetID, p.UserID, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, g)
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	g, err := h.svc.Publish(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, g)
}

type tokenRequest struct {
	Label     string     `json:"label"`
	MaxVisits int        `json:"max_visits"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	var req tokenRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	tok, err := h.svc.CreateToken(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr, TokenInput{
		Label:     req.Label,
		MaxVisits: req.MaxVisits,
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, tok)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid token id", "")
		return
	}
	if err := h.svc.RevokeToken(r.Context(), p.TenantID, id, tokenID, p.UserID, r.RemoteAddr); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(ctx context.Context, tenantID, id, actor uuid.UUID, ip string) (*Gallery, error) {
		return h.svc.Revoke(ctx, tenantID, id, actor, ip)
	})
}

func (h *Handler) reopen(w http.ResponseWriter, r *http.Request) {
	h.mutate(w, r, func(ctx context.Context, tenantID, id, actor uuid.UUID, ip string) (*Gallery, error) {
		return h.svc.Reopen(ctx, tenantID, id, actor, ip)
	})
}

func (h *Handler) mutate(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, tenantID, id, actor uuid.UUID, ip string) (*Gallery, error)) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, ok := h.galleryID(w, r)
	if !ok {
		return
	}
	g, err := fn(r.Context(), p.TenantID, id, p.UserID, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, g)
}

func (h *Handler) galleryID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "galleryID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid gallery id", "")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var incomplete *DerivativesIncompleteError
	switch {
	case errors.As(err, &incomplete):
		httpx.WriteProblem(w, r, http.StatusConflict, "derivatives_incomplete", "Gallery previews are not ready",
			fmt.Sprintf("%d assets are missing thumb/display previews (e.g. %s); derivation jobs were queued, retry after the worker finishes",
				incomplete.Missing, sampleIDs(incomplete.Sample)))
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Gallery not found", "")
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "project not found in this tenant")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "gallery was modified, reload and retry")
	case errors.Is(err, ErrInvalidState):
		httpx.WriteProblem(w, r, http.StatusConflict, "invalid_state", "Gallery state does not allow this change", err.Error())
	case errors.Is(err, ErrAssetInvalid):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "asset_invalid", "Invalid gallery asset", err.Error())
	case errors.Is(err, ErrAssetLimit):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "asset_limit", "Gallery asset limit reached", err.Error())
	case errors.Is(err, ErrAssetNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "asset_not_found", "Asset not in gallery", "")
	case errors.Is(err, ErrTokenNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "token_not_found", "Token not found or already revoked", "")
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
