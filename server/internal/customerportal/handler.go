package customerportal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/payment"
	"photography-workflow/server/internal/quote"
	"photography-workflow/server/internal/schedule"
)

type Config struct {
	AppID      string
	AppSecret  string
	DevLogin   bool
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Timezone   *time.Location
	HoldTTL    time.Duration
	PublicBase string
}

type Handler struct {
	pool        *pgxpool.Pool
	log         *slog.Logger
	tokens      *auth.Tokens
	cfg         Config
	wechat      *wechatClient
	publicLimit *requestLimiter
	quotes      *quote.Service
	payments    *payment.Service
}

type requestLimiter struct {
	mu     sync.Mutex
	hits   map[string]requestWindow
	limit  int
	window time.Duration
}

type requestWindow struct {
	started time.Time
	count   int
}

func newRequestLimiter(limit int, window time.Duration) *requestLimiter {
	return &requestLimiter{hits: make(map[string]requestWindow), limit: limit, window: window}
}

func (l *requestLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
		now := time.Now()
		l.mu.Lock()
		hit := l.hits[ip]
		if hit.started.IsZero() || now.Sub(hit.started) >= l.window {
			hit = requestWindow{started: now}
		}
		if hit.count >= l.limit {
			l.hits[ip] = hit
			l.mu.Unlock()
			httpx.WriteProblem(w, r, http.StatusTooManyRequests, "too_many_requests", "Too many requests", "Please wait before retrying")
			return
		}
		hit.count++
		l.hits[ip] = hit
		if len(l.hits) > 10000 {
			for key, current := range l.hits {
				if now.Sub(current.started) >= l.window {
					delete(l.hits, key)
				}
			}
		}
		l.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

type Customer struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Phone string    `json:"phone"`
}

type customerKey struct{}

func CustomerFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(customerKey{}).(uuid.UUID)
	return id, ok
}

func NewHandler(pool *pgxpool.Pool, secret string, cfg Config, log *slog.Logger) *Handler {
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = 2 * time.Hour
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.Timezone == nil {
		cfg.Timezone = time.UTC
	}
	if cfg.HoldTTL <= 0 {
		cfg.HoldTTL = 30 * time.Minute
	}
	return &Handler{pool: pool, log: log, cfg: cfg, tokens: auth.NewTokens(secret, cfg.AccessTTL),
		wechat: newWechatClient(cfg.AppID, cfg.AppSecret), publicLimit: newRequestLimiter(60, time.Minute)}
}

func (h *Handler) SetQuoteService(svc *quote.Service) { h.quotes = svc }

func (h *Handler) SetPaymentService(svc *payment.Service) { h.payments = svc }

func (h *Handler) Register(r chi.Router) {
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/logout", h.logout)
	r.Group(func(r chi.Router) {
		r.Use(h.publicLimit.middleware)
		r.Post("/auth/wechat", h.wechatLogin)
		r.Post("/auth/phone/code", h.requestPhoneCode)
		r.Post("/auth/phone/login", h.phoneLogin)
		r.Post("/invites/resolve", h.invite)
		r.Post("/photographers/resolve", h.photographer)
		r.Post("/invites/slots", h.slots)
	})
	r.Group(func(r chi.Router) {
		r.Use(h.requireCustomer)
		r.Get("/me/studios", h.studios)
		r.Get("/me/projects", h.projects)
		r.Post("/me/quotes/{quoteID}/accept", h.acceptQuote)
		r.Post("/me/projects/{projectID}/payment-orders", h.createPaymentOrder)
		r.Post("/me/projects/{projectID}/payment-orders/{orderID}/sandbox-complete", h.completeSandboxPayment)
		r.Get("/me/bookings", h.bookings)
		r.Post("/bookings", h.createBooking)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(h.tokens))
		r.With(auth.RequireAnyRole("owner", "staff")).Get("/admin/services", h.listServices)
		r.With(auth.RequireAnyRole("owner", "staff")).Post("/admin/services", h.createService)
		r.With(auth.RequireAnyRole("owner", "staff")).Patch("/admin/services/{serviceID}", h.updateService)
		r.With(auth.RequireAnyRole("owner", "staff")).Post("/admin/invites", h.createInvite)
	})
}

type loginRequest struct {
	Code        string `json:"code"`
	PhoneCode   string `json:"phone_code"`
	Phone       string `json:"phone"`
	Name        string `json:"name"`
	InviteToken string `json:"invite_token"`
}

