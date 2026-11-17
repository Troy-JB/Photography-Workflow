package device

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

// RegisterAgent mounts the device routes next to the ingest routes on the same
// parent router. Paths are absolute so this package does not need to own the
// /agent/v1 subtree that ingest already mounts.
func (h *Handler) RegisterAgent(r chi.Router) {
	r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/agent/v1/devices", h.register)
	r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Post("/agent/v1/heartbeats", h.heartbeat)
}

type registerRequest struct {
	Name         string `json:"name"`
	Fingerprint  string `json:"fingerprint"`
	Platform     string `json:"platform"`
	AgentVersion string `json:"agent_version"`
	PublicKey    string `json:"public_key"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	d, created, err := h.svc.Register(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, RegisterInput{
		Name:         req.Name,
		Fingerprint:  req.Fingerprint,
		Platform:     req.Platform,
		AgentVersion: req.AgentVersion,
		PublicKey:    req.PublicKey,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, d)
}

type heartbeatRequest struct {
	DeviceID     uuid.UUID `json:"device_id"`
	AgentVersion string    `json:"agent_version"`
}

func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req heartbeatRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.DeviceID == uuid.Nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "device_id is required")
		return
	}
	hb, err := h.svc.Heartbeat(r.Context(), p.TenantID, HeartbeatInput{
		DeviceID:     req.DeviceID,
		AgentVersion: req.AgentVersion,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, hb)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "device_not_found", "Device not found", "")
	case errors.Is(err, ErrDisabled):
		httpx.WriteProblem(w, r, http.StatusForbidden, "device_disabled", "Device is disabled", "an operator must re-enable this device")
	case errors.Is(err, ErrInvalid):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
