package gallery

import (
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

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/httpx"
)

// SessionMiddleware re-validates the share token and gallery on every request.
// JWTs alone would let a revoked link keep working until expiry; the extra
// lookup is one indexed row and makes revocation immediate.
func (s *CustomerService) SessionMiddleware(tokens *auth.GalleryTokens, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Gallery session required", "")
				return
			}
			claims, err := tokens.Parse(raw)
			if err != nil {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Invalid or expired session", "")
				return
			}
			tenantID, _ := uuid.Parse(claims.TenantID)
			galleryID, _ := uuid.Parse(claims.GalleryID)
			tokenID, _ := uuid.Parse(claims.TokenID)

			err = db.WithTenant(r.Context(), s.pool, tenantID, func(tx pgx.Tx) error {
				now := time.Now()
				// Gallery state is checked before the token so a revoked album
				// surfaces as 410 gallery_revoked even though revocation also
				// stamped every token.
				g, err := loadGallery(r.Context(), tx, tenantID, galleryID, false)
				if err != nil {
					return err
				}
				if err := checkAlbumOpen(g, now); err != nil {
					return err
				}
				var (
					expiresAt *time.Time
					revokedAt *time.Time
				)
				if err := tx.QueryRow(r.Context(), `
					SELECT expires_at, revoked_at FROM gallery_tokens
					WHERE tenant_id = $1 AND id = $2`, tenantID, tokenID).Scan(&expiresAt, &revokedAt); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return ErrInvalidShareToken
					}
					return err
				}
				switch {
				case revokedAt != nil:
					return ErrShareRevoked
				case expiresAt != nil && !expiresAt.After(now):
					return ErrShareExpired
				}
				return nil
			})
			if err != nil {
				writeSessionError(w, r, log, err)
				return
			}
			gp := &auth.GalleryPrincipal{TenantID: tenantID, GalleryID: galleryID, TokenID: tokenID}
			next.ServeHTTP(w, r.WithContext(auth.WithGallery(r.Context(), gp)))
		})
	}
}

func writeSessionError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	switch {
	case errors.Is(err, ErrShareRevoked):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "session_revoked", "Share link was revoked", "")
	case errors.Is(err, ErrShareExpired):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "session_expired", "Share link has expired", "")
	case errors.Is(err, ErrGalleryRevoked):
		httpx.WriteProblem(w, r, http.StatusGone, "gallery_revoked", "Gallery was revoked", "")
	case errors.Is(err, ErrGalleryExpired):
		httpx.WriteProblem(w, r, http.StatusGone, "gallery_expired", "Gallery has expired", "")
	case errors.Is(err, ErrInvalidShareToken), errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_session", "Gallery is no longer available", "")
	default:
		httpx.Internal(w, r, log, err)
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

// securityHeaders keeps gallery responses out of shared caches and prevents the
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

// rateLimiter is a fixed-window counter. There is one API replica in the
// reference deployment; multi-replica setups need a shared store (recorded as
// a residual risk in the runbook).
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

// sessionRateKey prefers the session token so a shared office NAT does not
// exhaust the budget for every customer behind it.
func sessionRateKey(r *http.Request) string {
	if raw, ok := bearerToken(r); ok {
		sum := sha256.Sum256([]byte(raw))
		return "s:" + hex.EncodeToString(sum[:8])
	}
	return "ip:" + clientIP(r)
}