func (h *Handler) requestPhoneCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	phone := normalizePhone(req.Phone)
	if !validPhone(phone) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "valid phone is required")
		return
	}
	if !h.cfg.DevLogin {
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "sms_unavailable", "SMS unavailable", "Phone verification codes cannot be sent until an SMS provider is configured")
		return
	}
	var recentlyIssued bool
	if err := h.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM customer_login_codes WHERE phone=$1 AND created_at > now()-interval '60 seconds')`, phone).Scan(&recentlyIssued); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if recentlyIssued {
		httpx.WriteProblem(w, r, http.StatusTooManyRequests, "code_rate_limited", "Please wait before requesting another code", "A code was already requested for this phone number")
		return
	}
	codeBytes := make([]byte, 4)
	if _, err := rand.Read(codeBytes); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	code := fmt.Sprintf("%06d", (uint32(codeBytes[0])<<24|uint32(codeBytes[1])<<16|uint32(codeBytes[2])<<8|uint32(codeBytes[3]))%1_000_000)
	sum := sha256.Sum256([]byte(phone + ":" + code))
	if _, err := h.pool.Exec(r.Context(), `INSERT INTO customer_login_codes(phone,code_hash,expires_at,attempts) VALUES($1,$2,now()+interval '5 minutes',0) ON CONFLICT(phone) DO UPDATE SET code_hash=EXCLUDED.code_hash, expires_at=EXCLUDED.expires_at, attempts=0, created_at=now()`, phone, sum[:]); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	h.log.Info("customer phone login development code", "phone", maskPhone(phone), "code", code)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"expires_in": 300, "development_code": code})
}

func (h *Handler) phoneLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
		Name  string `json:"name"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	phone := normalizePhone(req.Phone)
	if !validPhone(phone) || len(req.Code) != 6 || len([]rune(strings.TrimSpace(req.Name))) > 120 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "phone, six-digit code and a name of at most 120 characters are required")
		return
	}
	var stored []byte
	var attempts int
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	err = tx.QueryRow(r.Context(), `SELECT code_hash, attempts FROM customer_login_codes WHERE phone=$1 AND expires_at>now() FOR UPDATE`, phone).Scan(&stored, &attempts)
	if err != nil || attempts >= 5 {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_login_code", "Invalid login code", "Request a new code and try again")
		return
	}
	sum := sha256.Sum256([]byte(phone + ":" + req.Code))
	if len(stored) != len(sum) || subtle.ConstantTimeCompare(stored, sum[:]) != 1 {
		_, _ = tx.Exec(r.Context(), `UPDATE customer_login_codes SET attempts=attempts+1 WHERE phone=$1`, phone)
		_ = tx.Commit(r.Context())
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_login_code", "Invalid login code", "Request a new code and try again")
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM customer_login_codes WHERE phone=$1`, phone); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	var customer Customer
	var matches int
	if err := h.pool.QueryRow(r.Context(), `SELECT count(*) FROM customer_accounts WHERE phone=$1 AND phone_verified_at IS NOT NULL`, phone).Scan(&matches); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if matches > 1 {
		httpx.WriteProblem(w, r, http.StatusConflict, "customer_phone_conflict", "Customer account needs attention", "Multiple customer records use this phone; contact the studio to merge them")
		return
	}
	err = h.pool.QueryRow(r.Context(), `SELECT id,name,phone FROM customer_accounts WHERE phone=$1 AND phone_verified_at IS NOT NULL`, phone).Scan(&customer.ID, &customer.Name, &customer.Phone)
	if errors.Is(err, pgx.ErrNoRows) {
		err = h.pool.QueryRow(r.Context(), `INSERT INTO customer_accounts(name,phone,phone_verified_at) VALUES($1,$2,now()) RETURNING id,name,phone`, strings.TrimSpace(req.Name), phone).Scan(&customer.ID, &customer.Name, &customer.Phone)
	}
	if err != nil {
		httpx.Internal(w, r, h.log, fmt.Errorf("load phone customer account: %w", err))
		return
	}
	if strings.TrimSpace(req.Name) != "" && customer.Name != strings.TrimSpace(req.Name) {
		if err := h.pool.QueryRow(r.Context(), `UPDATE customer_accounts SET name=$2,updated_at=now() WHERE id=$1 RETURNING name`, customer.ID, strings.TrimSpace(req.Name)).Scan(&customer.Name); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
	}
	session, err := h.issueSession(r.Context(), customer)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, session)
}

func maskPhone(phone string) string {
	if len(phone) < 5 {
		return "***"
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

type customerSession struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	Customer         Customer  `json:"customer"`
	Role             string    `json:"role"`
}

func (h *Handler) wechatLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "code is required")
		return
	}
	var invite *inviteInfo
	if req.InviteToken != "" {
		var err error
		invite, err = h.resolveInvite(r.Context(), req.InviteToken)
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusNotFound, "invite_not_found", "Booking link unavailable", "")
			return
		}
	}
	var openID, phone string
	if h.cfg.DevLogin && h.cfg.AppID == "" {
		openID, phone = strings.TrimSpace(req.Code), normalizePhone(req.Phone)
		if !strings.HasPrefix(openID, "dev:") || !validPhone(phone) {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "development login requires a dev: code and a valid phone")
			return
		}
	} else {
		if req.PhoneCode == "" {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "phone_authorization_required", "Phone authorization required", "Authorize your phone number in WeChat")
			return
		}
		var err error
		openID, err = h.wechat.exchangeCode(r.Context(), req.Code)
		if err == nil {
			phone, err = h.wechat.exchangePhone(r.Context(), req.PhoneCode)
		}
		if err != nil {
			httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "wechat_login_failed", "WeChat login failed", "Please retry WeChat authorization")
			return
		}
	}
	name := strings.TrimSpace(req.Name)
	if len([]rune(name)) > 120 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "name must be at most 120 characters")
		return
	}
	var customer Customer
	var err error
	if validPhone(phone) {
		// A customer who first signed in with SMS can bind WeChat to the same account.
		err = h.pool.QueryRow(r.Context(), `UPDATE customer_accounts SET wechat_openid=$2,
			name=CASE WHEN $3='' THEN name ELSE $3 END, updated_at=now()
			WHERE phone=$1 AND phone_verified_at IS NOT NULL AND wechat_openid IS NULL
			AND NOT EXISTS (SELECT 1 FROM customer_accounts WHERE wechat_openid=$2)
			RETURNING id,name,phone`, phone, openID, name).Scan(&customer.ID, &customer.Name, &customer.Phone)
	}
	if errors.Is(err, pgx.ErrNoRows) || !validPhone(phone) {
		err = h.pool.QueryRow(r.Context(), `
			INSERT INTO customer_accounts (wechat_openid, name, phone, phone_verified_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (wechat_openid) DO UPDATE SET
				name = CASE WHEN EXCLUDED.name = '' THEN customer_accounts.name ELSE EXCLUDED.name END,
				phone = EXCLUDED.phone, phone_verified_at = now(), updated_at = now()
			RETURNING id, name, phone`, openID, name, phone).Scan(&customer.ID, &customer.Name, &customer.Phone)
	}
	if err != nil {
		httpx.Internal(w, r, h.log, fmt.Errorf("upsert customer account: %w", err))
		return
	}
	if invite != nil {
		err := db.WithTenant(r.Context(), h.pool, invite.TenantID, func(tx pgx.Tx) error {
			var lockedID uuid.UUID
			if err := tx.QueryRow(r.Context(), `SELECT id FROM customer_accounts WHERE id = $1 FOR UPDATE`, customer.ID).Scan(&lockedID); err != nil {
				return err
			}
			_, err := ensureCustomerLink(r.Context(), tx, invite.TenantID, customer)
			return err
		})
		if err != nil {
			if errors.Is(err, errInvalidBooking) {
				httpx.WriteProblem(w, r, http.StatusConflict, "client_link_conflict", "Customer record needs attention", err.Error())
				return
			}
			httpx.Internal(w, r, h.log, fmt.Errorf("bind invited customer: %w", err))
			return
		}
	}
	session, err := h.issueSession(r.Context(), customer)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, session)
}

func (h *Handler) issueSession(ctx context.Context, customer Customer) (*customerSession, error) {
	access, accessExp, err := h.tokens.Issue(customer.ID, uuid.Nil, "client")
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	refresh := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(refresh))
	refreshExp := time.Now().Add(h.cfg.RefreshTTL)
	if _, err := h.pool.Exec(ctx, `INSERT INTO customer_refresh_tokens (customer_id, token_hash, expires_at) VALUES ($1,$2,$3)`, customer.ID, sum[:], refreshExp); err != nil {
		return nil, fmt.Errorf("store customer refresh token: %w", err)
	}
	return &customerSession{AccessToken: access, AccessExpiresAt: accessExp, RefreshToken: refresh,
		RefreshExpiresAt: refreshExp, Customer: customer, Role: "client"}, nil
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.RefreshToken) == "" {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "refresh_token is required")
		return
	}
	sum := sha256.Sum256([]byte(req.RefreshToken))
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	var tokenID uuid.UUID
	var customer Customer
	if err := tx.QueryRow(r.Context(), `
		SELECT rt.id, c.id, c.name, c.phone FROM customer_refresh_tokens rt
		JOIN customer_accounts c ON c.id = rt.customer_id
		WHERE rt.token_hash = $1 AND rt.revoked_at IS NULL AND rt.expires_at > now()
		FOR UPDATE OF rt`, sum[:]).Scan(&tokenID, &customer.ID, &customer.Name, &customer.Phone); err != nil {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_refresh_token", "Invalid refresh token", "")
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE customer_refresh_tokens SET revoked_at = now() WHERE id = $1`, tokenID); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	refresh := hex.EncodeToString(raw)
	refreshHash := sha256.Sum256([]byte(refresh))
	refreshExp := time.Now().Add(h.cfg.RefreshTTL)
	if _, err := tx.Exec(r.Context(), `INSERT INTO customer_refresh_tokens (customer_id, token_hash, expires_at) VALUES ($1,$2,$3)`, customer.ID, refreshHash[:], refreshExp); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	access, accessExp, err := h.tokens.Issue(customer.ID, uuid.Nil, "client")
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, customerSession{AccessToken: access, AccessExpiresAt: accessExp,
		RefreshToken: refresh, RefreshExpiresAt: refreshExp, Customer: customer, Role: "client"})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.RefreshToken != "" {
		sum := sha256.Sum256([]byte(req.RefreshToken))
		if _, err := h.pool.Exec(r.Context(), `UPDATE customer_refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, sum[:]); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) requireCustomer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Customer session required", "")
			return
		}
		claims, err := h.tokens.Parse(strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		if err != nil || claims == nil {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_customer_session", "Invalid customer session", "")
			return
		}
		id, parseErr := uuid.Parse(claims.Subject)
		tenantID, tenantErr := uuid.Parse(claims.TenantID)
		if parseErr != nil || tenantErr != nil || claims.Role != "client" || tenantID != uuid.Nil {
			httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_customer_session", "Invalid customer session", "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), customerKey{}, id)))
	})
}

func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	invite, err := h.resolveInvite(r.Context(), req.Token)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "invite_not_found", "Booking link unavailable", "This photographer link is invalid or expired")
		return
	}
	var studio string
	var services []serviceView
	err = db.WithTenant(r.Context(), h.pool, invite.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT name FROM tenants WHERE id = $1`, invite.TenantID).Scan(&studio); err != nil {
			return err
		}
		var err error
		services, err = queryServices(r.Context(), tx, invite.TenantID, false)
		return err
	})
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"studio_id": invite.TenantID, "studio_name": studio, "services": services})
}

