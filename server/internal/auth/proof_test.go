package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProofSessionRoundTrip(t *testing.T) {
	tokens := NewProofTokens("proof-secret")
	tenantID, roundID, tokenID := uuid.New(), uuid.New(), uuid.New()

	raw, exp, err := tokens.Issue(tenantID, roundID, tokenID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Fatal("expiry should be in the future")
	}
	claims, err := tokens.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.TenantID != tenantID.String() || claims.RoundID != roundID.String() ||
		claims.TokenID != tokenID.String() {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.Subject != roundID.String() {
		t.Fatalf("subject = %q, want round id %s", claims.Subject, roundID)
	}
}

func TestProofSessionIsIsolatedFromOtherNamespaces(t *testing.T) {
	tenantID, roundID, tokenID := uuid.New(), uuid.New(), uuid.New()
	proof, _, err := NewProofTokens("shared-secret").Issue(tenantID, roundID, tokenID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	staff, _, err := NewTokens("shared-secret", time.Hour).Issue(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatalf("issue staff: %v", err)
	}
	gallery, _, err := NewGalleryTokens("shared-secret").Issue(uuid.New(), uuid.New(), uuid.New(), time.Hour)
	if err != nil {
		t.Fatalf("issue gallery: %v", err)
	}

	if _, err := NewTokens("shared-secret", time.Hour).Parse(proof); err == nil {
		t.Fatal("staff parser must reject proof sessions")
	}
	if _, err := NewGalleryTokens("shared-secret").Parse(proof); err == nil {
		t.Fatal("gallery parser must reject proof sessions")
	}
	if _, err := NewProofTokens("shared-secret").Parse(staff); err == nil {
		t.Fatal("proof parser must reject staff tokens")
	}
	if _, err := NewProofTokens("shared-secret").Parse(gallery); err == nil {
		t.Fatal("proof parser must reject gallery sessions")
	}
}

func TestProofSessionRejectsExpiredAndForged(t *testing.T) {
	tenantID, roundID, tokenID := uuid.New(), uuid.New(), uuid.New()
	expired, _, err := NewProofTokens("proof-secret").Issue(tenantID, roundID, tokenID, -time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := NewProofTokens("proof-secret").Parse(expired); err == nil {
		t.Fatal("expected expired session to fail")
	}
	forged, _, err := NewProofTokens("other-secret").Issue(tenantID, roundID, tokenID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := NewProofTokens("proof-secret").Parse(forged); err == nil {
		t.Fatal("expected session signed with another secret to fail")
	}
}

func TestTenantFromResolvesProofPrincipal(t *testing.T) {
	proofTenant := uuid.New()
	ctx := WithProof(context.Background(), &ProofPrincipal{TenantID: proofTenant})
	if got, ok := TenantFrom(ctx); !ok || got != proofTenant {
		t.Fatalf("proof tenant = %s/%v, want %s", got, ok, proofTenant)
	}
}
