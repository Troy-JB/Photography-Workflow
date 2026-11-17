package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIssueAndParse(t *testing.T) {
	tokens := NewTokens("test-secret", time.Hour)
	userID, tenantID := uuid.New(), uuid.New()

	raw, exp, err := tokens.Issue(userID, tenantID, "owner")
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
	if claims.Subject != userID.String() {
		t.Fatalf("subject = %q, want %q", claims.Subject, userID)
	}
	if claims.TenantID != tenantID.String() {
		t.Fatalf("tenant = %q, want %q", claims.TenantID, tenantID)
	}
	if claims.Role != "owner" {
		t.Fatalf("role = %q, want owner", claims.Role)
	}
}

func TestParseRejectsExpired(t *testing.T) {
	tokens := NewTokens("test-secret", -time.Minute)
	raw, _, err := tokens.Issue(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := tokens.Parse(raw); err == nil {
		t.Fatal("expected expired token to fail")
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	raw, _, err := NewTokens("secret-a", time.Hour).Issue(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := NewTokens("secret-b", time.Hour).Parse(raw); err == nil {
		t.Fatal("expected token signed with another secret to fail")
	}
}

func TestParseRejectsTampered(t *testing.T) {
	tokens := NewTokens("test-secret", time.Hour)
	raw, _, err := tokens.Issue(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token format: %q", raw)
	}
	parts[1] = parts[1][:len(parts[1])-2] + "xx"
	sig := parts[2]
	flip := "A"
	if sig[0] == 'A' {
		flip = "B"
	}
	parts[2] = flip + sig[1:]
	if _, err := tokens.Parse(strings.Join(parts, ".")); err == nil {
		t.Fatal("expected tampered token to fail")
	}
}