func (h *Handler) photographer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if len(code) < 6 || len(code) > 24 {
		httpx.WriteProblem(w, r, http.StatusNotFound, "photographer_not_found", "Photographer not found", "")
		return
	}
	var info inviteInfo
	var studio string
	err := h.pool.QueryRow(r.Context(), `SELECT t.id,t.name FROM users u JOIN memberships m ON m.user_id=u.id AND m.role='owner' JOIN tenants t ON t.id=m.tenant_id WHERE upper(u.photographer_code)=upper($1) ORDER BY m.created_at LIMIT 1`, code).Scan(&info.TenantID, &studio)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "photographer_not_found", "Photographer not found", "Check the code and try again")
		return
	}
	var services []serviceView
	err = db.WithTenant(r.Context(), h.pool, info.TenantID, func(tx pgx.Tx) error {
		var err error
		services, err = queryServices(r.Context(), tx, info.TenantID, false)
		return err
	})
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"photographer_code": code, "studio_id": info.TenantID, "studio_name": studio, "services": services})
}

type inviteInfo struct {
	ID       uuid.UUID
	TenantID uuid.UUID
}

func (h *Handler) resolveInvite(ctx context.Context, raw string) (*inviteInfo, error) {
	if len(raw) < 24 || len(raw) > 160 {
		return nil, errors.New("invalid invite")
	}
	sum := sha256.Sum256([]byte(raw))
	var info inviteInfo
	if err := h.pool.QueryRow(ctx, `SELECT invite_id, tenant_id FROM customer_invite_lookup($1)`, hex.EncodeToString(sum[:])).Scan(&info.ID, &info.TenantID); err != nil {
		return nil, err
	}
	return &info, nil
}

type serviceView struct {
	ID                         uuid.UUID `json:"id"`
	ResourceID                 uuid.UUID `json:"resource_id"`
	Name                       string    `json:"name"`
	Description                string    `json:"description"`
	DurationMinutes            int       `json:"duration_minutes"`
	PriceCents                 int64     `json:"price_cents"`
	IncludedRetouchCount       int       `json:"included_retouch_count"`
	ExtraRetouchUnitPriceCents int64     `json:"extra_retouch_unit_price_cents"`
	Active                     bool      `json:"active"`
	Version                    int       `json:"version"`
}

