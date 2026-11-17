package ingest

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/storage"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) RegisterAgent(r chi.Router) {
	r.Route("/agent/v1", func(r chi.Router) {
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/ingest-batches", h.createBatch)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/ingest-batches/{batchID}/assets/batch", h.registerFiles)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/ingest-batches/{batchID}/finalize", h.finalizeBatch)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Get("/ingest-batches/{batchID}", h.getBatchStatus)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/uploads/presign", h.presignUpload)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/uploads/{assetID}/complete", h.completeUpload)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/derivatives:presign", h.presignDerivative)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/derivatives/{assetID}:complete", h.completeDerivative)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Get("/jobs", h.listJobs)
		r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Get("/jobs/{jobID}", h.getJob)
	})
}

func (h *Handler) RegisterAdmin(r chi.Router) {
	r.With(auth.RequireAnyRole("owner", "staff", "retoucher", "assistant")).Get("/projects/{projectID}/assets", h.listAssets)
	r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/projects/{projectID}/derivatives:derive", h.deriveAssets)
}

type createBatchRequest struct {
	ProjectID       uuid.UUID `json:"project_id"`
	DeviceID        string    `json:"device_id"`
	CardFingerprint string    `json:"card_fingerprint"`
}

func (h *Handler) createBatch(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req createBatchRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.ProjectID == uuid.Nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "project_id is required")
		return
	}
	b, err := h.svc.CreateBatch(r.Context(), p.TenantID, CreateBatchInput{
		ProjectID:       req.ProjectID,
		DeviceID:        req.DeviceID,
		CardFingerprint: req.CardFingerprint,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, b)
}

type registerFilesRequest struct {
	Files []FileInput `json:"files"`
}

func (h *Handler) registerFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	batchID, err := uuid.Parse(chi.URLParam(r, "batchID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid batch id", "")
		return
	}
	var req registerFilesRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.RegisterFiles(r.Context(), p.TenantID, batchID, req.Files)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) finalizeBatch(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	batchID, err := uuid.Parse(chi.URLParam(r, "batchID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid batch id", "")
		return
	}
	res, err := h.svc.FinalizeBatch(r.Context(), p.TenantID, p.UserID, batchID, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/agent/v1/jobs/"+res.ID.String())
	httpx.WriteJSON(w, http.StatusAccepted, res)
}

func (h *Handler) getBatchStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	batchID, err := uuid.Parse(chi.URLParam(r, "batchID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid batch id", "")
		return
	}
	status, err := h.svc.GetBatchStatus(r.Context(), p.TenantID, batchID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid job id", "")
		return
	}
	job, err := h.svc.GetJob(r.Context(), p.TenantID, jobID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, job)
}

func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", "queued", "running", "succeeded", "failed":
	default:
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "unknown job status")
		return
	}
	items, err := h.svc.ListJobs(r.Context(), p.TenantID, status, intQuery(r, "limit", 50, 1, 200))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type presignRequest struct {
	AssetID  uuid.UUID `json:"asset_id"`
	Size     int64     `json:"size"`
	PartSize int64     `json:"part_size"`
}

func (h *Handler) presignUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req presignRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.AssetID == uuid.Nil || req.Size <= 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "asset_id and a positive size are required")
		return
	}
	plan, err := h.svc.PresignUpload(r.Context(), p.TenantID, PresignInput{
		AssetID:  req.AssetID,
		Size:     req.Size,
		PartSize: req.PartSize,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, plan)
}

type completeRequest struct {
	UploadID string         `json:"upload_id"`
	Parts    []storage.Part `json:"parts"`
}

func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid asset id", "")
		return
	}
	var req completeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.UploadID != "" {
		if len(req.Parts) == 0 {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "parts are required for multipart uploads")
			return
		}
		for _, part := range req.Parts {
			if part.Number <= 0 || part.ETag == "" {
				httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "each part needs a positive number and an etag")
				return
			}
		}
	}
	asset, err := h.svc.CompleteUpload(r.Context(), p.TenantID, CompleteInput{
		AssetID:  assetID,
		UploadID: req.UploadID,
		Parts:    req.Parts,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, asset)
}

type derivativePresignRequest struct {
	AssetID uuid.UUID `json:"asset_id"`
	Kind    string    `json:"kind"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
}

func (h *Handler) presignDerivative(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req derivativePresignRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.AssetID == uuid.Nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "asset_id is required")
		return
	}
	plan, err := h.svc.PresignDerivative(r.Context(), p.TenantID, DerivativePresignInput{
		AssetID: req.AssetID,
		Kind:    req.Kind,
		Size:    req.Size,
		SHA256:  strings.ToLower(strings.TrimSpace(req.SHA256)),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, plan)
}

type derivativeCompleteRequest struct {
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func (h *Handler) completeDerivative(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid asset id", "")
		return
	}
	var req derivativeCompleteRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.CompleteDerivative(r.Context(), p.TenantID, DerivativeCompleteInput{
		AssetID: assetID,
		Kind:    req.Kind,
		Size:    req.Size,
		SHA256:  strings.ToLower(strings.TrimSpace(req.SHA256)),
		Width:   req.Width,
		Height:  req.Height,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) listAssets(w http.ResponseWriter, r *http.Request) {
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
	limit := intQuery(r, "limit", 50, 1, 200)
	offset := intQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.ListAssets(r.Context(), p.TenantID, projectID, limit, offset)
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

type deriveRequest struct {
	AssetIDs []uuid.UUID `json:"asset_ids"`
	Force    bool        `json:"force"`
}

func (h *Handler) deriveAssets(w http.ResponseWriter, r *http.Request) {
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
	var req deriveRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	res, err := h.svc.EnqueueDerivations(r.Context(), p.TenantID, projectID, DeriveInput{
		AssetIDs: req.AssetIDs,
		Force:    req.Force,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, res)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrProjectNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found", "")
	case errors.Is(err, ErrProjectClosed):
		httpx.WriteProblem(w, r, http.StatusConflict, "project_closed", "Project closed", err.Error())
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "not_found", "Resource not found", "")
	case errors.Is(err, ErrBatchClosed):
		httpx.WriteProblem(w, r, http.StatusConflict, "batch_closed", "Ingest batch is closed", "only open batches accept changes")
	case errors.Is(err, ErrAssetImmutable):
		httpx.WriteProblem(w, r, http.StatusConflict, "asset_immutable", "Asset already stored", "content-addressed assets cannot be re-uploaded")
	case errors.Is(err, ErrSizeMismatch):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "size_mismatch", "File size mismatch", err.Error())
	case errors.Is(err, ErrInvalidFile):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "invalid_file", "Invalid file entry", err.Error())
	case errors.Is(err, ErrUploadNotStarted):
		httpx.WriteProblem(w, r, http.StatusConflict, "upload_not_started", "Upload not started", "request a presigned upload first")
	case errors.Is(err, ErrUploadNotReceived):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "upload_missing", "Uploaded object not found", "")
	case errors.Is(err, ErrDerivativeKind):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "unsupported_derivative", "Unsupported derivative kind", err.Error())
	case errors.Is(err, ErrDerivativeTooLarge):
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "derivative_too_large", "Derivative too large", err.Error())
	case errors.Is(err, ErrDerivativeMissing):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "derivative_missing", "Uploaded derivative not found", "PUT the preview to the presigned URL first")
	case errors.Is(err, ErrDerivativeHash):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "derivative_hash_mismatch", "Derivative hash mismatch", err.Error())
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
