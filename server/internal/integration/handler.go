package integration

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

// Handler exposes the operational integration view and the manual scan trigger.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Register(r chi.Router) {
	read := auth.RequireAnyRole("owner", "staff", "assistant", "retoucher")
	owner := auth.RequireAnyRole("owner")
	r.With(read).Get("/integrations", h.status)
	r.With(owner).Post("/integrations/folder-watch/sync", h.sync)
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	status, err := h.svc.Status(r.Context(), p.TenantID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	if h.svc.provider.Name() != ProviderFolderWatch {
		httpx.WriteProblem(w, r, http.StatusConflict, "channel_unavailable",
			"Folder watch is not the active retouch provider", "")
		return
	}
	n, err := h.svc.SyncNow(r.Context(), p.TenantID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"imported": n})
}