func queryServices(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, includeInactive bool) ([]serviceView, error) {
	rows, err := tx.Query(ctx, `SELECT id, resource_id, name, description, duration_minutes, price_cents, included_retouch_count, extra_retouch_unit_price_cents, active, version
		FROM booking_services WHERE tenant_id = $1 AND ($2 OR active) ORDER BY name`, tenantID, includeInactive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []serviceView{}
	for rows.Next() {
		var item serviceView
		if err := rows.Scan(&item.ID, &item.ResourceID, &item.Name, &item.Description, &item.DurationMinutes,
			&item.PriceCents, &item.IncludedRetouchCount, &item.ExtraRetouchUnitPriceCents, &item.Active, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (h *Handler) slots(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InviteToken      string    `json:"invite_token"`
		PhotographerCode string    `json:"photographer_code"`
		ServiceID        uuid.UUID `json:"service_id"`
		Day              string    `json:"day"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	invite, err := h.resolveBookingTarget(r.Context(), req.InviteToken, req.PhotographerCode)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "invite_not_found", "Booking link unavailable", "")
		return
	}
	serviceID := req.ServiceID
	if serviceID == uuid.Nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "service_id is required")
		return
	}
	day := req.Day
	localStart, err := time.ParseInLocation("2006-01-02", day, h.cfg.Timezone)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "day must be YYYY-MM-DD")
		return
	}
	if localStart.Before(time.Now().In(h.cfg.Timezone).AddDate(0, 0, -1)) || localStart.After(time.Now().In(h.cfg.Timezone).AddDate(0, 0, 90)) {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "day must be within the next 90 days")
		return
	}
	var slots []map[string]time.Time
	err = db.WithTenant(r.Context(), h.pool, invite.TenantID, func(tx pgx.Tx) error {
		var resourceID uuid.UUID
		var duration int
		var before, after int
		if err := tx.QueryRow(r.Context(), `SELECT s.resource_id, s.duration_minutes, r.buffer_before_minutes, r.buffer_after_minutes
			FROM booking_services s JOIN resources r ON r.id = s.resource_id AND r.tenant_id = s.tenant_id
			WHERE s.tenant_id = $1 AND s.id = $2 AND s.active AND r.active`, invite.TenantID, serviceID).
			Scan(&resourceID, &duration, &before, &after); err != nil {
			return err
		}
		var rules []schedule.AvailabilityRule
		rows, err := tx.Query(r.Context(), `SELECT id, weekday, start_minute, end_minute FROM availability_rules
			WHERE tenant_id = $1 AND resource_id = $2 ORDER BY weekday, start_minute`, invite.TenantID, resourceID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var rule schedule.AvailabilityRule
			if err := rows.Scan(&rule.ID, &rule.Weekday, &rule.StartMinute, &rule.EndMinute); err != nil {
				rows.Close()
				return err
			}
			rules = append(rules, rule)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		start := localStart
		end := localStart.AddDate(0, 0, 1)
		busyRows, err := tx.Query(r.Context(), `SELECT lower(during), upper(during) FROM resource_blocks
			WHERE tenant_id = $1 AND resource_id = $2 AND during && tstzrange($3, $4, '[)')
			AND (kind <> 'hold' OR expires_at > now())`, invite.TenantID, resourceID,
			start.Add(-time.Duration(before)*time.Minute), end.Add(24*time.Hour+time.Duration(after)*time.Minute))
		if err != nil {
			return err
		}
		var busy [][2]time.Time
		for busyRows.Next() {
			var a, b time.Time
			if err := busyRows.Scan(&a, &b); err != nil {
				busyRows.Close()
				return err
			}
			busy = append(busy, [2]time.Time{a, b})
		}
		if err := busyRows.Err(); err != nil {
			busyRows.Close()
			return err
		}
		busyRows.Close()
		for t := start; t.Before(end); t = t.Add(30 * time.Minute) {
			e := t.Add(time.Duration(duration) * time.Minute)
			if !t.After(time.Now()) || !schedule.WithinAvailability(rules, t, e, h.cfg.Timezone) {
				continue
			}
			blockStart, blockEnd := schedule.BlockRange(t, e, before, after)
			blocked := false
			for _, span := range busy {
				if blockStart.Before(span[1]) && span[0].Before(blockEnd) {
					blocked = true
					break
				}
			}
			if !blocked {
				slots = append(slots, map[string]time.Time{"starts_at": t, "ends_at": e})
			}
		}
		if slots == nil {
			slots = []map[string]time.Time{}
		}
		return nil
	})
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "service_not_found", "Service not found", "")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"day": day, "timezone": h.cfg.Timezone.String(), "slots": slots})
}

type createBookingRequest struct {
	InviteToken      string    `json:"invite_token"`
	PhotographerCode string    `json:"photographer_code"`
	ServiceID        uuid.UUID `json:"service_id"`
	RequestID        string    `json:"request_id"`
	StartsAt         time.Time `json:"starts_at"`
	Note             string    `json:"note"`
}

type customerBooking struct {
	ID            uuid.UUID `json:"id"`
	ProjectID     uuid.UUID `json:"project_id"`
	ProjectTitle  string    `json:"project_title"`
	ServiceName   string    `json:"service_name"`
	Status        string    `json:"status"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`
	HoldExpiresAt time.Time `json:"hold_expires_at"`
}

func (h *Handler) createBooking(w http.ResponseWriter, r *http.Request) {
	customerID, _ := CustomerFrom(r.Context())
	var req createBookingRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if len(req.Note) > 2000 || req.ServiceID == uuid.Nil || req.StartsAt.IsZero() || len(req.RequestID) < 16 || len(req.RequestID) > 80 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "service_id, starts_at and a request_id between 16 and 80 characters are required")
		return
	}
	invite, err := h.resolveBookingTarget(r.Context(), req.InviteToken, req.PhotographerCode)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "invite_not_found", "Booking link unavailable", "")
		return
	}
	var customer Customer
	if err := h.pool.QueryRow(r.Context(), `SELECT id, name, phone FROM customer_accounts WHERE id = $1 AND phone_verified_at IS NOT NULL`, customerID).
		Scan(&customer.ID, &customer.Name, &customer.Phone); err != nil {
		httpx.WriteProblem(w, r, http.StatusForbidden, "phone_required", "Verified phone required", "Authorize your phone number before booking")
		return
	}
	booking, err := h.createHold(r.Context(), invite.TenantID, customer, req)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23P01" || pgErr.Code == "23505") {
			httpx.WriteProblem(w, r, http.StatusConflict, "slot_taken", "Time slot is no longer available", "Choose another time")
			return
		}
		if errors.Is(err, errInvalidBooking) {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
			return
		}
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, booking)
}

func (h *Handler) resolveBookingTarget(ctx context.Context, token, code string) (*inviteInfo, error) {
	if strings.TrimSpace(code) == "" {
		return h.resolveInvite(ctx, token)
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	var info inviteInfo
	err := h.pool.QueryRow(ctx, `SELECT t.id FROM users u JOIN memberships m ON m.user_id=u.id AND m.role='owner' JOIN tenants t ON t.id=m.tenant_id WHERE upper(u.photographer_code)=upper($1) ORDER BY m.created_at LIMIT 1`, code).Scan(&info.TenantID)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

var errInvalidBooking = errors.New("invalid customer booking")

func (h *Handler) createHold(ctx context.Context, tenantID uuid.UUID, customer Customer, req createBookingRequest) (*customerBooking, error) {
	var result customerBooking
	err := db.WithTenant(ctx, h.pool, tenantID, func(tx pgx.Tx) error {
		var lockedCustomerID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM customer_accounts WHERE id = $1 FOR UPDATE`, customer.ID).Scan(&lockedCustomerID); err != nil {
			return err
		}
		var prior customerBooking
		err := tx.QueryRow(ctx, `SELECT b.id, b.project_id, p.title, s.name, b.status, b.starts_at, b.ends_at, b.hold_expires_at
			FROM bookings b JOIN projects p ON p.tenant_id = b.tenant_id AND p.id = b.project_id
			JOIN booking_services s ON s.id = b.service_id
			WHERE b.tenant_id = $1 AND b.customer_id = $2 AND b.customer_request_id = $3`,
			tenantID, customer.ID, req.RequestID).Scan(&prior.ID, &prior.ProjectID, &prior.ProjectTitle, &prior.ServiceName,
			&prior.Status, &prior.StartsAt, &prior.EndsAt, &prior.HoldExpiresAt)
		if err == nil {
			result = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var service serviceView
		var before, after int
		if err := tx.QueryRow(ctx, `SELECT s.id, s.resource_id, s.name, s.duration_minutes, s.price_cents, s.included_retouch_count, s.extra_retouch_unit_price_cents,
			r.buffer_before_minutes, r.buffer_after_minutes FROM booking_services s
			JOIN resources r ON r.id = s.resource_id AND r.tenant_id = s.tenant_id
			WHERE s.tenant_id = $1 AND s.id = $2 AND s.active AND r.active`, tenantID, req.ServiceID).
			Scan(&service.ID, &service.ResourceID, &service.Name, &service.DurationMinutes, &service.PriceCents, &service.IncludedRetouchCount, &service.ExtraRetouchUnitPriceCents, &before, &after); err != nil {
			return err
		}
		end := req.StartsAt.Add(time.Duration(service.DurationMinutes) * time.Minute)
		if err := schedule.ValidateWindow(req.StartsAt, end, h.cfg.Timezone); err != nil {
			return fmt.Errorf("%w: %v", errInvalidBooking, err)
		}
		now := time.Now().In(h.cfg.Timezone)
		if !req.StartsAt.After(now) || req.StartsAt.After(now.AddDate(0, 0, 90)) {
			return fmt.Errorf("%w: starts_at must be in the next 90 days", errInvalidBooking)
		}
		var rules []schedule.AvailabilityRule
		rows, err := tx.Query(ctx, `SELECT id, weekday, start_minute, end_minute FROM availability_rules WHERE tenant_id = $1 AND resource_id = $2`, tenantID, service.ResourceID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var rule schedule.AvailabilityRule
			if err := rows.Scan(&rule.ID, &rule.Weekday, &rule.StartMinute, &rule.EndMinute); err != nil {
				rows.Close()
				return err
			}
			rules = append(rules, rule)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if !schedule.WithinAvailability(rules, req.StartsAt, end, h.cfg.Timezone) {
			return fmt.Errorf("%w: selected time is outside availability", errInvalidBooking)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM resource_blocks WHERE tenant_id=$1 AND resource_id=$2 AND kind='hold' AND expires_at <= now()
			AND during && tstzrange($3,$4,'[)')`, tenantID, service.ResourceID,
			req.StartsAt.Add(-time.Duration(before)*time.Minute), end.Add(time.Duration(after)*time.Minute)); err != nil {
			return err
		}
		clientID, err := ensureCustomerLink(ctx, tx, tenantID, customer)
		if err != nil {
			return err
		}
		projectID := uuid.New()
		bookingID := uuid.New()
		code := "WX-" + strings.ToUpper(strings.ReplaceAll(projectID.String()[:8], "-", ""))
		title := customer.Name + " - " + service.Name
		if _, err := tx.Exec(ctx, `INSERT INTO projects (id, tenant_id, code, title, client_id, state, currency)
			VALUES ($1,$2,$3,$4,$5,'draft','CNY')`, projectID, tenantID, code, title, clientID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_state_logs (tenant_id, project_id, from_state, to_state) VALUES ($1,$2,'','draft')`, tenantID, projectID); err != nil {
			return err
		}
		expires := time.Now().UTC().Add(h.cfg.HoldTTL)
		if _, err := tx.Exec(ctx, `INSERT INTO bookings (id, tenant_id, project_id, customer_id, customer_request_id, service_id,
			status, starts_at, ends_at, note, hold_expires_at, included_retouch_count, extra_retouch_unit_price_cents)
			VALUES ($1,$2,$3,$4,$5,$6,'held',$7,$8,$9,$10,$11,$12)`, bookingID, tenantID, projectID, customer.ID,
			req.RequestID, service.ID, req.StartsAt, end, strings.TrimSpace(req.Note), expires, service.IncludedRetouchCount, service.ExtraRetouchUnitPriceCents); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO booking_resources (tenant_id, booking_id, resource_id) VALUES ($1,$2,$3)`, tenantID, bookingID, service.ResourceID); err != nil {
			return err
		}
		blockStart, blockEnd := schedule.BlockRange(req.StartsAt, end, before, after)
		if _, err := tx.Exec(ctx, `INSERT INTO resource_blocks (tenant_id, resource_id, booking_id, kind, during, expires_at, note)
			VALUES ($1,$2,$3,'hold',tstzrange($4,$5,'[)'),$6,'customer miniapp booking')`, tenantID, service.ResourceID,
			bookingID, blockStart, blockEnd, expires); err != nil {
			return err
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "booking.held", "booking", bookingID.String(), "",
			map[string]any{"project_id": projectID, "customer_id": customer.ID, "service_id": service.ID, "starts_at": req.StartsAt.UTC()}); err != nil {
			return err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "booking.held",
			map[string]any{"project_id": projectID, "customer_id": customer.ID, "source": "customer_miniapp"}); err != nil {
			return err
		}
		if err := notice.EmitWorkflowEvent(ctx, tx, tenantID, projectID,
			"workflow:booking:"+bookingID.String(), "客户提交了预约", "客户已选择服务和拍摄时间。",
			map[string]any{"project_id": projectID, "booking_id": bookingID, "workflow_stage": "not_shot", "event": "booking_created"}); err != nil {
			return err
		}
		result = customerBooking{ID: bookingID, ProjectID: projectID, ProjectTitle: title, ServiceName: service.Name,
			Status: "held", StartsAt: req.StartsAt, EndsAt: end, HoldExpiresAt: expires}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func ensureCustomerLink(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, customer Customer) (uuid.UUID, error) {
	var clientID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT client_id FROM customer_studio_links WHERE tenant_id = $1 AND customer_id = $2`, tenantID, customer.ID).Scan(&clientID)
	if err == nil {
		return clientID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM clients WHERE tenant_id = $1 AND phone = $2`, tenantID, customer.Phone).Scan(&count); err != nil {
		return uuid.Nil, err
	}
	if count > 1 {
		return uuid.Nil, fmt.Errorf("%w: phone matches multiple client records; ask the studio to merge them", errInvalidBooking)
	}
	if count == 1 {
		if err := tx.QueryRow(ctx, `SELECT id FROM clients WHERE tenant_id = $1 AND phone = $2`, tenantID, customer.Phone).Scan(&clientID); err != nil {
			return uuid.Nil, err
		}
	} else if err := tx.QueryRow(ctx, `INSERT INTO clients (tenant_id, name, phone, source) VALUES ($1,$2,$3,'wechat_miniapp') RETURNING id`,
		tenantID, customer.Name, customer.Phone).Scan(&clientID); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO customer_studio_links (tenant_id, customer_id, client_id) VALUES ($1,$2,$3)`, tenantID, customer.ID, clientID); err != nil {
		return uuid.Nil, err
	}
	return clientID, nil
}

func (h *Handler) studios(w http.ResponseWriter, r *http.Request) {
	id, _ := CustomerFrom(r.Context())
	rows, err := h.pool.Query(r.Context(), `SELECT tenant_id, tenant_name, client_id FROM customer_studio_list($1)`, id)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var tenantID, clientID uuid.UUID
		var name string
		if err := rows.Scan(&tenantID, &name, &clientID); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
		items = append(items, map[string]any{"tenant_id": tenantID, "studio_name": name, "client_id": clientID})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) projects(w http.ResponseWriter, r *http.Request) {
	id, _ := CustomerFrom(r.Context())
	rows, err := h.pool.Query(r.Context(), `SELECT tenant_id, tenant_name, client_id FROM customer_studio_list($1)`, id)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	defer rows.Close()
	type item struct {
		ID               uuid.UUID             `json:"id"`
		TenantID         uuid.UUID             `json:"tenant_id"`
		Studio           string                `json:"studio_name"`
		Title            string                `json:"title"`
		State            string                `json:"state"`
		StartsAt         *time.Time            `json:"shoot_start_at,omitempty"`
		EndsAt           *time.Time            `json:"shoot_end_at,omitempty"`
		CreatedAt        time.Time             `json:"created_at"`
		PaymentStatus    string                `json:"payment_status"`
		Currency         string                `json:"currency"`
		TotalAmountCents int64                 `json:"total_amount_cents"`
		DepositCents     int64                 `json:"deposit_cents"`
		BalanceCents     int64                 `json:"balance_cents"`
		LiveBalanceOrder *customerPaymentOrder `json:"live_balance_order,omitempty"`
		Quotes           []customerQuote       `json:"quotes"`
	}
	items := []item{}
	for rows.Next() {
		var tenantID, clientID uuid.UUID
		var studio string
		if err := rows.Scan(&tenantID, &studio, &clientID); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
		if err := db.WithTenant(r.Context(), h.pool, tenantID, func(tx pgx.Tx) error {
			pr, err := tx.Query(r.Context(), `SELECT p.id,
				CASE
					WHEN p.state IN ('cancelled','archived') THEN p.state
					WHEN EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
					 AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
					THEN 'cancelled'
					ELSE p.state
				END,
				p.title, p.shoot_start_at, p.shoot_end_at, p.created_at,
				p.payment_status, p.currency, p.total_amount_cents, p.deposit_cents, p.balance_cents
				FROM projects p WHERE p.tenant_id = $1 AND p.client_id = $2 ORDER BY p.created_at DESC LIMIT 100`, tenantID, clientID)
			if err != nil {
				return err
			}
			projectItems := []item{}
			for pr.Next() {
				var out item
				out.TenantID, out.Studio = tenantID, studio
				if err := pr.Scan(&out.ID, &out.State, &out.Title, &out.StartsAt, &out.EndsAt, &out.CreatedAt,
					&out.PaymentStatus, &out.Currency, &out.TotalAmountCents, &out.DepositCents, &out.BalanceCents); err != nil {
					pr.Close()
					return err
				}
				projectItems = append(projectItems, out)
			}
			if err := pr.Err(); err != nil {
				pr.Close()
				return err
			}
			pr.Close()
			projectIDs := make([]uuid.UUID, len(projectItems))
			for i := range projectItems {
				projectIDs[i] = projectItems[i].ID
			}
			quotesByProject, err := loadCustomerQuotes(r.Context(), tx, tenantID, projectIDs)
			if err != nil {
				return fmt.Errorf("load customer project quotes: %w", err)
			}
			for i := range projectItems {
				projectItems[i].Quotes = quotesByProject[projectItems[i].ID]
				var order customerPaymentOrder
				err := tx.QueryRow(r.Context(), `SELECT id,kind,amount_cents,pay_url FROM payment_orders
					WHERE tenant_id=$1 AND project_id=$2 AND kind='balance' AND status='pending' ORDER BY created_at DESC LIMIT 1`, tenantID, projectItems[i].ID).
					Scan(&order.ID, &order.Kind, &order.AmountCents, &order.PayURL)
				if err == nil {
					projectItems[i].LiveBalanceOrder = &order
				} else if !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			items = append(items, projectItems...)
			return nil
		}); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type customerQuote struct {
	ID           uuid.UUID           `json:"id"`
	ProjectID    uuid.UUID           `json:"project_id"`
	Version      int                 `json:"version"`
	Status       string              `json:"status"`
	Currency     string              `json:"currency"`
	TotalCents   int64               `json:"total_cents"`
	DepositCents int64               `json:"deposit_cents"`
	ValidUntil   *time.Time          `json:"valid_until,omitempty"`
	Terms        string              `json:"terms"`
	RowVersion   int                 `json:"row_version"`
	SentAt       *time.Time          `json:"sent_at,omitempty"`
	AcceptedAt   *time.Time          `json:"accepted_at,omitempty"`
	Items        []customerQuoteItem `json:"items"`
}

type customerPaymentOrder struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"`
	AmountCents int64     `json:"amount_cents"`
	PayURL      string    `json:"pay_url"`
}

type customerQuoteItem struct {
	Kind           string `json:"kind"`
	Description    string `json:"description"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	AmountCents    int64  `json:"amount_cents"`
}

func loadCustomerQuotes(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, projectIDs []uuid.UUID) (map[uuid.UUID][]customerQuote, error) {
	quotesByProject := make(map[uuid.UUID][]customerQuote, len(projectIDs))
	if len(projectIDs) == 0 {
		return quotesByProject, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT q.id, q.project_id, q.version, q.status, q.currency, q.total_cents,
			q.deposit_cents, q.valid_until, q.terms, q.row_version, q.sent_at, q.accepted_at,
			qi.kind, qi.description, qi.quantity, qi.unit_price_cents, qi.amount_cents
		FROM quotes q
		JOIN quote_items qi ON qi.tenant_id = q.tenant_id AND qi.quote_id = q.id
		WHERE q.tenant_id = $1 AND q.project_id = ANY($2::uuid[]) AND q.status IN ('sent','accepted')
		ORDER BY q.project_id, q.version DESC, qi.sort, qi.id`, tenantID, projectIDs)
	if err != nil {
		return nil, err
	}
	quoteIndexes := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			item      customerQuote
			quoteItem customerQuoteItem
		)
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.Version, &item.Status, &item.Currency,
			&item.TotalCents, &item.DepositCents, &item.ValidUntil, &item.Terms, &item.RowVersion,
			&item.SentAt, &item.AcceptedAt, &quoteItem.Kind, &quoteItem.Description,
			&quoteItem.Quantity, &quoteItem.UnitPriceCents, &quoteItem.AmountCents); err != nil {
			rows.Close()
			return nil, err
		}
		index, ok := quoteIndexes[item.ID]
		if !ok {
			item.Items = []customerQuoteItem{}
			quotesByProject[item.ProjectID] = append(quotesByProject[item.ProjectID], item)
			index = len(quotesByProject[item.ProjectID]) - 1
			quoteIndexes[item.ID] = index
		}
		quotes := quotesByProject[item.ProjectID]
		quotes[index].Items = append(quotes[index].Items, quoteItem)
		quotesByProject[item.ProjectID] = quotes
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return quotesByProject, nil
}

func (h *Handler) acceptQuote(w http.ResponseWriter, r *http.Request) {
	if h.quotes == nil {
		httpx.Internal(w, r, h.log, errors.New("quote service is unavailable"))
		return
	}
	customerID, _ := CustomerFrom(r.Context())
	quoteID, err := uuid.Parse(chi.URLParam(r, "quoteID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid quote id", "")
		return
	}
	var req struct {
		ExpectedVersion int `json:"expected_version"`
	}
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	rows, err := h.pool.Query(r.Context(), `SELECT tenant_id FROM customer_studio_list($1)`, customerID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	var tenantIDs []uuid.UUID
	for rows.Next() {
		var tenantID uuid.UUID
		if err := rows.Scan(&tenantID); err != nil {
			rows.Close()
			httpx.Internal(w, r, h.log, err)
			return
		}
		tenantIDs = append(tenantIDs, tenantID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpx.Internal(w, r, h.log, err)
		return
	}
	rows.Close()
	var accepted *quote.Quote
	for _, tenantID := range tenantIDs {
		accepted, err = h.quotes.AcceptForCustomer(r.Context(), tenantID, customerID, quoteID, r.RemoteAddr, req.ExpectedVersion)
		if !errors.Is(err, quote.ErrNotFound) {
			break
		}
	}
	if accepted == nil && err == nil {
		err = quote.ErrNotFound
	}
	q := accepted
	if err != nil {
		switch {
		case errors.Is(err, quote.ErrNotFound):
			httpx.WriteProblem(w, r, http.StatusNotFound, "quote_not_found", "Quote not found", "")
		case errors.Is(err, quote.ErrVersionConflict):
			httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "reload the quote and retry")
		case errors.Is(err, quote.ErrExpired):
			httpx.WriteProblem(w, r, http.StatusConflict, "quote_expired", "Quote expired", "contact the photographer for a new quote")
		case errors.Is(err, quote.ErrProjectClosed):
			httpx.WriteProblem(w, r, http.StatusConflict, "project_cancelled", "Project cancelled", "this project can no longer accept its quote")
		case errors.Is(err, quote.ErrIllegalState):
			httpx.WriteProblem(w, r, http.StatusConflict, "illegal_state", "Quote cannot be accepted", err.Error())
		case errors.Is(err, quote.ErrInvalidInput):
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
		default:
			httpx.Internal(w, r, h.log, err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, q)
}

type customerPaymentOrderRequest struct {
	Kind string `json:"kind"`
}

func (h *Handler) createPaymentOrder(w http.ResponseWriter, r *http.Request) {
	if h.payments == nil {
		httpx.Internal(w, r, h.log, errors.New("payment service is unavailable"))
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
		return
	}
	var req customerPaymentOrderRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if req.Kind != payment.KindDeposit && req.Kind != payment.KindBalance && req.Kind != payment.KindFull {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "kind must be deposit, balance or full")
		return
	}
	customerID, _ := CustomerFrom(r.Context())
	tenantID, err := h.customerProjectTenant(r.Context(), customerID, projectID)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found for customer", "")
		return
	}
	var accepted bool
	err = db.WithTenant(r.Context(), h.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT EXISTS (
			SELECT 1 FROM quotes WHERE tenant_id = $1 AND project_id = $2 AND status = 'accepted')`,
			tenantID, projectID).Scan(&accepted)
	})
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	if !accepted {
		httpx.WriteProblem(w, r, http.StatusConflict, "quote_not_accepted", "Quote must be accepted first", "")
		return
	}
	order, created, err := h.payments.CreateOrder(r.Context(), tenantID, uuid.Nil, r.RemoteAddr,
		payment.CreateOrderInput{ProjectID: projectID, Kind: req.Kind})
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusConflict, "payment_order_failed", "Payment order could not be created", err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, order)
}

func (h *Handler) completeSandboxPayment(w http.ResponseWriter, r *http.Request) {
	if h.payments == nil {
		httpx.Internal(w, r, h.log, errors.New("payment service is unavailable"))
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid project id", "")
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid payment order id", "")
		return
	}
	customerID, _ := CustomerFrom(r.Context())
	tenantID, err := h.customerProjectTenant(r.Context(), customerID, projectID)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusNotFound, "project_not_found", "Project not found for customer", "")
		return
	}
	order, err := h.payments.GetOrder(r.Context(), tenantID, orderID)
	if err != nil || order.ProjectID != projectID {
		httpx.WriteProblem(w, r, http.StatusNotFound, "payment_order_not_found", "Payment order not found", "")
		return
	}
	result, err := h.payments.CompleteSandboxOrder(r.Context(), tenantID, orderID)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusConflict, "sandbox_payment_failed", "Sandbox payment could not be completed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) customerProjectTenant(ctx context.Context, customerID, projectID uuid.UUID) (uuid.UUID, error) {
	rows, err := h.pool.Query(ctx, `SELECT tenant_id, client_id FROM customer_studio_list($1)`, customerID)
	if err != nil {
		return uuid.Nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tenantID, clientID uuid.UUID
		if err := rows.Scan(&tenantID, &clientID); err != nil {
			return uuid.Nil, err
		}
		var belongs bool
		err := db.WithTenant(ctx, h.pool, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2 AND client_id = $3)`,
				tenantID, projectID, clientID).Scan(&belongs)
		})
		if err != nil {
			return uuid.Nil, err
		}
		if belongs {
			return tenantID, nil
		}
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	return uuid.Nil, errors.New("project is not linked to customer")
}

func (h *Handler) bookings(w http.ResponseWriter, r *http.Request) {
	id, _ := CustomerFrom(r.Context())
	rows, err := h.pool.Query(r.Context(), `SELECT tenant_id, tenant_name, client_id FROM customer_studio_list($1)`, id)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var tenantID, clientID uuid.UUID
		var studio string
		if err := rows.Scan(&tenantID, &studio, &clientID); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
		if err := db.WithTenant(r.Context(), h.pool, tenantID, func(tx pgx.Tx) error {
			br, err := tx.Query(r.Context(), `SELECT b.id, b.project_id, b.status, b.starts_at, b.ends_at, b.hold_expires_at, p.title
				FROM bookings b JOIN projects p ON p.id = b.project_id AND p.tenant_id = b.tenant_id
				WHERE b.tenant_id = $1 AND p.client_id = $2 ORDER BY b.starts_at DESC LIMIT 100`, tenantID, clientID)
			if err != nil {
				return err
			}
			defer br.Close()
			for br.Next() {
				var id, projectID uuid.UUID
				var status, title string
				var starts, ends time.Time
				var expires *time.Time
				if err := br.Scan(&id, &projectID, &status, &starts, &ends, &expires, &title); err != nil {
					return err
				}
				items = append(items, map[string]any{"id": id, "project_id": projectID, "status": status,
					"starts_at": starts, "ends_at": ends, "hold_expires_at": expires, "project_title": title, "studio_name": studio, "tenant_id": tenantID})
			}
			return br.Err()
		}); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type serviceRequest struct {
	ResourceID                 uuid.UUID `json:"resource_id"`
	Name                       string    `json:"name"`
	Description                string    `json:"description"`
	DurationMinutes            int       `json:"duration_minutes"`
	PriceCents                 int64     `json:"price_cents"`
	IncludedRetouchCount       int       `json:"included_retouch_count"`
	ExtraRetouchUnitPriceCents int64     `json:"extra_retouch_unit_price_cents"`
	Active                     *bool     `json:"active"`
	ExpectedVersion            int       `json:"expected_version"`
}

func (h *Handler) listServices(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var items []serviceView
	err := db.WithTenant(r.Context(), h.pool, p.TenantID, func(tx pgx.Tx) error {
		var err error
		items, err = queryServices(r.Context(), tx, p.TenantID, true)
		return err
	})
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createService(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var req serviceRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	if req.Name == "" || len([]rune(req.Name)) > 120 || len(req.Description) > 2000 || req.DurationMinutes < 15 || req.DurationMinutes > 1440 || req.PriceCents < 0 || req.ResourceID == uuid.Nil || req.IncludedRetouchCount < 0 || req.IncludedRetouchCount > 10000 || req.ExtraRetouchUnitPriceCents < 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "name, resource_id, duration_minutes and a non-negative price are required")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	var out serviceView
	err := db.WithTenant(r.Context(), h.pool, p.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `INSERT INTO booking_services (tenant_id, resource_id, name, description, duration_minutes, price_cents, included_retouch_count, extra_retouch_unit_price_cents, active)
			SELECT $1, r.id, $3, $4, $5, $6, $7, $8, $9 FROM resources r WHERE r.tenant_id = $1 AND r.id = $2 AND r.kind = 'photographer' AND r.active
			RETURNING id, resource_id, name, description, duration_minutes, price_cents, included_retouch_count, extra_retouch_unit_price_cents, active, version`,
			p.TenantID, req.ResourceID, req.Name, req.Description, req.DurationMinutes, req.PriceCents, req.IncludedRetouchCount, req.ExtraRetouchUnitPriceCents, active).
			Scan(&out.ID, &out.ResourceID, &out.Name, &out.Description, &out.DurationMinutes, &out.PriceCents, &out.IncludedRetouchCount, &out.ExtraRetouchUnitPriceCents, &out.Active, &out.Version)
	})
	if err != nil {
		if pgErrCode(err) == "23505" {
			httpx.WriteProblem(w, r, http.StatusConflict, "service_name_conflict", "Service name already exists", "Choose a different name")
		} else {
			httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "The photographer resource is invalid")
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (h *Handler) updateService(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "serviceID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid service id", "")
		return
	}
	var req serviceRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" || len([]rune(strings.TrimSpace(req.Name))) > 120 || len(strings.TrimSpace(req.Description)) > 2000 || req.DurationMinutes < 15 || req.DurationMinutes > 1440 || req.PriceCents < 0 || req.IncludedRetouchCount < 0 || req.IncludedRetouchCount > 10000 || req.ExtraRetouchUnitPriceCents < 0 || req.ExpectedVersion <= 0 {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "name, duration, price and expected_version are required")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	var out serviceView
	err = db.WithTenant(r.Context(), h.pool, p.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `UPDATE booking_services SET name=$3, description=$4, duration_minutes=$5,
			price_cents=$6, included_retouch_count=$7, extra_retouch_unit_price_cents=$8, active=$9, version=version+1, updated_at=now()
			WHERE tenant_id=$1 AND id=$2 AND version=$10
			RETURNING id, resource_id, name, description, duration_minutes, price_cents, included_retouch_count, extra_retouch_unit_price_cents, active, version`,
			p.TenantID, id, strings.TrimSpace(req.Name), strings.TrimSpace(req.Description), req.DurationMinutes,
			req.PriceCents, req.IncludedRetouchCount, req.ExtraRetouchUnitPriceCents, active, req.ExpectedVersion).
			Scan(&out.ID, &out.ResourceID, &out.Name, &out.Description, &out.DurationMinutes, &out.PriceCents, &out.IncludedRetouchCount, &out.ExtraRetouchUnitPriceCents, &out.Active, &out.Version)
	})
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Service changed", "Reload the service and retry")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := time.Now().Add(60 * 24 * time.Hour)
	var id uuid.UUID
	err := db.WithTenant(r.Context(), h.pool, p.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `INSERT INTO customer_invites (tenant_id, token_hash, expires_at, created_by)
			VALUES ($1,$2,$3,$4) RETURNING id`, p.TenantID, hex.EncodeToString(sum[:]), expires, p.UserID).Scan(&id); err != nil {
			return err
		}
		return audit.Write(r.Context(), tx, p.TenantID, &p.UserID, "customer.invite_created", "customer_invite", id.String(), r.RemoteAddr,
			map[string]any{"expires_at": expires})
	})
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	url := h.cfg.PublicBase + "/#/pages/invite/invite?token=" + token
	miniProgramPath := "pages/invite/invite?scene=" + token
	var miniProgramCode string
	if h.cfg.AppID != "" && h.cfg.AppSecret != "" {
		image, codeErr := h.wechat.miniProgramCode(r.Context(), token)
		if codeErr != nil {
			h.log.Warn("generate customer mini program code", "invite_id", id, "err", codeErr)
		} else {
			miniProgramCode = "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": id, "token": token, "url": url, "mini_program_path": miniProgramPath,
		"mini_program_code": miniProgramCode, "expires_at": expires,
	})
}

func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func validPhone(phone string) bool {
	digits := strings.TrimPrefix(phone, "+")
	if len(digits) < 8 || len(digits) > 15 {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func normalizePhone(phone string) string {
	phone = strings.TrimSpace(phone)
	phone = strings.ReplaceAll(phone, " ", "")
	phone = strings.ReplaceAll(phone, "-", "")
	return phone
}
