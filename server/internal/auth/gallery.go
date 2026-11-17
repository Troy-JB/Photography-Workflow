package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// GalleryIssuer keeps customer sessions out of the staff token namespace: the
// user parser requires issuer "pwf", so one token can never stand in for the
// other even though both are HMAC-signed with the deployment secret.
const GalleryIssuer = "pwf-gallery"

// GalleryClaims identify the share token a customer session was derived from.
// Every request re-checks that token and the gallery, so revoking either takes
// effect immediately rather than at JWT expiry.
type GalleryClaims struct {
	TenantID  string `json:"tid"`
	GalleryID string `json:"gid"`
	TokenID   string `json:"tk"`
	jwt.RegisteredClaims
}

type GalleryTokens struct {
	secret []byte
}

func NewGalleryTokens(secret string) *GalleryTokens {
	return &GalleryTokens{secret: []byte(secret)}
}

func (t *GalleryTokens) Issue(tenantID, galleryID, tokenID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	claims := &GalleryClaims{
		TenantID:  tenantID.String(),
		GalleryID: galleryID.String(),
		TokenID:   tokenID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   galleryID.String(),
			Issuer:    GalleryIssuer,
			Audience:  jwt.ClaimStrings{"gallery"},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign gallery session: %w", err)
	}
	return token, exp, nil
}

func (t *GalleryTokens) Parse(raw string) (*GalleryClaims, error) {
	claims := &GalleryClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(tk *jwt.Token) (any, error) {
		if tk.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %s", tk.Method.Alg())
		}
		return t.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(GalleryIssuer), jwt.WithAudience("gallery"), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("parse gallery session: %w", err)
	}
	if !tok.Valid {
		return nil, fmt.Errorf("invalid gallery session")
	}
	if _, err := uuid.Parse(claims.TenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant: %w", err)
	}
	if _, err := uuid.Parse(claims.GalleryID); err != nil {
		return nil, fmt.Errorf("invalid gallery: %w", err)
	}
	if _, err := uuid.Parse(claims.TokenID); err != nil {
		return nil, fmt.Errorf("invalid token id: %w", err)
	}
	return claims, nil
}

// GalleryPrincipal is the customer-side identity: no user, only a share token
// bound to one gallery.
type GalleryPrincipal struct {
	TenantID  uuid.UUID
	GalleryID uuid.UUID
	TokenID   uuid.UUID
}

type galleryPrincipalKey struct{}

func GalleryFrom(ctx context.Context) (*GalleryPrincipal, bool) {
	p, ok := ctx.Value(galleryPrincipalKey{}).(*GalleryPrincipal)
	return p, ok
}

func WithGallery(ctx context.Context, p *GalleryPrincipal) context.Context {
	return context.WithValue(ctx, galleryPrincipalKey{}, p)
}

// TenantFrom resolves the tenant for tenant-scoped middleware such as
// idempotency, for both staff and customer request contexts.
func TenantFrom(ctx context.Context) (uuid.UUID, bool) {
	if p, ok := PrincipalFrom(ctx); ok {
		return p.TenantID, true
	}
	if g, ok := GalleryFrom(ctx); ok {
		return g.TenantID, true
	}
	if p, ok := ProofFrom(ctx); ok {
		return p.TenantID, true
	}
	if p, ok := DeliveryFrom(ctx); ok {
		return p.TenantID, true
	}
	return uuid.Nil, false
}
