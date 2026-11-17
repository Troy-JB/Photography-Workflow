package lead

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type PublicHandler struct {
	svc     *Service
	tenant  uuid.UUID
	origins map[string]struct{}
	mu      sync.Mutex
	clients map[string]rateWindow
}

type rateWindow struct {
	started time.Time
	count   int
}

func NewPublicHandler(svc *Service, tenant uuid.UUID, origins []string) *PublicHandler {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		origin = strings.TrimRight(strings.TrimSpace(origin), "/")
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	return &PublicHandler{svc: svc, tenant: tenant, origins: allowed, clients: map[string]rateWindow{}}
}

func (h *PublicHandler) Register(r chi.Router) {
	r.HandleFunc("/public/leads", h.submit)
}

func (h *PublicHandler) submit(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
	if _, ok := h.origins[origin]; origin == "" || !ok {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.allow(clientIP(r.RemoteAddr), time.Now()) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Email   string `json:"email"`
		Website string `json:"website"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Website) != "" {
		writeAccepted(w)
		return
	}
	if !validEmail(strings.TrimSpace(req.Email)) {
		http.Error(w, "invalid email", http.StatusUnprocessableEntity)
		return
	}
	if h.tenant == uuid.Nil {
		http.Error(w, "lead intake unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := h.svc.CreatePublic(r.Context(), h.tenant, clientIP(r.RemoteAddr), strings.TrimSpace(req.Email)); err != nil {
		http.Error(w, "lead intake unavailable", http.StatusServiceUnavailable)
		return
	}
	writeAccepted(w)
}

func (h *PublicHandler) allow(ip string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) > 4096 {
		for key, current := range h.clients {
			if now.Sub(current.started) >= 2*time.Minute {
				delete(h.clients, key)
			}
		}
	}
	if _, exists := h.clients[ip]; !exists && len(h.clients) >= 4096 {
		return false
	}
	window := h.clients[ip]
	if now.Sub(window.started) >= time.Minute || window.started.IsZero() {
		window = rateWindow{started: now}
	}
	window.count++
	h.clients[ip] = window
	return window.count <= 5
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func writeAccepted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"accepted":true}`))
}
