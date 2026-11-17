package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGallerySessionRoundTrip(t *testing.T) {
	tokens := NewGalleryTokens("gallery-secret")
	tenantID, galleryID, tokenID := uuid.New(), uuid.New(), uuid.New()

	raw, exp, err := tokens.Issue(tenantID, galleryID, tokenID, time.Hour)
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
	if claims.TenantID != tenantID.String() || claims.GalleryID != galleryID.String() ||
		claims.TokenID != tokenID.String() {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.Subject != galleryID.String() {
		t.Fatalf("subject = %q, want gallery id %s", claims.Subject, galleryID)
	}
}

func TestGallerySessionIsNotAStaffToken(t *testing.T) {
	tenantID, galleryID, tokenID := uuid.New(), uuid.New(), uuid.New()
	raw, _, err := NewGalleryTokens("shared-secret").Issue(tenantID, galleryID, tokenID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	staff := NewTokens("shared-secret", time.Hour)
	if _, err := staff.Parse(raw); err == nil {
		t.Fatal("staff parser must reject gallery sessions")
	}
}

func TestStaffTokenIsNotAGallerySession(t *testing.T) {
	raw, _, err := NewTokens("shared-secret", time.Hour).Issue(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := NewGalleryTokens("shared-secret").Parse(raw); err == nil {
		t.Fatal("gallery parser must reject staff tokens")
	}
}

func TestGallerySessionRejectsExpiredAndForged(t *testing.T) {
	tenantID, galleryID, tokenID := uuid.New(), uuid.New(), uuid.New()
	expired, _, err := NewGalleryTokens("gallery-secret").Issue(tenantID, galleryID, tokenID, -time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := NewGalleryTokens("gallery-secret").Parse(expired); err == nil {
		t.Fatal("expected expired session to fail")
	}
	forged, _, err := NewGalleryTokens("other-secret").Issue(tenantID, galleryID, tokenID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := NewGalleryTokens("gallery-secret").Parse(forged); err == nil {
		t.Fatal("expected session signed with another secret to fail")
	}
}

func TestTenantFromPrefersStaffPrincipal(t *testing.T) {
	staffTenant, galleryTenant := uuid.New(), uuid.New()
	ctx := WithGallery(context.Background(), &GalleryPrincipal{TenantID: galleryTenant})

	if _, ok := TenantFrom(context.Background()); ok {
		t.Fatal("no principal should not resolve a tenant")
	}
	if got, ok := TenantFrom(context.WithValue(ctx, principalKey{}, &Principal{TenantID: staffTenant})); !ok || got != staffTenant {
		t.Fatalf("gallery context tenant = %s/%v, want %s", got, ok, staffTenant)
	}
	if got, ok := TenantFrom(ctx); !ok || got != galleryTenant {
		t.Fatalf("gallery tenant = %s/%v, want %s", got, ok, galleryTenant)
	}
}
