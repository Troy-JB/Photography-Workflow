package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ProofIssuer is a third namespace: a staff token ("pwf") and a selection
// session ("pwf-gallery") must never be accepted by the proof API, whose links
// outlive the selection deadline.
const ProofIssuer = "pwf-proof"

// ProofClaims bind a customer session to one proof round token. Every request
// re-validates the round, gallery and token, so revocation is immediate rather
// than waiting for JWT expiry.
type ProofClaims struct {
	TenantID string `json:"tid"`
	RoundID  string `json:"rid"`
	TokenID  string `json:"tk"`
	jwt.RegisteredClaims
}

type ProofTokens struct {
	secret []byte
}

func NewProofTokens(secret string) *ProofTokens {
	return &ProofTokens{secret: []byte(secret)}
}

func (t *ProofTokens) Issue(tenantID, roundID, tokenID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	claims := &ProofClaims{
		TenantID: tenantID.String(),
		RoundID:  roundID.String(),
		TokenID:  tokenID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   roundID.String(),
			Issuer:    ProofIssuer,
			Audience:  jwt.ClaimStrings{"proof"},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign proof session: %w", err)
	}
	return token, exp, nil
}

func (t *ProofTokens) Parse(raw string) (*ProofClaims, error) {
	claims := &ProofClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(tk *jwt.Token) (any, error) {
		if tk.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %s", tk.Method.Alg())
		}
		return t.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(ProofIssuer), jwt.WithAudience("proof"), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("parse proof session: %w", err)
	}
	if !tok.Valid {
		return nil, fmt.Errorf("invalid proof session")
	}
	if _, err := uuid.Parse(claims.TenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant: %w", err)
	}
	if _, err := uuid.Parse(claims.RoundID); err != nil {
		return nil, fmt.Errorf("invalid proof round: %w", err)
	}
	if _, err := uuid.Parse(claims.TokenID); err != nil {
		return nil, fmt.Errorf("invalid token id: %w", err)
	}
	return claims, nil
}

// ProofPrincipal is the customer-side identity for a proof round: no user, only
// a round token.
type ProofPrincipal struct {
	TenantID uuid.UUID
	RoundID  uuid.UUID
	TokenID  uuid.UUID
}

type proofPrincipalKey struct{}

func ProofFrom(ctx context.Context) (*ProofPrincipal, bool) {
	p, ok := ctx.Value(proofPrincipalKey{}).(*ProofPrincipal)
	return p, ok
}

func WithProof(ctx context.Context, p *ProofPrincipal) context.Context {
	return context.WithValue(ctx, proofPrincipalKey{}, p)
}
