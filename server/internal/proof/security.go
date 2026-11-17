package proof

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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/httpx"
)

var (
	errSessionRevoked = errors.New("proof session token was revoked")
	errSessionExpired = errors.New("proof session token has expired")
	errSessionInvalid = errors.New("proof session is not valid")
	ErrGalleryRevoked = errors.New("gallery has been revoked")
)

// Guard re-validates the proof token, round and gallery on every customer
// request. JWT expiry alone would keep a revoked link alive for hours; the
// indexed lookup makes revocation immediate. A gallery's expires_at is
// deliberately ignored: proof rounds outlive the selection deadline, and
// revoking the gallery is the only global kill switch.
type Guard struct {
	pool   *pgxpool.Pool
	tokens *auth.ProofTokens
	log    *slog.Logger
}

func NewGuard(pool *pgxpool.Pool, tokens *auth.ProofTokens, log *slog.Logger) *Guard {
	return &Guard{pool: pool, tokens: tokens, log: log}
}

// Middleware expects the route to carry {roundID}. A session issued for one
// round cannot read another even inside the same gallery.
func (g *Guard) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof session required", "")
				return
			}
			claims, err := g.tokens.Parse(raw)
			if err != nil {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Invalid or expired session", "")
				return
			}
			tenantID, _ := uuid.Parse(claims.TenantID)
			roundID, _ := uuid.Parse(claims.RoundID)
			tokenID, _ := uuid.Parse(claims.TokenID)

			if routeRound := chi.URLParam(r, "roundID"); routeRound != "" && routeRound != claims.RoundID {
				httpx.WriteProblem(w, r, http.StatusForbidden, "round_mismatch", "Session belongs to another proof round", "")
				return
			}

			err = db.WithTenant(r.Context(), g.pool, tenantID, func(tx pgx.Tx) error {
				return checkProofAccess(r.Context(), tx, tenantID, roundID, tokenID)
			})
			if err != nil {
				g.writeError(w, r, err)
				return
			}
			gp := &auth.ProofPrincipal{TenantID: tenantID, RoundID: roundID, TokenID: tokenID}
			next.ServeHTTP(w, r.WithContext(auth.WithProof(r.Context(), gp)))
		})
	}
}

func checkProofAccess(ctx context.Context, tx pgx.Tx, tenantID, roundID, tokenID uuid.UUID) error {
	now := time.Now()
	var (
		expiresAt *time.Time
		revokedAt *time.Time
		galleryID uuid.UUID
	)
	err := tx.QueryRow(ctx, `
		SELECT t.expires_at, t.revoked_at, r.gallery_id
		FROM proof_round_tokens t
		JOIN proof_rounds r ON r.id = t.proof_round_id AND r.tenant_id = t.tenant_id
		WHERE t.tenant_id = $1 AND t.id = $2 AND t.proof_round_id = $3`,
		tenantID, tokenID, roundID).Scan(&expiresAt, &revokedAt, &galleryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionInvalid
		}
		return err
	}
	switch {
	case revokedAt != nil:
		return errSessionRevoked
	case expiresAt != nil && !expiresAt.After(now):
		return errSessionExpired
	}
	// The album is the global kill switch. Expiry of the selection gallery is
	// ignored on purpose.
	var status string
	var galleryRevokedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT status, revoked_at FROM galleries
		WHERE tenant_id = $1 AND id = $2`, tenantID, galleryID).Scan(&status, &galleryRevokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionInvalid
		}
		return err
	}
	if status == "revoked" || galleryRevokedAt != nil {
		return ErrGalleryRevoked
	}
	return nil
}

func (g *Guard) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errSessionRevoked):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "session_revoked", "Proof link was revoked", "")
	case errors.Is(err, errSessionExpired):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "session_expired", "Proof link has expired", "")
	case errors.Is(err, ErrGalleryRevoked):
		httpx.WriteProblem(w, r, http.StatusGone, "gallery_revoked", "Gallery was revoked", "")
	case errors.Is(err, errSessionInvalid):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Proof round is no longer available", "")
	default:
		httpx.Internal(w, r, g.log, err)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return raw, raw != ""
}

// securityHeaders keeps proof responses out of shared caches and stops the
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

// rateLimiter is a fixed-window counter. The reference deployment runs one API
// replica; multi-replica setups need shared state (recorded as a residual risk).
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
