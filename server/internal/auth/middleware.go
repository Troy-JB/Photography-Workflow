package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"photography-workflow/server/internal/httpx"
)

type Principal struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Role     string
}

type principalKey struct{}

func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok
}

func RequireAuth(tokens *Tokens) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
				return
			}
			raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
			claims, err := tokens.Parse(raw)
			if err != nil {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "invalid_token", "Invalid token", "")
				return
			}
			userID, _ := uuid.Parse(claims.Subject)
			tenantID, _ := uuid.Parse(claims.TenantID)
			p := &Principal{UserID: userID, TenantID: tenantID, Role: claims.Role}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		})
	}
}

// RequireAnyRole enforces authorization after RequireAuth has established the
// tenant-scoped principal. Roles are checked at the route boundary so a valid
// token cannot automatically grant every tenant operation.
func RequireAnyRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
				return
			}
			if _, ok := allowed[p.Role]; !ok {
				httpx.WriteProblem(w, r, http.StatusForbidden, "forbidden", "Insufficient permissions", "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
