package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/httpx"
)

var (
	errSessionInvalid = errors.New("delivery session is not valid")
	errSessionExpired = errors.New("delivery session has expired")
)

// tokenState is the gate view shared by the guard, the session exchange and
// the download grant. Every customer request reloads it so refunds and
// revocations take effect immediately instead of at JWT expiry.
type tokenState struct {
	TokenID       uuid.UUID
	TenantID      uuid.UUID
	DeliveryID    uuid.UUID
	ProjectID     uuid.UUID
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	Downloads     int
	MaxDownloads  int
	DeliveryState string
	BlockedReason string
	BuildVersion  int
	ItemCount     int
	ProjectTitle  string
	PaymentStatus string
	ProjectState  string
	RoundNo       int
	RoundStatus   string
}

func loadTokenState(ctx context.Context, tx pgx.Tx, tenantID, tokenID, deliveryID uuid.UUID,
	forUpdate bool) (*tokenState, error) {
	q := `SELECT t.id, t.tenant_id, t.delivery_id, t.expires_at, t.revoked_at, t.downloads,
			t.max_downloads, d.status, d.blocked_reason, d.build_version, d.item_count, d.project_id,
			p.title, p.payment_status,
			CASE WHEN p.state NOT IN ('cancelled','archived')
			  AND EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
			  AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
			  THEN 'cancelled' ELSE p.state END,
			r.round_no, r.status
		FROM download_tokens t
		JOIN deliveries d ON d.tenant_id = t.tenant_id AND d.id = t.delivery_id
		JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
		JOIN proof_rounds r ON r.tenant_id = d.tenant_id AND r.id = d.proof_round_id
		WHERE t.tenant_id = $1 AND t.id = $2 AND t.delivery_id = $3`
	if forUpdate {
		q += ` FOR UPDATE OF t`
	}
	var st tokenState
	err := tx.QueryRow(ctx, q, tenantID, tokenID, deliveryID).Scan(
		&st.TokenID, &st.TenantID, &st.DeliveryID, &st.ExpiresAt, &st.RevokedAt, &st.Downloads,
		&st.MaxDownloads, &st.DeliveryState, &st.BlockedReason, &st.BuildVersion, &st.ItemCount,
		&st.ProjectID, &st.ProjectTitle, &st.PaymentStatus, &st.ProjectState,
		&st.RoundNo, &st.RoundStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// checkTokenGate applies every commercial gate in a fixed order: link state,
// project state, payment, round approval and delivery readiness. Payment is
// checked on every request, so a refund invalidates an issued link at once.
func checkTokenGate(st *tokenState, now time.Time) error {
	switch {
	case st.RevokedAt != nil:
		return ErrTokenRevoked
	case !st.ExpiresAt.After(now):
		return ErrTokenExpired
	}
	if err := checkProjectState(st.ProjectState); err != nil {
		return err
	}
	if st.PaymentStatus != "paid" {
		return ErrPaymentRequired
	}
	if st.RoundStatus != "approved" {
		return ErrRoundNotApproved
	}
	if st.DeliveryState == StatusBlocked {
		if st.BlockedReason == BlockedReasonPayment {
			return ErrPaymentRequired
		}
		return ErrDeliveryNotReady
	}
	if st.DeliveryState != StatusReady {
		return ErrDeliveryNotReady
	}
	return nil
}

// Guard authenticates a delivery session and re-checks the token on every
// request. The session carries no delivery id in the URL, so the claim itself
// selects the delivery; handlers only ever query inside that delivery.
type Guard struct {
	pool   *pgxpool.Pool
	tokens *auth.DeliveryTokens
	log    *slog.Logger
}

func NewGuard(pool *pgxpool.Pool, tokens *auth.DeliveryTokens, log *slog.Logger) *Guard {
	return &Guard{pool: pool, tokens: tokens, log: log}
}

func (g *Guard) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Delivery session required", "")
				return
			}
			claims, err := g.tokens.Parse(raw)
			if err != nil {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Invalid or expired session", "")
				return
			}
			tenantID, _ := uuid.Parse(claims.TenantID)
			deliveryID, _ := uuid.Parse(claims.DeliveryID)
			tokenID, _ := uuid.Parse(claims.TokenID)

			err = db.WithTenant(r.Context(), g.pool, tenantID, func(tx pgx.Tx) error {
				st, err := loadTokenState(r.Context(), tx, tenantID, tokenID, deliveryID, false)
				if err != nil {
					return err
				}
				return checkTokenGate(st, time.Now())
			})
			if err != nil {
				g.writeError(w, r, err)
				return
			}
			dp := &auth.DeliveryPrincipal{TenantID: tenantID, DeliveryID: deliveryID, TokenID: tokenID}
			next.ServeHTTP(w, r.WithContext(auth.WithDelivery(r.Context(), dp)))
		})
	}
}

func (g *Guard) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errSessionInvalid) {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Download link is no longer available", "")
		return
	}
	fail(w, r, g.log, err)
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return raw, raw != ""
}

// securityHeaders keep delivery responses out of shared caches and stop the
// fragment token from leaking through Referer.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Pragma", "no-cache")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a fixed-window counter shared by the customer routes. Multi
// replica deployments need shared state; the single-replica reference
// deployment relies on this in-process limiter.
type rateLimiter struct {
	mu      sync.Mutex
	hits    map[string]windowHit
	limit   int
	window  time.Duration
	nowFunc func() time.Time
}

type windowHit struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:    make(map[string]windowHit),
		limit:   limit,
		window:  window,
		nowFunc: time.Now,
	}
}

func (l *rateLimiter) allow(key string) bool {
	now := l.nowFunc()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 4096 {
		for k, h := range l.hits {
			if now.After(h.reset) {
				delete(l.hits, k)
			}
		}
	}
	hit, ok := l.hits[key]
	if !ok || now.After(hit.reset) {
		l.hits[key] = windowHit{count: 1, reset: now.Add(l.window)}
		return true
	}
	if hit.count >= l.limit {
		return false
	}
	hit.count++
	l.hits[key] = hit
	return true
}

func (l *rateLimiter) middleware(keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow(keyFn(r)) {
				w.Header().Set("Retry-After", "60")
				httpx.WriteProblem(w, r, http.StatusTooManyRequests, "rate_limited", "Too many requests", "slow down and retry")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sessionRateKey prefers the session token so a shared NAT does not exhaust the
// budget for every customer behind it.
func sessionRateKey(r *http.Request) string {
	if raw, ok := bearerToken(r); ok {
		sum := sha256.Sum256([]byte(raw))
		return "s:" + hex.EncodeToString(sum[:8])
	}
	return "ip:" + clientIP(r)
}
