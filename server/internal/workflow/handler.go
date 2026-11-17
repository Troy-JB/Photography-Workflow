package workflow

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

func (h *Handler) Register(r chi.Router) {
	r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Get("/workflow", h.list)
	r.With(auth.RequireAnyRole("owner", "staff", "assistant")).Get("/workbench", h.workbench)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	items, err := h.svc.List(r.Context(), p.TenantID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	stage := strings.TrimSpace(r.URL.Query().Get("stage"))
	if stage != "" {
		filtered := items[:0]
		for _, item := range items {
			if string(item.Stage) == stage {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) workbench(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	items, err := h.svc.List(r.Context(), p.TenantID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}
