package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// DeliveryIssuer is a fourth namespace: a delivery session derived from a
// download token must never be accepted by the staff, selection or proof APIs,
// even though all share the deployment secret.
const DeliveryIssuer = "pwf-delivery"

// DeliveryClaims bind a customer session to one download token and delivery.
// Every request re-checks the delivery, project payment and token, so a refund
// or revocation takes effect immediately instead of at JWT expiry.
type DeliveryClaims struct {
	TenantID   string `json:"tid"`
	DeliveryID string `json:"did"`
	TokenID    string `json:"tk"`
	jwt.RegisteredClaims
}

type DeliveryTokens struct {
	secret []byte
}

func NewDeliveryTokens(secret string) *DeliveryTokens {
	return &DeliveryTokens{secret: []byte(secret)}
}

func (t *DeliveryTokens) Issue(tenantID, deliveryID, tokenID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	claims := &DeliveryClaims{
		TenantID:   tenantID.String(),
		DeliveryID: deliveryID.String(),
		TokenID:    tokenID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   deliveryID.String(),
			Issuer:    DeliveryIssuer,
			Audience:  jwt.ClaimStrings{"delivery"},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign delivery session: %w", err)
	}
	return token, exp, nil
}

func (t *DeliveryTokens) Parse(raw string) (*DeliveryClaims, error) {
	claims := &DeliveryClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(tk *jwt.Token) (any, error) {
		if tk.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %s", tk.Method.Alg())
		}
		return t.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(DeliveryIssuer), jwt.WithAudience("delivery"), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("parse delivery session: %w", err)
	}
	if !tok.Valid {
		return nil, fmt.Errorf("invalid delivery session")
	}
	if _, err := uuid.Parse(claims.TenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant: %w", err)
	}
	if _, err := uuid.Parse(claims.DeliveryID); err != nil {
		return nil, fmt.Errorf("invalid delivery: %w", err)
	}
	if _, err := uuid.Parse(claims.TokenID); err != nil {
		return nil, fmt.Errorf("invalid token id: %w", err)
	}
	return claims, nil
}

// DeliveryPrincipal is the customer-side identity for a delivery: no user, only
// a download token bound to one delivery.
type DeliveryPrincipal struct {
	TenantID   uuid.UUID
	DeliveryID uuid.UUID
	TokenID    uuid.UUID
}

type deliveryPrincipalKey struct{}

func DeliveryFrom(ctx context.Context) (*DeliveryPrincipal, bool) {
	p, ok := ctx.Value(deliveryPrincipalKey{}).(*DeliveryPrincipal)
	return p, ok
}

func WithDelivery(ctx context.Context, p *DeliveryPrincipal) context.Context {
	return context.WithValue(ctx, deliveryPrincipalKey{}, p)
}
