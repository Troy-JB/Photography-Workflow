package auth

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

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
	r.Route("/auth", func(r chi.Router) {
		r.Post("/register", h.register)
		r.Post("/login", h.login)
		r.Post("/refresh", h.refresh)
		r.Post("/logout", h.logout)
		r.With(RequireAuth(h.svc.Tokens())).Get("/me", h.me)
	})
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req RegisterInput
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	session, err := h.svc.Register(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidRegistration):
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "Please check the name, email, password and studio name")
		case errors.Is(err, ErrEmailTaken):
			httpx.WriteProblem(w, r, http.StatusConflict, "email_already_registered", "Email already registered", "An account with this email already exists")
		default:
			httpx.Internal(w, r, h.log, err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, session)
}

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	TenantSlug string `json:"tenant_slug"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.Email == "" || req.Password == "" {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "email and password are required")
		return
	}
	session, err := h.svc.Login(r.Context(), req.Email, req.Password, req.TenantSlug)
	if err != nil {
		var selection *TenantSelectionError
		if errors.As(err, &selection) {
			httpx.WriteJSON(w, http.StatusConflict, map[string]any{
				"code":    "tenant_selection_required",
				"tenants": selection.Tenants,
			})
			return
		}
		if errors.Is(err, ErrInvalidCredentials) {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_credentials", "Invalid credentials", "email or password is incorrect")
			return
		}
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, session)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.RefreshToken == "" {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "refresh_token is required")
		return
	}
	session, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrInvalidRefresh) {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token", "")
			return
		}
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, session)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.RefreshToken != "" {
		if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	profile, err := h.svc.Me(r.Context(), p.UserID, p.TenantID)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_membership", "Membership not found", "")
			return
		}
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, profile)
}
