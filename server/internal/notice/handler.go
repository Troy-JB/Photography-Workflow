package notice

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	read := auth.RequireAnyRole("owner", "staff", "retoucher", "assistant")
	r.With(read).Get("/notifications", h.list)
	r.With(read).Get("/notifications/stream", h.stream)
	r.With(read).Get("/notifications/{notificationID}", h.get)
	r.With(read).Post("/notifications/{notificationID}/read", h.markRead)
}

func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteProblem(w, r, http.StatusInternalServerError, "stream_unsupported", "Event stream is unavailable", "")
		return
	}
	after := time.Now().UTC()
	afterID := uuid.Nil
	if cursor := strings.TrimSpace(r.Header.Get("Last-Event-ID")); cursor != "" {
		parts := strings.SplitN(cursor, "|", 2)
		if len(parts) == 2 {
			if parsedAt, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
				if parsedID, err := uuid.Parse(parts[1]); err == nil {
					after, afterID = parsedAt, parsedID
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	streamLimit := time.NewTimer(45 * time.Second)
	defer streamLimit.Stop()
	ticks := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-streamLimit.C:
			return
		case <-ticker.C:
			ticks++
			items, err := h.svc.ListAfter(r.Context(), p.TenantID, after, afterID)
			if err != nil {
				h.log.Warn("notification stream read failed", "tenant_id", p.TenantID, "err", err)
				_, _ = fmt.Fprint(w, ": retry\n\n")
				flusher.Flush()
				continue
			}
			if len(items) == 0 {
				if ticks%5 == 0 {
					_, _ = fmt.Fprint(w, "event: refresh\ndata: {}\n\n")
				} else {
					_, _ = fmt.Fprint(w, ": keepalive\n\n")
				}
				flusher.Flush()
				continue
			}
			for _, item := range items {
				data, err := json.Marshal(item)
				if err != nil {
					continue
				}
				after, afterID = item.CreatedAt, item.ID
				_, _ = fmt.Fprintf(w, "id: %s|%s\nevent: notification\ndata: %s\n\n",
					after.UTC().Format(time.RFC3339Nano), afterID, data)
			}
			flusher.Flush()
		}
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var bookingID *uuid.UUID
	if raw := r.URL.Query().Get("booking_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed",
				"Validation failed", "invalid booking_id")
			return
		}
		bookingID = &id
	}
	unread := false
	switch r.URL.Query().Get("unread") {
	case "", "false":
	case "true":
		unread = true
	default:
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed",
			"Validation failed", "unread must be true or false")
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, unreadCount, err := h.svc.List(r.Context(), p.TenantID, Filter{
		Type: r.URL.Query().Get("type"), Status: r.URL.Query().Get("status"),
		BookingID: bookingID, Unread: unread,
	}, limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "unread": unreadCount, "limit": limit, "offset": offset,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := notificationID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid notification id", "")
		return
	}
	n, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := notificationID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid notification id", "")
		return
	}
	n, err := h.svc.MarkRead(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "notification_not_found", "Notification not found", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func notificationID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "notificationID"))
}
